package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestExpiredDocSnapshotPurged：外部设计文档快照正文是过程数据，工作项结束并超过保留期后清空正文，
// 保留 doc_id/标题/修订/摘要与行本身；还在执行的工作项不清。
func TestExpiredDocSnapshotPurged(t *testing.T) {
	p := newSplitPort(t, portConfig)
	wi := p.addWorkItem(7, p.base)
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', '需求文档', 2, 'sha', '机密正文', 'https://outline.example/doc/d1')`, wi)
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)

	cutoff := time.Now().Add(-12 * time.Hour)
	// 工作项还在执行：不清正文。
	if _, err := p.s.ai.Purge(p.ctx, cutoff); err != nil {
		t.Fatalf("Purge：%v", err)
	}
	if got := p.docContent(wi, "d1"); got != "机密正文" {
		t.Fatalf("在途工作项的正文不应被清：%q", got)
	}

	// 结束 48 小时：正文清空，行与元数据保留。
	p.exec(`UPDATE dev_work_items SET state='merged', active=false, finished_at=now()-interval '48 hours' WHERE id=$1`, wi)
	out, err := p.s.ai.Purge(p.ctx, cutoff)
	if err != nil {
		t.Fatalf("Purge（已结束）：%v", err)
	}
	if out["work_item_doc_snapshots"] < 1 {
		t.Fatalf("应清理至少一条文档正文，汇报 %+v", out)
	}
	if got := p.docContent(wi, "d1"); got != "" {
		t.Fatalf("结束超过保留期的正文应清空，得到 %q", got)
	}
	var title, sha string
	var revision int
	if err := p.s.db.QueryRow(p.ctx, `SELECT title, revision, content_sha256 FROM work_item_doc_snapshots WHERE work_item_id=$1 AND doc_id='d1'`, wi).
		Scan(&title, &revision, &sha); err != nil {
		t.Fatalf("元数据应保留：%v", err)
	}
	if title != "需求文档" || revision != 2 || sha != "sha" {
		t.Fatalf("元数据被破坏：%q %d %q", title, revision, sha)
	}
}

// docContent 返回工作项某篇快照的正文（不存在时为空）。
func (p *splitPort) docContent(wi int64, docID string) string {
	p.t.Helper()
	var content string
	if err := p.s.db.QueryRow(p.ctx, `SELECT content FROM work_item_doc_snapshots WHERE work_item_id=$1 AND doc_id=$2`, wi, docID).
		Scan(&content); err != nil {
		p.t.Fatalf("读取快照正文：%v", err)
	}
	return content
}

// snapshotSet 写入一个独立审计的文档快照集（age 之前的 frozen_at）；reqID 非 0 时把这个审计请求改为引用它。
func (p *splitPort) snapshotSet(reqID int64, age time.Duration, contents ...string) int64 {
	p.t.Helper()
	var id int64
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `INSERT INTO doc_snapshot_sets (repo_id, provider, base_url, collection_id, binding_generation,
		doc_count, manifest_digest, frozen_at)
		VALUES ($1, 'outline', 'https://outline.example.com', 'col1', 1, $2, repeat('a', 64), now() - $3::interval) RETURNING id`,
		portRepoID, len(contents), age.String()).Scan(&id))
	for i, content := range contents {
		p.exec(`INSERT INTO doc_snapshot_docs (snapshot_set_id, doc_id, title, revision, content_sha256, content, url)
			VALUES ($1, $2, $3, 1, repeat('b', 64), $4, 'https://outline.example/doc/' || $2)`,
			id, fmt.Sprintf("d%d", i+1), fmt.Sprintf("文档 %d", i+1), content)
	}
	if reqID != 0 {
		p.exec(`UPDATE ai_review_requests SET docs_source='snapshot_set', doc_snapshot_set_id=$2, work_item_id=NULL,
			docs_manifest_digest=repeat('a', 64) WHERE request_id=$1`, reqID, id)
	}
	return id
}

