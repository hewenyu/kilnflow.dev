package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 的过程数据清理（保留策略，PLAN #10.3、#30.1、#33.10）：结束超过保留期的请求删除各次 attempt 的日志、清空执行关联
// （Orca 的 runtime / run / dispatch / worktree 和进度），记下 purged_at；AI 开发的候选快照（领取时核对用的过程数据）在执行
// 结束、超过保留期后删除，还在执行的仍要按它核对领取。结果（状态、原因、报告、交付、结果摘要）长期保留。由组装层注册到
// 保留策略的 Collector（retention.Purger）。

// purgeBatch 是每次读取的待清理请求数。
const purgeBatch = 100

// Purge 清理结束时间早于 cutoff 的 AI 过程数据。
func (c *Core) Purge(ctx context.Context, cutoff time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	var errs []error
	var after int64
	for ctx.Err() == nil {
		ids, err := store.QueryIDs(ctx, c.db, `SELECT id FROM ai_requests WHERE finished_at IS NOT NULL AND purged_at IS NULL AND finished_at < $1
			AND id > $2 ORDER BY id LIMIT $3`, cutoff, after, purgeBatch)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取待清理的 AI 请求：%w", err))
			break
		}
		for _, id := range ids {
			p, err := c.purgeRequest(ctx, id)
			if err != nil {
				errs = append(errs, fmt.Errorf("清理 AI 请求 #%d：%w", id, err))
				continue
			}
			out["ai_requests"]++
			out["ai_logs"] += p.logs
			out["ai_orca"] += p.orca
		}
		if len(ids) < purgeBatch {
			break
		}
		after = ids[len(ids)-1]
	}
	tag, err := c.db.Exec(ctx, `DELETE FROM dev_offers o WHERE o.updated_at < $1
		AND NOT EXISTS (SELECT 1 FROM ai_attempts a WHERE a.id=o.attempt_id AND a.finished_at IS NULL)`, cutoff)
	if err != nil {
		errs = append(errs, fmt.Errorf("清理 AI 开发的候选快照：%w", err))
	} else {
		out["dev_offers"] = tag.RowsAffected()
	}
	// 外部设计文档快照的正文是过程数据（PLAN #10.3）：独立审计的快照集与工作项快照都只保留元数据，正文在保留期后清空。
	// 清理与"新建引用 / 重跑提交"共用同一把对象锁（kiln:doc-set:<id> / kiln:doc-freeze:<workItemID>），拿锁后重新核对引用，
	// 保证不会出现"刚清正文却建成执行任务"或"刚建任务却被清正文"。
	errs = append(errs, c.purgeDocSets(ctx, cutoff, out)...)
	errs = append(errs, c.purgeWorkItemDocs(ctx, cutoff, out)...)
	return out, errors.Join(errs...)
}

// docSetPurge 是清理一个独立审计快照集的结果。
type docSetPurge struct {
	deleted bool
	purged  bool
	docs    int64
}

// purgeDocSets 清理已过保留期的独立审计文档快照集：没有引用的孤立集整集删除；有引用且引用都已结束、超期的清空正文并
// 记 content_purged_at（保留元数据与 doc_snapshot_sets 记录）。还在排队 / 执行、或刚结束的引用一律跳过。
func (c *Core) purgeDocSets(ctx context.Context, cutoff time.Time, out map[string]int64) []error {
	var errs []error
	var after int64
	for ctx.Err() == nil {
		ids, err := store.QueryIDs(ctx, c.db, `SELECT id FROM doc_snapshot_sets
			WHERE content_purged_at IS NULL AND frozen_at < $1 AND id > $2 ORDER BY id LIMIT $3`, cutoff, after, purgeBatch)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取待清理的文档快照集：%w", err))
			break
		}
		for _, id := range ids {
			res, err := c.purgeDocSet(ctx, id, cutoff)
			if err != nil {
				errs = append(errs, fmt.Errorf("清理文档快照集 #%d：%w", id, err))
				continue
			}
			switch {
			case res.deleted:
				out["doc_snapshot_sets"]++
			case res.purged:
				out["doc_snapshot_sets_purged"]++
				out["doc_snapshot_docs"] += res.docs
			}
		}
		if len(ids) < purgeBatch {
			break
		}
		after = ids[len(ids)-1]
	}
	return errs
}

