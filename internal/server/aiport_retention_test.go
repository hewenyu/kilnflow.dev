package server

import (
	"testing"
	"time"
)

// 保留策略清理 AI 的过程数据（PLAN #33.1 O5、#33.10、C10）：日志、进度和 Orca 的执行关联清掉，结果（状态、时间、
// outcome、结果摘要）保留；请求记下 purged_at，下一轮不再处理它。
func TestAIPort_purgeProcessData(t *testing.T) {
	p := newSplitPort(t, portConfig)
	req, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	if _, err := p.heartbeat(r, task); err != nil {
		t.Fatalf("心跳：%v", err)
	}
	if err := p.appendLog(r, task, 0, "日志\n"); err != nil {
		t.Fatalf("日志：%v", err)
	}
	mustComplete(t, p, r, task, succeeded(okReport))
	var digest, orca, progress string
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT coalesce(result_digest, ''), orca::text, progress::text FROM ai_attempts WHERE id=$1`,
		task.attempt).Scan(&digest, &orca, &progress))
	if orca == "{}" || progress == "{}" {
		t.Fatalf("提交之后没有过程数据可清：orca=%s progress=%s", orca, progress)
	}
	if digest == "" {
		t.Fatal("提交之后没有结果摘要")
	}
	out, err := p.s.ai.Purge(p.ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("清理：%v", err)
	}
	if out["ai_requests"] != 1 || out["ai_logs"] != 1 || out["ai_orca"] != 1 {
		t.Fatalf("清理的数量 = %v，应为 1 个请求、1 份日志、1 个执行关联", out)
	}
	var gotDigest, gotOrca, gotProgress string
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT coalesce(result_digest, ''), orca::text, progress::text FROM ai_attempts WHERE id=$1`,
		task.attempt).Scan(&gotDigest, &gotOrca, &gotProgress))
	if gotOrca != "{}" || gotProgress != "{}" {
		t.Fatalf("清理之后 orca=%s progress=%s，应为 {}（过程数据）", gotOrca, gotProgress)
	}
	if gotDigest != digest {
		t.Fatalf("结果摘要 = %q，应为 %q（结果保留）", gotDigest, digest)
	}
	if size, err := p.s.ai.Logs().Size(task.attempt); err != nil || size != 0 {
		t.Fatalf("清理之后的日志长度 = %d（%v），应为 0", size, err)
	}
	wantTask(t, p, id, "succeeded")
	var outcome string
	var finished bool
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT coalesce(outcome, ''), finished_at IS NOT NULL FROM ai_attempts WHERE id=$1`,
		task.attempt).Scan(&outcome, &finished))
	if outcome != "succeeded" || !finished {
		t.Fatalf("清理之后 attempt 的 outcome=%q finished=%t，结果应保留", outcome, finished)
	}
	if n := p.count(`SELECT count(*) FROM ai_requests WHERE id=$1 AND purged_at IS NOT NULL`, req.id); n != 1 {
		t.Fatal("没有记下 purged_at")
	}
	again, err := p.s.ai.Purge(p.ctx, time.Now().Add(time.Hour))
	if err != nil || again["ai_requests"] != 0 {
		t.Fatalf("第二轮清理 = %v（%v），不应重复处理", again, err)
	}
}
