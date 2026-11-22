package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// ---- 文件系统 ----

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func newTestDirs(t *testing.T) *attemptDirs {
	t.Helper()
	d, err := newAttemptDirs(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---- git ----

func runGit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v：%v：%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// newTestRepo 创建一个包含普通文件、可执行文件和符号链接的仓库，返回仓库目录和 tree 条目。
func newTestRepo(t *testing.T) (string, []actionkey.TreeEntry) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	repo := t.TempDir()
	runGit(t, repo, "", "init", "-q")
	mustWrite(t, filepath.Join(repo, "a.txt"), "hello\n")
	mustWrite(t, filepath.Join(repo, "bin", "run.sh"), "#!/bin/sh\necho run\n")
	if err := os.Chmod(filepath.Join(repo, "bin", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "a.txt", filepath.Join(repo, "link"))
	runGit(t, repo, "", "add", "-A")
	runGit(t, repo, "", "commit", "-q", "-m", "init")
	out := runGit(t, repo, "", "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	return repo, parseLsTree([]byte(out))
}

// commitFiles 在仓库中写入文件（路径 → 内容）并提交，返回 commit sha。
func commitFiles(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		mustWrite(t, filepath.Join(repo, filepath.FromSlash(p)), c)
	}
	runGit(t, repo, "", "add", "-A")
	runGit(t, repo, "", "commit", "-q", "-m", "update")
	return runGit(t, repo, "", "rev-parse", "HEAD")
}

// ---- 假的 RunnerService ----

// fakeServer 是内存版的 RunnerService（实现 runnerv1connect.RunnerServiceHandler），
// 同时在 /cas/<digest>?sig=… 上提供预签名的 PUT / GET。不依赖 Server 的包，只按协议约定实现：
// 认证（ed25519 签名、Bearer token）、epoch 校验、日志按 offset 幂等追加、CAS 上传时校验 sha256。
type fakeServer struct {
	runnerv1connect.UnimplementedRunnerServiceHandler

	srv *httptest.Server

	mu sync.Mutex
	// 认证
	enrollToken string            // Enroll 接受的令牌
	pub         ed25519.PublicKey // Runner 的公钥（Enroll 时登记，或由 newTestRunner 设置）
	runnerID    int64
	tokenSeq    int
	token       string // 当前有效的 access token
	authCalls   int
	revoked     bool // Authenticate 和其他 RPC 都返回已吊销
	enrolls     []*runnerv1.EnrollRequest
	enrollKind  string // Enroll 返回的 Runner 类型（空表示旧版 Server，按 ci 处理）

	// Acquire
	jobs        []*runnerv1.JobSpec // 下一次 Acquire 返回的 job
	acquires    []*runnerv1.AcquireRequest
	acquireErrs []error // 依次作为 Acquire 的错误返回（之后恢复正常）
	drain       bool    // Acquire 的响应中带 drain 指令
	// upgrade 不为 nil 时 Acquire 的响应中带这个升级指令，并且立即返回（不长轮询：让 Runner 在执行任务期间也不停地收到指令）
	upgrade     *runnerv1.UpgradeDirective
	acquireHook func(*runnerv1.AcquireRequest)
	wake        chan struct{} // 有新的 job 时唤醒长轮询

	// 每个 attempt 当前的 epoch；请求中的 epoch 不一致时返回 STALE_EPOCH
	epochs map[int64]int64

	// specs 是登记过的 job（IssueGitToken 据此校验仓库）。
	specs map[int64]*runnerv1.JobSpec

	secrets    map[string]string // FetchSecrets 返回的 secret
	secretsErr error
	// legacy 模拟只实现旧 RPC 的 Server：FetchSecrets、IssueGitToken、RevokeGitToken 返回 Unimplemented；
	// legacyGit 模拟探测之后回滚的 Server：只有 IssueGitToken、RevokeGitToken 返回 Unimplemented。
	legacy, legacyGit bool
	credCalls         int // FetchCredentials 的调用次数（新 Runner 不调用）
	// gitOps 是签发登记（与 Server 相同，键是 attempt、epoch、op），gitTokenSeq 用于生成 token。
	gitOps      map[fakeGitKey]*fakeGitOp
	gitTokenSeq int
	// issueErrs 依次作为 IssueGitToken 的错误返回（Server 明确返回的错误：不建操作，之后恢复正常）；issueLost 是之后多少次
	// 签发"已签出 token 但响应丢失"（登记 token 后直接断开连接，客户端得到它自己合成的 Unavailable，与真实的传输相同）；
	// issueBadGateway 是之后多少次 IssueGitToken 请求由"网关"回复普通的 HTTP 502（不到达 Server）。
	issueErrs       []error
	issueLost       int
	issueBadGateway int
	// issueGarbled 是之后多少次签发"已签出 token，但回复的正文无法解码"（代理改坏了 200 的正文）。
	issueGarbled int
	// issueProxy401 是之后多少次签发"已签出 token，但代理回复普通的 HTTP 401"。
	issueProxy401 int
	// issueHTTP 是到达 transport 的 IssueGitToken HTTP 请求数（含被"网关"拦下的）。
	issueHTTP int
	// issueHook 不为 nil 时在签发之前调用（不持有锁）：模拟 Server 慢。之后操作已被关闭的，与 Server 相同不签发、返回
	// FailedPrecondition。issueSentHook 在 token 已经交出、响应返回之前调用：模拟响应在途（之后关闭也拿得到这枚 token）。
	issueHook     func(*runnerv1.IssueGitTokenRequest)
	issueSentHook func(*runnerv1.IssueGitTokenRequest)
	// revokeHook 不为 nil 时在处理每次 RevokeGitToken 之后调用（不持有锁）。
	revokeHook func(*runnerv1.RevokeGitTokenRequest)
	issues     []*runnerv1.IssueGitTokenRequest
	revokes    []*runnerv1.RevokeGitTokenRequest
	// gitEvents 是按到达顺序的 "issue <op>"、"revoke <op>"。
	gitEvents []string

	heartbeats    []*runnerv1.HeartbeatRequest
	heartbeatHook func(*runnerv1.HeartbeatRequest) (*runnerv1.HeartbeatResponse, error)

	logs        map[int64][]byte                 // attempt → 已提交的日志
	appendHook  func(*runnerv1.AppendLogRequest) // 不为 nil 时在处理每次 AppendLog 前调用（不持有锁）
	appendCalls int
	logLimit    int // >0 时日志总长超过它的追加被拒收（返回原 offset，与 Server 的上限一致）
	appendErrs  []error

	blobs    map[string][]byte
	putCalls int
	// putHook 不为 nil 时在每次 PUT 前调用，返回非 0 的状态码时直接以它响应（模拟上传失败）。
	putHook func(digest string) int

	completes    []*runnerv1.CompleteRequest
	completeErrs []error
	completeHook func(*runnerv1.CompleteRequest) // 不为 nil 时在处理每次 Complete 前调用（不持有锁）
	completed    chan *runnerv1.CompleteRequest
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		enrollToken: "enroll-token",
		runnerID:    7,
		wake:        make(chan struct{}, 1),
		epochs:      map[int64]int64{},
		specs:       map[int64]*runnerv1.JobSpec{},
		gitOps:      map[fakeGitKey]*fakeGitOp{},
		logs:        map[int64][]byte{},
		blobs:       map[string][]byte{},
		completed:   make(chan *runnerv1.CompleteRequest, 16),
	}
	mux := http.NewServeMux()
	mux.Handle(runnerv1connect.NewRunnerServiceHandler(f))
	mux.HandleFunc("/cas/{digest}", f.serveCAS)
	f.srv = httptest.NewServer(f.transport(mux))
	t.Cleanup(func() {
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *fakeServer) URL() string { return f.srv.URL }

// faultKey 是请求 context 中的 *respFault：处理中设置，transport 据此改写回复（模拟 Server 与 Runner 之间的传输）。
type faultKey struct{}

// respFault 是 transport 对回复的改写。
type respFault int

const (
	// faultNone 原样写回。
	faultNone respFault = iota
	// faultDrop 丢掉回复、直接断开连接（Server 已经处理）。
	faultDrop
	// faultNotFound 回复普通的 HTTP 404（与真实的旧 Server 相同：生成的 handler 对不认识的 RPC 调用 http.NotFound）。
	faultNotFound
	// faultGarble 回复 200，但正文不是可以解码的 protobuf（例如代理改坏了正文；Server 已经处理）。
	faultGarble
	// faultProxy401 回复普通的 HTTP 401（反向代理的回复，不是 Kiln 的 Connect 错误；Server 已经处理）。
	faultProxy401
)

// setFault 设置这次请求的回复改写（请求不经过 transport 时什么也不做）。
func setFault(ctx context.Context, f respFault) {
	if p, ok := ctx.Value(faultKey{}).(*respFault); ok {
		*p = f
	}
}

// faultProcedures 是经过 transport 改写的 RPC。
var faultProcedures = map[string]bool{
	runnerv1connect.RunnerServiceIssueGitTokenProcedure:  true,
	runnerv1connect.RunnerServiceRevokeGitTokenProcedure: true,
	runnerv1connect.RunnerServiceFetchSecretsProcedure:   true,
}

// transport 在 Server 前面模拟传输：IssueGitToken 按 issueBadGateway 回复普通的 502（不到达 Server）；faultProcedures 的
// 回复先缓冲，处理结束后按处理中设置的 respFault 写回、断开连接、回复 404 或者改坏正文。
func (f *fakeServer) transport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !faultProcedures[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == runnerv1connect.RunnerServiceIssueGitTokenProcedure {
			f.mu.Lock()
			f.issueHTTP++
			bad := f.issueBadGateway > 0
			if bad {
				f.issueBadGateway--
			}
			f.mu.Unlock()
			if bad {
				http.Error(w, "Bad Gateway", http.StatusBadGateway)
				return
			}
		}
		fault := new(respFault)
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), faultKey{}, fault)))
		switch *fault {
		case faultDrop:
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case faultNotFound:
			http.NotFound(w, r)
			return
		case faultProxy401:
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		case faultGarble:
			maps.Copy(w.Header(), rec.Header())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("\xff\xff\xff not a protobuf message"))
			return
		case faultNone:
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// casSig 是 fakeServer 签发的"预签名"参数。
func casSig(method, digest string) string {
	s := sha256.Sum256([]byte("fake-cas:" + method + ":" + digest))
	return hex.EncodeToString(s[:8])
}

