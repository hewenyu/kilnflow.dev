package gitcred

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// logBuffer 收集 JSON 日志（并发安全）。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// entries 返回消息为 msg 的日志。
func (b *logBuffer) entries(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(b.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志不是 JSON：%q", line)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// logTime 解析日志中的时间字段。
func logTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("日志中的时间 %v：%v", v, err)
	}
	return tm
}

// clock 是可以拨动的时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	fake  *fakegithub.Server
	repo  *fakegithub.Repo
	other *fakegithub.Repo
	reg   *Registry
	logs  *logBuffer
	clock *clock
}

// wait 等 Invalidate 在后台发起的吊销全部结束。
func (f *fixture) wait(t *testing.T) {
	t.Helper()
	if err := f.reg.Wait(t.Context()); err != nil {
		t.Fatalf("等待后台吊销：%v", err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, Config{})
}

func newFixtureWith(t *testing.T, cfg Config) *fixture {
	t.Helper()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
	other := &fakegithub.Repo{ID: 2, Owner: "alice", Name: "dep", Dir: t.TempDir(), Private: true}
	fake.AddRepo(repo)
	fake.AddRepo(other)
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	logs := &logBuffer{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	reg := New(cfg, Deps{GitHub: client, Log: slog.New(slog.NewJSONHandler(logs, nil)), Now: c.now})
	f := &fixture{fake: fake, repo: repo, other: other, reg: reg, logs: logs, clock: c}
	t.Cleanup(func() { f.assertNoTokenInLogs(t) })
	return f
}

var attempt = Attempt{Domain: DomainAI, ID: 7}

func key(op string) Key { return Key{Attempt: attempt, Epoch: 3, Op: op} }

func (f *fixture) scope(r *fakegithub.Repo, perm string) Scope {
	return Scope{Installation: f.fake.InstallationID, RepoID: r.ID, Repository: r.FullName(), Permissions: map[string]string{"contents": perm}}
}

func pass(context.Context) error { return nil }

// createCalls 返回向 GitHub 请求签发 token 的次数（含失败的）。
func (f *fixture) createCalls() int {
	n := 0
	for _, r := range f.fake.Requests() {
		if r == fakegithub.CreateTokenRoute(f.fake.InstallationID) {
			n++
		}
	}
	return n
}

// revokeCalls 返回吊销请求的次数。
func (f *fixture) revokeCalls() int {
	n := 0
	for _, r := range f.fake.Requests() {
		if r == fakegithub.RevokeTokenRoute {
			n++
		}
	}
	return n
}

// revoked 返回模拟 GitHub 中 token 是否已经吊销。
func (f *fixture) revoked(t *testing.T, token string) bool {
	t.Helper()
	tok, ok := f.fake.IssuedToken(token)
	if !ok {
		t.Fatalf("模拟 GitHub 没有签发过 %s", Fingerprint(token))
	}
	return tok.Revoked
}

// onlyIssued 断言模拟 GitHub 只签发过一枚 token 并返回它。
func (f *fixture) onlyIssued(t *testing.T) fakegithub.Token {
	t.Helper()
	toks := f.fake.IssuedTokens()
	if len(toks) != 1 {
		t.Fatalf("应只签发 1 枚 token，实际 %d 枚", len(toks))
	}
	return toks[0]
}

func (f *fixture) assertNoTokenInLogs(t *testing.T) {
	t.Helper()
	logs := f.logs.String()
	for _, tok := range f.fake.IssuedTokens() {
		if strings.Contains(logs, tok.Token) {
			t.Errorf("日志中出现了 token %s", Fingerprint(tok.Token))
		}
	}
}

// pause 让下一次签发停在模拟 GitHub 中：返回的 entered 在请求到达时关闭，关闭 release 后继续处理。
func (f *fixture) pause(fault fakegithub.Fault) (entered <-chan struct{}, release chan<- struct{}) {
	in, out := make(chan struct{}), make(chan struct{})
	fault.Pause = func() {
		close(in)
		<-out
	}
	f.fake.SetFault(fakegithub.CreateTokenRoute(f.fake.InstallationID), fault)
	return in, out
}

type result struct {
	tok *gh.AccessToken
	err error
}

// issueAsync 在后台签发，结果写到返回的 channel。
func (f *fixture) issueAsync(ctx context.Context, k Key, s Scope) <-chan result {
	ch := make(chan result, 1)
	go func() {
		tok, err := f.reg.Issue(ctx, k, s, pass)
		ch <- result{tok, err}
	}()
	return ch
}

// waitLimit 是测试等待后台请求进入等待状态的上限（超过即失败，而不是挂到测试超时）。
const waitLimit = 10 * time.Second

// waitWaiters 等到签发在途的操作 k 上有 n 个请求在等待。
func (f *fixture) waitWaiters(t *testing.T, k Key, n int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		f.reg.mu.Lock()
		got := 0
		if o := f.reg.attempts[k.Attempt].ops[opKey{k.Epoch, k.Op}]; o != nil {
			got = o.waiters
		}
		f.reg.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待的请求只有 %d 个，应为 %d 个", got, n)
		}
		runtime.Gosched()
	}
}

