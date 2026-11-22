package server

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 文档 RPC 的授权边界：以数据库关系为权威，任务 spec 必须与数据库一致；跨仓库、来源 / 摘要 / 快照集
// 不一致、工作项快照缺失或过期时一律拒绝，绝不放行后来才出现的文档。

// docRPC 调用两个文档 RPC，返回列表结果与第一个错误。
func (p *splitPort) docRPC(r *portRunner, task *portTask, docID string) (*airunnerv2.ListTaskDocsResponse, error) {
	p.t.Helper()
	lr, err := p.ai.ListTaskDocs(p.ctx, withToken(r.token, &airunnerv2.ListTaskDocsRequest{Ref: ref(task)}))
	if err != nil {
		return nil, err
	}
	if docID != "" {
		if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: ref(task), DocId: docID})); err != nil {
			return lr.Msg, err
		}
	}
	return lr.Msg, nil
}

// insertRepo 写入另一个仓库（跨仓库用例）。
func (p *splitPort) insertRepo(id int64) {
	p.t.Helper()
	p.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES ($1, 1, 100, 'alice', $2, true, 'main', 'https://example.com/alice/r.git')`, id, fmt.Sprintf("other-%d", id))
}

// TestDocRPCSnapshotSetAuthorization：独立审计的快照集只能被同仓库的、spec 与数据库一致的审计任务读取。
func TestDocRPCSnapshotSetAuthorization(t *testing.T) {
	setup := func(t *testing.T) (*splitPort, *portRunner, *portTask, int64) {
		t.Helper()
		p := newSplitPort(t, portConfig)
		req := p.planReview(7, p.prHead(7))
		set := p.snapshotSet(req.id, time.Hour, "正文")
		// 与 detail 提交时回填的形态一致。
		p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('docs_source','snapshot_set',
			'doc_snapshot_set_id',$2::bigint,'docs_manifest_digest',repeat('a',64)) WHERE request_id=$1`, req.id, set)
		r := p.runner(runnerSpec{})
		task, err := p.acquire(r, "doc-set-authz")
		if err != nil || task == nil {
			t.Fatalf("领取审计任务：%v %+v", err, task)
		}
		return p, r, task, set
	}
	wantRejected := func(t *testing.T, p *splitPort, r *portRunner, task *portTask) {
		t.Helper()
		if _, err := p.docRPC(r, task, "d1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("应 FailedPrecondition，得到 %v", err)
		}
	}

	t.Run("合法", func(t *testing.T) {
		p, r, task, _ := setup(t)
		msg, err := p.docRPC(r, task, "d1")
		if err != nil || len(msg.Docs) != 1 || msg.Docs[0].Id != "d1" {
			t.Fatalf("合法读取 = %+v, %v", msg.GetDocs(), err)
		}
	})

	t.Run("跨仓库的快照集", func(t *testing.T) {
		p, r, task, set := setup(t)
		p.insertRepo(portRepoID + 500)
		p.exec(`UPDATE doc_snapshot_sets SET repo_id=$2 WHERE id=$1`, set, portRepoID+500)
		wantRejected(t, p, r, task)
	})

	t.Run("spec 来源与明细不一致", func(t *testing.T) {
		p, r, task, _ := setup(t)
		p.exec(`UPDATE ai_tasks SET spec = spec || '{"docs_source":"none"}'::jsonb WHERE id=$1`, task.id)
		wantRejected(t, p, r, task)
	})

	t.Run("spec 摘要与明细不一致", func(t *testing.T) {
		p, r, task, _ := setup(t)
		p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('docs_manifest_digest',repeat('c',64)) WHERE id=$1`, task.id)
		wantRejected(t, p, r, task)
	})

	t.Run("spec 快照集与明细不一致", func(t *testing.T) {
		p, r, task, set := setup(t)
		p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('doc_snapshot_set_id',$2::bigint) WHERE id=$1`, task.id, set+1)
		wantRejected(t, p, r, task)
	})

	t.Run("缺少来源的 spec 不放行", func(t *testing.T) {
		p, r, task, _ := setup(t)
		p.exec(`UPDATE ai_tasks SET spec = spec - 'docs_source' WHERE id=$1`, task.id)
		wantRejected(t, p, r, task)
	})

	t.Run("租约过期", func(t *testing.T) {
		p, r, task, _ := setup(t)
		p.expireLease(task)
		if _, err := p.docRPC(r, task, "d1"); err == nil {
			t.Fatal("租约过期后不应读到文档")
		}
	})
}

