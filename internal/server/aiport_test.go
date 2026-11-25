package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/gittest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 生命周期的行为级测试端口（PLAN #30.11 第 1 步）：测试只通过 aiPort 规划、领取、心跳、提交、让租约过期、取消和观察
// AI 任务，断言的是行为（状态、原因、结果至多提交一次、结束钩子），不是表结构。端口的实现换了（AI 搬出 jobs / runs /
// attempts、换成 airunner.v2）断言也不改：拆分前后都要通过。
//
// 名词：请求（aiReq）是一次审计或一次开发执行（拆分前是 run 的一次运行），任务（portTask）是请求中一个平台上的一次执行
// （拆分前是 job），领取得到 attempt 和 epoch。

// aiPort 是端口，实现见同目录的 *_impl_test.go。
type aiPort interface {
	// newRules 表示实现已经按拆分后的规则执行（PLAN #30.13）：审计遇到基础设施错误不重试等。只用于跳过按旧规则无法成立的断言。
	newRules() bool

	// planReview 为 PR pr 的 head（分支 head 上的 commit）规划审计，返回请求；同一个 head 重复规划返回同一个请求。
	planReview(pr int, head string) aiReq
	// rerunReview 按 GitHub 的重跑（webhook delivery）重跑请求；同一个 delivery 对同一个请求族至多重跑一次。
	rerunReview(req aiReq, delivery string) (aiReq, error)
	// planFix 为工作项 wi 规划第 round 轮返修（工作项的状态为 fixing）。
	planFix(wi int64, round int) aiReq
	// addWorkItem 建一个返修中的工作项（PR pr，正式分支 branch），返回它的 ID。
	addWorkItem(pr int, branch string) int64

	runner(o runnerSpec) *portRunner
	acquire(r *portRunner, requestID string) (*portTask, error)
	heartbeat(r *portRunner, t *portTask) (cancel bool, err error)
	appendLog(r *portRunner, t *portTask, offset int64, data string) error
	// complete 提交结果，返回 Server 是否把这次提交当作已接受的同一结果的重试（replayed）。
	complete(r *portRunner, t *portTask, res portResult) (replayed bool, err error)

	// expireLease 让任务的租约过期并执行一轮 sweeper。
	expireLease(t *portTask)
	// age 把任务的开始时间提前 d（模拟执行了 d）。
	age(t *portTask, d time.Duration)
	sweep()
	// restart 模拟 Server 重启（延长租约、推迟超时判定）。
	restart()
	// breakSpec 让任务无法生成下发给 Runner 的内容（spec 生成失败）。
	breakSpec(taskID int64)
	cancel(req aiReq)

	tasks(req aiReq) []int64
	task(id int64) taskView
	reqState(req aiReq) string
	// settle 执行结束钩子转交给其他领域的部分（拆分后经 outbox 事件转交，拆分前同一事务中直接写入）。
	settle()
	// reqSHA 返回请求的 commit。
	reqSHA(req aiReq) string
	// processAIDev 让 AI 开发的执行者处理仓库一次。
	processAIDev()
}

// aiReq 是一个 AI 请求。
type aiReq struct {
	id int64
}

// runnerSpec 是要注册的 AI 执行节点。
type runnerSpec struct {
	role   string   // store.RunnerRoleReview（默认）或 store.RunnerRoleDevelopment
	host   string   // 机器标识（默认 host-<name>）
	agents []string // 可用的 Agent（默认 codex）
}

// portRunner 是一台已注册、已认证的 AI 执行节点。running 是它下次领取时上报的执行中数量。
type portRunner struct {
	id      int64
	name    string
	token   string
	role    string
	host    string
	agents  []string
	running int
	// capacity 是上报的并发上限（0 表示默认的 1）。
	capacity int
	// platform 是上报的平台（为空时是 macOS arm64）。
	platform string
}

// portTask 是领到的任务。
type portTask struct {
	id      int64
	attempt int64
	epoch   int64
	// agent、prNumber 来自下发的内容（审计）。
	agent    string
	prNumber int
}

// portResult 是 Runner 提交的结果。
type portResult struct {
	outcome  string // store.Outcome*
	errClass string
	message  string
	report   string // 审计报告（JSON）
	delivery string // 开发的交付报告（JSON），非空时随 Complete 提交 DevDelivery
}

// taskView 是任务的状态。
type taskView struct {
	state      string
	reason     string
	skipReason string
	epoch      int64
	attempts   int
	// errClass 是当前 attempt 的错误分类。
	errClass string
}

// ---------- 环境 ----------

const (
	portRepoID = 100
	portOwner  = "tester"
	portLease  = 2 * time.Minute
	portGrace  = 30 * time.Second
	// portConfig 是启用审计（macOS arm64，Codex，PR review 评论）和开发的 .kiln/agent/config.yml。
	portConfig = "version: 1\nreview:\n  agent: codex\n  required_platforms:\n    - {os: macos, arch: arm64}\n" +
		"development:\n  agent: codex\n  max_fix_rounds: 3\n"
)

