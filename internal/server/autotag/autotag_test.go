package autotag

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自动打 tag 的功能测试（PLAN #29.4 的崩溃点表、T1–T3）：数据库用 pgtest，GitHub 用 fakegithub（本地 git 仓库），
// 规划用记录请求的 fakePlanner（规划本身按 dedupe_key 幂等，由 planner 的测试覆盖）。

func TestMain(m *testing.M) { pgtest.Main(m) }

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

const (
	testAccount = "alice"
	repoID      = 42
	refsRoute   = "POST /repos/alice/demo/git/refs"
	tagConfig   = "tag: {}\n"
)

// fakePlanner 记录规划请求；fail 大于 0 时接下来的这么多次返回错误（在记录请求之后，模拟 run 已经建好、
// 但记下 planned 之前崩溃）。
// db 不为 nil 时规划先从这个连接池借连接查询一次（与真实的 Planner 相同）。
type fakePlanner struct {
	mu   sync.Mutex
	reqs []TagPush
	fail int
	db   *store.DB
}

func (p *fakePlanner) PlanTagPush(ctx context.Context, ev TagPush) ([]int64, error) {
	if p.db != nil {
		var one int
		if err := p.db.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, ev)
	if p.fail > 0 {
		p.fail--
		return nil, errors.New("规划失败（注入）")
	}
	return []int64{int64(len(p.reqs))}, nil
}

func (p *fakePlanner) requests() []TagPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TagPush(nil), p.reqs...)
}

type harness struct {
	t       *testing.T
	ctx     context.Context
	db      *store.DB
	fake    *fakegithub.Server
	dir     string
	svc     *Service
	planner *fakePlanner
}

func newHarness(t *testing.T, mergeYML string) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), dir: t.TempDir(), planner: &fakePlanner{}}
	h.git("init", "-q", "-b", "main")
	files := map[string]string{"README.md": "demo\n"}
	if mergeYML != "" {
		files[".kiln/merge.yml"] = mergeYML
	}
	h.commit(files, "init")
	h.fake = fakegithub.New(testAccount)
	t.Cleanup(h.fake.Close)
	h.fake.AddRepo(&fakegithub.Repo{ID: repoID, Owner: testAccount, Name: "demo", Dir: h.dir, Private: true})
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES ($1,$2,1,$3,'demo','main','')`,
		repoID, h.fake.InstallationID, testAccount)
	client := &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: h.fake.URL, UploadURL: h.fake.URL}
	h.svc = New(Deps{
		DB:      h.db,
		Forge:   func(r *store.Repo) forge.Repo { return forge.GitHub(client, r.InstallationID, r.FullName()) },
		Planner: h.planner, Outbox: outbox.NewWriter(true), Log: slog.New(slog.DiscardHandler),
	})
	return h
}

func (h *harness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(h.ctx), "git", args...)
	cmd.Dir = h.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit 在 main 上写入 files 并提交，返回新的 commit。
func (h *harness) commit(files map[string]string, msg string) string {
	h.t.Helper()
	for name, content := range files {
		p := filepath.Join(h.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.git("add", "-A")
	h.git("commit", "-q", "--allow-empty", "-m", msg)
	return h.git("rev-parse", "HEAD")
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// record 记下 PR pr 合并出的 commit sha（与合并队列、webhook 写入的方式相同）。
func (h *harness) record(sha string, pr int, labels ...string) {
	h.t.Helper()
	if _, err := h.svc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: repoID, SHA: sha, PR: pr, Labels: labels, MergedBy: "bob", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) process() error {
	h.t.Helper()
	return h.svc.Process(h.ctx, repoID)
}

func (h *harness) mustProcess() {
	h.t.Helper()
	if err := h.process(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(sha string) *Record {
	h.t.Helper()
	rows, err := h.db.Query(h.ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 AND sha=$2`, repoID, sha)
	if err != nil {
		h.t.Fatal(err)
	}
	rs, err := scanRecords(rows)
	if err != nil || len(rs) != 1 {
		h.t.Fatalf("记录 %s：%v（%d 条）", sha, err, len(rs))
	}
	return rs[0]
}