// 同一 op_id 并发签发只调用一次 GitHub：后到的请求等同一个结果，拿到同一枚 token；之后的重复请求也是这一枚。
func TestIssue_SameOpConcurrent(t *testing.T) {
	f := newFixture(t)
	const n = 8
	entered, release := f.pause(fakegithub.Fault{})
	k, s := key("op-1"), f.scope(f.repo, "read")
	results := []<-chan result{f.issueAsync(t.Context(), k, s)}
	<-entered
	for range n - 1 {
		results = append(results, f.issueAsync(t.Context(), k, s))
	}
	f.waitWaiters(t, k, n-1)
	close(release)
	var first string
	for _, ch := range results {
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
		if first == "" {
			first = r.tok.Token
		}
		if r.tok.Token != first {
			t.Errorf("同一操作拿到了不同的 token")
		}
	}
	again, err := f.reg.Issue(t.Context(), k, s, pass)
	if err != nil || again.Token != first {
		t.Fatalf("重复请求应返回同一枚：%v", err)
	}
	if got := f.createCalls(); got != 1 {
		t.Errorf("应只调用 GitHub 1 次，实际 %d 次", got)
	}
	tok := f.onlyIssued(t)
	if tok.Token != first || len(tok.RepoIDs) != 1 || tok.RepoIDs[0] != f.repo.ID || tok.Permissions["contents"] != "read" || len(tok.Permissions) != 1 {
		t.Errorf("签发的范围：%+v", tok)
	}
	if !again.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("过期时间 %v，GitHub 给的是 %v", again.ExpiresAt, tok.ExpiresAt)
	}
}

// 重复请求的仓库或权限与第一次不同：拒绝，原 token 不受影响。
func TestIssue_ScopeMismatch(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	tok, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Scope{
		"权限不同":            f.scope(f.repo, "write"),
		"仓库不同":            f.scope(f.other, "read"),
		"installation 不同": {Installation: 9, RepoID: f.repo.ID, Permissions: map[string]string{"contents": "read"}},
	} {
		if _, err := f.reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrScopeMismatch) {
			t.Errorf("%s：应拒绝，实际 %v", name, err)
		}
	}
	if f.createCalls() != 1 || f.revoked(t, tok.Token) {
		t.Errorf("范围不一致的请求不应签发或吊销：签发 %d 次", f.createCalls())
	}
}

// 先关闭（只带 op_id）后签发：拒绝，不调用 GitHub。
func TestIssue_ClosedBeforeIssue(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Fatalf("应拒绝：%v", err)
	}
	if got := f.createCalls(); got != 0 {
		t.Errorf("不应调用 GitHub，实际 %d 次", got)
	}
}

// 签发在途时操作被关闭或 attempt 失效（AI 任务结束、CI job 退出）：GitHub 迟到的 token 立即吊销、不交出；之后同键被拒绝。
func TestIssue_ClosedWhileIssuing(t *testing.T) {
	tests := []struct {
		name  string
		close func(f *fixture, k Key)
	}{
		{"关闭操作", func(f *fixture, k Key) {
			if err := f.reg.Close(context.Background(), k, f.fake.InstallationID, ""); err != nil {
				panic(err)
			}
		}},
		{"attempt 失效", func(f *fixture, k Key) { f.reg.Invalidate(context.Background(), k.Attempt) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			k, s := key("op-1"), f.scope(f.repo, "read")
			entered, release := f.pause(fakegithub.Fault{})
			ch := f.issueAsync(t.Context(), k, s)
			<-entered
			tt.close(f, k)
			close(release)
			r := <-ch
			if !errors.Is(r.err, ErrClosed) || r.tok != nil {
				t.Fatalf("在途时关闭应不交出：%+v %v", r.tok, r.err)
			}
			if tok := f.onlyIssued(t); !tok.Revoked {
				t.Error("迟到的 token 应被吊销")
			}
			if _, err := f.reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrClosed) {
				t.Errorf("关闭后再签发应被拒绝：%v", err)
			}
			if f.createCalls() != 1 || f.revokeCalls() != 1 {
				t.Errorf("签发 %d 次、吊销 %d 次，应各 1 次", f.createCalls(), f.revokeCalls())
			}
		})
	}
}

// 签发的响应在 Server 到客户端之间丢失：只带 op_id 关闭，Server 已拿到的 token 被吊销；Server 重启丢了登记时，客户端带
// token 的关闭仍能吊销，并且之后同键被拒绝。
func TestClose_RevokesKnownToken(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	tok, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, tok.Token) {
		t.Error("只带 op_id 关闭应吊销 Server 已拿到的 token")
	}
	// 重复关闭不再吊销
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, tok.Token); err != nil {
		t.Fatal(err)
	}
	if got := f.revokeCalls(); got != 1 {
		t.Errorf("重复关闭不应再吊销，吊销 %d 次", got)
	}

	// 重启：新的登记中没有这个操作
	k2 := key("op-2")
	tok2, err := f.reg.Issue(t.Context(), k2, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(Config{}, Deps{GitHub: f.reg.gh, Log: f.reg.log, Now: f.clock.now})
	if err := restarted.Close(t.Context(), k2, f.fake.InstallationID, tok2.Token); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, tok2.Token) {
		t.Error("重启后带 token 的关闭应吊销")
	}
	if _, err := restarted.Issue(t.Context(), k2, f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Errorf("重启后关闭的操作再签发应被拒绝：%v", err)
	}
}

