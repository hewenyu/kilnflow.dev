package aidev

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestSchemaInvariants：不变量由数据库约束保证（PLAN #26.9），不依赖代码先查再写：直接写入违反约束的行必须失败。
// 同一个仓库可以有多个未结束的工作项（并行开发），这里逐个单独验证 I1、I3、I6。
func TestSchemaInvariants(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	// 工作项的 claim_attempt_id 引用 ai_attempts：先在一个选题任务上建几个 attempt（第 n 个用 attempts[n]）
	task := h.taskOf(h.startSelect())
	const attemptCount = 5
	attempts := map[int64]int64{}
	for n := int64(1); n <= attemptCount; n++ {
		attempts[n] = h.queryInt(`INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id) VALUES ($1,$2,$3,'req') RETURNING id`,
			task.ID, n, h.runnerID)
	}
	insert := func(state string, active bool, attempt int64, pr any) (int64, error) {
		var id int64
		err := h.db.QueryRow(h.ctx, `INSERT INTO dev_work_items (repo_id, state, active, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, pr_number)
			VALUES ($1,$2,$3,'main','x',2,'claude-code',$4,$5) RETURNING id`, testRepoID, state, active, attempts[attempt], pr).Scan(&id)
		return id, err
	}
	issue := func(w int64, n int, active bool) error {
		_, err := h.db.Exec(h.ctx, `INSERT INTO dev_work_item_issues (work_item_id, repo_id, issue_number, digest, active) VALUES ($1,$2,$3,'d',$4)`,
			w, testRepoID, n, active)
		return err
	}
	wantViolation := func(name string, err error, code string) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Fatalf("%s：err = %v，想要 %s", name, err, code)
		}
	}
	const unique, check = "23505", "23514"

	a, err := insert(StateDeveloping, true, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert(StateBlocked, true, 2, nil); err != nil {
		t.Fatalf("同一个仓库第二个未结束的工作项（并行开发）：%v", err)
	}
	_, err = insert(StateMerged, false, 1, nil)
	wantViolation("I3：同一个 attempt 第二个工作项", err, unique)
	_, err = insert(StateDeveloping, false, 3, nil)
	wantViolation("active 与 state 不一致", err, check)
	_, err = insert(StateStopped, true, 3, nil)
	wantViolation("active 与 state 不一致", err, check)

	// I1：Issue 同时至多属于一个未结束的工作项（已结束的工作项的 Issue 行没有释放时也挡住）
	b, err := insert(StateStopped, false, 4, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := issue(b, 1, true); err != nil {
		t.Fatal(err)
	}
	wantViolation("I1：Issue 被两个工作项占用", issue(a, 1, true), unique)
	if err := issue(a, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := issue(b, 2, false); err != nil {
		t.Fatalf("已释放的 Issue 行不受 I1 限制：%v", err)
	}
	// I6：一个 PR 只属于一个工作项
	_, err = insert(StateMerged, false, 5, 7)
	wantViolation("I6：PR 属于两个工作项", err, unique)
	// 评论意图只写一次
	for range 2 {
		h.exec(`INSERT INTO dev_comments (work_item_id, repo_id, number, kind, dedupe_key, body) VALUES ($1,$2,1,'claimed','k','b') ON CONFLICT (dedupe_key) DO NOTHING`, a, testRepoID)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_comments`); n != 1 {
		t.Fatalf("同一个意图写了 %d 次", n)
	}
}