func (f *fakeServer) serveCAS(w http.ResponseWriter, r *http.Request) {
	d := r.PathValue("digest")
	if r.URL.Query().Get("sig") != casSig(r.Method, d) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		f.mu.Lock()
		hook := f.putHook
		f.mu.Unlock()
		if hook != nil {
			if code := hook(d); code != 0 {
				_, _ = io.Copy(io.Discard, r.Body)
				http.Error(w, "injected failure", code)
				return
			}
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if cas.Digest(b) != d {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.blobs[d] = b
		f.putCalls++
		f.mu.Unlock()
	case http.MethodGet:
		f.mu.Lock()
		b, ok := f.blobs[d]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// authorize 校验 Bearer token 和 epoch。
func (f *fakeServer) authorize(h http.Header, ref *runnerv1.AttemptRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked {
		return rpcerr.RunnerRevoked()
	}
	if f.token == "" || h.Get("Authorization") != "Bearer "+f.token {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("token 无效"))
	}
	if ref != nil {
		if e, ok := f.epochs[ref.AttemptId]; ok && e != ref.Epoch {
			return rpcerr.StaleEpoch()
		}
	}
	return nil
}

// expireToken 让当前 access token 失效（模拟 Server 重启或 token 过期）。
func (f *fakeServer) expireToken() {
	f.mu.Lock()
	f.token = "expired"
	f.mu.Unlock()
}

// addJob 让下一次 Acquire 返回 spec，并登记它的 epoch。
func (f *fakeServer) addJob(spec *runnerv1.JobSpec) {
	f.mu.Lock()
	f.jobs = append(f.jobs, spec)
	f.epochs[spec.Ref.AttemptId] = spec.Ref.Epoch
	f.specs[spec.Ref.AttemptId] = spec
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// register 只登记 epoch 和 job（直接执行 job、不经过 Acquire 的测试使用）。
func (f *fakeServer) register(spec *runnerv1.JobSpec) {
	f.mu.Lock()
	f.epochs[spec.Ref.AttemptId] = spec.Ref.Epoch
	f.specs[spec.Ref.AttemptId] = spec
	f.mu.Unlock()
}

// putBlob 把内容放进 Server 的 CAS，返回 digest。
func (f *fakeServer) putBlob(b []byte) string {
	d := cas.Digest(b)
	f.mu.Lock()
	f.blobs[d] = b
	f.mu.Unlock()
	return d
}

func (f *fakeServer) blob(d string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[d]
	return b, ok
}

func (f *fakeServer) log(attempt int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.logs[attempt])
}

func (f *fakeServer) heartbeatCalls() []*runnerv1.HeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*runnerv1.HeartbeatRequest(nil), f.heartbeats...)
}

