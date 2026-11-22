package db_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
)

// aiOrcaNodeVersion 是 AI 执行节点改为 Orca 的迁移（0029_ai_orca_node.sql，PLAN #33）。
const aiOrcaNodeVersion = 29

// 0029：dev_work_items.dev_agents 回填为工作项所有实际参与过开发的 Agent（PLAN #33.7）：首次领取的 agent 在前，其余按
// 首次参与的时间；选题（领取的 attempt）、实现、返修任务中有 attempt 的才算，只规划没领取的、生成不了下发内容的不算；去重；
// agent 为空的不记。迁移已应用后再次启动不改变结果。
func TestMigrate_AIOrcaNodeDevAgents(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, aiOrcaNodeVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '');
INSERT INTO runners (id, name, public_key, kind) VALUES (2, 'ai-dev', '\x00', 'ai');

-- 开发请求：每个请求一个任务（id 相同），mode 与关联的工作项见 ai_dev_requests
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
SELECT n, 'develop', 42, 'manual', 'refs/heads/main', 'a', n, 'succeeded' FROM generate_series(1, 15) n;
INSERT INTO ai_tasks (id, request_id, key, type, platform, agent, display_name, spec, state, timeout_minutes) VALUES
  (1,  1,  'k', 'develop', 'macos-arm64', 'claude-code', 't', '{}', 'succeeded', 60), -- 10 选题（领取）
  (2,  2,  'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60), -- 10 返修（换了 development.agent）
  (3,  3,  'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'failed',    60), -- 10 再一轮返修（同一个 Agent）
  (4,  4,  'k', 'develop', 'macos-arm64', 'claude-code', 't', '{}', 'succeeded', 60), -- 10 实现（首次领取的 Agent 又来了）
  (5,  5,  'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60), -- 11 选题（领取）
  (6,  6,  'k', 'develop', 'macos-arm64', 'claude-code', 't', '{}', 'pending',   60), -- 11 返修：只规划，没有领取
  (7,  7,  'k', 'develop', 'macos-arm64', 'claude-code', 't', '{}', 'failed',    60), -- 11 返修：领取时生成不了下发内容
  (8,  8,  'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60), -- 12 选题（领取，工作项 agent 为空）
  (9,  9,  'k', 'develop', 'macos-arm64', 'claude-code', 't', '{}', 'succeeded', 60), -- 12 实现
  (10, 10, 'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60), -- 13 选题（领取，之后没有开发任务）
  (11, 11, 'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60), -- 选题（没有领取到 Issue）
  (12, 12, 'k', 'develop', 'macos-arm64', '',            't', '{}', 'succeeded', 60), -- 14 选题（领取，Agent 为空）
  (13, 13, 'k', 'develop', 'macos-arm64', '',            't', '{}', 'succeeded', 60), -- 14 返修（Agent 为空）
  (14, 14, 'k', 'develop', 'macos-arm64', '',            't', '{}', 'succeeded', 60), -- 15 选题（领取，任务的 Agent 为空）
  (15, 15, 'k', 'develop', 'macos-arm64', 'codex',       't', '{}', 'succeeded', 60); -- 15 返修
INSERT INTO ai_attempts (id, task_id, epoch, runner_id, acquire_request_id, error_class, started_at) VALUES
  (1,  1,  1, 2, 'acq1',  NULL,   now() - interval '5 hours'),
  (2,  2,  1, 2, 'acq2',  NULL,   now() - interval '4 hours'),
  (3,  3,  1, 2, 'acq3',  NULL,   now() - interval '3 hours'),
  (4,  4,  1, 2, 'acq4',  NULL,   now() - interval '2 hours'),
  (5,  5,  1, 2, 'acq5',  NULL,   now() - interval '5 hours'),
  (7,  7,  1, 2, 'acq7',  'spec', now() - interval '4 hours'),
  -- 12：实现的 attempt id 比选题的小，但开始得晚：按时间排序
  (9,  9,  1, 2, 'acq9',  NULL,   now() - interval '3 hours'),
  (18, 8,  1, 2, 'acq18', NULL,   now() - interval '5 hours'),
  (10, 10, 1, 2, 'acq10', NULL,   now() - interval '5 hours'),
  (11, 11, 1, 2, 'acq11', NULL,   now() - interval '5 hours'),
  (12, 12, 1, 2, 'acq12', NULL,   now() - interval '5 hours'),
  (13, 13, 1, 2, 'acq13', NULL,   now() - interval '4 hours'),
  (14, 14, 1, 2, 'acq14', NULL,   now() - interval '5 hours'),
  (15, 15, 1, 2, 'acq15', NULL,   now() - interval '4 hours');
INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, pr_number, max_fix_rounds, agent, claim_attempt_id, dev_hosts) VALUES
  (10, 42, 'pr_open',    'main', 'a', 5,    3, 'claude-code', 1,  '{host-a,host-b}'),
  (11, 42, 'fixing',     'main', 'a', 6,    3, 'codex',       5,  '{host-a}'),
  (12, 42, 'pr_open',    'main', 'a', 7,    3, '',            18, '{}'),
  (13, 42, 'developing', 'main', 'a', NULL, 3, 'codex',       10, '{host-a}'),
  (14, 42, 'developing', 'main', 'a', NULL, 3, '',            12, '{}'),
  -- 15：只有工作项上记着首次领取的 Agent，它仍然在前
  (15, 42, 'fixing',     'main', 'a', 8,    3, 'claude-code', 14, '{}');
INSERT INTO ai_dev_requests (request_id, mode, work_item_id, round) VALUES
  (1, 'select', NULL, 0), (2, 'fix', 10, 1), (3, 'fix', 10, 2), (4, 'implement', 10, 2),
  (5, 'select', NULL, 0), (6, 'fix', 11, 1), (7, 'fix', 11, 1),
  (8, 'select', NULL, 0), (9, 'implement', 12, 0),
  (10, 'select', NULL, 0), (11, 'select', NULL, 0),
  (12, 'select', NULL, 0), (13, 'fix', 14, 1),
  (14, 'select', NULL, 0), (15, 'fix', 15, 1);
`)
	pool := open(t, u)
	const want = "10:{claude-code,codex} 11:{codex} 12:{codex,claude-code} 13:{codex} 14:{} 15:{claude-code,codex}"
	devAgents := `SELECT string_agg(id || ':' || dev_agents::text, ' ' ORDER BY id) FROM dev_work_items`
	if got := queryString(t, pool, devAgents); got != want {
		t.Errorf("dev_agents = %q，想要 %q", got, want)
	}

	// 审计的复现：claude-code 首次领取、codex 返修过的工作项 10，Codex 审计它的 PR 被拒绝（不自审）
	exec(t, u, `
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state) VALUES (100, 'review', 42, 'pull_request', 'refs/pull/5/head', 'b', 100, 'active');
INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
VALUES (100, 100, 'review-macos', 'review', '{darwin/arm64}', 'codex', 't', '{}', 'pending', 60);`)
	spec, err := json.Marshal(map[string]any{"agent": "codex", "pr_number": 5, "head_ref": "feature"})
	if err != nil {
		t.Fatal(err)
	}
	task := &ai.Task{ID: 100, RequestID: 100, Agent: "codex", Spec: spec}
	var out map[int64]string
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		out, err = review.NewType(nil, nil).Mismatch(t.Context(), tx, &ai.Runner{ID: 2}, []*ai.Task{task})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out[task.ID], "与开发 Agent 相同") || !strings.Contains(out[task.ID], "#10") {
		t.Errorf("Codex 审计工作项 10 的 PR：Mismatch = %v，应拒绝（Codex 返修过它）", out)
	}

	// 迁移已应用：再次启动不重新执行，结果不变
	pool.Close()
	if got := queryString(t, open(t, u), devAgents); got != want {
		t.Errorf("再次启动后 dev_agents = %q，想要 %q", got, want)
	}
}