// purgeDocSet 在一个事务里、持快照集的锁清理一个集。
func (c *Core) purgeDocSet(ctx context.Context, id int64, cutoff time.Time) (docSetPurge, error) {
	var res docSetPurge
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		res = docSetPurge{}
		if err := store.LockXact(ctx, tx, docsync.DocSetLockKey(id)); err != nil {
			return err
		}
		var (
			frozen time.Time
			purged *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT frozen_at, content_purged_at FROM doc_snapshot_sets WHERE id=$1`, id).Scan(&frozen, &purged)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || purged != nil {
			return err
		}
		var refs, protected int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (
				WHERE r.state IN ('queued','active') OR r.finished_at IS NULL OR r.finished_at >= $2)
			FROM ai_review_requests d JOIN ai_requests r ON r.id=d.request_id
			WHERE d.doc_snapshot_set_id=$1`, id, cutoff).Scan(&refs, &protected); err != nil {
			return err
		}
		if protected > 0 {
			return nil // 还有在途或没到保留期的引用
		}
		if refs == 0 {
			if frozen.Before(cutoff) {
				if _, err := tx.Exec(ctx, `DELETE FROM doc_snapshot_sets WHERE id=$1`, id); err != nil {
					return err
				}
				res.deleted = true
			}
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE doc_snapshot_docs SET content='' WHERE snapshot_set_id=$1 AND content <> ''`, id)
		if err != nil {
			return err
		}
		res.docs = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `UPDATE doc_snapshot_sets SET content_purged_at=now() WHERE id=$1 AND content_purged_at IS NULL`, id)
		if err != nil {
			return err
		}
		res.purged = tag.RowsAffected() > 0
		return nil
	})
	return res, err
}

// purgeWorkItemDocs 清理已结束、超期的工作项文档快照正文。工作项重新激活（active）或仍有在途 / 未到保留期的审计引用时跳过。
// 候选只包含仍有正文的冻结：旧清理器留下的空正文本身已视为过期，既不补记 content_purged_at，也不会被反复扫描。
func (c *Core) purgeWorkItemDocs(ctx context.Context, cutoff time.Time, out map[string]int64) []error {
	var errs []error
	var after int64
	for ctx.Err() == nil {
		ids, err := store.QueryIDs(ctx, c.db, `SELECT f.work_item_id FROM work_item_doc_freezes f
			JOIN dev_work_items w ON w.id=f.work_item_id
			WHERE f.content_purged_at IS NULL AND NOT w.active AND w.finished_at IS NOT NULL AND w.finished_at < $1
			AND EXISTS (SELECT 1 FROM work_item_doc_snapshots s WHERE s.work_item_id = f.work_item_id AND s.content <> '')
			AND f.work_item_id > $2 ORDER BY f.work_item_id LIMIT $3`, cutoff, after, purgeBatch)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取待清理的工作项文档快照：%w", err))
			break
		}
		for _, id := range ids {
			n, purged, err := c.purgeWorkItemDocsTx(ctx, id, cutoff)
			if err != nil {
				errs = append(errs, fmt.Errorf("清理工作项 #%d 的文档快照：%w", id, err))
				continue
			}
			if purged {
				out["work_item_doc_snapshots"] += n
			}
		}
		if len(ids) < purgeBatch {
			break
		}
		after = ids[len(ids)-1]
	}
	return errs
}

// purgeWorkItemDocsTx 在一个事务里、持工作项冻结的锁清理一个工作项，返回清空的正文行数以及是否真的清理了。
func (c *Core) purgeWorkItemDocsTx(ctx context.Context, workItemID int64, cutoff time.Time) (int64, bool, error) {
	var (
		n      int64
		purged bool
	)
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		n, purged = 0, false
		if err := store.LockXact(ctx, tx, docsync.DocFreezeLockKey(workItemID)); err != nil {
			return err
		}
		var (
			active   bool
			finished *time.Time
			purgedAt *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT w.active, w.finished_at, f.content_purged_at
			FROM dev_work_items w JOIN work_item_doc_freezes f ON f.work_item_id=w.id
			WHERE w.id=$1`, workItemID).Scan(&active, &finished, &purgedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || active || finished == nil || !finished.Before(cutoff) || purgedAt != nil {
			return err
		}
		var protected int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ai_review_requests rr JOIN ai_requests r ON r.id=rr.request_id
			WHERE rr.work_item_id=$1 AND rr.docs_source IN ('work_item','legacy')
			AND (r.state IN ('queued','active') OR r.finished_at IS NULL OR r.finished_at >= $2)`, workItemID, cutoff).Scan(&protected); err != nil {
			return err
		}
		if protected > 0 {
			return nil // 在途审计还要读正文
		}
		tag, err := tx.Exec(ctx, `UPDATE work_item_doc_snapshots SET content='' WHERE work_item_id=$1 AND content <> ''`, workItemID)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		if _, err := tx.Exec(ctx, `UPDATE work_item_doc_freezes SET content_purged_at=now()
			WHERE work_item_id=$1 AND content_purged_at IS NULL`, workItemID); err != nil {
			return err
		}
		purged = true
		return nil
	})
	return n, purged, err
}

// purged 是一个请求清掉的过程数据数量。
type purged struct {
	// logs 是删掉的日志数（每删除一个 attempt 的日志加一）。
	logs int64
	// orca 是清空执行关联和进度的 attempt 数。
	orca int64
}

// purgeRequest 在一个事务中删除请求的各次 attempt 的日志、清空它们的执行关联（orca、progress，都是过程数据，PLAN #33.1 O5、
// #33.10），并记下 purged_at。结果（outcome、error_class、message、result_digest、结果本身、时间和状态）保留。
func (c *Core) purgeRequest(ctx context.Context, id int64) (purged, error) {
	out := purged{}
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		out = purged{}
		ids, err := store.QueryIDs(ctx, tx, `SELECT a.id FROM ai_attempts a JOIN ai_tasks t ON t.id=a.task_id WHERE t.request_id=$1`, id)
		if err != nil {
			return err
		}
		for _, aid := range ids {
			if err := c.logs.Delete(aid); err != nil {
				return err
			}
			out.logs++
		}
		// 执行关联和进度是过程数据：清成空对象，保留结果和结果摘要（同一次 Complete 的重放仍按摘要识别）。已经被标记
		// purged 的旧请求不会再次清理（purged_at 非空），但 orca 是本次新增的列、默认 '{}'，现有数据中没有需要补清理的行。
		tag, err := tx.Exec(ctx, `UPDATE ai_attempts SET orca='{}', progress='{}'
			WHERE task_id IN (SELECT id FROM ai_tasks WHERE request_id=$1) AND (orca<>'{}' OR progress IS DISTINCT FROM '{}')`, id)
		if err != nil {
			return err
		}
		out.orca = tag.RowsAffected()
		_, err = tx.Exec(ctx, `UPDATE ai_requests SET purged_at=now() WHERE id=$1`, id)
		return err
	})
	return out, err
}