// TestDocRPCWorkItemAuthorization：工作项审计只读工作项冻结的快照；来源为 none / unplanned 时绝不放行后来冻结的文档；
// legacy 只有在 spec 引用与快照逐篇一致时才放行。
func TestDocRPCWorkItemAuthorization(t *testing.T) {
	setup := func(t *testing.T) (*splitPort, *portRunner, *portTask, int64) {
		t.Helper()
		p := newSplitPort(t, portConfig)
		head := p.prHead(7)
		wi := p.addWorkItem(7, head)
		sha := docprovider.Sum("正文")
		p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
			VALUES ($1, 'd1', '需求文档', 2, $2, '正文', 'https://outline.example/doc/d1')`, wi, sha)
		p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
		req := p.planReview(7, head)
		var source string
		if err := p.s.db.QueryRow(p.ctx, `SELECT docs_source FROM ai_review_requests WHERE request_id=$1`, req.id).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if source != "work_item" {
			t.Fatalf("工作项审计的文档来源 = %q", source)
		}
		r := p.runner(runnerSpec{})
		task, err := p.acquire(r, "doc-wi-authz")
		if err != nil || task == nil {
			t.Fatalf("领取审计任务：%v %+v", err, task)
		}
		return p, r, task, wi
	}

	t.Run("合法", func(t *testing.T) {
		p, r, task, _ := setup(t)
		msg, err := p.docRPC(r, task, "d1")
		if err != nil || len(msg.Docs) != 1 || msg.Docs[0].Id != "d1" || msg.Docs[0].Title != "需求文档" {
			t.Fatalf("合法读取 = %+v, %v", msg.GetDocs(), err)
		}
	})

	t.Run("正文已过保留期", func(t *testing.T) {
		p, r, task, wi := setup(t)
		p.exec(`UPDATE work_item_doc_snapshots SET content='' WHERE work_item_id=$1`, wi)
		// 元数据仍可列出，正文不可读。
		msg, err := p.docRPC(r, task, "")
		if err != nil || len(msg.Docs) != 1 {
			t.Fatalf("清理后仍应列出元数据：%+v, %v", msg.GetDocs(), err)
		}
		if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: ref(task), DocId: "d1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("过期的正文应 FailedPrecondition，得到 %v", err)
		}
	})

	t.Run("工作项快照不存在", func(t *testing.T) {
		p, r, task, wi := setup(t)
		p.exec(`DELETE FROM work_item_doc_freezes WHERE work_item_id=$1`, wi)
		p.exec(`DELETE FROM work_item_doc_snapshots WHERE work_item_id=$1`, wi)
		if _, err := p.docRPC(r, task, "d1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("快照不存在应 FailedPrecondition，得到 %v", err)
		}
	})

	noDocs := func(t *testing.T, source, digest string) {
		t.Helper()
		p, r, task, _ := setup(t)
		p.exec(`UPDATE ai_review_requests SET docs_source=$2, docs_manifest_digest=$3 WHERE request_id=(SELECT request_id FROM ai_tasks WHERE id=$1)`,
			task.id, source, digest)
		p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('docs_source',$2::text,'docs_manifest_digest',$3::text) WHERE id=$1`,
			task.id, source, digest)
		msg, err := p.docRPC(r, task, "")
		if err != nil || len(msg.Docs) != 0 {
			t.Fatalf("%s 来源不应有文档：%+v, %v", source, msg.GetDocs(), err)
		}
		if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: ref(task), DocId: "d1"})); err == nil {
			t.Fatalf("%s 来源不应读到正文", source)
		}
	}
	t.Run("none 不放行", func(t *testing.T) {
		noDocs(t, "none", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	})
	t.Run("unplanned 不放行", func(t *testing.T) { noDocs(t, "unplanned", "") })

	// legacy 是迁移前的来源：spec 里没有来源/摘要字段，只有旧引用。expr 是在原 spec 上去掉这些字段后的表达式。
	legacy := func(t *testing.T, expr string) (*splitPort, *portRunner, *portTask) {
		t.Helper()
		p, r, task, _ := setup(t)
		p.exec(`UPDATE ai_review_requests SET docs_source='legacy', docs_manifest_digest='' WHERE request_id=(SELECT request_id FROM ai_tasks WHERE id=$1)`, task.id)
		p.exec(`UPDATE ai_tasks SET spec = `+expr+` WHERE id=$1`, task.id)
		return p, r, task
	}
	const plain = `spec - 'docs_source' - 'docs_manifest_digest'`
	t.Run("legacy 引用一致时放行", func(t *testing.T) {
		p, r, task := legacy(t, plain)
		msg, err := p.docRPC(r, task, "d1")
		if err != nil || len(msg.Docs) != 1 || msg.Docs[0].Id != "d1" {
			t.Fatalf("legacy 引用一致应放行：%+v, %v", msg.GetDocs(), err)
		}
	})
	t.Run("legacy 引用不一致时不放行", func(t *testing.T) {
		p, r, task := legacy(t, `jsonb_set(`+plain+`, '{docs,0,title}', '"改过的标题"')`)
		msg, err := p.docRPC(r, task, "")
		if err != nil || len(msg.Docs) != 0 {
			t.Fatalf("legacy 引用不一致不应放行：%+v, %v", msg.GetDocs(), err)
		}
	})
	t.Run("legacy 没有引用时不放行", func(t *testing.T) {
		p, r, task := legacy(t, plain+` - 'docs'`)
		msg, err := p.docRPC(r, task, "")
		if err != nil || len(msg.Docs) != 0 {
			t.Fatalf("legacy 没有引用不应放行：%+v, %v", msg.GetDocs(), err)
		}
	})
}

