package mergequeue

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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gittest"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.MainWith(m, gittest.Cleanup) }

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
)

// harness 是合并队列的测试环境：模拟的 GitHub（本地 git 仓库）、独立的数据库和 Service。
type harness struct {
	t    *testing.T
	ctx  context.Context
	db   *store.DB
	fake *fakegithub.Server
	repo *fakegithub.Repo
	dir  string
	svc  *Service
	now  time.Time
	// holds 是有待加 kiln:hold 的 PR 编号和 head 分支（Deps.HoldPending 的替身）。
	holdsMu sync.Mutex
	holds   map[any]bool
	// failComments 是接下来在 PR 上评论时被限流（请求没有发出，可以重试）的次数。
	failComments atomic.Int32
	// failMerges 是接下来合并时被限流（请求没有发出，平台明确没有执行）的次数。
	failMerges atomic.Int32
	// loseUpdateResponses 是接下来更新 PR 分支时平台完成了更新、但响应丢失（返回暂时错误）的次数。
	loseUpdateResponses atomic.Int32
	// loseMergeResponses 是接下来合并 PR 时平台完成了合并、但响应丢失（返回暂时错误）的次数。
	loseMergeResponses atomic.Int32
	// loseCloseResponses 是接下来关闭 issue 时平台完成了关闭、但响应丢失（返回暂时错误）的次数。
	loseCloseResponses atomic.Int32
	// limitIssues 是接下来读取 PR 关联的 issue 时被平台限流的次数。
	limitIssues atomic.Int32
	// beforeMerge 非空时在请求合并之前执行一次（模拟读完快照后、合并之前 PR 有了新的 commit）。
	beforeMerge func()
	// afterMerge 非空时在合并成功之后执行一次（模拟合并之后、关闭关联的 issue 之前发生的事）。
	afterMerge func()
	// afterComment 非空时在评论成功之后执行一次（例如取消这次处理的 context，模拟评论生效后、记录下一步之前进程中断）。
	afterComment func()
	// dev 是 Deps.DevConflicts 的替身（AI 开发对冲突的处理）。
	dev *fakeDev
}

// fakeDev 是 DevConflicts 的替身：owners 是 PR 的归属（没有的是第三方），handBacks 是成功交回的冲突。
type fakeDev struct {
	mu        sync.Mutex
	owners    map[int]DevOwner
	handBacks []DevConflict
	// refuse 为真时 HandBack 返回 false（工作项期间结束了）；fail 非 nil 时下一次 HandBack 返回它（事务回滚）。
	refuse bool
	fail   error
}

func (d *fakeDev) Owner(_ context.Context, _ store.Querier, repo int64, pr int, _ string) (DevOwner, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if repo != repoID {
		return DevOwner{}, nil
	}
	return d.owners[pr], nil
}

func (d *fakeDev) HandBack(ctx context.Context, tx pgx.Tx, c DevConflict) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fail; err != nil {
		d.fail = nil
		return false, err
	}
	// 在调用方的事务中：看得到同一事务中刚出队的记录
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 ORDER BY id DESC LIMIT 1`, c.RepoID, c.PR).
		Scan(&state); err != nil {
		return false, err
	}
	if state != StateRemoved {
		return false, errors.New("交回时队列项还没有出队（不在同一个事务中）")
	}
	if d.refuse {
		return false, nil
	}
	d.handBacks = append(d.handBacks, c)
	return true, nil
}

func (d *fakeDev) setOwner(pr int, o DevOwner) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.owners[pr] = o
}

func (d *fakeDev) backs() []DevConflict {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.handBacks)
}

// flakyForge 让评论按 harness.failComments 失败、更新分支和合并按 harness.loseUpdateResponses、loseMergeResponses 丢失响应、
// 合并之前执行 harness.beforeMerge，
// 其余操作照常交给模拟的 GitHub。
type flakyForge struct {
	forge.Repo
	h *harness
}

func (f flakyForge) Comment(ctx context.Context, number int, body string) error {
	if f.h.failComments.Add(-1) >= 0 {
		return &gh.RateLimitError{Installation: 1, Kind: gh.RateLimitSecondary, Until: time.Now().Add(time.Minute)}
	}
	err := f.Repo.Comment(ctx, number, body)
	if hook := f.h.afterComment; hook != nil && err == nil {
		f.h.afterComment = nil
		hook()
	}
	return err
}

func (f flakyForge) Merge(ctx context.Context, number int, method, expectedHead string) (string, error) {
	if f.h.failMerges.Add(-1) >= 0 {
		return "", &gh.RateLimitError{Installation: 1, Kind: gh.RateLimitSecondary, Until: time.Now().Add(time.Minute)}
	}
	if hook := f.h.beforeMerge; hook != nil {
		f.h.beforeMerge = nil
		hook()
	}
	sha, err := f.Repo.Merge(ctx, number, method, expectedHead)
	if hook := f.h.afterMerge; hook != nil && err == nil {
		f.h.afterMerge = nil
		hook()
	}
	if err == nil && f.h.loseMergeResponses.Add(-1) >= 0 {
		return "", errors.New("connection reset by peer")
	}
	return sha, err
}

func (f flakyForge) ClosingIssues(ctx context.Context, number int) ([]forge.Issue, error) {
	if f.h.limitIssues.Add(-1) >= 0 {
		return nil, &gh.RateLimitError{Installation: 1, GraphQL: true, Kind: gh.RateLimitPrimary, Until: time.Now().Add(time.Hour)}
	}
	return f.Repo.ClosingIssues(ctx, number)
}

func (f flakyForge) CloseIssue(ctx context.Context, number int) error {
	err := f.Repo.CloseIssue(ctx, number)
	if err == nil && f.h.loseCloseResponses.Add(-1) >= 0 {
		return errors.New("connection reset by peer")
	}
	return err
}

func (f flakyForge) UpdateBranch(ctx context.Context, number int, expectedHead string) error {
	err := f.Repo.UpdateBranch(ctx, number, expectedHead)
	if err == nil && f.h.loseUpdateResponses.Add(-1) >= 0 {
		return errors.New("connection reset by peer")
	}
	return err
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessDB(t, store.New(pgtest.Pool(t)))
}

// newHarnessDB 与 newHarness 相同，数据库用 db（例如带 QueryTracer 的连接池）。
func newHarnessDB(t *testing.T, db *store.DB) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	h := &harness{t: t, ctx: t.Context(), db: db, dir: t.TempDir(), now: time.Now(), dev: &fakeDev{owners: map[int]DevOwner{}}}
	gittest.Init(t, h.dir, "main", map[string]string{"README.md": "demo\n"})
	h.fake = fakegithub.New(testAccount)
	t.Cleanup(h.fake.Close)
	h.repo = &fakegithub.Repo{ID: repoID, Owner: testAccount, Name: "demo", Dir: h.dir, Private: true}
	h.fake.AddRepo(h.repo)
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES ($1,$2,1,$3,'demo','main','')`,
		repoID, h.fake.InstallationID, testAccount)
	client := &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: h.fake.URL, UploadURL: h.fake.URL}
	h.svc = New(Deps{
		DB: h.db,
		Forge: func(r *store.Repo) forge.Repo {
			return flakyForge{Repo: forge.GitHub(client, r.InstallationID, r.FullName()), h: h}
		},
		Outbox: outbox.NewWriter(true), Log: slog.New(slog.DiscardHandler), PublicURL: "https://kiln.example",
		Verifications: []store.VerificationSource{testVerifications},
		HoldPending: func(_ context.Context, _ store.Querier, repo int64, pr int, headRef string) (bool, error) {
			h.holdsMu.Lock()
			defer h.holdsMu.Unlock()
			return repo == repoID && (h.holds[pr] || h.holds[headRef]), nil
		},
		DevConflicts: h.dev,
	})
	h.svc.now = func() time.Time { return h.now }
	return h
}