// token 到期后同一 op_id 重放：关闭记录不随 token 到期删除，仍被拒绝；没有关闭的操作不续签。
func TestIssue_ReplayAfterExpiry(t *testing.T) {
	f := newFixture(t)
	closed, open := key("op-1"), key("op-2")
	s := f.scope(f.repo, "read")
	if _, err := f.reg.Issue(t.Context(), closed, s, pass); err != nil {
		t.Fatal(err)
	}
	first, err := f.reg.Issue(t.Context(), open, s, pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(t.Context(), closed, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	f.clock.add(2 * gh.TokenLifetime)
	if _, err := f.reg.Issue(t.Context(), closed, s, pass); !errors.Is(err, ErrClosed) {
		t.Errorf("到期后重放应被拒绝：%v", err)
	}
	again, err := f.reg.Issue(t.Context(), open, s, pass)
	if err != nil || again.Token != first.Token {
		t.Errorf("没有关闭的操作不续签，应返回原来那枚：%v", err)
	}
	if got := f.createCalls(); got != 2 {
		t.Errorf("签发 %d 次，应为 2 次", got)
	}
}

// 最终检查不通过：不交出，吊销，操作关闭。
func TestIssue_FinalCheckFails(t *testing.T) {
	f := newFixture(t)
	k, s := key("op-1"), f.scope(f.repo, "read")
	errStale := errors.New("attempt 已经不在执行中")
	tok, err := f.reg.Issue(t.Context(), k, s, func(context.Context) error { return errStale })
	if !errors.Is(err, errStale) || tok != nil {
		t.Fatalf("应返回检查的错误、不交出：%+v %v", tok, err)
	}
	if !f.onlyIssued(t).Revoked {
		t.Error("检查不通过的 token 应被吊销")
	}
	if _, err := f.reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrClosed) {
		t.Errorf("检查不通过后操作应关闭：%v", err)
	}
}

// attempt 结束与签发交错：结束先于最终检查（检查不通过）、最终检查之后交出之前结束、交出之后结束，token 都被吊销且只吊销一次；
// 失效之后的签发一律拒绝，不调用 GitHub。
func TestInvalidate_InterleavedWithIssue(t *testing.T) {
	errEnded := errors.New("attempt 已经结束")
	tests := []struct {
		name      string
		check     func(f *fixture) Check
		wantErr   error
		handedOut bool
	}{
		{"结束先于最终检查", func(f *fixture) Check {
			return func(ctx context.Context) error {
				f.reg.Invalidate(ctx, attempt)
				return errEnded
			}
		}, errEnded, false},
		{"最终检查之后、交出之前结束", func(f *fixture) Check {
			return func(ctx context.Context) error {
				f.reg.Invalidate(ctx, attempt)
				return nil
			}
		}, ErrClosed, false},
		{"交出之后结束", func(*fixture) Check { return pass }, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			k, s := key("op-1"), f.scope(f.repo, "read")
			tok, err := f.reg.Issue(t.Context(), k, s, tt.check(f))
			if !errors.Is(err, tt.wantErr) || (tok != nil) != tt.handedOut {
				t.Fatalf("Issue = %+v, %v；应为交出 %v、错误 %v", tok, err, tt.handedOut, tt.wantErr)
			}
			issued := f.onlyIssued(t)
			if tt.handedOut {
				if issued.Revoked {
					t.Fatal("交出时 token 不应已经吊销")
				}
				f.reg.Invalidate(t.Context(), attempt)
			}
			f.wait(t)
			if !f.revoked(t, issued.Token) {
				t.Error("token 应被吊销")
			}
			if got := f.revokeCalls(); got != 1 {
				t.Errorf("应只吊销 1 次，实际 %d 次", got)
			}
			for _, op := range []string{"op-1", "op-new"} {
				if _, err := f.reg.Issue(t.Context(), key(op), s, pass); !errors.Is(err, ErrClosed) {
					t.Errorf("失效后 %s 的签发应被拒绝：%v", op, err)
				}
			}
			if got := f.createCalls(); got != 1 {
				t.Errorf("失效后不应调用 GitHub，签发 %d 次", got)
			}
		})
	}
}

// 失效只影响这个 attempt；并发的操作互不吊销：关闭一个操作不影响同一 attempt 的另一个操作。
func TestOpsIndependent(t *testing.T) {
	f := newFixture(t)
	s := f.scope(f.repo, "read")
	a, err := f.reg.Issue(t.Context(), key("op-a"), s, pass)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.reg.Issue(t.Context(), key("op-b"), s, pass)
	if err != nil {
		t.Fatal(err)
	}
	otherAttempt := Key{Attempt{Domain: DomainCI, ID: attempt.ID}, 3, "op-a"}
	c, err := f.reg.Issue(t.Context(), otherAttempt, s, pass)
	if err != nil {
		t.Fatal(err)
	}
	if a.Token == b.Token || a.Token == c.Token {
		t.Fatal("不同操作应各签一枚")
	}
	if err := f.reg.Close(t.Context(), key("op-a"), f.fake.InstallationID, a.Token); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, a.Token) || f.revoked(t, b.Token) || f.revoked(t, c.Token) {
		t.Error("关闭 op-a 只应吊销它自己的 token")
	}
	f.reg.Invalidate(t.Context(), attempt)
	f.wait(t)
	if !f.revoked(t, b.Token) || f.revoked(t, c.Token) {
		t.Error("AI attempt 失效应吊销它的 op-b，不影响 CI 中编号相同的 attempt")
	}
}