// portEnv 是端口实现共用的环境：Server（模拟的 GitHub App）、一个已启用的仓库和本地 git 仓库。
type portEnv struct {
	t    *testing.T
	ctx  context.Context
	s    *Server
	fake *fakegithub.Server
	repo *store.Repo
	dir  string
	url  string
	base string // main 的 head
	seq  int
}

// envOf 返回端口实现的环境。
func envOf(t *testing.T, p aiPort) *portEnv {
	t.Helper()
	e, ok := p.(interface{ env() *portEnv })
	if !ok {
		t.Fatalf("端口实现 %T 没有环境", p)
	}
	return e.env()
}

// newPortEnv 建端口的环境；opts 在默认配置之后修改 Server 的配置。
func newPortEnv(t *testing.T, config string, opts ...func(*Config)) *portEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	fake := fakegithub.New(portOwner)
	t.Cleanup(fake.Close)
	s := newTestServer(t, withGitHubApp(t, fake), func(c *Config) {
		c.LeaseTTL, c.CancelGrace, c.AcquireLongPoll = portLease, portGrace, 50*time.Millisecond
		for _, o := range opts {
			o(c)
		}
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &portEnv{t: t, ctx: t.Context(), s: s, fake: fake, dir: t.TempDir(), url: srv.URL}
	e.base = gittest.Init(t, e.dir, "main", map[string]string{"README.md": "demo\n", ".kiln/agent/config.yml": config})
	fake.AddRepo(&fakegithub.Repo{ID: portRepoID, Owner: portOwner, Name: "demo", Dir: e.dir, Private: true, Installation: fake.InstallationID})
	if err := store.UpsertInstallation(e.ctx, s.db, store.InstallationMeta{ID: fake.InstallationID, AccountID: fake.AccountID, AccountLogin: portOwner}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRepo(e.ctx, s.db, store.RepoMeta{
		ID: portRepoID, InstallationID: fake.InstallationID, OwnerID: fake.AccountID, Owner: portOwner, Name: "demo", Private: true,
		DefaultBranch: "main", CloneURL: "file://" + e.dir,
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.repo, err = store.GetRepo(e.ctx, s.db, portRepoID); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *portEnv) next() int { e.seq++; return e.seq }

func (e *portEnv) git(args ...string) string {
	e.t.Helper()
	cmd := exec.CommandContext(e.ctx, "git", args...)
	cmd.Dir = e.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+e.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit 在分支 branch 上（不存在时从 main 建）写入文件并提交，返回 commit。
func (e *portEnv) commit(branch string, files map[string]string) string {
	e.t.Helper()
	if e.base != "" {
		if e.git("branch", "--list", branch) == "" {
			e.git("checkout", "-q", "-b", branch, "main")
		} else {
			e.git("checkout", "-q", branch)
		}
	}
	for name, content := range files {
		p := filepath.Join(e.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	e.git("add", "-A")
	e.git("commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%d", e.next()))
	sha := e.git("rev-parse", "HEAD")
	if e.base != "" {
		e.git("checkout", "-q", "main")
	}
	return sha
}

// prHead 在 PR pr 的分支上提交一个改动，返回新的 head。
func (e *portEnv) prHead(pr int) string {
	return e.commit(prBranch(pr), map[string]string{fmt.Sprintf("f%d.txt", e.next()): "x\n"})
}

func prBranch(pr int) string { return fmt.Sprintf("feature-%d", pr) }

func (e *portEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.s.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

func (e *portEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
	return n
}

// mustComplete 提交结果：要求 Server 接受了它（不是对已接受结果的重试），也没有错误。
func mustComplete(t *testing.T, p aiPort, r *portRunner, task *portTask, res portResult) {
	t.Helper()
	replayed, err := p.complete(r, task, res)
	mustNil(t, err)
	if replayed {
		t.Fatal("提交被当作已接受结果的重试")
	}
}

// outboxKinds 返回 outbox 中某种记录的数量（合并队列、AI 开发的通知不随拆分改变）。
func (e *portEnv) outboxCount(kind string) int {
	return e.count(`SELECT count(*) FROM outbox WHERE kind=$1 AND state='pending'`, kind)
}

// nominated 判断 PR 是否被提名为自动入队的候选。
func (e *portEnv) nominated(pr int) bool {
	return e.count(`SELECT count(*) FROM merge_auto_candidates WHERE repo_id=$1 AND pr_number=$2`, portRepoID, pr) > 0
}

// githubChecks 投递 outbox 后返回 GitHub 上 commit sha 的 Check Run 名称。
func (e *portEnv) githubChecks(sha string) []string {
	e.s.outboxWorker.DeliverDue(e.ctx)
	var names []string
	for _, c := range e.fake.CheckRuns(sha) {
		names = append(names, c.Name)
	}
	return names
}

// resetHooks 清掉结束钩子写下的通知和提名（规划时也会写入），之后的断言只看新写入的。
func (e *portEnv) resetHooks() {
	e.exec(`DELETE FROM merge_auto_candidates`)
	e.exec(`DELETE FROM outbox WHERE kind IN ('mergequeue.repo', 'aidev.repo')`)
}