// testVerifications 是测试用的验证来源：PR head 的 pull_request run（与 CI 的来源相同的条件，在调用方的事务中读取）；
// workflow 正好是 ai-review（没有扩展名）的 run 模拟内建的 AI 审计（AI 的来源由 ai/review 的测试覆盖）。
func testVerifications(ctx context.Context, q store.Querier, repo int64, pr int, head, baseRef string, withJobs bool) ([]store.Verification, error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.workflow_name, r.workflow, r.state,
			COALESCE(array_agg(j.job_name ORDER BY j.id) FILTER (WHERE j.id IS NOT NULL), '{}'),
			COALESCE(array_agg(j.state::text ORDER BY j.id) FILTER (WHERE j.id IS NOT NULL), '{}')
		FROM runs r LEFT JOIN jobs j ON $5 AND j.run_id=r.id AND j.rerun=r.current_rerun
		WHERE r.repo_id=$1 AND r.event='pull_request' AND r.pr_number=$2 AND r.sha=$3 AND r.github_ctx->>'base_ref'=$4
		GROUP BY r.id ORDER BY r.id`, repo, pr, head, baseRef, withJobs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Verification, error) {
		var v store.Verification
		var file string
		var names, states []string
		if err := row.Scan(&v.ID, &v.Name, &file, &v.State, &names, &states); err != nil {
			return v, err
		}
		v.Source, v.Builtin = workflow.BaseName(file), file == mergecfg.AIReview
		for i, n := range names {
			v.Jobs = append(v.Jobs, store.VerificationJob{Name: n, State: states[i]})
		}
		return v, nil
	})
}

func (h *harness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(h.ctx), "git", args...)
	cmd.Dir = h.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit 在当前分支上写入 files 并提交，返回新的 commit。
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

// branch 从 main 建分支 name，提交一个文件后切回 main，返回分支的 head。
func (h *harness) branch(name string) string {
	h.t.Helper()
	h.git("checkout", "-q", "-b", name, "main")
	sha := h.commit(map[string]string{name + ".txt": name + "\n"}, name)
	h.git("checkout", "-q", "main")
	return sha
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// openPR 登记一个打开的、带 kiln:merge 的 PR，并加入队列。
func (h *harness) openPR(n int, branch, head string) {
	h.t.Helper()
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: n, Title: "PR " + branch, HeadRef: branch, HeadSHA: head, Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, n, head, "main", "github:"+testAccount); err != nil {
		h.t.Fatal(err)
	}
}

// verified 模拟 Kiln 按默认分支规划完 head 的 PR 事件，并且 run 处于 states（每个一个 run）。
func (h *harness) verified(n int, head string, states ...store.RunState) {
	h.t.Helper()
	h.planned(n, head, "main")
	for i, st := range states {
		h.exec(`INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
			VALUES ($1,'ci.yml',$2,'pull_request',$3,$4,'webhook',gen_random_uuid()::text,$5,$6,'{"base_ref":"main"}')`,
			repoID, "CI "+string(rune('A'+i)), workflow.PullRef(n), head, n, string(st))
	}
}

// planned 记录 PR 的 head 按目标分支 base 规划完成（pr_plans；base 为空表示没有规划过，例如只记录了基线）。
func (h *harness) planned(n int, head, base string) {
	h.t.Helper()
	if base != "" {
		h.exec(`INSERT INTO pr_plans (repo_id, pr_number, sha, base_ref) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repoID, n, head, base)
	}
}

// setRuns 把 PR head 的全部 run 改为 state。
func (h *harness) setRuns(n int, head string, state store.RunState) {
	h.exec(`UPDATE runs SET state=$3 WHERE repo_id=$1 AND pr_number=$2 AND sha=$4`, repoID, n, string(state), head)
}