// setState 返回快照集的清理状态：正文列表、content_purged_at 是否存在、行是否还在。
func (p *splitPort) setState(id int64) (contents []string, purged bool, exists bool) {
	p.t.Helper()
	var purgedAt *time.Time
	err := p.s.db.QueryRow(p.ctx, `SELECT content_purged_at FROM doc_snapshot_sets WHERE id=$1`, id).Scan(&purgedAt)
	if err != nil {
		return nil, false, false
	}
	rows, err := p.s.db.Query(p.ctx, `SELECT content FROM doc_snapshot_docs WHERE snapshot_set_id=$1 ORDER BY doc_id`, id)
	if err != nil {
		p.t.Fatalf("读取快照集正文：%v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		mustNil(p.t, rows.Scan(&c))
		contents = append(contents, c)
	}
	mustNil(p.t, rows.Err())
	return contents, purgedAt != nil, true
}

// TestDocSnapshotSetRetention：独立审计快照集是过程数据：引用都结束并超过保留期后清空正文（保留元数据与引用关系）；
// 还在排队 / 执行或刚结束的引用一律保护。
func TestDocSnapshotSetRetention(t *testing.T) {
	cutoff := time.Now().Add(-12 * time.Hour)
	// setup 规划一个审计请求并给它挂一个 48 小时前的快照集。
	setup := func(t *testing.T) (*splitPort, int64, int64) {
		t.Helper()
		p := newSplitPort(t, portConfig)
		req := p.planReview(7, p.prHead(7))
		set := p.snapshotSet(req.id, 48*time.Hour, "机密正文一", "机密正文二")
		return p, req.id, set
	}

	t.Run("在途引用保护", func(t *testing.T) {
		p, reqID, set := setup(t)
		p.exec(`UPDATE ai_requests SET state='queued', finished_at=NULL WHERE id=$1`, reqID)
		if _, err := p.s.ai.Purge(p.ctx, cutoff); err != nil {
			t.Fatalf("Purge：%v", err)
		}
		if contents, purged, exists := p.setState(set); !exists || purged || contents[0] != "机密正文一" {
			t.Fatalf("在途审计的快照集不应被清理：%v purged=%v exists=%v", contents, purged, exists)
		}
	})

	t.Run("刚结束的引用保护", func(t *testing.T) {
		p, reqID, set := setup(t)
		p.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() - interval '1 hour' WHERE id=$1`, reqID)
		if _, err := p.s.ai.Purge(p.ctx, cutoff); err != nil {
			t.Fatalf("Purge：%v", err)
		}
		if contents, purged, _ := p.setState(set); purged || contents[0] != "机密正文一" {
			t.Fatalf("没到保留期的引用应保护正文：%v purged=%v", contents, purged)
		}
	})

	t.Run("引用结束后清空正文", func(t *testing.T) {
		p, reqID, set := setup(t)
		p.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() - interval '48 hours' WHERE id=$1`, reqID)
		out, err := p.s.ai.Purge(p.ctx, cutoff)
		if err != nil {
			t.Fatalf("Purge：%v", err)
		}
		if out["doc_snapshot_sets_purged"] != 1 || out["doc_snapshot_docs"] != 2 {
			t.Fatalf("清理汇报 = %+v", out)
		}
		contents, purged, exists := p.setState(set)
		if !exists || !purged || len(contents) != 2 || contents[0] != "" || contents[1] != "" {
			t.Fatalf("正文应清空且保留快照集：%v purged=%v exists=%v", contents, purged, exists)
		}
		// 元数据保留：引用关系与文档行都还在，审计还能说明依据过什么。
		var docs int
		mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT count(*) FROM doc_snapshot_docs WHERE snapshot_set_id=$1 AND title<>''`, set).Scan(&docs))
		if docs != 2 {
			t.Fatalf("元数据应保留 2 行，得到 %d", docs)
		}
		var link *int64
		mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT doc_snapshot_set_id FROM ai_review_requests WHERE request_id=$1`, reqID).Scan(&link))
		if link == nil || *link != set {
			t.Fatalf("引用关系应保留：%v", link)
		}
		// 第二次清理不重复汇报。
		again, err := p.s.ai.Purge(p.ctx, cutoff)
		if err != nil {
			t.Fatalf("Purge（第二次）：%v", err)
		}
		if again["doc_snapshot_sets_purged"] != 0 || again["doc_snapshot_docs"] != 0 {
			t.Fatalf("已清理的快照集不应重复清理：%+v", again)
		}
	})
}

