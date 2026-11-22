package db_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

// aiDevPlatformsVersion 是 AI 开发的平台可以配置的迁移（0033_ai_dev_platforms.sql，PLAN #26.7）。
const aiDevPlatformsVersion = 33

// 0033：开发冻结的工具链改为按平台（platform_tools，归到任务的平台），去掉不再使用的 platform_id；没有工具链的开发任务、
// 审计任务不变；ai_repo_state 多出 dev_platforms（已有的行为 NULL：还没读过）。迁移已应用后再次启动不改变结果。
func TestMigrate_AIDevPlatforms(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, aiDevPlatformsVersion)
	const goTool = `[{"name":"go","version":"1.27.3","platform":"darwin/arm64","digest":"sha256:a"}]`
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '');
INSERT INTO ai_repo_state (repo_id, dev_agent) VALUES (42, 'codex');
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
SELECT n, 'develop', 42, 'develop', 'refs/heads/main', 'a', n, 'active' FROM generate_series(1, 3) n;
INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes) VALUES
  (1, 1, 'develop',            'develop', '{darwin/arm64}', 'codex', 't', '{"mode":"select","tools":`+goTool+`,"platform_id":"macos-arm64"}', 'running', 60),
  (2, 2, 'develop',            'develop', '{darwin/arm64}', 'codex', 't', '{"mode":"fix","platform_id":"macos-arm64"}', 'pending', 60),
  (3, 3, 'review-macos-arm64', 'review',  '{darwin/arm64}', 'codex', 't', '{"agent":"codex","platform_id":"macos-arm64"}', 'pending', 60);
`)
	check := func() {
		t.Helper()
		pool := open(t, u)
		defer pool.Close()
		rows, err := pool.Query(t.Context(), `SELECT id, spec FROM ai_tasks ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int64]map[string]any{}
		for rows.Next() {
			var id int64
			var spec []byte
			if err := rows.Scan(&id, &spec); err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(spec, &m); err != nil {
				t.Fatal(err)
			}
			got[id] = m
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		var tool []any
		if err := json.Unmarshal([]byte(goTool), &tool); err != nil {
			t.Fatal(err)
		}
		want := map[int64]map[string]any{
			1: {"mode": "select", "platform_tools": map[string]any{"darwin/arm64": tool}},
			2: {"mode": "fix"},
			3: {"agent": "codex", "platform_id": "macos-arm64"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("迁移后 = %+v\n想要 %+v", got, want)
		}
		var platforms []string
		if err := pool.QueryRow(t.Context(), `SELECT dev_platforms FROM ai_repo_state WHERE repo_id=42`).Scan(&platforms); err != nil || platforms != nil {
			t.Errorf("dev_platforms = %v（%v），应为 NULL", platforms, err)
		}
	}
	check()
	check()
}
