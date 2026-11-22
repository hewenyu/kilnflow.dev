package ai

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// verdictType 是只实现结束路径的任务类型：Finish 不管收到什么，都返回一个新建的失败结果（不带提交的摘要、Orca 关联、
// Agent 版本和模型），模拟以后新增的、忘了复制这些字段的结果转换分支。其余方法不会在结束路径上调用。
type verdictType struct {
	TaskType
}

func (verdictType) Name() string                                        { return "review" }
func (verdictType) Policy() Policy                                      { return Policy{} }
func (verdictType) AfterFinish(context.Context, pgx.Tx, *Request) error { return nil }

func (verdictType) Finish(context.Context, pgx.Tx, *Task, *Attempt, Result) (Result, error) {
	return Result{Outcome: store.OutcomeFailed, ErrClass: "ai.findings", Message: "发现问题"}, nil
}

// 摘要、最终的 Orca 关联、Agent 版本和模型属于这次提交本身：任务类型的 Finish 返回什么，finishTask 都保存提交的值，
// 结果重放（按摘要）才成立（PLAN #33.6）；业务结论（outcome、错误分类、说明）取 Finish 的。
func TestFinishTaskKeepsSubmission(t *testing.T) {
	ctx := t.Context()
	db := store.New(pgtest.Pool(t))
	c := New(Config{LeaseTTL: time.Minute}, Deps{
		DB: db, Outbox: outbox.NewWriter(true), Hub: notify.NewHub(), Log: slog.New(slog.DiscardHandler), Types: NewRegistry(verdictType{}),
	})
	const repoID = 7
	if err := store.UpsertRepo(ctx, db, store.RepoMeta{
		ID: repoID, InstallationID: 1, OwnerID: 1, Owner: "alice", Name: "demo", Private: true, DefaultBranch: "main",
		CloneURL: "https://example.invalid/alice/demo.git",
	}); err != nil {
		t.Fatal(err)
	}
	var reqID, taskID, attemptID int64
	if err := db.QueryRow(ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
		SELECT id, 'review', $1, 'manual', 'refs/heads/main', 'abc', id, 'active' FROM n RETURNING id`, repoID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes,
		epoch, attempt_count, started_at, lease_expires_at) VALUES ($1, 'review', 'review', '{darwin/arm64}', 'codex', '审计', '{}', 'running', 10,
		1, 1, now(), now() + interval '1 hour') RETURNING id`, reqID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_name, acquire_request_id) VALUES ($1, 1, 'ai-1', 'req') RETURNING id`,
		taskID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE ai_tasks SET current_attempt_id=$2 WHERE id=$1`, taskID, attemptID); err != nil {
		t.Fatal(err)
	}

	sub := Result{
		Outcome: store.OutcomeSucceeded, AgentVersion: "1.2.3", AgentModel: "model-x", Digest: "digest-1",
		Orca: &OrcaBinding{RuntimeID: "runtime-1", DispatchID: "dispatch-1"},
	}
	if err := c.FinishAttempt(ctx, attemptID, sub); err != nil {
		t.Fatal(err)
	}
	a, err := GetAttempt(ctx, db, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.DecodeOrca()
	if err != nil {
		t.Fatal(err)
	}
	if deref(a.ResultDigest) != sub.Digest || b.DispatchID != sub.Orca.DispatchID || a.AgentVersion != sub.AgentVersion ||
		a.AgentModel != sub.AgentModel {
		t.Fatalf("attempt：摘要 %v，Orca %+v，Agent %q / %q；应保存提交的值", a.ResultDigest, b, a.AgentVersion, a.AgentModel)
	}
	if a.Outcome == nil || *a.Outcome != store.OutcomeFailed || deref(a.ErrorClass) != "ai.findings" {
		t.Fatalf("attempt 的结果 = %v / %v，应为 Finish 给出的失败", a.Outcome, a.ErrorClass)
	}
}
