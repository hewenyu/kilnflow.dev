package db_test

import (
	"fmt"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/migrations"
)

// ciAISplitVersion 是 CI 与 AI 领域拆分的迁移（0026_ci_ai_split.sql，PLAN #30.10）。
const ciAISplitVersion = 26

// databaseAt 返回一个新数据库的地址，其中只执行了版本号小于 before 的迁移（用于在旧的表结构上准备数据）。
func databaseAt(t *testing.T, before int) string {
	t.Helper()
	u := emptyDatabase(t)
	exec(t, u, `CREATE TABLE schema_migrations (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		var v int
		if _, err := fmt.Sscanf(n, "%d_", &v); err != nil {
			t.Fatal(err)
		}
		if v >= before {
			break
		}
		body, err := migrations.FS.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		exec(t, u, string(body))
		exec(t, u, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, v, n)
	}
	return u
}

func queryString(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s：%v", sql, err)
	}
	return s
}

// 0026：Runner 拆成共用身份和 CI / AI 各自的表；迁移前的 AI 数据（run / job / attempt、审计报告、引用它们的 outbox 记录）和
// AI 执行节点的注册全部删除，不迁移（#30.11）；CI Runner 和 CI 的数据不变；仓库的 AI 开发状态搬到 ai_repo_state（PLAN #30.10）。
func TestMigrate_CIAISplit(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, ciAISplitVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url, ai_dev_paused, ai_dev_select_seq, ai_dev_agent)
VALUES (42, 1, 1, 'alice', 'demo', 'main', '', true, 3, 'codex'), (43, 1, 1, 'alice', 'quiet', 'main', '', false, 0, '');

INSERT INTO runners (id, name, public_key, kind, role, pool, platforms, executors, capabilities, running_jobs)
VALUES (1, 'ci-1', '\x00', 'ci', '', 'default', '{linux-amd64}', '{host,oci}', '{"host_fingerprint": {"cpu": 8}, "free_cpu": 4}', 1),
       (2, 'ai-1', '\x00', 'ai', 'review', 'default', '{macos-arm64}', '{}', '{"ai_protocol": 3, "host_id": "h1"}', 1);
INSERT INTO runner_upgrade_requests (runner_id, version, actor) VALUES (1, '0.2.0', 'alice'), (2, '0.2.0', 'alice');

-- CI run：保留
INSERT INTO runs (id, repo_id, workflow, event, ref, sha, source, dedupe_key, state) VALUES (10, 42, 'ci.yml', 'push', 'refs/heads/main', 'aaaa', 'webhook', 'ci-key', 'active');
INSERT INTO run_reruns (run_id, rerun, state) VALUES (10, 0, 'active');
INSERT INTO jobs (id, run_id, rerun, kind, job_key, job_name, display_name, spec, platform, executor_kind, state)
VALUES (100, 10, 0, 'normal', 'build', 'build', 'build', '{}', 'linux-amd64', 'host', 'running');
INSERT INTO attempts (id, job_id, epoch, runner_id, acquire_request_id) VALUES (1000, 100, 1, 1, 'ci-acq');

-- AI 审计 run：首次和一次重跑（沿用 b 任务、a 任务正在执行），全部删除
INSERT INTO runs (id, repo_id, workflow, event, ref, sha, source, dedupe_key, state, pr_number) VALUES (20, 42, 'ai-review', 'pull_request', 'refs/pull/5/head', 'bbbb', 'webhook', 'ai-key', 'active', 5);
INSERT INTO run_reruns (run_id, rerun, state, check_run_id) VALUES (20, 0, 'failed', 501), (20, 1, 'active', 502);
INSERT INTO jobs (id, run_id, rerun, kind, job_key, job_name, display_name, spec, platform, executor_kind, state)
VALUES (201, 20, 0, 'ai_review', 'b', 'b', 'AI 审计 b', '{}', 'macos-arm64', 'ai', 'succeeded'),
       (210, 20, 1, 'ai_review', 'a', 'a', 'AI 审计 a', '{}', 'macos-arm64', 'ai', 'running');
INSERT INTO jobs (id, run_id, rerun, kind, job_key, job_name, display_name, spec, platform, executor_kind, state, reused_from_job_id)
VALUES (211, 20, 1, 'ai_review', 'b', 'b', 'AI 审计 b', '{}', 'macos-arm64', 'ai', 'succeeded', 201);
INSERT INTO attempts (id, job_id, epoch, runner_id, acquire_request_id, outcome, finished_at) VALUES (2000, 201, 1, 2, 'ai-acq-1', 'succeeded', now());
INSERT INTO attempts (id, job_id, epoch, runner_id, acquire_request_id) VALUES (2100, 210, 1, 2, 'ai-acq-2');
UPDATE jobs SET current_attempt_id = 2100, lease_expires_at = now() WHERE id = 210;
INSERT INTO ai_reviews (job_id, run_id, repo_id, attempt_id, head_sha, agent, verdict, report, model, agent_version, runner_version)
VALUES (201, 20, 42, 2000, 'bbbb', 'codex', 'pass', '{}', 'gpt-x', '1.2', '0.1.45');

-- AI 开发 run：删除
INSERT INTO runs (id, repo_id, workflow, event, ref, sha, source, dedupe_key, state) VALUES (30, 42, 'ai-develop', 'manual', 'refs/heads/main', 'cccc', 'manual', 'dev-key', 'succeeded');
INSERT INTO run_reruns (run_id, rerun, state) VALUES (30, 0, 'succeeded');
INSERT INTO jobs (id, run_id, rerun, kind, job_key, job_name, display_name, spec, platform, executor_kind, state)
VALUES (300, 30, 0, 'ai_develop', 'dev', 'dev', 'AI 开发', '{}', 'macos-arm64', 'ai', 'succeeded');

-- 引用 AI 的 outbox 记录删除，CI 的不变
INSERT INTO outbox (dedupe_key, kind, payload, state) VALUES
  ('checkrun:job:210', 'checkrun.job', '{"job_id": 210}', 'pending'),
  ('checkrun:job:100', 'checkrun.job', '{"job_id": 100}', 'pending'),
  ('checkrun:run:20:1', 'checkrun.run', '{"run_id": 20, "rerun": 1}', 'pending'),
  ('checkrun:run:10:0', 'checkrun.run', '{"run_id": 10, "rerun": 0}', 'pending'),
  ('ai-review:201', 'ai.pr_review', '{"job_id": 201}', 'pending');
`)
	pool := open(t, u)
	checkSplitRunners(t, pool)
	checkSplitAIDiscarded(t, pool)
	checkSplitCI(t, pool)
}