func (f *fakeServer) completeCalls() []*runnerv1.CompleteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*runnerv1.CompleteRequest(nil), f.completes...)
}

func (f *fakeServer) acquireRequests() []*runnerv1.AcquireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*runnerv1.AcquireRequest(nil), f.acquires...)
}

// popErr 取出 errs 中的第一个错误。
func popErr(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (f *fakeServer) Enroll(_ context.Context, req *connect.Request[runnerv1.EnrollRequest]) (*connect.Response[runnerv1.EnrollResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrolls = append(f.enrolls, req.Msg)
	if req.Msg.Token != f.enrollToken {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("注册令牌无效"))
	}
	f.pub = ed25519.PublicKey(req.Msg.PublicKey)
	return connect.NewResponse(&runnerv1.EnrollResponse{RunnerId: f.runnerID, Pool: "default", Kind: f.enrollKind}), nil
}

func (f *fakeServer) Authenticate(_ context.Context, req *connect.Request[runnerv1.AuthenticateRequest]) (*connect.Response[runnerv1.AuthenticateResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	if f.revoked {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("已吊销"))
	}
	m := req.Msg
	msg := fmt.Sprintf("kiln-runner-auth:%d:%d:%s", m.RunnerId, m.Timestamp, m.Nonce)
	if m.RunnerId != f.runnerID || f.pub == nil || !ed25519.Verify(f.pub, []byte(msg), m.Signature) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("签名无效"))
	}
	f.tokenSeq++
	f.token = fmt.Sprintf("tok-%d", f.tokenSeq)
	return connect.NewResponse(&runnerv1.AuthenticateResponse{AccessToken: f.token, ExpiresAt: time.Now().Add(time.Hour).Unix()}), nil
}