// GitHub 签发结果不明：不重试，操作以错误关闭，记 warn（过期时间是估计值）；同一 op_id 重放被拒绝、不再调用 GitHub。
func TestIssue_UnknownOutcome(t *testing.T) {
	f := newFixture(t)
	k, s := key("op-1"), f.scope(f.repo, "read")
	f.fake.SetFault(fakegithub.CreateTokenRoute(f.fake.InstallationID), fakegithub.Fault{Lost: true})
	tok, err := f.reg.Issue(t.Context(), k, s, pass)
	if !errors.Is(err, ErrUnknown) || tok != nil {
		t.Fatalf("应返回结果不明：%+v %v", tok, err)
	}
	if _, err := f.reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrClosed) {
		t.Errorf("结果不明后重放应被拒绝：%v", err)
	}
	if got := f.createCalls(); got != 1 {
		t.Errorf("结果不明时不重试，签发 %d 次", got)
	}
	warns := f.logs.entries(t, "GitHub 签发 git 凭据的结果不明，不重试，操作以错误关闭；可能有一枚谁也没拿到的 token，到期自然失效")
	if len(warns) != 1 {
		t.Fatalf("应记 1 条 warn，实际 %d 条：%s", len(warns), f.logs.String())
	}
	w := warns[0]
	if w["level"] != "WARN" || w["expires_at_estimated"] != true || !logTime(t, w["expires_at"]).Equal(f.clock.now().Add(gh.TokenLifetime)) ||
		w["op"] != "op-1" || w["attempt"] != float64(attempt.ID) || w["repository"] != f.repo.FullName() {
		t.Errorf("warn 的字段：%v", w)
	}
	// 模拟 GitHub 中有一枚谁也没拿到的 token：登记中没有它，关闭不吊销它（只能到期）
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.revokeCalls(); got != 0 {
		t.Errorf("没有拿到的 token 无法吊销，吊销请求 %d 次", got)
	}
}

// GitHub 返回了 token 却没有过期时间，补吊销又失败（被限流、403）：签发已经生效，按结果不明处理——操作以错误关闭、记
// warn，同一 op_id 的重放被拒绝、不再调用创建接口（补吊销的错误不能让它像"确定没有签发"一样可以同键再签）。
func TestIssue_MissingExpiryRevokeFails(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]int{"限流": http.StatusTooManyRequests, "403": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var creates, deletes atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					if status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "60")
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"message":"no"}`))
					return
				}
				creates.Add(1)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"token":"ghs_noexp_secret"}`))
			}))
			t.Cleanup(srv.Close)
			logs := &logBuffer{}
			c := &clock{t: time.Unix(1_800_000_000, 0)}
			reg := New(Config{}, Deps{GitHub: &gh.Client{AppID: 1, Key: testKey(), APIURL: srv.URL}, Log: slog.New(slog.NewJSONHandler(logs, nil)), Now: c.now})
			k := key("op-1")
			s := Scope{Installation: 9, RepoID: 1, Repository: "alice/demo", Permissions: map[string]string{"contents": "read"}}
			tok, err := reg.Issue(t.Context(), k, s, pass)
			if !errors.Is(err, ErrUnknown) || tok != nil || gh.NotApplied(err) {
				t.Fatalf("应返回结果不明：%+v %v", tok, err)
			}
			if _, err := reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrClosed) {
				t.Errorf("结果不明后同一 op_id 的重放应被拒绝：%v", err)
			}
			if creates.Load() != 1 || deletes.Load() != 1 {
				t.Errorf("签发 %d 次、吊销 %d 次，期望各 1 次", creates.Load(), deletes.Load())
			}
			if got := RPCError(err); connect.CodeOf(got) != connect.CodeAborted {
				t.Errorf("RPC 错误码 = %v，期望 Aborted（不可重试）", connect.CodeOf(got))
			}
			warns := logs.entries(t, "GitHub 签发 git 凭据的结果不明，不重试，操作以错误关闭；可能有一枚谁也没拿到的 token，到期自然失效")
			if len(warns) != 1 {
				t.Errorf("应记 1 条 warn：%s", logs.String())
			}
			if strings.Contains(logs.String(), "ghs_noexp_secret") {
				t.Error("日志中有 token")
			}
		})
	}
}