// TestDocRPCDevRepoIsolation：开发任务只能读本仓库工作项的快照；spec 指向别的仓库的工作项时拒绝（不泄露其他仓库的文档）。
func TestDocRPCDevRepoIsolation(t *testing.T) {
	p := newSplitPort(t, portConfig)
	head := p.prHead(7)
	wi := p.addWorkItem(7, head)
	p.insertRepo(portRepoID + 600)
	req := p.planFix(wi, 1)
	taskID := p.tasks(req)[0]
	// 另一个仓库的工作项（claim_attempt_id 必须引用一个 attempt，这里新建一个已结束的 attempt）。
	var other int64
	mustNil(t, p.s.db.QueryRow(p.ctx, `WITH a AS (
			INSERT INTO ai_attempts (task_id, epoch, runner_name, acquire_request_id)
			VALUES ($2, 2, 'node', 'other') RETURNING id)
		INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		SELECT $1, 'developing', 'main', 'a', 3, 'codex', a.id FROM a RETURNING id`,
		portRepoID+600, taskID).Scan(&other))
	p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('work_item_id',$2::bigint) WHERE id=$1`, taskID, other)

	r := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
	task, err := p.acquire(r, "doc-dev-authz")
	if err != nil || task == nil {
		t.Fatalf("领取开发任务：%v %+v", err, task)
	}
	if _, err := p.ai.ListTaskDocs(p.ctx, withToken(r.token, &airunnerv2.ListTaskDocsRequest{Ref: ref(task)})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("跨仓库的工作项应 FailedPrecondition，得到 %v", err)
	}
}

// TestDocRPCLegacyDuplicateDocs：迁移前的 spec 引用与快照数量相同、但重复引用同一篇（[A, A] 对 [A, B]）时，
// 不能按 ID 查表误判为一致：不放行任何文档（绝不因为文档后来才冻结而泄露）。
func TestDocRPCLegacyDuplicateDocs(t *testing.T) {
	p := newSplitPort(t, portConfig)
	head := p.prHead(7)
	wi := p.addWorkItem(7, head)
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, $2, '正文一', 'u1'), ($1, 'd2', 'b', 1, $3, '正文二', 'u2')`,
		wi, docprovider.Sum("正文一"), docprovider.Sum("正文二"))
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 2)`, wi)
	req := p.planReview(7, head)
	r := p.runner(runnerSpec{})
	task, err := p.acquire(r, "doc-dup-authz")
	if err != nil || task == nil {
		t.Fatalf("领取审计任务：%v %+v", err, task)
	}
	p.exec(`UPDATE ai_review_requests SET docs_source='legacy', docs_manifest_digest='' WHERE request_id=$1`, req.id)
	p.exec(`UPDATE ai_tasks SET spec = jsonb_set(spec - 'docs_source' - 'docs_manifest_digest', '{docs}',
		jsonb_build_array(spec->'docs'->0, spec->'docs'->0)) WHERE id=$1`, task.id)
	msg, err := p.docRPC(r, task, "")
	if err != nil || len(msg.Docs) != 0 {
		t.Fatalf("重复的迁移前引用不应放行：%+v, %v", msg.GetDocs(), err)
	}
}