// tagsAt 返回仓库中指向 commit 的 tag。
func (h *harness) tagsAt(commit string) []string {
	h.t.Helper()
	return strings.Fields(h.git("tag", "--points-at", commit))
}

// countRequests 返回 fakegithub 收到的形如 route 的请求数。
func (h *harness) countRequests(route string) int {
	n := 0
	for _, r := range h.fake.Requests() {
		if strings.HasPrefix(r, route) {
			n++
		}
	}
	return n
}

func (h *harness) audits(action string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM audit_log WHERE action=$1`, action).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) wantState(sha, state, tag string) *Record {
	h.t.Helper()
	r := h.get(sha)
	if r.State != state || r.Tag != tag {
		h.t.Fatalf("记录 %s：state=%s tag=%q（%s），想要 %s %q", sha[:7], r.State, r.Tag, r.Reason, state, tag)
	}
	return r
}

// TestTagAndPlan：按已有的最大版本递增，tag 指向合并的 commit；规划这个 tag 的 push，发送者是合并者、不算 Bot。
func TestTagAndPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.git("tag", "v0.1.41")
	h.git("tag", "v0.1.9")
	h.git("tag", "v0.0.0-docker-test.1")
	sha := h.commit(map[string]string{"a.txt": "a\n"}, "PR #7")
	h.record(sha, 7)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.42")
	if got := h.tagsAt(sha); len(got) != 1 || got[0] != "v0.1.42" {
		t.Fatalf("commit 上的 tag：%v", got)
	}
	reqs := h.planner.requests()
	if len(reqs) != 1 {
		t.Fatalf("规划 %d 次", len(reqs))
	}
	r := reqs[0]
	if r.Tag != "v0.1.42" || r.SHA != sha || r.Sender != "bob" || r.Repo.ID != repoID {
		t.Fatalf("规划请求：%+v", r)
	}
	if h.audits("autotag.tagged") != 1 || h.audits("autotag.planned") != 1 {
		t.Error("应写审计日志 autotag.tagged、autotag.planned")
	}
	// 再处理一次什么也不做
	h.mustProcess()
	if len(h.planner.requests()) != 1 || h.countRequests(refsRoute) != 1 {
		t.Error("已规划的记录不应再处理")
	}
}

// TestFirstVersionAndLabels：没有版本 tag 时用 initial；release:minor / release:major 递增对应的位。
func TestFirstVersionAndLabels(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  prefix: cli/v\n  initial: 1.0.0\n")
	h.git("tag", "v9.0.0") // 其他前缀不参与
	c1 := h.commit(nil, "one")
	c2 := h.commit(nil, "two")
	c3 := h.commit(nil, "three")
	c4 := h.commit(nil, "four")
	h.record(c1, 1)
	h.record(c2, 2, LabelMinor)
	h.record(c3, 3, LabelMajor, LabelMinor)
	h.record(c4, 4)
	h.mustProcess()
	h.wantState(c1, StatePlanned, "cli/v1.0.0")
	h.wantState(c2, StatePlanned, "cli/v1.1.0")
	h.wantState(c3, StatePlanned, "cli/v2.0.0")
	h.wantState(c4, StatePlanned, "cli/v2.0.1")
}

// TestSkipped：没有 tag: 配置、有 release:skip 时跳过，不建 tag、不规划。
func TestSkipped(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		yml    string
		labels []string
	}{
		"没有 merge.yml": {"", nil},
		"没有 tag:":      {"method: squash\n", nil},
		"release:skip": {tagConfig, []string{LabelMajor, LabelSkip}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yml)
			sha := h.commit(nil, "PR")
			h.record(sha, 1, tc.labels...)
			h.mustProcess()
			h.wantState(sha, StateSkipped, "")
			if h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
				t.Error("跳过的记录不应建 tag 或规划")
			}
		})
	}
}

// TestConfigError：默认分支的配置错误时记为 failed，后面的记录照常处理（也会失败），不阻塞。
func TestConfigError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  prefix: ../v\n")
	c1 := h.commit(nil, "one")
	h.record(c1, 1)
	h.mustProcess()
	if r := h.wantState(c1, StateFailed, ""); !strings.Contains(r.Reason, "tag.prefix") {
		t.Errorf("原因：%s", r.Reason)
	}
}

// TestLostCreateResponse（崩溃点：tag 已经建好，响应丢失）：保持 tagging，下一次按名字读到 tag 指向这个 commit，
// 记为 tagged 并规划；不会再建第二个 tag（T1）。
func TestLostCreateResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.git("tag", "v1.0.0")
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("响应丢失时应返回错误（稍后重试）")
	}
	r := h.wantState(sha, StateTagging, "v1.0.1")
	if r.Tries != 1 {
		t.Errorf("tries = %d", r.Tries)
	}
	if got := h.tagsAt(sha); len(got) != 1 || got[0] != "v1.0.1" {
		t.Fatalf("tag 应该已经建好：%v", got)
	}
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v1.0.1")
	if n := h.countRequests(refsRoute); n != 1 {
		t.Errorf("创建 tag 的请求 %d 次，想要 1 次", n)
	}
	if got := h.tagsAt(sha); len(got) != 1 {
		t.Errorf("一个 commit 只应有一个 tag：%v", got)
	}
}

// TestCreateRejected：没有权限（403）时记为 failed 并提示需要的权限；Retry 后重新处理。
func TestCreateRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Status: http.StatusForbidden})
	h.mustProcess()
	if r := h.wantState(sha, StateFailed, "v0.1.0"); !strings.Contains(r.Reason, "Contents") {
		t.Errorf("原因应提示权限：%s", r.Reason)
	}
	if h.audits("autotag.failed") != 1 {
		t.Error("应写审计日志 autotag.failed")
	}
	if err := h.svc.Retry(h.ctx, repoID, sha, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Retry(h.ctx, repoID, sha, "admin"); !errors.Is(err, ErrNotRetryable) {
		t.Errorf("pending 的记录不能重试：%v", err)
	}
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
}

// TestFailedFreesName：建 tag 被拒、记为 failed 的记录不占用那个名字：之后的合并照常打出同一个版本号，
// 不会因为唯一约束一直卡在 pending（独立审查发现）；重试旧记录时按 T3 跳过。
func TestFailedFreesName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	first := h.commit(nil, "PR 1")
	h.record(first, 1)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Status: http.StatusForbidden})
	h.mustProcess()
	h.wantState(first, StateFailed, "v0.1.0")
	second := h.commit(nil, "PR 2")
	h.record(second, 2)
	h.mustProcess()
	h.wantState(second, StatePlanned, "v0.1.0")
	if err := h.svc.Retry(h.ctx, repoID, first, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	h.wantState(first, StateSkipped, "")
}

// TestCreateDefiniteError：平台明确拒绝（4xx，不是"已存在"）时记为 failed，不无限重试。
func TestCreateDefiniteError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Status: http.StatusUnprocessableEntity})
	h.mustProcess()
	h.wantState(sha, StateFailed, "v0.1.0")
}

// TestCrashAfterCreate（崩溃点：tag 建好、记下 tagged 之前崩溃）：按名字读到 tag 指向这个 commit，不再创建。
func TestCrashAfterCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.exec(`UPDATE auto_tags SET state='tagging', tag='v0.1.0' WHERE sha=$1`, sha)
	h.git("tag", "v0.1.0", sha)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
	if h.countRequests(refsRoute) != 0 {
		t.Error("tag 已经存在时不应再创建")
	}
}

// TestTagExistsBeforeIntent（崩溃点：记录回到 pending 但 commit 已经有版本 tag，例如上一次处理在 Retry 之前建好了）：
// 认下已有的 tag，不建第二个。
func TestTagExistsBeforeIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.git("tag", "v0.3.0", sha)
	h.record(sha, 1)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.3.0")
	if h.countRequests(refsRoute) != 0 {
		t.Error("不应创建 tag")
	}
}

// TestPlanRetried（崩溃点：规划失败或规划后记下 planned 之前崩溃）：保持 tagged，下一次重新规划（规划按 dedupe_key 幂等）；
// 不会重新打 tag。
func TestPlanRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.planner.fail = 1
	if err := h.process(); err == nil {
		t.Fatal("规划失败时应返回错误")
	}
	h.wantState(sha, StateTagged, "v0.1.0")
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
	reqs := h.planner.requests()
	if len(reqs) != 2 || reqs[0].Tag != reqs[1].Tag || reqs[0].SHA != reqs[1].SHA {
		t.Fatalf("应按同一个 tag 和 commit 重新规划：%+v", reqs)
	}
	if h.countRequests(refsRoute) != 1 {
		t.Error("重新规划不应再建 tag")
	}
}

// TestSingleConnectionPool（独立审查发现）：连接池上限为 1 时（pool_max_conns=1）也能打 tag 并规划发布：
// 规划从连接池借连接，在释放仓库锁、归还持有锁的连接之后进行。
func TestSingleConnectionPool(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	cfg := h.db.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(h.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	h.svc.db, h.planner.db = db, db
	first := h.commit(nil, "PR 1")
	h.record(first, 1)
	second := h.commit(nil, "PR 2")
	h.record(second, 2)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.svc.Process(ctx, repoID); err != nil {
		t.Fatalf("Process：%v", err)
	}
	h.wantState(first, StatePlanned, "v0.1.0")
	h.wantState(second, StatePlanned, "v0.1.1")
	if n := len(h.planner.requests()); n != 2 || h.audits("autotag.planned") != 2 {
		t.Fatalf("规划 %d 次，审计 %d 条", n, h.audits("autotag.planned"))
	}
}

// TestStaleTagList：tag 列表滞后、看不到 Kiln 刚建的 tag 时，用自己记下的 tag 计算，不会算出同一个名字。
func TestStaleTagList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	c1 := h.commit(nil, "one")
	h.record(c1, 1)
	h.mustProcess()
	h.wantState(c1, StatePlanned, "v0.1.0")
	h.fake.HideRefs("refs/tags/v0.1.0")
	c2 := h.commit(nil, "two")
	h.record(c2, 2)
	h.mustProcess()
	h.wantState(c2, StatePlanned, "v0.1.1")
}

// TestStaleListOccupiedByOthers：tag 列表看不到别人刚手动建的 tag：按名字创建时被占用，记住它重新计算，
// 不会在同一个名字上反复。
func TestStaleListOccupiedByOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	base := h.git("rev-parse", "HEAD")
	h.git("tag", "v0.1.0", base) // 别人建的，在 commit 的历史中
	h.fake.HideRefs("refs/tags/v0.1.0")
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.1")
	if got := h.git("rev-list", "-n1", "v0.1.0"); got != base {
		t.Error("别人建的 tag 不应被改动")
	}
}

// TestLateEventOlderCommit（T3）：较新的 commit 已经打了 tag，较旧的 commit 的记录才到（例如 webhook 迟到）：
// 跳过，不会给旧 commit 打更大的版本。
func TestLateEventOlderCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	old := h.commit(nil, "old")
	newer := h.commit(nil, "new")
	h.record(newer, 2)
	h.mustProcess()
	h.wantState(newer, StatePlanned, "v0.1.0")
	h.record(old, 1)
	h.mustProcess()
	if r := h.wantState(old, StateSkipped, ""); !strings.Contains(r.Reason, "v0.1.0") {
		t.Errorf("原因：%s", r.Reason)
	}
	if got := h.tagsAt(old); len(got) != 0 {
		t.Errorf("旧 commit 不应打 tag：%v", got)
	}
}

// TestBiggerVersionElsewhere（T3）：有人在其他分支打了更大的版本，默认分支上的 commit 跳过（不在它之后接着编号）。
func TestBiggerVersionElsewhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.git("checkout", "-q", "-b", "side")
	side := h.commit(nil, "side")
	h.git("tag", "v5.0.0", side)
	h.git("checkout", "-q", "main")
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.mustProcess()
	h.wantState(sha, StateSkipped, "")
}

// TestOrderBlocked：前一条暂时失败时不处理后面的（后面的 commit 更新，先打它会让前一条拿不到 tag）。
func TestOrderBlocked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	c1 := h.commit(nil, "one")
	c2 := h.commit(nil, "two")
	h.record(c1, 1)
	h.record(c2, 2)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Status: http.StatusBadGateway})
	if err := h.process(); err == nil {
		t.Fatal("应返回错误")
	}
	h.wantState(c1, StateTagging, "v0.1.0")
	h.wantState(c2, StatePending, "")
	h.mustProcess()
	h.wantState(c1, StatePlanned, "v0.1.0")
	h.wantState(c2, StatePlanned, "v0.1.1")
}

// TestRecordIdempotent：同一个 commit 只记一次（队列和 webhook 都会记，T1）。
func TestRecordIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	for i, src := range []string{mqevents.AutoTagFromQueue, mqevents.AutoTagFromWebhook} {
		added, err := h.svc.Record(h.ctx, mqevents.AutoTagRecord{RepoID: repoID, SHA: sha, PR: 1, MergedBy: "bob", Source: src})
		if err != nil || added != (i == 0) {
			t.Fatalf("第 %d 次记录：added=%v err=%v", i+1, added, err)
		}
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind=$1`, mqevents.KindAutoTag).Scan(&n); err != nil || n != 1 {
		t.Errorf("通知 %d 条（%v），想要 1 条", n, err)
	}
}