// GitHub 确定没有签发（明确拒绝）：返回它的错误，操作不变，同一 op_id 可以再试。
func TestIssue_NotIssued(t *testing.T) {
	f := newFixture(t)
	k, s := key("op-1"), f.scope(f.repo, "read")
	f.fake.SetFault(fakegithub.CreateTokenRoute(f.fake.InstallationID), fakegithub.Fault{Status: http.StatusUnprocessableEntity})
	if _, err := f.reg.Issue(t.Context(), k, s, pass); err == nil || !gh.NotApplied(err) || errors.Is(err, ErrUnknown) {
		t.Fatalf("应返回 GitHub 的拒绝：%v", err)
	}
	// 范围仍按第一次绑定：换仓库或权限的重放被拒绝，不调用 GitHub
	for name, other := range map[string]Scope{"换仓库": f.scope(f.other, "read"), "换权限": f.scope(f.repo, "write")} {
		if _, err := f.reg.Issue(t.Context(), k, other, pass); !errors.Is(err, ErrScopeMismatch) {
			t.Errorf("确定没有签发后%s应被拒绝：%v", name, err)
		}
	}
	if got := f.createCalls(); got != 1 {
		t.Fatalf("范围不一致的重放不应调用 GitHub，签发 %d 次", got)
	}
	// 同范围重试：在途时后到的同键请求等这一次的结果
	entered, release := f.pause(fakegithub.Fault{})
	retry := f.issueAsync(t.Context(), k, s)
	<-entered
	waiter := f.issueAsync(t.Context(), k, s)
	f.waitWaiters(t, k, 1)
	close(release)
	r1, r2 := <-retry, <-waiter
	if r1.err != nil || r2.err != nil || r1.tok.Token != r2.tok.Token {
		t.Fatalf("同范围重试：%v / %v", r1.err, r2.err)
	}
	if f.createCalls() != 2 || f.onlyIssued(t).Token != r1.tok.Token {
		t.Errorf("签发 %d 次，应为 2 次", f.createCalls())
	}
}

// 发起请求的客户端取消（RPC 断开）不是 attempt 失效的证据：被取消的调用拿不到 token，但操作不关闭；未取消的同键重放
// 拿到同一枚，token 没有被吊销。最终检查在不随客户端取消的 context 中执行。
func TestIssue_CallerCanceled(t *testing.T) {
	f := newFixture(t)
	k, s := key("op-1"), f.scope(f.repo, "read")
	entered, release := f.pause(fakegithub.Fault{})
	ctx, cancel := context.WithCancel(t.Context())
	ch := make(chan result, 1)
	go func() {
		// 检查像数据库查询一样遵守 context：如果用的是客户端的 context，取消之后它会失败
		tok, err := f.reg.Issue(ctx, k, s, func(c context.Context) error { return c.Err() })
		ch <- result{tok, err}
	}()
	<-entered
	cancel()
	close(release)
	r := <-ch
	if !errors.Is(r.err, context.Canceled) || r.tok != nil {
		t.Fatalf("被取消的调用应不交出：%+v %v", r.tok, r.err)
	}
	tok, err := f.reg.Issue(t.Context(), k, s, pass)
	if err != nil {
		t.Fatalf("未取消的重放应拿到 token：%v", err)
	}
	issued := f.onlyIssued(t)
	if tok.Token != issued.Token || issued.Revoked || f.revokeCalls() != 0 {
		t.Errorf("重放应拿到同一枚且未被吊销：吊销 %d 次", f.revokeCalls())
	}
}

// Invalidate 之后客户端带同一 token 的 Close：只多一次幂等的吊销（GitHub 401，按已失效处理），不报错、不建记录、
// 不记 warn，attempt 仍是失效状态。
func TestClose_AfterInvalidateHarmless(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	tok, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.Invalidate(t.Context(), attempt)
	f.wait(t)
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, tok.Token); err != nil {
		t.Fatalf("失效后的 Close 不应报错：%v", err)
	}
	if !f.revoked(t, tok.Token) || f.revokeCalls() != 2 {
		t.Errorf("应吊销且只多一次吊销请求：%d 次", f.revokeCalls())
	}
	if w := f.logs.entries(t, "吊销 git 凭据失败，不重试，到期自然失效"); len(w) != 0 {
		t.Errorf("重复吊销不应记 warn：%v", w)
	}
	f.reg.mu.Lock()
	a := f.reg.attempts[attempt]
	f.reg.mu.Unlock()
	if a == nil || !a.invalid || len(a.ops) != 0 {
		t.Errorf("失效后的 Close 不应建出记录：%+v", a)
	}
	if _, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Errorf("仍应拒绝签发：%v", err)
	}
}

// RevokeEnded：attempt 已结束时的补吊销不建任何记录（重启后登记为空）；登记中仍活动时关闭这个操作并吊销 Server 已知的
// token。
func TestRevokeEnded(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	tok, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(Config{}, Deps{GitHub: f.reg.gh, Log: f.reg.log, Now: f.clock.now})
	if err := restarted.RevokeEnded(t.Context(), k, f.fake.InstallationID, tok.Token); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, tok.Token) {
		t.Error("补吊销应吊销客户端带来的 token")
	}
	restarted.mu.Lock()
	n := len(restarted.attempts)
	restarted.mu.Unlock()
	if n != 0 {
		t.Errorf("补吊销不应建记录，登记中有 %d 个 attempt", n)
	}

	// 登记中仍活动（失效处理还没运行）：只带 op_id 也吊销 Server 已知的 token，并关闭这个操作
	k2 := key("op-2")
	tok2, err := f.reg.Issue(t.Context(), k2, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RevokeEnded(t.Context(), k2, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, tok2.Token) {
		t.Error("应吊销登记中的 token")
	}
	if _, err := f.reg.Issue(t.Context(), k2, f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Errorf("补吊销后操作应关闭：%v", err)
	}
	if err := f.reg.RevokeEnded(t.Context(), Key{Attempt: attempt}, 1, ""); err == nil {
		t.Error("没有 op_id 应拒绝")
	}
}

