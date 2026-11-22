package db_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

// aiTaskPlatformsVersion 是 AI 任务改为一组可以执行的平台的迁移（0032_ai_task_platforms.sql，PLAN #26.1）。
const aiTaskPlatformsVersion = 32

// 0032：ai_tasks.platform 改为 platforms（一个元素）；已有的 attempt 记下任务的平台；审计冻结的工具链改为按平台
// （platform_tools；开发的由之后的 0033 同样转换，见 TestMigrate_AIDevPlatforms）；迁移已应用后再次启动不改变结果。
func TestMigrate_AITaskPlatforms(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, aiTaskPlatformsVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '');
INSERT INTO runners (id, name, public_key, kind) VALUES (2, 'ai', '\x00', 'ai');
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
SELECT n, 'review', 42, 'pull_request', 'refs/pull/1/head', 'a', n, 'active' FROM generate_series(1, 3) n;
INSERT INTO ai_tasks (id, request_id, key, type, platform, agent, display_name, spec, state, timeout_minutes) VALUES
  (1, 1, 'review-macos-arm64', 'review',  'darwin/arm64', 'codex', 't',
   '{"agent":"codex","tools":[{"name":"go","version":"1.27.3","platform":"darwin/arm64","digest":"sha256:a"}]}', 'running', 60),
  (2, 2, 'review-linux-amd64', 'review',  'linux/amd64',  'codex', 't', '{"agent":"codex"}', 'pending', 60),
  (3, 3, 'develop',            'develop', 'darwin/arm64', 'codex', 't',
   '{"mode":"select","tools":[{"name":"go","version":"1.27.3","platform":"darwin/arm64","digest":"sha256:a"}]}', 'running', 60);
INSERT INTO ai_attempts (id, task_id, epoch, runner_id, acquire_request_id) VALUES (1, 1, 1, 2, 'acq1'), (3, 3, 1, 2, 'acq3');
`)
	check := func() {
		t.Helper()
		pool := open(t, u)
		rows, err := pool.Query(t.Context(), `SELECT t.id, t.platforms, coalesce(a.platform, ''), t.spec FROM ai_tasks t
			LEFT JOIN ai_attempts a ON a.task_id=t.id ORDER BY t.id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type row struct {
			platforms []string
			attempt   string
			spec      map[string]any
		}
		got := map[int64]row{}
		for rows.Next() {
			var id int64
			var r row
			var spec []byte
			if err := rows.Scan(&id, &r.platforms, &r.attempt, &spec); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(spec, &r.spec); err != nil {
				t.Fatal(err)
			}
			got[id] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		goTool := []any{map[string]any{"name": "go", "version": "1.27.3", "platform": "darwin/arm64", "digest": "sha256:a"}}
		want := map[int64]row{
			1: {[]string{"darwin/arm64"}, "darwin/arm64", map[string]any{"agent": "codex", "platform_tools": map[string]any{"darwin/arm64": goTool}}},
			2: {[]string{"linux/amd64"}, "", map[string]any{"agent": "codex"}},
			3: {[]string{"darwin/arm64"}, "darwin/arm64", map[string]any{"mode": "select", "platform_tools": map[string]any{"darwin/arm64": goTool}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("迁移后 = %+v\n想要 %+v", got, want)
		}
		pool.Close()
	}
	check()
	// 迁移已应用：再次启动不重新执行，结果不变
	check()
}