func (f *fakeServer) Acquire(ctx context.Context, req *connect.Request[runnerv1.AcquireRequest]) (*connect.Response[runnerv1.AcquireResponse], error) {
	if err := f.authorize(req.Header(), nil); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.acquires = append(f.acquires, req.Msg)
	hook := f.acquireHook
	err := popErr(&f.acquireErrs)
	f.mu.Unlock()
	if hook != nil {
		hook(req.Msg)
	}
	if err != nil {
		return nil, err
	}
	for {
		f.mu.Lock()
		// registry 指向不可达的本机地址：升级测试中 Runner 查询 latest 时不访问外网
		resp := &runnerv1.AcquireResponse{Directives: &runnerv1.RunnerDirectives{Drain: f.drain, Upgrade: f.upgrade, UpgradeRegistry: "http://127.0.0.1:1/"}}
		if !f.drain && req.Msg.MaxJobs > 0 && len(f.jobs) > 0 {
			n := min(int(req.Msg.MaxJobs), len(f.jobs))
			resp.Jobs, f.jobs = f.jobs[:n], f.jobs[n:]
		}
		drain := f.drain
		f.mu.Unlock()
		// 与 Server 一致：不领取任务（max_jobs 为 0）时立即返回
		if len(resp.Jobs) > 0 || drain || req.Msg.MaxJobs <= 0 || resp.Directives.Upgrade != nil {
			return connect.NewResponse(resp), nil
		}
		// 长轮询：等新的 job 或请求结束
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		case <-f.wake:
		}
	}
}