// GitHub 的异常响应中带着 token（解析错误回显响应中的值、错误正文回显）：返回的错误和日志都不含 token。
func TestNoTokenInErrorsAndLogs(t *testing.T) {
	const secret = "ghs_ECHOED_SECRET"
	var mode atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch m, _ := mode.Load().(string); {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"cannot revoke ` + r.Header.Get("Authorization") + `"}`))
		case m == "echo":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"` + secret + `","expires_at":"` + secret + `"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"` + secret + `","expires_at":"2099-01-01T00:00:00Z"}`))
		}
	}))
	t.Cleanup(srv.Close)
	logs := &logBuffer{}
	reg := New(Config{}, Deps{
		GitHub: &gh.Client{AppID: 1, Key: testKey(), APIURL: srv.URL},
		Log:    slog.New(slog.NewJSONHandler(logs, nil)), Now: time.Now,
	})
	s := Scope{Installation: 7, RepoID: 1, Repository: "o/r", Permissions: map[string]string{"contents": "read"}}

	mode.Store("echo")
	_, err := reg.Issue(t.Context(), key("op-1"), s, pass)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("解析失败应是结果不明：%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("返回的错误中有 token：%v", err)
	}

	mode.Store("ok")
	if _, err := reg.Issue(t.Context(), key("op-2"), s, pass); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(t.Context(), key("op-2"), 7, ""); err != nil {
		t.Fatal(err)
	}
	if len(logs.entries(t, "吊销 git 凭据失败，不重试，到期自然失效")) != 1 {
		t.Fatalf("应有吊销失败的 warn：%s", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("日志中有 token：%s", logs.String())
	}
}

// 吊销失败只记 warn（带过期时间和指纹），不重试。
func TestRevokeFailureLogged(t *testing.T) {
	f := newFixture(t)
	k := key("op-1")
	tok, err := f.reg.Issue(t.Context(), k, f.scope(f.repo, "read"), pass)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.SetFault(fakegithub.RevokeTokenRoute, fakegithub.Fault{Status: http.StatusInternalServerError})
	if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, ""); err != nil {
		t.Fatal(err)
	}
	if f.revoked(t, tok.Token) || f.revokeCalls() != 1 {
		t.Fatalf("注入的吊销失败：吊销请求 %d 次", f.revokeCalls())
	}
	warns := f.logs.entries(t, "吊销 git 凭据失败，不重试，到期自然失效")
	if len(warns) != 1 {
		t.Fatalf("应记 1 条 warn，实际 %d 条", len(warns))
	}
	w := warns[0]
	if w["level"] != "WARN" || !logTime(t, w["expires_at"]).Equal(tok.ExpiresAt) || w["token_fingerprint"] != Fingerprint(tok.Token) || w["op"] != "op-1" {
		t.Errorf("warn 的字段：%v", w)
	}
	// 签发与吊销的日志按指纹配对
	issued := f.logs.entries(t, "签发 git 凭据")
	if len(issued) != 1 || issued[0]["token_fingerprint"] != Fingerprint(tok.Token) {
		t.Errorf("签发日志：%v", issued)
	}
}

// 已失效 attempt 的记录保留 Retention 后删除；保留期内拒绝签发。
func TestInvalidate_Retention(t *testing.T) {
	f := newFixture(t)
	f.reg.Invalidate(t.Context(), attempt)
	if _, err := f.reg.Issue(t.Context(), key("op-1"), f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Fatalf("失效后应拒绝：%v", err)
	}
	f.clock.add(DefaultRetention - time.Second)
	f.reg.Invalidate(t.Context(), Attempt{Domain: DomainCI, ID: 1})
	f.reg.mu.Lock()
	_, kept := f.reg.attempts[attempt]
	f.reg.mu.Unlock()
	if !kept {
		t.Fatal("保留期内不应删除")
	}
	f.clock.add(time.Second)
	f.reg.Invalidate(t.Context(), Attempt{Domain: DomainCI, ID: 2})
	f.reg.mu.Lock()
	_, kept = f.reg.attempts[attempt]
	n := len(f.reg.attempts)
	f.reg.mu.Unlock()
	if kept || n != 2 {
		t.Errorf("保留期满应删除：仍保留 %v，共 %d 个", kept, n)
	}
}