// TestDocSnapshotSetOrphanPurge：没有引用的快照集：超过保留期才整集删除，没到保留期保留。
func TestDocSnapshotSetOrphanPurge(t *testing.T) {
	cutoff := time.Now().Add(-12 * time.Hour)

	t.Run("孤立集整集删除", func(t *testing.T) {
		p := newSplitPort(t, portConfig)
		orphan := p.snapshotSet(0, 48*time.Hour, "没人读的正文")
		req := p.planReview(7, p.prHead(7))
		referenced := p.snapshotSet(req.id, 48*time.Hour, "有引用的正文")
		p.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() - interval '48 hours' WHERE id=$1`, req.id)
		out, err := p.s.ai.Purge(p.ctx, cutoff)
		if err != nil {
			t.Fatalf("Purge：%v", err)
		}
		if out["doc_snapshot_sets"] != 1 {
			t.Fatalf("应删除 1 个孤立集：%+v", out)
		}
		if _, _, exists := p.setState(orphan); exists {
			t.Fatal("孤立集应整集删除")
		}
		var n int
		mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT count(*) FROM doc_snapshot_docs WHERE snapshot_set_id=$1`, orphan).Scan(&n))
		if n != 0 {
			t.Fatalf("孤立集的文档行应一并删除：%d", n)
		}
		// 同一批里有引用的集只清正文，不整集删除（清理按引用分别处理）。
		if contents, purged, exists := p.setState(referenced); !exists || !purged || contents[0] != "" {
			t.Fatalf("有引用的集应清正文：%v purged=%v exists=%v", contents, purged, exists)
		}
	})

	t.Run("没到保留期的孤立集保留", func(t *testing.T) {
		p := newSplitPort(t, portConfig)
		orphan := p.snapshotSet(0, time.Hour, "刚冻结的正文")
		if _, err := p.s.ai.Purge(p.ctx, cutoff); err != nil {
			t.Fatalf("Purge：%v", err)
		}
		if contents, _, exists := p.setState(orphan); !exists || contents[0] != "刚冻结的正文" {
			t.Fatalf("没到保留期的孤立集应保留：%v exists=%v", contents, exists)
		}
	})
}