// wantAutoTag 检查 PR n 合并后记下的自动打 tag 记录，以及通知了执行者。
func (h *harness) wantAutoTag(n int, sha, mergedBy, source string, labels ...string) {
	h.t.Helper()
	var (
		gotSHA, gotBy, gotSource, gotHead, gotBase string
		gotLabels                                  []string
		gotBot                                     bool
		notes                                      int
	)
	if err := h.db.QueryRow(h.ctx, `SELECT sha, merged_by, source, labels, head_ref, base_ref, sender_is_bot
		FROM auto_tags WHERE repo_id=$1 AND pr_number=$2`, repoID, n).
		Scan(&gotSHA, &gotBy, &gotSource, &gotLabels, &gotHead, &gotBase, &gotBot); err != nil {
		h.t.Fatalf("PR #%d 的自动打 tag 记录：%v", n, err)
	}
	if gotSHA != sha || gotBy != mergedBy || gotSource != source || !slices.Equal(gotLabels, labels) {
		h.t.Fatalf("PR #%d 的自动打 tag 记录：%s %s %s %v，想要 %s %s %s %v", n, gotSHA, gotBy, gotSource, gotLabels, sha, mergedBy, source, labels)
	}
	if gotHead == "" || gotBase != "main" || gotBot {
		h.t.Fatalf("PR #%d 的自动打 tag 记录：head=%q base=%q sender_is_bot=%v", n, gotHead, gotBase, gotBot)
	}
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind=$1`, mqevents.KindAutoTag).Scan(&notes); err != nil || notes == 0 {
		h.t.Fatalf("应通知自动打 tag 的执行者：%d（%v）", notes, err)
	}
}

func (h *harness) process() {
	h.t.Helper()
	if err := h.svc.Process(h.ctx, repoID); err != nil {
		h.t.Fatalf("Process：%v", err)
	}
}

func (h *harness) entry(n int) *Entry {
	h.t.Helper()
	e, err := Latest(h.ctx, h.db, repoID, n)
	if err != nil || e == nil {
		h.t.Fatalf("读取 PR #%d 的队列项：%v，%v", n, e, err)
	}
	wantNote(h.t, "状态", e.Status, e.StatusNote)
	wantNote(h.t, "原因", e.Reason, e.ReasonNote)
	return e
}

// wantNote 断言执行者写下的中文说明都带着原因码（Dashboard 按语言显示），并且就是原因码的说明（同一个判断）。
func wantNote(t *testing.T, what, text string, n Note) {
	t.Helper()
	if (text == "") != (n.Code == "") {
		t.Fatalf("%s %q 的原因码是 %q：说明和原因码应一起写入、一起清空", what, text, n.Code)
	}
	if n.Code != "" && shorten.Text(n.Text(), maxReasonLen) != text {
		t.Fatalf("%s的原因码 %s（%+v）的说明是 %q，记下的是 %q", what, n.Code, n.Params, n.Text(), text)
	}
}

func (h *harness) pull(n int) fakegithub.Pull {
	h.t.Helper()
	p, ok := h.fake.GetPull(h.repo, n)
	if !ok {
		h.t.Fatalf("没有 PR #%d", n)
	}
	return p
}

func (h *harness) wantState(n int, state, reason string) *Entry {
	h.t.Helper()
	e := h.entry(n)
	if e.State != state || !strings.Contains(e.Reason, reason) {
		h.t.Fatalf("PR #%d：state=%s reason=%q status=%q，想要 %s、原因包含 %q", n, e.State, e.Reason, e.Status, state, reason)
	}
	return e
}

func (h *harness) wantStatus(n int, status string) {
	h.t.Helper()
	if e := h.entry(n); e.State != StateQueued || !strings.Contains(e.Status, status) {
		h.t.Fatalf("PR #%d：state=%s status=%q，想要排队中、状态包含 %q", n, e.State, e.Status, status)
	}
}

// TestMergeWhenVerified：PR 入队 → 等 Kiln 处理 PR 事件 → 等验证 → 验证通过且不落后时合并。
func TestMergeWhenVerified(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)

	h.process()
	h.wantStatus(7, "等待 Kiln 按 main 规划")

	h.verified(7, head, store.RunActive, store.RunSucceeded)
	h.process()
	h.wantStatus(7, "等待验证：1/2")

	h.setRuns(7, head, store.RunSucceeded)
	h.process()
	e := h.wantState(7, StateMerged, "")
	p := h.pull(7)
	if !p.Merged || p.MergeMethod != "squash" || e.MergeSHA != p.MergeCommitSHA || h.git("rev-parse", "main") != e.MergeSHA {
		t.Fatalf("PR = %+v，记录 = %+v", p, e)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM audit_log WHERE action IN ('mergequeue.enqueue','mergequeue.merged')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("审计日志 %d 条（%v），想要入队和合并各一条", n, err)
	}
	// 与合并结果同一事务记下要自动打 tag 的 commit（PLAN #29.4）：发送者是入队的人，不是合并的 App
	h.wantAutoTag(7, e.MergeSHA, testAccount, mqevents.AutoTagFromQueue, Label)
	// 再处理一次：队列为空，什么也不做
	mark := len(h.fake.Requests())
	h.process()
	if reqs := h.fake.Requests()[mark:]; len(reqs) != 0 {
		t.Fatalf("队列为空时不应请求 GitHub：%v", reqs)
	}
}

// TestMergeResponseLostKeepsLabels（PLAN #29.4）：合并已经生效但响应丢失，之后有人整理了已合并 PR 的
// label；下一次处理看到已合并时，自动打 tag 用请求合并那一刻的 label（release:minor 仍在），版本号不变。
func TestMergeResponseLostKeepsLabels(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Labels = []string{Label, "release:minor"} })
	h.verified(7, head, store.RunSucceeded)
	h.fake.SetFault("PUT /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/7/merge", fakegithub.Fault{Lost: true})
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("合并的响应丢失时应返回错误")
	}
	if !h.pull(7).Merged {
		t.Fatal("注入的故障应让合并照常生效")
	}
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Labels = []string{Label} }) // 合并后被人整理掉
	h.process()
	e := h.wantState(7, StateMerged, "")
	h.wantAutoTag(7, e.MergeSHA, testAccount, mqevents.AutoTagFromQueue, Label, "release:minor")
}

// TestMergedByBotWithoutSuffixRecoveredByQueue（PLAN #29.4）：合并请求的结果未知（响应丢失、记录结果之前
// Server 重启），下一次处理看到 PR 已合并，平台记录的合并者是没有 [bot] 后缀的机器人（类型 Bot）：仍然按队列自己合并
// 处理，按请求合并时冻结的 label 记自动打 tag。只看 login 后缀会把它当成期间用户自己合并，队列和 webhook 两边都不写。
func TestMergedByBotWithoutSuffixRecoveredByQueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	// 队列请求合并：冻结那一刻的 label（release:skip）；之后 PR 上的 label 被整理过
	h.exec(`UPDATE merge_queue SET action=$2, merge_labels=$3 WHERE repo_id=$1 AND pr_number=7 AND state='queued'`,
		repoID, actionMerging, []string{"release:skip"})
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.MergedBy, p.MergedByBot = "legacy-bot", true
		p.Labels = []string{Label}
	})
	sha, err := h.fake.MergePull(h.repo, 7, "squash")
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	e := h.entry(7)
	if e.State != StateMerged || e.Reason != "" {
		t.Errorf("PR #7：state=%s reason=%q，想要按队列自己合并记为 merged、没有出队原因", e.State, e.Reason)
	}
	h.wantAutoTag(7, sha, testAccount, mqevents.AutoTagFromQueue, "release:skip")
}

// TestRateLimitedMergeThenManual（PLAN #29.4）：队列的合并被限流（平台明确没有执行），之后用户加上
// release:skip 自己合并：不能当成队列合并的、按请求合并时的 label 记自动打 tag；由 webhook 按合并那一刻的 label 记录。
func TestRateLimitedMergeThenManual(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)
	h.failMerges.Store(1)
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("合并被限流时应返回错误（稍后重试）")
	}
	if e := h.entry(7); e.Action != "none" || e.MergeLabels != nil {
		t.Fatalf("平台明确没有执行时应清掉合并意图：action=%q labels=%v", e.Action, e.MergeLabels)
	}
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.Labels = append(p.Labels, "release:skip")
		p.MergedBy = "bob"
	})
	if _, err := h.fake.MergePull(h.repo, 7, "squash"); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.wantState(7, StateMerged, "队列之外")
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM auto_tags WHERE repo_id=$1 AND pr_number=7`, repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("用户自己合并的不应由队列记自动打 tag：%d（%v）", n, err)
	}

	// 合并的结果不明（502，合并意图保留），之后平台记录的合并者是人：同样按在队列之外合并处理
	head2 := h.branch("second")
	h.openPR(8, "second", head2)
	h.verified(8, head2, store.RunSucceeded)
	h.fake.SetFault("PUT /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/8/merge", fakegithub.Fault{Status: http.StatusBadGateway})
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("合并的结果不明时应返回错误（稍后重试）")
	}
	if e := h.entry(8); e.Action != actionMerging {
		t.Fatalf("结果不明时应保留合并意图：action=%q", e.Action)
	}
	h.fake.UpdatePull(h.repo, 8, func(p *fakegithub.Pull) { p.MergedBy = "bob" })
	if _, err := h.fake.MergePull(h.repo, 8, "squash"); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.wantState(8, StateMerged, "队列之外")
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM auto_tags WHERE repo_id=$1 AND pr_number=8`, repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("用户自己合并的不应由队列记自动打 tag：%d（%v）", n, err)
	}
}

// TestPushBeforeMerge：读完快照（旧 head 验证通过）后、请求合并之前用户推送了新 commit：合并请求带着验证过的 head，
// 平台拒绝合并新 head，队列按"推送了新的 commit"出队，没有验证过的 commit 不会被合入。
func TestPushBeforeMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)
	var pushed string
	h.beforeMerge = func() {
		h.git("checkout", "-q", "feature")
		pushed = h.commit(map[string]string{"unverified.txt": "x\n"}, "没有验证过的 commit")
		h.git("checkout", "-q", "main")
		h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.HeadSHA = pushed })
	}
	mainBefore := h.git("rev-parse", "main")

	h.process()
	if pushed == "" {
		t.Fatal("应请求合并")
	}
	h.wantState(7, StateRemoved, "推送了新的 commit")
	if p := h.pull(7); p.Merged || h.git("rev-parse", "main") != mainBefore {
		t.Fatalf("没有验证过的 commit 不应被合并：%+v", p)
	}
}

// TestUpdateThenMerge：落后于默认分支时更新 PR 分支，接受队列产生的新 head，新 head 验证通过后合并。
// 用户自己推送的 commit 不会被当成队列的更新。
func TestUpdateThenMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)

	h.process()
	p := h.pull(7)
	e := h.entry(7)
	if p.Updates != 1 || p.HeadSHA == head || e.Action != actionUpdating || e.ActionHead != head {
		t.Fatalf("应更新 PR 分支：PR = %+v，记录 = %+v", p, e)
	}
	newHead := p.HeadSHA

	// synchronize 之后再处理：接受新 head，等 Kiln 规划它
	h.process()
	e = h.entry(7)
	if e.HeadSHA != newHead || e.Action != actionNone || e.ActionHead != head {
		t.Fatalf("应接受队列产生的新 head：%+v", e)
	}
	h.wantStatus(7, "等待 Kiln 按 main 规划")

	h.verified(7, newHead, store.RunSucceeded)
	h.process()
	h.wantState(7, StateMerged, "")
	if p := h.pull(7); !p.Merged || p.Updates != 1 {
		t.Fatalf("PR = %+v", p)
	}
}

// TestNotPlannedForDefaultBranch：head 没有按默认分支规划过时一直等待，不把"没有 run"当成验证通过：
// 只记录了基线（接入前打开的 PR）、按别的目标分支规划过（改了目标分支，还没有重新规划）。
func TestNotPlannedForDefaultBranch(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"", "release"} {
		t.Run("base="+base, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			head := h.branch("feature")
			h.openPR(7, "feature", head)
			h.planned(7, head, base)
			// 按旧目标分支规划的 run 失败了；针对 main 的 run 还没有
			h.exec(`INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
				VALUES ($1,'ci.yml','CI','pull_request',$2,$3,'webhook',gen_random_uuid()::text,7,'failed','{"base_ref":"release"}')`,
				repoID, workflow.PullRef(7), head)

			h.process()
			h.process()
			h.wantStatus(7, "等待 Kiln 按 main 规划")
			if p := h.pull(7); p.Merged || len(p.Comments) != 0 {
				t.Fatalf("没有按 main 规划过时不应合并或出队：%+v", p)
			}

			// 按 main 重新规划、验证通过后才合并
			h.verified(7, head, store.RunSucceeded)
			h.process()
			h.wantState(7, StateMerged, "")
		})
	}
}

// TestRemovalResumesAfterCommentFailure：自动出队时去掉 label 后评论被限流（没有发出），重试按记下的原因继续评论并出队，
// 不会当成手动去掉了 label。
func TestRemovalResumesAfterCommentFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunFailed)
	h.failComments.Store(1)

	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("评论失败应返回错误（稍后重试）")
	}
	e := h.entry(7)
	if p := h.pull(7); e.State != StateQueued || e.Action != actionRemoving || !strings.Contains(e.Reason, "验证没有通过") || slices.Contains(p.Labels, Label) {
		t.Fatalf("应记下出队原因并已去掉 label：记录 = %+v，PR = %+v", e, p)
	}

	h.process()
	h.wantState(7, StateRemoved, "验证没有通过")
	p := h.pull(7)
	if len(p.Comments) != 1 || !strings.Contains(p.Comments[0], "验证没有通过") {
		t.Fatalf("重试应补上评论：%q", p.Comments)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM audit_log WHERE action='mergequeue.removed' AND detail->>'reason' LIKE '%验证没有通过%'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("审计日志 %d 条（%v），想要一条带真实原因的出队记录", n, err)
	}
}

// TestRemovals：自动出队（评论原因、去掉 label）和手动出队（不评论）。
func TestRemovals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// change 在入队并处理一次之后修改 PR 或验证结果
		change    func(h *harness, head string)
		reason    string
		comment   bool
		keepLabel bool
	}{
		{
			name:   "验证失败",
			change: func(h *harness, head string) { h.setRuns(7, head, store.RunFailed) },
			reason: "验证没有通过", comment: true,
		},
		{
			name: "推送了新 commit",
			change: func(h *harness, head string) {
				h.git("checkout", "-q", "feature")
				sha := h.commit(map[string]string{"more.txt": "x\n"}, "more")
				h.git("checkout", "-q", "main")
				h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.HeadSHA = sha })
			},
			reason: "推送了新的 commit", comment: true,
		},
		{
			name:   "转为 draft",
			change: func(h *harness, _ string) { h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Draft = true }) },
			reason: "draft", comment: true,
		},
		{
			name: "改了目标分支",
			change: func(h *harness, _ string) {
				h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.BaseRef = "release" })
			},
			reason: "目标分支是 release", comment: true,
		},
		{
			name:   "去掉了 label",
			change: func(h *harness, _ string) { h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Labels = nil }) },
			reason: "去掉了", keepLabel: false,
		},
		{
			name: "关闭",
			change: func(h *harness, _ string) {
				h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.State = "closed" })
			},
			reason: "PR 已关闭",
		},
		{
			name: "合并被拒绝",
			change: func(h *harness, head string) {
				h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
					p.MergeErr = &fakegithub.APIErr{Status: http.StatusMethodNotAllowed, Message: "Required status check is expected"}
				})
				h.setRuns(7, head, store.RunSucceeded)
			},
			reason: "Required status check is expected", comment: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			head := h.branch("feature")
			h.openPR(7, "feature", head)
			h.verified(7, head, store.RunActive)
			h.process()
			h.wantStatus(7, "等待验证")

			tt.change(h, head)
			h.process()
			h.wantState(7, StateRemoved, tt.reason)
			p := h.pull(7)
			if got := len(p.Comments) > 0; got != tt.comment {
				t.Fatalf("评论 = %q，想要评论：%v", p.Comments, tt.comment)
			}
			if tt.comment && !strings.Contains(p.Comments[0], tt.reason) {
				t.Fatalf("评论 %q 应包含原因 %q", p.Comments[0], tt.reason)
			}
			if p.Merged {
				t.Fatal("出队的 PR 不应被合并")
			}
			for _, l := range p.Labels {
				if l == Label {
					t.Fatalf("出队后应去掉 %s：%v", Label, p.Labels)
				}
			}
		})
	}
}

// TestUpdateConflict：第三方 PR（没有 AI 开发的工作项负责）与目标分支冲突：出队、请提交者解决，不交给 AI 开发。
func TestUpdateConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git("checkout", "-q", "-b", "feature", "main")
	head := h.commit(map[string]string{"README.md": "feature\n"}, "feature")
	h.git("checkout", "-q", "main")
	h.commit(map[string]string{"README.md": "main\n"}, "main 改了同一行")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)

	h.process()
	e := h.wantState(7, StateRemoved, "请提交者")
	if p := h.pull(7); len(p.Comments) != 1 || p.Updates != 0 || !strings.Contains(p.Comments[0], "冲突") ||
		!strings.Contains(p.Comments[0], "重新加上 `"+Label+"`") {
		t.Fatalf("PR = %+v", p)
	}
	if e.Removal != RemovalConflictAuthor || e.ConflictBase != h.git("rev-parse", "main") || len(h.dev.backs()) != 0 {
		t.Fatalf("第三方 PR 的冲突：removal=%q base=%q，交回 %v", e.Removal, e.ConflictBase, h.dev.backs())
	}
}

// TestUpdateTimeout：请求更新后新 head 一直没到时重新请求，maxUpdateTries 次后出队。
func TestUpdateTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
	h.openPR(7, "feature", head)
	// 平台接受了请求（202）但没有完成更新
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.UpdateErr = &fakegithub.APIErr{Status: http.StatusAccepted, Message: "Updating"}
	})
	h.verified(7, head, store.RunSucceeded)

	h.process()
	if e := h.entry(7); e.Action != actionUpdating || e.ActionTries != 1 {
		t.Fatalf("记录 = %+v", e)
	}
	h.process()
	h.wantStatus(7, "正在更新")
	for i := 2; i <= maxUpdateTries; i++ {
		h.now = h.now.Add(updateTimeout + time.Second)
		h.process()
		if e := h.entry(7); e.ActionTries != i {
			t.Fatalf("第 %d 次请求：%+v", i, e)
		}
	}
	h.now = h.now.Add(updateTimeout + time.Second)
	h.process()
	h.wantState(7, StateRemoved, "都没有完成")
}

// TestTransientUpdateError：更新请求暂时失败时保留更新意图、不计次数，返回错误（稍后重试），不出队；恢复后立即重新请求。
func TestTransientUpdateError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.UpdateErr = &fakegithub.APIErr{Status: http.StatusBadGateway, Message: "boom"}
	})
	h.verified(7, head, store.RunSucceeded)

	for range maxUpdateTries + 1 {
		if err := h.svc.Process(h.ctx, repoID); err == nil {
			t.Fatal("暂时失败应返回错误")
		}
	}
	e := h.entry(7)
	if e.State != StateQueued || e.Action != actionUpdating || e.ActionHead != head || e.ActionAt != nil || e.ActionTries != 0 {
		t.Fatalf("记录 = %+v", e)
	}
	h.wantStatus(7, "暂时失败")
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.UpdateErr = nil })
	h.process()
	if p := h.pull(7); p.Updates != 1 {
		t.Fatalf("恢复后应更新分支：%+v", p)
	}
	if e := h.entry(7); e.Action != actionUpdating || e.ActionTries != 1 || e.ActionAt == nil {
		t.Fatalf("记录 = %+v", e)
	}
}

// TestUpdateResponseLost：平台完成了更新但响应丢失（暂时错误）时，下一次处理按队列更新接受新 head，不当成用户推送出队。
func TestUpdateResponseLost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)
	h.loseUpdateResponses.Store(1)

	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("响应丢失应返回错误")
	}
	p := h.pull(7)
	if p.Updates != 1 || p.HeadSHA == head {
		t.Fatalf("平台应已完成更新：%+v", p)
	}
	h.process()
	e := h.entry(7)
	if e.State != StateQueued || e.HeadSHA != p.HeadSHA || e.ActionHead != head || e.Action != actionNone {
		t.Fatalf("应接受队列更新产生的新 head：%+v", e)
	}
	if p := h.pull(7); p.Updates != 1 || len(p.Comments) != 0 {
		t.Fatalf("不应重复更新或评论：%+v", p)
	}
	h.wantStatus(7, "等待 Kiln")
}

// TestUpdateForbidden：更新 PR 分支没有权限（非限流的 403）时出队，不重复请求。
func TestUpdateForbidden(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.UpdateErr = &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	})
	h.verified(7, head, store.RunSucceeded)

	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 1})

	h.process()
	e := h.wantState(7, StateRemoved, "Resource not accessible by integration")
	if p := h.pull(7); len(p.Comments) != 1 || slices.Contains(p.Labels, Label) {
		t.Fatalf("PR = %+v", p)
	}
	// 没有权限不是冲突：即使是 AI 开发的 PR 也不交回
	if e.Removal != "" || len(h.dev.backs()) != 0 {
		t.Fatalf("没有权限时：removal=%q，交回 %v", e.Removal, h.dev.backs())
	}
}

// TestQueueOrder：两个 PR 排队，后一个显示排在第几个；前一个合并后，后一个落后，被更新后合并。
func TestQueueOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head1 := h.branch("one")
	head2 := h.branch("two")
	h.openPR(1, "one", head1)
	h.openPR(2, "two", head2)
	h.verified(1, head1, store.RunActive)
	h.verified(2, head2, store.RunSucceeded)

	h.process()
	h.wantStatus(1, "等待验证")
	h.wantStatus(2, "排在第 2 个")

	h.setRuns(1, head1, store.RunSucceeded)
	h.process()
	h.wantState(1, StateMerged, "")
	p := h.pull(2)
	if p.Updates != 1 {
		t.Fatalf("第二个 PR 落后，应被更新：%+v", p)
	}
	h.process()
	h.verified(2, p.HeadSHA, store.RunSucceeded)
	h.process()
	h.wantState(2, StateMerged, "")
}

// TestMergedOutside：PR 在队列之外被合并时记为 merged（不评论）；排在后面的 PR 关闭时直接出队。
func TestMergedOutside(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head1 := h.branch("one")
	head2 := h.branch("two")
	h.openPR(1, "one", head1)
	h.openPR(2, "two", head2)
	if _, err := h.fake.MergePull(h.repo, 1, "merge"); err != nil {
		t.Fatal(err)
	}
	h.fake.UpdatePull(h.repo, 2, func(p *fakegithub.Pull) { p.State = "closed" })
	// 改了目标分支后在队列之外合并（执行者还没处理 edited 事件）：不是合入默认分支，不打 tag（T4，独立审查发现）
	head3 := h.branch("three")
	h.openPR(3, "three", head3)
	h.fake.UpdatePull(h.repo, 3, func(p *fakegithub.Pull) {
		p.BaseRef, p.State, p.Merged, p.MergeCommitSHA, p.MergedBy = "release", "closed", true, head3, testAccount
	})

	h.process()
	h.wantState(1, StateMerged, "队列之外")
	h.wantState(2, StateRemoved, "PR 已关闭")
	h.wantState(3, StateMerged, "队列之外")
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM auto_tags WHERE repo_id=$1 AND pr_number=3`, repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("合入其他分支的 PR 不应记自动打 tag：%d（%v）", n, err)
	}
	// 在队列之外合并的不在这里记（由 webhook 按合并那一刻的 label 记录）：这里读到的 label 可能已经被整理过，
	// 抢先写入会让 webhook 的正确记录被忽略
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM auto_tags WHERE repo_id=$1 AND pr_number=1`, repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("在队列之外合并的不应由队列记自动打 tag：%d（%v）", n, err)
	}
	if p := h.pull(1); len(p.Comments) != 0 {
		t.Fatalf("不应评论：%v", p.Comments)
	}
	// 在队列之外合并时平台自己关闭关联的 issue，Kiln 不处理
	if st := h.issuesState(1); st != "" {
		t.Fatalf("在队列之外合并不应关闭关联的 issue：issues_state=%q", st)
	}
}

// TestConfig：从默认分支读取合并方式；配置有错误时等待并显示原因。
func TestConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "method: fast\n"}, "错误的配置")
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)

	h.process()
	h.wantStatus(7, "配置错误")

	h.commit(map[string]string{".kiln/merge.yml": "method: merge\n"}, "修好配置")
	h.process()
	// 修好配置让 main 前进了：先更新，再合并
	p := h.pull(7)
	h.process()
	h.verified(7, p.HeadSHA, store.RunSucceeded)
	h.process()
	h.wantState(7, StateMerged, "")
	if p := h.pull(7); p.MergeMethod != "merge" {
		t.Fatalf("合并方式 %s，想要 merge", p.MergeMethod)
	}
}

// TestRequired：必需的验证来自默认分支的 .kiln/merge.yml。PR head 的 workflow 由 PR 决定，PR 删掉必需的
// job 后其余验证都通过也不合并；必需的验证都在并且成功时合并。
func TestRequired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "required: [ci/test, ai-review]\n"}, "要求 ci/test 和 AI 审计")
	run := func(n int, head, wf, name string, jobs map[string]store.JobState) {
		h.t.Helper()
		var id int64
		if err := h.db.QueryRow(h.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
			VALUES ($1,$2,$3,'pull_request',$4,$5,'webhook',gen_random_uuid()::text,$6,'succeeded','{"base_ref":"main"}') RETURNING id`,
			repoID, wf, name, workflow.PullRef(n), head, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for job, st := range jobs {
			h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state)
				VALUES ('normal', $1, $2, $2, $2, '{}', 'linux/arm64', 'oci', $3)`, id, job, string(st))
		}
	}

	// PR #7 删掉了 ci.yml 中的 test job：lint 和 AI 审计都通过也不合并
	head := h.branch("drop-test")
	h.openPR(7, "drop-test", head)
	h.planned(7, head, "main")
	run(7, head, "ci.yml", "ci", map[string]store.JobState{"lint": store.JobSucceeded})
	run(7, head, mergecfg.AIReview, "AI 审计", map[string]store.JobState{"review": store.JobSucceeded})
	h.process()
	h.wantState(7, StateRemoved, "缺少必需的验证：ci/test")
	if p := h.pull(7); p.Merged || len(p.Comments) != 1 || !strings.Contains(p.Comments[0], "ci/test") {
		t.Fatalf("应出队并评论原因：%+v", p)
	}

	// PR #8：必需的验证都在并且成功
	head = h.branch("ok")
	h.openPR(8, "ok", head)
	h.planned(8, head, "main")
	run(8, head, "ci.yml", "ci", map[string]store.JobState{"lint": store.JobSucceeded, "test": store.JobSucceeded})
	run(8, head, mergecfg.AIReview, "AI 审计", map[string]store.JobState{"review": store.JobSucceeded})
	h.process()
	h.wantState(8, StateMerged, "")
}

// afterQuery 在 armed 之后、第一个包含 match 的 SELECT 结束时（Query 的结果读完、关闭时）调用 fn。
type afterQuery struct {
	match string
	fn    func()
	armed atomic.Bool
	done  atomic.Bool
}

type sqlKey struct{}

func (a *afterQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, sqlKey{}, d.SQL)
}

func (a *afterQuery) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(sqlKey{}).(string)
	if a.armed.Load() && strings.HasPrefix(strings.TrimSpace(sql), "SELECT") && strings.Contains(sql, a.match) &&
		a.done.CompareAndSwap(false, true) {
		a.fn()
	}
}

// TestRerunDuringLoad：读取 run 之后、读取 job 之前用户重跑了必需的 workflow（重跑在一个事务中把 run 改回 active、
// current_rerun+1 并创建新的 pending job）。以前分两次读，旧一轮已结束的 run 配上新一轮的 pending job，PR 被当成
// 验证没有成功而出队；现在 run 和 job 来自同一个快照。
func TestRerunDuringLoad(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	other, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	var runID int64
	tracer := &afterQuery{match: "FROM runs", fn: func() {
		ctx := context.WithoutCancel(t.Context())
		tx, err := other.Begin(ctx)
		if err != nil {
			panic(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, q := range []string{
			`UPDATE runs SET current_rerun=1, state='active' WHERE id=$1`,
			`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 1, 'active')`,
			`INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state)
				VALUES ('normal', $1, 1, 'test', 'test', 'test', '{}', 'linux/arm64', 'oci', 'pending')`,
		} {
			if _, err := tx.Exec(ctx, q, runID); err != nil {
				panic(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			panic(err)
		}
	}}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := newHarnessDB(t, store.New(pool))
	h.commit(map[string]string{".kiln/merge.yml": "required: [ci/test]\n"}, "要求 ci/test")
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.planned(7, head, "main")
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
		VALUES ($1,'ci.yml','ci','pull_request',$2,$3,'webhook',gen_random_uuid()::text,7,'succeeded','{"base_ref":"main"}') RETURNING id`,
		repoID, workflow.PullRef(7), head).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state)
		VALUES ('normal', $1, 'test', 'test', 'test', '{}', 'linux/arm64', 'oci', 'succeeded')`, runID)
	tracer.armed.Store(true)
	h.process()
	if !tracer.done.Load() {
		t.Fatal("没有在读取 run 之后插入重跑")
	}
	if e := h.entry(7); e.State == StateRemoved {
		t.Fatalf("重跑与读取交错时 PR 被移出队列：%s", e.Reason)
	}
	if p := h.pull(7); len(p.Comments) != 0 {
		t.Fatalf("不应评论：%q", p.Comments)
	}
	// 这一次按重跑之前的快照处理（验证已经成功）；之后的处理看到正在重跑的 run，等待它结束
	if e := h.entry(7); e.State == StateQueued {
		h.process()
		h.wantStatus(7, "等待验证")
	}
}

// TestBusy：另一次处理持有仓库的锁时返回 ErrBusy。
func TestBusy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	conn, err := h.db.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(h.ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey(repoID)); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Process(h.ctx, repoID); !errors.Is(err, ErrBusy) {
		t.Fatalf("Process() = %v，想要 ErrBusy", err)
	}
}

// TestAddRemove：Dashboard / CLI 入队时加 label、记录 head；移除时去掉 label，由执行者出队（不评论）。
func TestAddRemove(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: head})
	repo, err := store.GetRepo(h.ctx, h.db, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := h.svc.Add(h.ctx, repo, 7, "admin"); err != nil || !added {
		t.Fatalf("Add = %v, %v", added, err)
	}
	if added, err := h.svc.Add(h.ctx, repo, 7, "admin"); err != nil || added {
		t.Fatalf("重复 Add = %v, %v，想要已经在队列中", added, err)
	}
	if e := h.entry(7); e.HeadSHA != head || e.EnqueuedBy != "admin" {
		t.Fatalf("记录 = %+v", e)
	}
	if p := h.pull(7); len(p.Labels) != 1 || p.Labels[0] != Label {
		t.Fatalf("应加上 label：%v", p.Labels)
	}
	if err := h.svc.Remove(h.ctx, repo, 7, "admin"); err != nil {
		t.Fatal(err)
	}
	h.process()
	h.wantState(7, StateRemoved, "去掉了")
	if p := h.pull(7); len(p.Labels) != 0 || len(p.Comments) != 0 {
		t.Fatalf("PR = %+v", p)
	}
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.State = "closed" })
	if _, err := h.svc.Add(h.ctx, repo, 7, "admin"); err == nil {
		t.Fatal("关闭的 PR 不能入队")
	}
}

// TestNotify：只在仓库（或 PR）有排队中的项时写入通知。
func TestNotify(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	count := func() int {
		var n int
		if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind=$1 AND state='pending'`, mqevents.KindMergeQueue).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := h.svc.Notify(h.ctx, repoID, 0); err != nil || count() != 0 {
		t.Fatalf("队列为空时不应通知：%v，%d", err, count())
	}
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.exec(`DELETE FROM outbox`)
	if err := h.svc.Notify(h.ctx, repoID, 8); err != nil || count() != 0 {
		t.Fatalf("不在队列中的 PR 不应通知：%v，%d", err, count())
	}
	if err := h.svc.Notify(h.ctx, repoID, 7); err != nil || count() != 1 {
		t.Fatalf("队列中的 PR 应通知：%v，%d", err, count())
	}
}

