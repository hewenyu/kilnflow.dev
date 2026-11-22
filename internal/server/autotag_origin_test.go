package server

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/db"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 组装层把 AI 开发的归属判定（devTagOrigin）接到自动打 tag 上（PLAN #29.4、#30.3）。自动打 tag 的执行者
// 持有仓库锁，来源判定必须用传入的连接：连接池上限为 1 时再借一个连接会一直等待（测试用超时兜住）。

// assemblyPlanner 记录自动打 tag 规划的发布。
type assemblyPlanner struct {
	mu   sync.Mutex
	reqs []autotag.TagPush
}

func (p *assemblyPlanner) PlanTagPush(_ context.Context, ev autotag.TagPush) ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, ev)
	return []int64{int64(len(p.reqs))}, nil
}

func (p *assemblyPlanner) requests() []autotag.TagPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]autotag.TagPush(nil), p.reqs...)
}

// asmGit 在 dir 中执行 git（测试自带的提交环境）。
func asmGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestAutoTagOriginSingleConnectionPool：连接池上限为 1 时，自动打 tag 的执行者用真实的 devTagOrigin（aidev 的
// ResolveAutoTagOrigin）判定来源也不会死锁：判定、写记录、读配置都在持有仓库锁的那一个连接上完成。
func TestAutoTagOriginSingleConnectionPool(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	ctx := t.Context()
	// 连接池上限为 1（连接串里显式设置，db.Open 不会覆盖）
	u, err := url.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sdb := store.New(pool)

	// 仓库：本地 git 仓库（合并进来一个带 .kiln/merge.yml 的 main）和模拟的 GitHub
	dir := t.TempDir()
	asmGit(t, dir, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, ".kiln"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kiln", "merge.yml"), []byte("tag: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asmGit(t, dir, "add", "-A")
	asmGit(t, dir, "commit", "-q", "-m", "init")
	sha := asmGit(t, dir, "rev-parse", "HEAD")

	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	fake.AddRepo(&fakegithub.Repo{ID: 42, Owner: "alice", Name: "demo", Dir: dir})
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
		VALUES (42,$1,1,'alice','demo','main','')`, fake.InstallationID); err != nil {
		t.Fatal(err)
	}

	// 已经合并、由合并队列记下的可信实现 PR：归属是 AI 开发工作项 #1 第 1 轮。这个测试不建 AI 任务和 attempt，去掉领取
	// 记录的外键（只影响本测试库；判定只读工作项的领取编号、状态和交付）。
	if _, err := pool.Exec(ctx, `ALTER TABLE dev_work_items DROP CONSTRAINT dev_work_items_claim_attempt_id_fkey`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO dev_work_items (id, repo_id, state, active, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, delivery_sha)
		VALUES (1, 42, 'merged', false, 'main', $1, 2, 'claude-code', 5, $1)`, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO dev_pull_origins (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref, creator_login, delivery_sha)
		VALUES (42, 7, 1, 'implement', 1, 'kiln/dev-1', 'main', 'kiln-test[bot]', $1)`, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, head_ref, base_ref, source, state)
		VALUES (42, $1, 7, 'kiln-test[bot]', true, 'kiln/dev-1', 'main', 'queue', 'pending')`, sha); err != nil {
		t.Fatal(err)
	}

	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	planner := &assemblyPlanner{}
	svc := autotag.New(autotag.Deps{
		DB: sdb,
		Forge: func(r *store.Repo) forge.Repo {
			return forge.GitHub(client, r.InstallationID, r.FullName())
		},
		Planner: planner, Outbox: outbox.NewWriter(true), Log: slog.New(slog.DiscardHandler), DevOrigin: devTagOrigin{},
	})
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := svc.Process(runCtx, 42); err != nil {
		t.Fatalf("Process：%v", err)
	}

	var state, tag string
	if err := pool.QueryRow(ctx, `SELECT state, tag FROM auto_tags WHERE repo_id=42 AND sha=$1`, sha).Scan(&state, &tag); err != nil {
		t.Fatal(err)
	}
	if state != autotag.StatePlanned || tag != "v0.1.0" {
		t.Fatalf("记录 state=%q tag=%q", state, tag)
	}
	if reqs := planner.requests(); len(reqs) != 1 || reqs[0].Tag != "v0.1.0" || !reqs[0].SenderIsBot {
		t.Fatalf("应规划一次发布：%+v", reqs)
	}
}