// FetchCredentials 是旧 RPC：新 Runner 不调用，只记次数。
func (f *fakeServer) FetchCredentials(_ context.Context, req *connect.Request[runnerv1.FetchCredentialsRequest]) (*connect.Response[runnerv1.FetchCredentialsResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credCalls++
	return connect.NewResponse(&runnerv1.FetchCredentialsResponse{GitToken: "ghs_legacy_job_token", Secrets: f.secrets}), nil
}

// fakeNotFound 让这次请求像真实的旧 Server 一样得到普通的 404（transport 改写回复；客户端合成 Unimplemented，不是
// Server 发来的 Connect 错误）。返回的错误只用于结束处理，不会交给客户端。
func fakeNotFound(ctx context.Context) error {
	setFault(ctx, faultNotFound)
	return connect.NewError(connect.CodeUnimplemented, errors.New("transport 回复 404"))
}

func (f *fakeServer) FetchSecrets(ctx context.Context, req *connect.Request[runnerv1.FetchSecretsRequest]) (*connect.Response[runnerv1.FetchSecretsResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.legacy:
		return nil, fakeNotFound(ctx)
	case f.secretsErr != nil:
		return nil, f.secretsErr
	}
	return connect.NewResponse(&runnerv1.FetchSecretsResponse{Secrets: f.secrets}), nil
}

// fakeGitKey 是签发登记中操作的键（与 Server 的 gitcred 相同）。
type fakeGitKey struct {
	attempt, epoch int64
	op             string
}

func fakeKey(ref *runnerv1.AttemptRef, op string) fakeGitKey {
	return fakeGitKey{attempt: ref.GetAttemptId(), epoch: ref.GetEpoch(), op: op}
}

// fakeGitOp 是签发登记中的一个操作（与 Server 的 gitcred 相同的规则：每个操作至多一枚 token，关闭是终态，关闭之后不再交出）。
type fakeGitOp struct {
	repo   string
	token  string
	closed bool
	// revoked 表示 token 已吊销（吊销后 gitHTTP 拒绝使用）。
	revoked bool
}

func (f *fakeServer) IssueGitToken(ctx context.Context, req *connect.Request[runnerv1.IssueGitTokenRequest]) (*connect.Response[runnerv1.IssueGitTokenResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	m := req.Msg
	key := fakeKey(m.Ref, m.OpId)
	f.mu.Lock()
	f.issues = append(f.issues, m)
	f.gitEvents = append(f.gitEvents, "issue "+m.OpId)
	if f.legacy || f.legacyGit {
		f.mu.Unlock()
		return nil, fakeNotFound(ctx)
	}
	if err := popErr(&f.issueErrs); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	spec := f.specs[m.Ref.AttemptId]
	allowed := spec != nil && strings.EqualFold(m.Repository, spec.GetSource().GetRepository())
	for _, r := range spec.GetRepositories() {
		allowed = allowed || strings.EqualFold(m.Repository, r)
	}
	o := f.gitOps[key]
	switch {
	case !allowed:
		f.mu.Unlock()
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("仓库 %q 不是任务仓库，也不在 repositories 中", m.Repository))
	case o != nil && o.closed:
		f.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("操作已关闭"))
	case o != nil && !strings.EqualFold(o.repo, m.Repository):
		f.mu.Unlock()
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("同一操作换了仓库"))
	case o == nil:
		o = &fakeGitOp{repo: m.Repository}
		f.gitOps[key] = o
	}
	hook := f.issueHook
	f.mu.Unlock()
	if hook != nil {
		hook(m)
	}
	f.mu.Lock()
	if o.closed {
		// 签发期间操作被关闭：不交出（与 Server 相同）
		f.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("操作已关闭"))
	}
	if o.token == "" {
		f.gitTokenSeq++
		o.token = fmt.Sprintf("ghs_fake%04d_%s", f.gitTokenSeq, strings.Repeat("x", 20))
	}
	tok, sent := o.token, f.issueSentHook
	lost := f.issueLost > 0
	if lost {
		f.issueLost--
	}
	garbled := !lost && f.issueGarbled > 0
	if garbled {
		f.issueGarbled--
	}
	proxy401 := !lost && !garbled && f.issueProxy401 > 0
	if proxy401 {
		f.issueProxy401--
	}
	f.mu.Unlock()
	if lost {
		setFault(ctx, faultDrop)
		return nil, connect.NewError(connect.CodeInternal, errors.New("响应丢失（transport 不会把它交给客户端）"))
	}
	if garbled {
		// token 已经登记在操作上，回复的正文被改坏：客户端解码失败（本地合成的错误）
		setFault(ctx, faultGarble)
	}
	if proxy401 {
		// token 已经登记在操作上，代理却回复普通的 401：客户端合成 Unauthenticated（不是 wire error）
		setFault(ctx, faultProxy401)
	}
	if sent != nil {
		sent(m)
	}
	return connect.NewResponse(&runnerv1.IssueGitTokenResponse{Token: tok, ExpiresAt: time.Now().Add(time.Hour).Unix()}), nil
}