// checkSplitRunners：CI Runner 拆成共用身份和 ci_runners；AI 执行节点的注册（和它的升级请求）删除；仓库的 AI 开发状态只为
// 有状态的仓库建行。
func checkSplitRunners(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if got := queryString(t, pool, `SELECT kind || ' ' || pool || ' ' || array_to_string(executors, ',') || ' ' || capabilities::text FROM runners r
		JOIN ci_runners c ON c.runner_id = r.id WHERE r.id = 1`); got != `ci default host,oci {"free_cpu": 4, "host_fingerprint": {"cpu": 8}}` {
		t.Errorf("CI Runner = %q", got)
	}
	if got := queryString(t, pool, `SELECT concat_ws(' ', (SELECT string_agg(id::text, ',') FROM runners), (SELECT count(*) FROM ai_runners),
		(SELECT string_agg(runner_id::text, ',') FROM runner_upgrade_requests))`); got != "1 0 1" {
		t.Errorf("Runner、AI 执行节点、升级请求 = %q，AI 执行节点应删除", got)
	}
	if got := queryString(t, pool, `SELECT string_agg(concat_ws(' ', repo_id, dev_paused, dev_select_seq, dev_agent), '; ') FROM ai_repo_state`); got != "42 t 3 codex" {
		t.Errorf("ai_repo_state = %q", got)
	}
}

// checkSplitAIDiscarded：迁移前的 AI 数据全部删除，AI 的新表为空。
func checkSplitAIDiscarded(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if got := queryString(t, pool, `SELECT concat_ws(' ', (SELECT count(*) FROM ai_requests), (SELECT count(*) FROM ai_tasks),
		(SELECT count(*) FROM ai_attempts), (SELECT count(*) FROM ai_reviews))`); got != "0 0 0 0" {
		t.Errorf("AI 请求、任务、attempt、审计报告 = %q，应为空", got)
	}
}

// checkSplitCI：CI 表中只剩 CI 的数据，CI 的数据和 outbox 记录不变。
func checkSplitCI(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if got := queryString(t, pool, `SELECT concat_ws(' ', (SELECT string_agg(id::text, ',') FROM runs), (SELECT string_agg(id::text, ',') FROM jobs),
		(SELECT string_agg(id::text, ',') FROM attempts), (SELECT string_agg(run_id::text, ',') FROM run_reruns))`); got != "10 100 1000 10" {
		t.Errorf("CI 表 = %q", got)
	}
	if got := queryString(t, pool, `SELECT state FROM jobs WHERE id = 100`); got != "running" {
		t.Errorf("CI job 的状态 = %q，迁移不应改动", got)
	}
	if got := queryString(t, pool, `SELECT string_agg(dedupe_key, ', ' ORDER BY id) FROM outbox`); got != "checkrun:job:100, checkrun:run:10:0" {
		t.Errorf("outbox = %q，应只剩 CI 的记录", got)
	}
}
