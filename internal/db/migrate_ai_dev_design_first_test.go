package db_test

import "testing"

// aiDevDesignFirstVersion 是 AI 开发自主设计的迁移（0027_ai_dev_design_first.sql，PLAN #31）。
const aiDevDesignFirstVersion = 27

// 0027：已经开了 PR 的工作项回填 head_task_id（交付在正式分支上的任务）：没有待推进的交付时是 delivery_task_id；有待推进的
// 交付时是它之前最近一次已交付的报告的任务（不是待推进的、也不是受阻的报告）；还没开 PR 的为空。
func TestMigrate_AIDevDesignFirst(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, aiDevDesignFirstVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '');
INSERT INTO runners (id, name, public_key, kind) VALUES (2, 'ai-dev', '\x00', 'ai');
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state) VALUES (1, 'develop', 42, 'manual', 'refs/heads/main', 'a', 1, 'succeeded');
INSERT INTO ai_tasks (id, request_id, key, type, platform, agent, display_name, spec, state, timeout_minutes)
SELECT n, 1, 'k' || n, 'develop', 'macos-arm64', 'codex', 't', '{}', 'succeeded', 60 FROM generate_series(1, 7) n;
INSERT INTO ai_attempts (id, task_id, epoch, runner_id, acquire_request_id) SELECT n, n, 1, 2, 'acq' || n FROM generate_series(1, 7) n;
INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, pr_number, max_fix_rounds, agent, claim_attempt_id, delivery_sha, delivery_task_id) VALUES
  (10, 42, 'pr_open',    'main', 'a', 5,    2, 'codex', 1, '',     2),
  (11, 42, 'delivering', 'main', 'a', 6,    2, 'codex', 3, 'ffff', 6),
  (12, 42, 'delivering', 'main', 'a', NULL, 2, 'codex', 7, 'eeee', 7);
INSERT INTO dev_reports (task_id, attempt_id, kind, work_item_id, report, created_at) VALUES
  (1, 1, 'delivery', 10, '{"status": "delivered"}', now() - interval '3 hours'),
  (2, 2, 'delivery', 10, '{"status": "delivered"}', now() - interval '2 hours'),
  (3, 3, 'delivery', 11, '{"status": "delivered"}', now() - interval '4 hours'),
  (4, 4, 'delivery', 11, '{"status": "delivered"}', now() - interval '3 hours'),
  (5, 5, 'delivery', 11, '{"status": "blocked"}',   now() - interval '2 hours'),
  (6, 6, 'delivery', 11, '{"status": "delivered"}', now() - interval '1 hours'),
  (7, 7, 'delivery', 12, '{"status": "delivered"}', now());
`)
	pool := open(t, u)
	if got := queryString(t, pool, `SELECT string_agg(id || ':' || coalesce(head_task_id::text, '-'), ' ' ORDER BY id) FROM dev_work_items`); got != "10:2 11:4 12:-" {
		t.Errorf("head_task_id = %q，想要 10:2 11:4 12:-", got)
	}
}