func (f *fakeServer) RevokeGitToken(ctx context.Context, req *connect.Request[runnerv1.RevokeGitTokenRequest]) (*connect.Response[runnerv1.RevokeGitTokenResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	m := req.Msg
	f.mu.Lock()
	f.revokes = append(f.revokes, m)
	f.gitEvents = append(f.gitEvents, "revoke "+m.OpId)
	if f.legacy || f.legacyGit {
		f.mu.Unlock()
		return nil, fakeNotFound(ctx)
	}
	key := fakeKey(m.Ref, m.OpId)
	o := f.gitOps[key]
	if o == nil {
		o = &fakeGitOp{}
		f.gitOps[key] = o
	}
	o.closed, o.revoked = true, true
	hook := f.revokeHook
	f.mu.Unlock()
	if hook != nil {
		hook(m)
	}
	return connect.NewResponse(&runnerv1.RevokeGitTokenResponse{}), nil
}

// gitTokenValid 判断 tok 是不是签发给仓库 repo、还没有吊销的 token（gitHTTP 据此认证）。
func (f *fakeServer) gitTokenValid(tok, repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.gitOps {
		if o.token == tok {
			return !o.revoked && strings.EqualFold(o.repo, repo)
		}
	}
	return false
}

// fakeGitOpState 是签发登记中一个操作的快照。
type fakeGitOpState struct {
	op, repo, token string
	revoked         bool
}

// gitOpStates 返回签发过 token 的操作（按 token 排序：签发顺序）。
func (f *fakeServer) gitOpStates() []fakeGitOpState {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeGitOpState
	for k, o := range f.gitOps {
		if o.token != "" {
			out = append(out, fakeGitOpState{op: k.op, repo: o.repo, token: o.token, revoked: o.revoked})
		}
	}
	slices.SortFunc(out, func(a, b fakeGitOpState) int { return strings.Compare(a.token, b.token) })
	return out
}

// revokeRequests、issueRequests 返回收到的 RevokeGitToken、IssueGitToken 请求。
func (f *fakeServer) revokeRequests() []*runnerv1.RevokeGitTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.revokes)
}

// eventIndex 返回事件 ev 第一次出现的位置（没有时为 -1）。
func (f *fakeServer) eventIndex(ev string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Index(f.gitEvents, ev)
}

func (f *fakeServer) issueRequests() []*runnerv1.IssueGitTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.issues)
}