// TestRecentByFinishedAt：最近结束的项按结束时间排序，最早入队、最后结束的项排在最前（不会被截断掉）。
func TestRecentByFinishedAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, finished_at) VALUES ($1, 1, 'merged', now())`, repoID)
	for pr := 2; pr <= 4; pr++ {
		h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, finished_at) VALUES ($1, $2, 'removed', now() - interval '1 day')`, repoID, pr)
	}
	h.exec(`INSERT INTO merge_queue (repo_id, pr_number) VALUES ($1, 5)`, repoID)
	es, err := Recent(h.ctx, h.db, repoID, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, 0, len(es))
	for _, e := range es {
		got = append(got, e.PR)
	}
	if want := []int{1, 4, 3}; !slices.Equal(got, want) {
		t.Fatalf("Recent = %v，想要 %v", got, want)
	}
}

// TestInactiveRepo：仓库停用（从 installation 中移除、App 被卸载）后，处理时把排队中的项全部出队，不再请求平台；
// 兜底检查之后不再通知这个仓库。
func TestInactiveRepo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.openPR(7, "feature", h.branch("feature"))
	h.openPR(8, "other", h.branch("other"))
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	h.fake.RemoveRepo(h.repo)

	mark := len(h.fake.Requests())
	h.process()
	if reqs := h.fake.Requests()[mark:]; len(reqs) != 0 {
		t.Fatalf("停用的仓库不应请求平台：%v", reqs)
	}
	for _, n := range []int{7, 8} {
		h.wantState(n, StateRemoved, "仓库已停用")
	}
	var audits int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM audit_log WHERE action='mergequeue.removed'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("审计日志 %d 条（%v），想要 2 条", audits, err)
	}
	h.exec(`DELETE FROM outbox`)
	if err := h.svc.sweep(h.ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind=$1`, mqevents.KindMergeQueue).Scan(&n); err != nil || n != 0 {
		t.Fatalf("兜底检查不应再通知停用的仓库：%d（%v）", n, err)
	}
}

// TestSingleConnectionPool：连接池上限为 1 时（DATABASE_URL 的 pool_max_conns=1）也能处理：持有仓库锁的连接
// 用于这次处理的全部查询和事务，不再从连接池借第二个连接。
func TestSingleConnectionPool(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cfg := h.db.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(h.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h.svc.db = store.New(pool)

	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.svc.Process(ctx, repoID); err != nil {
		t.Fatalf("Process：%v", err)
	}
	h.wantState(7, StateMerged, "")

	// 停用的仓库同样
	h.openPR(8, "other", h.branch("other"))
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	if err := h.svc.Process(ctx, repoID); err != nil {
		t.Fatalf("Process：%v", err)
	}
	h.wantState(8, StateRemoved, "仓库已停用")
}