// 参数不完整时拒绝，不调用 GitHub。
func TestIssue_InvalidArguments(t *testing.T) {
	f := newFixture(t)
	s := f.scope(f.repo, "read")
	for name, call := range map[string]func() error{
		"没有 op_id": func() error { _, err := f.reg.Issue(t.Context(), Key{Attempt: attempt}, s, pass); return err },
		"没有权限": func() error {
			_, err := f.reg.Issue(t.Context(), key("op"), Scope{Installation: 1, RepoID: 1}, pass)
			return err
		},
		"没有最终检查":     func() error { _, err := f.reg.Issue(t.Context(), key("op"), s, nil); return err },
		"关闭没有 op_id": func() error { return f.reg.Close(t.Context(), Key{Attempt: attempt}, 1, "") },
	} {
		if call() == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	if f.createCalls() != 0 {
		t.Error("不应调用 GitHub")
	}
}

// pauseRevoke 让接下来 times 次吊销请求在 GitHub 处理之前停住，直到 release 关闭；entered 在第一次停住时关闭。
func (f *fixture) pauseRevoke(t *testing.T, times int) (entered <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.fake.SetFault(fakegithub.RevokeTokenRoute, fakegithub.Fault{Times: times, Pause: func() {
		once.Do(func() { close(in) })
		<-out
	}})
	return in, out
}

// Invalidate 同步完成失效（之后的签发被拒绝），对 GitHub 的吊销在后台：吊销被挡住时 Invalidate 已经返回，放行后 Wait
// 返回、token 已吊销。
func TestInvalidate_RevokesInBackground(t *testing.T) {
	f := newFixture(t)
	s := f.scope(f.repo, "read")
	tok, err := f.reg.Issue(t.Context(), key("op-1"), s, pass)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := f.pauseRevoke(t, 1)
	f.reg.Invalidate(t.Context(), attempt) // 同步吊销时会在这里停住
	if _, err := f.reg.Issue(t.Context(), key("op-2"), s, pass); !errors.Is(err, ErrClosed) {
		t.Fatalf("Invalidate 返回后签发应被拒绝：%v", err)
	}
	<-entered
	if f.revoked(t, tok.Token) {
		t.Fatal("吊销被挡住时 token 不应已经吊销")
	}
	close(release)
	f.wait(t)
	if !f.revoked(t, tok.Token) {
		t.Fatal("放行后 token 应被吊销")
	}
}

// 一次失效的全部吊销共用 InvalidateTimeout 的总时限（不按 token 数累乘）：吊销一直被挡住时 Wait 在总时限后返回，
// 每枚 token 记一条 warn。
func TestInvalidate_TotalDeadline(t *testing.T) {
	f := newFixtureWith(t, Config{InvalidateTimeout: 50 * time.Millisecond})
	s := f.scope(f.repo, "read")
	for _, op := range []string{"op-1", "op-2"} {
		if _, err := f.reg.Issue(t.Context(), key(op), s, pass); err != nil {
			t.Fatal(err)
		}
	}
	_, release := f.pauseRevoke(t, 2)
	defer close(release)
	f.reg.Invalidate(t.Context(), attempt)
	f.wait(t)
	if w := f.logs.entries(t, "吊销 git 凭据失败，不重试，到期自然失效"); len(w) != 2 {
		t.Fatalf("两枚 token 的吊销都应在总时限内结束并记 warn：%d 条", len(w))
	}
}

// 最终检查自己的时限到了：操作关闭、token 吊销，错误是 ErrCheckFailed，错误链中没有 context 的错误（调用方不会当成
// 客户端取消、给出可重放的信号）；同一 op_id 重放被拒绝。
func TestIssue_CheckTimeout(t *testing.T) {
	f := newFixtureWith(t, Config{CheckTimeout: 10 * time.Millisecond})
	k, s := key("op-1"), f.scope(f.repo, "read")
	_, err := f.reg.Issue(t.Context(), k, s, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, ErrCheckFailed) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，应为 ErrCheckFailed 且不含 context 的错误", err)
	}
	if !f.revoked(t, f.onlyIssued(t).Token) {
		t.Error("token 应被吊销")
	}
	if _, err := f.reg.Issue(t.Context(), k, s, pass); !errors.Is(err, ErrClosed) {
		t.Errorf("同一 op_id 重放应被拒绝：%v", err)
	}
}

// 签发在途时操作被关闭，GitHub 随后确定没有签发或结果不明：优先报告已关闭（不给出可以同键重试的信号）；结果不明的
// 同时满足 ErrUnknown（调用方据此记审计），仍记 warn。
func TestIssue_ClosedWins(t *testing.T) {
	tests := []struct {
		name        string
		fault       fakegithub.Fault
		wantUnknown bool
	}{
		{"确定没有签发", fakegithub.Fault{Status: http.StatusForbidden}, false},
		{"结果不明", fakegithub.Fault{Lost: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			k, s := key("op-1"), f.scope(f.repo, "read")
			entered, release := f.pause(tt.fault)
			ch := f.issueAsync(t.Context(), k, s)
			<-entered
			if err := f.reg.Close(t.Context(), k, f.fake.InstallationID, ""); err != nil {
				t.Fatal(err)
			}
			close(release)
			r := <-ch
			if !errors.Is(r.err, ErrClosed) || errors.Is(r.err, ErrUnknown) != tt.wantUnknown || gh.NotApplied(r.err) {
				t.Fatalf("错误 = %v，应为 ErrClosed（结果不明 %v），不能是可重试的 NotApplied", r.err, tt.wantUnknown)
			}
			warned := len(f.logs.entries(t, "GitHub 签发 git 凭据的结果不明，不重试，操作以错误关闭；可能有一枚谁也没拿到的 token，到期自然失效")) > 0
			if warned != tt.wantUnknown {
				t.Errorf("结果不明的 warn：%v，期望 %v", warned, tt.wantUnknown)
			}
		})
	}
}