func (f *fakeServer) Heartbeat(_ context.Context, req *connect.Request[runnerv1.HeartbeatRequest]) (*connect.Response[runnerv1.HeartbeatResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.heartbeats = append(f.heartbeats, req.Msg)
	hook := f.heartbeatHook
	f.mu.Unlock()
	if hook != nil {
		resp, err := hook(req.Msg)
		if err != nil || resp != nil {
			return connect.NewResponse(resp), err
		}
	}
	return connect.NewResponse(&runnerv1.HeartbeatResponse{}), nil
}

func (f *fakeServer) AppendLog(_ context.Context, req *connect.Request[runnerv1.AppendLogRequest]) (*connect.Response[runnerv1.AppendLogResponse], error) {
	// 与 Server 一致：日志只要求 attempt 属于这个 Runner
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.appendHook
	f.mu.Unlock()
	if hook != nil {
		hook(req.Msg)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	if err := popErr(&f.appendErrs); err != nil {
		return nil, err
	}
	id, off, data := req.Msg.Ref.AttemptId, req.Msg.Offset, req.Msg.Data
	cur := f.logs[id]
	size := int64(len(cur))
	if off > size {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("日志不连续：已有 %d 字节，收到 offset %d", size, off))
	}
	end := off + int64(len(data))
	if end <= size {
		return connect.NewResponse(&runnerv1.AppendLogResponse{Committed: size}), nil
	}
	data = data[size-off:]
	if f.logLimit > 0 && len(cur)+len(data) > f.logLimit {
		return connect.NewResponse(&runnerv1.AppendLogResponse{Committed: size}), nil
	}
	f.logs[id] = append(cur, data...)
	return connect.NewResponse(&runnerv1.AppendLogResponse{Committed: int64(len(f.logs[id]))}), nil
}

func (f *fakeServer) PrepareUpload(_ context.Context, req *connect.Request[runnerv1.PrepareUploadRequest]) (*connect.Response[runnerv1.PrepareUploadResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &runnerv1.PrepareUploadResponse{}
	for _, b := range req.Msg.Blobs {
		if !cas.ValidDigest(b.Digest) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("无效的 digest %q", b.Digest))
		}
		if _, ok := f.blobs[b.Digest]; ok {
			continue // 已有的对象不需要上传
		}
		// 相对地址：由 Runner 补全为 Server 地址
		resp.Uploads = append(resp.Uploads, &runnerv1.UploadTarget{Digest: b.Digest, Url: "/cas/" + b.Digest + "?sig=" + casSig(http.MethodPut, b.Digest)})
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeServer) PrepareDownload(_ context.Context, req *connect.Request[runnerv1.PrepareDownloadRequest]) (*connect.Response[runnerv1.PrepareDownloadResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &runnerv1.PrepareDownloadResponse{}
	for _, d := range req.Msg.Digests {
		if _, ok := f.blobs[d]; !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("对象 %s 不存在", d))
		}
		// 绝对地址：Runner 原样使用
		resp.Downloads = append(resp.Downloads, &runnerv1.UploadTarget{Digest: d, Url: f.srv.URL + "/cas/" + d + "?sig=" + casSig(http.MethodGet, d)})
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeServer) Complete(_ context.Context, req *connect.Request[runnerv1.CompleteRequest]) (*connect.Response[runnerv1.CompleteResponse], error) {
	if err := f.authorize(req.Header(), req.Msg.Ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.completeHook
	f.mu.Unlock()
	if hook != nil {
		hook(req.Msg)
	}
	f.mu.Lock()
	if err := popErr(&f.completeErrs); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.completes = append(f.completes, req.Msg)
	// 结果只能提交一次：之后同一个 epoch 的请求都是过期的
	f.epochs[req.Msg.Ref.AttemptId] = req.Msg.Ref.Epoch + 1
	f.mu.Unlock()
	f.completed <- req.Msg
	return connect.NewResponse(&runnerv1.CompleteResponse{}), nil
}

// ---- Runner 与 job ----

// testSeed 是测试 Runner 的 ed25519 私钥种子（固定值，签名可复现）。
var testSeed = bytes.Repeat([]byte{7}, ed25519.SeedSize)

// testProbeTimeout 是测试中探测命令的超时（Config.probeTimeout）。测试用的假 docker 等是刚写入的脚本：macOS 对新写入的
// 可执行文件第一次执行时要做系统检查，空闲时约 0.1–0.5 秒，全量 go test -race 同时链接几十个测试程序时可以超过
// runnerkit.ProbeTimeout（10 秒），所以测试给一个只防止卡死的宽松值。
const testProbeTimeout = 2 * time.Minute

// newTestRunner 创建连接到 fs 的 Runner（不经过 Enroll / New，不探测本机）。
func newTestRunner(t *testing.T, fs *fakeServer, opts ...func(*Config)) *Runner {
	t.Helper()
	cfg := &Config{Dir: t.TempDir(), MaxJobs: 2, ReserveCPU: 1, ReserveMemory: "1Gi", Docker: dockerDisabled, DiskHighWatermark: 100, probeTimeout: testProbeTimeout}
	for _, o := range opts {
		o(cfg)
	}
	key := ed25519.NewKeyFromSeed(testSeed)
	fs.mu.Lock()
	fs.pub = publicKey(t, key)
	fs.mu.Unlock()
	r := &Runner{
		cfg:     cfg,
		state:   &State{Server: fs.URL(), RunnerID: fs.runnerID, Name: "test-runner", Pool: "default"},
		c:       runnerkit.NewClient(fs.URL(), fs.runnerID, key),
		log:     slog.New(slog.DiscardHandler),
		host:    hostInfo{platforms: []string{runnerkit.NativePlatform()}, executors: []string{"host"}, totalCPU: 8, totalMem: 16 << 30},
		running: map[int64]*job{},
		done:    make(chan struct{}, 1),
		// 测试程序本身就是 kiln-runner：TestMain 按 argv[0] 和子命令分派 git 调用的命令（GitCommand）
		exe: testExe(t),
	}
	for _, d := range []string{casDirName, rtDirName, gitDirName, workDirName, toolcacheDirName} {
		if err := os.MkdirAll(cfg.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// publicKey 返回私钥对应的公钥。
func publicKey(t *testing.T, key ed25519.PrivateKey) ed25519.PublicKey {
	t.Helper()
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("不是 ed25519 公钥")
	}
	return pub
}

// testEpoch 是测试 job 的 epoch。
const testEpoch = 3

// hostJob 返回在 host executor 上依次执行 steps 的 JobSpec。
func hostJob(attempt int64, steps ...*runnerv1.Step) *runnerv1.JobSpec {
	return &runnerv1.JobSpec{
		Ref:             &runnerv1.AttemptRef{AttemptId: attempt, Epoch: testEpoch},
		LeaseTtlSeconds: 60,
		Executor:        &runnerv1.ExecutorSpec{Kind: "host", Platform: runnerkit.NativePlatform()},
		Steps:           steps,
		JobId:           100 + attempt,
		RunId:           1,
		DisplayName:     "test",
		Workflow:        "ci",
	}
}

// bash 返回执行 script 的 step。
func bash(name, script string) *runnerv1.Step {
	return &runnerv1.Step{Name: name, Run: script}
}

// runJob 直接执行 spec（不经过 Acquire），返回提交的结果和完整日志。
func runJob(t *testing.T, r *Runner, fs *fakeServer, spec *runnerv1.JobSpec) (*runnerv1.CompleteRequest, string) {
	t.Helper()
	fs.register(spec)
	r.newJob(spec).run(t.Context())
	return completedJob(fs, spec.Ref.AttemptId)
}

// completedJob 返回 attempt 最后提交的结果（没有时为 nil）和完整日志。
func completedJob(fs *fakeServer, attempt int64) (*runnerv1.CompleteRequest, string) {
	var got *runnerv1.CompleteRequest
	for _, c := range fs.completeCalls() {
		if c.Ref.AttemptId == attempt {
			got = c
		}
	}
	return got, fs.log(attempt)
}

// wantOutcome 断言结果的 outcome 和错误分类。
func wantOutcome(t *testing.T, got *runnerv1.CompleteRequest, log string, outcome runnerv1.Outcome, class string) {
	t.Helper()
	if got == nil {
		t.Fatalf("没有提交结果；日志：\n%s", logTail(log))
	}
	if got.Outcome != outcome || got.ErrorClass != class {
		t.Fatalf("结果 = %s / %q（%s），期望 %s / %q；日志：\n%s", got.Outcome, got.ErrorClass, got.Message, outcome, class, logTail(log))
	}
}

// logTail 返回日志最后的一部分，避免断言失败时输出过长。
func logTail(log string) string {
	const n = 4 << 10
	if len(log) > n {
		return "…" + log[len(log)-n:]
	}
	return log
}

// readResult 从 fs 的 CAS 中读出 result manifest，返回 输出名 → 输出 manifest。
func readResult(t *testing.T, fs *fakeServer, digest string) map[string]cas.OutputManifest {
	t.Helper()
	var rm cas.ResultManifest
	readJSONBlob(t, fs, digest, &rm)
	if rm.Schema != cas.ResultSchema {
		t.Fatalf("result manifest schema = %q", rm.Schema)
	}
	out := map[string]cas.OutputManifest{}
	for name, d := range rm.Outputs {
		var om cas.OutputManifest
		readJSONBlob(t, fs, d, &om)
		out[name] = om
	}
	return out
}

func readJSONBlob(t *testing.T, fs *fakeServer, digest string, v any) {
	t.Helper()
	b, ok := fs.blob(digest)
	if !ok {
		t.Fatalf("Server 的 CAS 中没有 %s", digest)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("解析 %s：%v", digest, err)
	}
}

// fakeDocker 在 dir 中生成一个假的 docker 命令：把每次调用的参数追加到 log 文件，
// 并按需输出 inspect 等命令的结果。返回命令路径和日志路径。
func fakeDocker(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "docker")
	log = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + log + `'
case "$1 $2" in
"info --format") echo "27.0.0" ;;
"image inspect")
  case "$*" in
  *missing*) exit 1 ;;
  *Config.User*nonroot*) echo "1000:1000" ;;
  *Config.User*) echo "" ;;
  *Config.Env*) printf 'PATH=/img/bin:/usr/bin\nHOME=/root\n' ;;
  *) echo sha256:abc ;;
  esac ;;
"inspect --format")
  case "$*" in
  *dead*) echo "exited none" ;;
  *) echo "running healthy" ;;
  esac ;;
"create "*) echo cid123 ;;
"cp "*) : > "$3" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// dockerCalls 读出假 docker 的调用记录（每行一次调用）。
func dockerCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// newTestShipper 创建向 fs 上传 attempt 1（epoch 3）日志的 LogShipper（与 job 相同，经 AppendLog 上传）。
func newTestShipper(t *testing.T, fs *fakeServer) *runnerkit.LogShipper {
	t.Helper()
	r := newTestRunner(t, fs)
	fs.mu.Lock()
	if _, ok := fs.epochs[1]; !ok {
		fs.epochs[1] = testEpoch
	}
	fs.mu.Unlock()
	j := r.newJob(&runnerv1.JobSpec{Ref: &runnerv1.AttemptRef{AttemptId: 1, Epoch: testEpoch}})
	return runnerkit.NewLogShipper(j.appendLog)
}

// shortTempDir 是放 unix socket 的临时目录：t.TempDir() 带测试名和随机数，在 macOS 的 $TMPDIR 下可能超过 sun_path 的
// 上限（104 字节），所以建在 /tmp 下（取 EvalSymlinks，macOS 上是 /private/tmp），测试结束时删除。
func shortTempDir(t *testing.T) string {
	t.Helper()
	//nolint:usetesting // t.TempDir() 的路径太长，见上
	d, err := os.MkdirTemp("/tmp", "kr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	resolved, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