// TestDocSetPurgeWaitsForLock：清理与引用快照集的一方（重跑固定依据时的 LockBasisTx）用同一把咨询锁；
// 锁被持有时清理必须等它释放，且**拿到锁之后重新检查引用**：持锁期间提交的新受保护引用必须挡住这次清理
// （把引用查询挪到拿锁之前就会漏掉它）。
func TestDocSetPurgeWaitsForLock(t *testing.T) {
	p := newSplitPort(t, portConfig)
	req := p.planReview(7, p.prHead(7))
	set := p.snapshotSet(req.id, 48*time.Hour, "正文")
	p.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() - interval '48 hours' WHERE id=$1`, req.id)

	locked := make(chan struct{})
	added := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(added) }) }
	defer release() // 断言失败时也要放开持锁方，避免把连接挂在测试里
	held := make(chan error, 1)
	go func() {
		held <- p.s.db.WithAdvisoryLock(p.ctx, docsync.DocSetLockKey(set), func(ctx context.Context, conn *pgxpool.Conn) error {
			close(locked)
			<-added
			// 持锁期间另一个事务提交一条受保护的新引用（排队 / 执行中），随后释放锁。
			return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
				return insertSetRef(ctx, tx, set, "active")
			})
		})
	}()
	<-locked

	done := make(chan error, 1)
	go func() {
		_, err := p.s.ai.Purge(p.ctx, time.Now().Add(-12*time.Hour))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("锁被持有时清理不应完成（err=%v）", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if err := <-held; err != nil {
		t.Fatalf("持锁事务：%v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("释放锁后 Purge：%v", err)
	}
	if contents, purged, exists := p.setState(set); !exists || purged || contents[0] != "正文" {
		t.Fatalf("锁后提交的受保护引用应保护正文：%v purged=%v exists=%v", contents, purged, exists)
	}
}

// insertSetRef 写入一个引用快照集的审计请求（state 决定是否受保护），返回请求 ID。
func insertSetRef(ctx context.Context, tx pgx.Tx, setID int64, state string) error {
	var reqID int64
	if err := tx.QueryRow(ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, source, family_id, state)
		SELECT id, 'review', $1, 'pull_request', 'refs/heads/y', 'def', 'webhook', id, $2 FROM n RETURNING id`,
		portRepoID, state).Scan(&reqID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO ai_review_requests (request_id, docs_source, doc_snapshot_set_id, docs_manifest_digest)
		VALUES ($1, 'snapshot_set', $2, repeat('a', 64))`, reqID, setID)
	return err
}

// TestWorkItemPurgeWaitsForLock：工作项文档清理同样在拿锁后重新检查关联审计引用：持锁期间提交的受保护引用
// （排队 / 执行中）必须让这次清理放行，正文与 content_purged_at 都保持原样。
func TestWorkItemPurgeWaitsForLock(t *testing.T) {
	p := newSplitPort(t, portConfig)
	wi := p.addWorkItem(7, p.base)
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', '需求文档', 2, repeat('c', 64), '机密正文', 'u1')`, wi)
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	p.exec(`UPDATE dev_work_items SET state='merged', active=false, finished_at=now()-interval '48 hours' WHERE id=$1`, wi)

	locked := make(chan struct{})
	added := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(added) }) }
	defer release() // 断言失败时也要放开持锁方，避免把连接挂在测试里
	held := make(chan error, 1)
	go func() {
		held <- p.s.db.WithAdvisoryLock(p.ctx, docsync.DocFreezeLockKey(wi), func(ctx context.Context, conn *pgxpool.Conn) error {
			close(locked)
			<-added
			return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
				var reqID int64
				if err := tx.QueryRow(ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
					INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, source, family_id, state)
					SELECT id, 'review', $1, 'pull_request', 'refs/heads/z', 'ghi', 'webhook', id, 'active' FROM n RETURNING id`,
					portRepoID).Scan(&reqID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO ai_review_requests (request_id, work_item_id, docs_source, docs_manifest_digest)
					VALUES ($1, $2, 'work_item', repeat('a', 64))`, reqID, wi)
				return err
			})
		})
	}()
	<-locked

	done := make(chan error, 1)
	go func() {
		_, err := p.s.ai.Purge(p.ctx, time.Now().Add(-12*time.Hour))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("锁被持有时清理不应完成（err=%v）", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if err := <-held; err != nil {
		t.Fatalf("持锁事务：%v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("释放锁后 Purge：%v", err)
	}
	if got := p.docContent(wi, "d1"); got != "机密正文" {
		t.Fatalf("锁后提交的受保护引用应保护工作项正文，得到 %q", got)
	}
	var purgedAt *time.Time
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT content_purged_at FROM work_item_doc_freezes WHERE work_item_id=$1`, wi).Scan(&purgedAt))
	if purgedAt != nil {
		t.Fatalf("受保护的工作项不应被标记清理：%v", purgedAt)
	}
}

// TestWorkItemOldCleanerEmptyNotRepurged：旧清理器留下的空正文（没有 content_purged_at）不再被扫描、也不补记清理标记：
// 空正文本身已视为过期，伪造标记会让"从未清理"与"已清理"两种历史状态混淆。
func TestWorkItemOldCleanerEmptyNotRepurged(t *testing.T) {
	p := newSplitPort(t, portConfig)
	wi := p.addWorkItem(7, p.base)
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, repeat('c', 64), '', 'u1')`, wi)
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	p.exec(`UPDATE dev_work_items SET state='merged', active=false, finished_at=now()-interval '48 hours' WHERE id=$1`, wi)

	out, err := p.s.ai.Purge(p.ctx, time.Now().Add(-12*time.Hour))
	if err != nil {
		t.Fatalf("Purge：%v", err)
	}
	if n := out["work_item_doc_snapshots"]; n != 0 {
		t.Fatalf("没有正文可清时不应汇报清理数量：%d", n)
	}
	var purgedAt *time.Time
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT content_purged_at FROM work_item_doc_freezes WHERE work_item_id=$1`, wi).Scan(&purgedAt))
	if purgedAt != nil {
		t.Fatalf("不应为旧清理器留下的空正文补记 content_purged_at：%v", purgedAt)
	}
}