// TestInactiveRepo：仓库停用时未完成的记录记为 failed。
func TestInactiveRepo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	tagged := h.commit(nil, "PR 2")
	h.record(tagged, 2)
	h.exec(`UPDATE auto_tags SET state='tagged', tag='v0.1.0' WHERE sha=$1`, tagged)
	h.git("tag", "v0.1.0", tagged)
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	h.mustProcess()
	h.wantState(sha, StateFailed, "")
	// 已经建好 tag 的保留，仓库恢复后照常规划发布（T2）
	h.wantState(tagged, StateTagged, "v0.1.0")
	h.exec(`UPDATE repos SET active=true WHERE id=$1`, repoID)
	h.mustProcess()
	h.wantState(tagged, StatePlanned, "v0.1.0")
}

// TestInactiveRepoKeepsTagging（独立审查发现）：创建 tag 的响应丢失（tagging，tag 实际已经建好）之后仓库停用：tagging
// 保留（兜底检查不再通知停用的仓库），仓库恢复后按名字确认、照常规划发布（T2）。
func TestInactiveRepoKeepsTagging(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.record(sha, 1)
	h.fake.SetFault(refsRoute, fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("响应丢失时应返回错误（稍后重试）")
	}
	h.wantState(sha, StateTagging, "v0.1.0")
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	h.mustProcess()
	h.wantState(sha, StateTagging, "v0.1.0")
	h.exec(`DELETE FROM outbox`)
	if err := h.svc.sweep(h.ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind=$1`, mqevents.KindAutoTag).Scan(&n); err != nil || n != 0 {
		t.Fatalf("兜底检查不应通知停用的仓库：%d（%v）", n, err)
	}
	h.exec(`UPDATE repos SET active=true WHERE id=$1`, repoID)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
	if len(h.planner.requests()) != 1 || h.countRequests(refsRoute) != 1 {
		t.Fatalf("应按名字确认后规划一次：规划 %d 次，创建 %d 次", len(h.planner.requests()), h.countRequests(refsRoute))
	}
}

// TestNextOutOfRange（独立审查发现）：递增后某个数字超过 9 位（之后的计算认不出这个 tag，会反复算出同一个名字）时
// 记为 failed、不打 tag，不挡住后面的记录。
func TestNextOutOfRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.git("tag", "v999999999.0.0")
	sha := h.commit(nil, "PR")
	h.record(sha, 1, LabelMajor)
	next := h.commit(nil, "PR 2")
	h.record(next, 2)
	h.mustProcess()
	if r := h.wantState(sha, StateFailed, ""); !strings.Contains(r.Reason, "超出范围") {
		t.Fatalf("原因：%q", r.Reason)
	}
	h.wantState(next, StatePlanned, "v999999999.0.1")
	if h.countRequests(refsRoute) != 1 {
		t.Errorf("只应为第二条记录打 tag：%d 次", h.countRequests(refsRoute))
	}
}

// TestSingleExecutor：同一个仓库同时只有一个执行者。
func TestSingleExecutor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	conn, err := h.db.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(h.ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey(repoID)); err != nil {
		t.Fatal(err)
	}
	if err := h.process(); !errors.Is(err, ErrBusy) {
		t.Fatalf("另一个执行者持有锁时应返回 ErrBusy：%v", err)
	}
	if _, err := conn.Exec(h.ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey(repoID)); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
}