// Discard 关闭已登记的操作并吊销它的 token；操作不存在时不新建记录（之后同键仍可签发）。
func TestDiscard(t *testing.T) {
	f := newFixture(t)
	s := f.scope(f.repo, "read")
	tok, err := f.reg.Issue(t.Context(), key("op-1"), s, pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Discard(t.Context(), key("op-1")); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, tok.Token) {
		t.Error("token 应被吊销")
	}
	if _, err := f.reg.Issue(t.Context(), key("op-1"), s, pass); !errors.Is(err, ErrClosed) {
		t.Errorf("Discard 之后同一 op_id 应被拒绝：%v", err)
	}
	if err := f.reg.Discard(t.Context(), key("op-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Issue(t.Context(), key("op-2"), s, pass); err != nil {
		t.Errorf("对不存在的操作 Discard 不应留下记录：%v", err)
	}
}

// 对账：没有记录时不查询；查询失败时不失效、下一次补上；仍在执行的不动；已经结束（查询结果中没有）的失效并吊销 token；
// 只处理指定的领域。
func TestReconcile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	calls := 0
	var running []int64
	var fail error
	live := func(_ context.Context, ids []int64) ([]int64, error) {
		calls++
		return running, fail
	}
	if n, err := f.reg.Reconcile(t.Context(), DomainAI, live); n != 0 || err != nil || calls != 0 {
		t.Fatalf("没有记录时不应查询：n=%d err=%v calls=%d", n, err, calls)
	}
	if _, err := f.reg.Issue(t.Context(), key("op"), f.scope(f.repo, "read"), pass); err != nil {
		t.Fatal(err)
	}
	other := Attempt{Domain: DomainCI, ID: attempt.ID} // 另一个领域、同一个编号
	f.reg.mu.Lock()
	f.reg.attempt(other)
	f.reg.mu.Unlock()
	tok := f.onlyIssued(t)
	// 查询失败：不失效
	fail = errors.New("数据库不可用")
	if n, err := f.reg.Reconcile(t.Context(), DomainAI, live); n != 0 || !errors.Is(err, fail) {
		t.Fatalf("查询失败时：n=%d err=%v", n, err)
	}
	// 仍在执行：不动
	fail, running = nil, []int64{attempt.ID}
	if n, err := f.reg.Reconcile(t.Context(), DomainAI, live); n != 0 || err != nil {
		t.Fatalf("仍在执行时：n=%d err=%v", n, err)
	}
	f.wait(t)
	if f.revoked(t, tok.Token) {
		t.Fatal("仍在执行的 attempt 的 token 不应被吊销")
	}
	// 已经结束：失效、吊销，之后不再签发
	running = nil
	if n, err := f.reg.Reconcile(t.Context(), DomainAI, live); n != 1 || err != nil {
		t.Fatalf("已经结束时：n=%d err=%v", n, err)
	}
	f.wait(t)
	if !f.revoked(t, tok.Token) {
		t.Fatal("已经结束的 attempt 的 token 应被吊销")
	}
	if _, err := f.reg.Issue(t.Context(), key("op2"), f.scope(f.repo, "read"), pass); !errors.Is(err, ErrClosed) {
		t.Fatalf("失效后不应再签发：%v", err)
	}
	if got := f.reg.holding(DomainAI); len(got) != 0 {
		t.Errorf("失效后不应再有 AI 的记录：%v", got)
	}
	if got := f.reg.holding(DomainCI); len(got) != 1 || got[0] != other.ID {
		t.Errorf("另一个领域的记录不应受影响：%v", got)
	}
	before := calls
	if _, err := f.reg.Reconcile(t.Context(), DomainAI, live); err != nil || calls != before {
		t.Errorf("没有记录时不应查询：err=%v calls=%d→%d", err, before, calls)
	}
}

// 对账分批查询，每批最多 reconcileBatch 个。
func TestReconcile_Batches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	total := 2*reconcileBatch + 1
	f.reg.mu.Lock()
	for i := range total {
		f.reg.attempt(Attempt{Domain: DomainCI, ID: int64(i + 1)})
	}
	f.reg.mu.Unlock()
	var sizes []int
	live := func(_ context.Context, ids []int64) ([]int64, error) {
		sizes = append(sizes, len(ids))
		return slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return id == 1 }), nil
	}
	n, err := f.reg.Reconcile(t.Context(), DomainCI, live)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !slices.Equal(sizes, []int{reconcileBatch, reconcileBatch, 1}) {
		t.Errorf("每批的数量：%v", sizes)
	}
	if got := f.reg.holding(DomainCI); len(got) != total-1 || slices.Contains(got, 1) {
		t.Errorf("只有 attempt 1 应失效，剩下 %d 个", len(got))
	}
}
