package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
)

// testToken 是假 Server 接受的 admin token。
const testToken = "secret-admin-token"

// call 是假 Server 收到的一次请求。
type call struct {
	method string
	msg    proto.Message
}

// fakeAdmin 是用 apiv1connect 的 handler 实现的假 AdminService：记录每个请求，按方法返回预设的响应或错误。
// 没有预设响应的方法返回空响应。
type fakeAdmin struct {
	apiv1connect.UnimplementedAdminServiceHandler
	token string

	mu      sync.Mutex
	calls   []call
	replies map[string][]any // 方法名 → 依次返回的响应（*Resp）
	errs    map[string]error // 方法名 → 返回的错误
	onCall  func(method string)
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{token: testToken, replies: map[string][]any{}, errs: map[string]error{}}
}

// reply 预设 method 的下一个响应；多次调用按顺序返回。
func (f *fakeAdmin) reply(method string, resp proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = append(f.replies[method], resp)
}

// setToken 更换假 Server 接受的 admin token。
func (f *fakeAdmin) setToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
}

// setOnCall 设置每次收到请求后（返回响应前）调用的函数。
func (f *fakeAdmin) setOnCall(fn func(method string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCall = fn
}

// fail 让 method 返回 err。
func (f *fakeAdmin) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = err
}

// methods 返回收到的请求的方法名（按顺序）。
func (f *fakeAdmin) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

// requests 返回 method 收到的全部请求。
func (f *fakeAdmin) requests(method string) []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.Message
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.msg)
		}
	}
	return out
}

// handle 是所有方法共用的实现：校验 token、记录请求、返回预设的响应或错误。
func handle[Resp, Req any](f *fakeAdmin, method string, r *connect.Request[Req]) (*connect.Response[Resp], error) {
	msg, ok := any(r.Msg).(proto.Message)
	if !ok {
		return nil, connect.NewError(connect.CodeInternal, errors.New("请求不是 proto 消息"))
	}
	f.mu.Lock()
	if r.Header().Get("Authorization") != "Bearer "+f.token {
		f.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("token 无效"))
	}
	f.calls = append(f.calls, call{method: method, msg: proto.Clone(msg)})
	err := f.errs[method]
	var resp *Resp
	if q := f.replies[method]; len(q) > 0 {
		resp, _ = q[0].(*Resp) // 类型不符时退化为空响应，断言会在测试中暴露
		f.replies[method] = q[1:]
	}
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = new(Resp)
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeAdmin) CreateEnrollToken(_ context.Context, r *connect.Request[apiv1.CreateEnrollTokenRequest]) (*connect.Response[apiv1.CreateEnrollTokenResponse], error) {
	return handle[apiv1.CreateEnrollTokenResponse](f, "CreateEnrollToken", r)
}

func (f *fakeAdmin) ListRunners(_ context.Context, r *connect.Request[apiv1.ListRunnersRequest]) (*connect.Response[apiv1.ListRunnersResponse], error) {
	return handle[apiv1.ListRunnersResponse](f, "ListRunners", r)
}

func (f *fakeAdmin) GetRunnerCache(_ context.Context, r *connect.Request[apiv1.GetRunnerCacheRequest]) (*connect.Response[apiv1.GetRunnerCacheResponse], error) {
	return handle[apiv1.GetRunnerCacheResponse](f, "GetRunnerCache", r)
}

func (f *fakeAdmin) PurgeRunnerCache(_ context.Context, r *connect.Request[apiv1.PurgeRunnerCacheRequest]) (*connect.Response[apiv1.PurgeRunnerCacheResponse], error) {
	return handle[apiv1.PurgeRunnerCacheResponse](f, "PurgeRunnerCache", r)
}

func (f *fakeAdmin) RevokeRunner(_ context.Context, r *connect.Request[apiv1.RevokeRunnerRequest]) (*connect.Response[apiv1.RevokeRunnerResponse], error) {
	return handle[apiv1.RevokeRunnerResponse](f, "RevokeRunner", r)
}

func (f *fakeAdmin) DeleteRunner(_ context.Context, r *connect.Request[apiv1.DeleteRunnerRequest]) (*connect.Response[apiv1.DeleteRunnerResponse], error) {
	return handle[apiv1.DeleteRunnerResponse](f, "DeleteRunner", r)
}

func (f *fakeAdmin) DrainRunner(_ context.Context, r *connect.Request[apiv1.DrainRunnerRequest]) (*connect.Response[apiv1.DrainRunnerResponse], error) {
	return handle[apiv1.DrainRunnerResponse](f, "DrainRunner", r)
}

func (f *fakeAdmin) ListRepos(_ context.Context, r *connect.Request[apiv1.ListReposRequest]) (*connect.Response[apiv1.ListReposResponse], error) {
	return handle[apiv1.ListReposResponse](f, "ListRepos", r)
}

func (f *fakeAdmin) SyncInstallations(_ context.Context, r *connect.Request[apiv1.SyncInstallationsRequest]) (*connect.Response[apiv1.SyncInstallationsResponse], error) {
	return handle[apiv1.SyncInstallationsResponse](f, "SyncInstallations", r)
}

func (f *fakeAdmin) ReconcileRepo(_ context.Context, r *connect.Request[apiv1.ReconcileRepoRequest]) (*connect.Response[apiv1.ReconcileRepoResponse], error) {
	return handle[apiv1.ReconcileRepoResponse](f, "ReconcileRepo", r)
}

func (f *fakeAdmin) BumpCacheSalt(_ context.Context, r *connect.Request[apiv1.BumpCacheSaltRequest]) (*connect.Response[apiv1.BumpCacheSaltResponse], error) {
	return handle[apiv1.BumpCacheSaltResponse](f, "BumpCacheSalt", r)
}

func (f *fakeAdmin) ListRuns(_ context.Context, r *connect.Request[apiv1.ListRunsRequest]) (*connect.Response[apiv1.ListRunsResponse], error) {
	return handle[apiv1.ListRunsResponse](f, "ListRuns", r)
}

func (f *fakeAdmin) GetRun(_ context.Context, r *connect.Request[apiv1.GetRunRequest]) (*connect.Response[apiv1.GetRunResponse], error) {
	return handle[apiv1.GetRunResponse](f, "GetRun", r)
}

func (f *fakeAdmin) CancelRun(_ context.Context, r *connect.Request[apiv1.CancelRunRequest]) (*connect.Response[apiv1.CancelRunResponse], error) {
	return handle[apiv1.CancelRunResponse](f, "CancelRun", r)
}

func (f *fakeAdmin) RerunRun(_ context.Context, r *connect.Request[apiv1.RerunRunRequest]) (*connect.Response[apiv1.RerunRunResponse], error) {
	return handle[apiv1.RerunRunResponse](f, "RerunRun", r)
}

func (f *fakeAdmin) RerunJob(_ context.Context, r *connect.Request[apiv1.RerunJobRequest]) (*connect.Response[apiv1.RerunJobResponse], error) {
	return handle[apiv1.RerunJobResponse](f, "RerunJob", r)
}

func (f *fakeAdmin) TriggerAIReview(_ context.Context, r *connect.Request[apiv1.TriggerAIReviewRequest]) (*connect.Response[apiv1.TriggerAIReviewResponse], error) {
	return handle[apiv1.TriggerAIReviewResponse](f, "TriggerAIReview", r)
}

func (f *fakeAdmin) ListAIRequests(_ context.Context, r *connect.Request[apiv1.ListAIRequestsRequest]) (*connect.Response[apiv1.ListAIRequestsResponse], error) {
	return handle[apiv1.ListAIRequestsResponse](f, "ListAIRequests", r)
}

func (f *fakeAdmin) GetAIRequest(_ context.Context, r *connect.Request[apiv1.GetAIRequestRequest]) (*connect.Response[apiv1.GetAIRequestResponse], error) {
	return handle[apiv1.GetAIRequestResponse](f, "GetAIRequest", r)
}

func (f *fakeAdmin) CancelAIRequest(_ context.Context, r *connect.Request[apiv1.CancelAIRequestRequest]) (*connect.Response[apiv1.CancelAIRequestResponse], error) {
	return handle[apiv1.CancelAIRequestResponse](f, "CancelAIRequest", r)
}

func (f *fakeAdmin) RerunAIRequest(_ context.Context, r *connect.Request[apiv1.RerunAIRequestRequest]) (*connect.Response[apiv1.RerunAIRequestResponse], error) {
	return handle[apiv1.RerunAIRequestResponse](f, "RerunAIRequest", r)
}

func (f *fakeAdmin) GetAITaskLog(_ context.Context, r *connect.Request[apiv1.GetAITaskLogRequest]) (*connect.Response[apiv1.GetAITaskLogResponse], error) {
	return handle[apiv1.GetAITaskLogResponse](f, "GetAITaskLog", r)
}

func (f *fakeAdmin) ListAIRunners(_ context.Context, r *connect.Request[apiv1.ListAIRunnersRequest]) (*connect.Response[apiv1.ListAIRunnersResponse], error) {
	return handle[apiv1.ListAIRunnersResponse](f, "ListAIRunners", r)
}

func (f *fakeAdmin) Dispatch(_ context.Context, r *connect.Request[apiv1.DispatchRequest]) (*connect.Response[apiv1.DispatchResponse], error) {
	return handle[apiv1.DispatchResponse](f, "Dispatch", r)
}

func (f *fakeAdmin) GetJobLog(_ context.Context, r *connect.Request[apiv1.GetJobLogRequest]) (*connect.Response[apiv1.GetJobLogResponse], error) {
	return handle[apiv1.GetJobLogResponse](f, "GetJobLog", r)
}

func (f *fakeAdmin) SetSecret(_ context.Context, r *connect.Request[apiv1.SetSecretRequest]) (*connect.Response[apiv1.SetSecretResponse], error) {
	return handle[apiv1.SetSecretResponse](f, "SetSecret", r)
}

func (f *fakeAdmin) SetNpmAccount(_ context.Context, r *connect.Request[apiv1.SetNpmAccountRequest]) (*connect.Response[apiv1.SetNpmAccountResponse], error) {
	return handle[apiv1.SetNpmAccountResponse](f, "SetNpmAccount", r)
}

func (f *fakeAdmin) DeleteNpmAccount(_ context.Context, r *connect.Request[apiv1.DeleteNpmAccountRequest]) (*connect.Response[apiv1.DeleteNpmAccountResponse], error) {
	return handle[apiv1.DeleteNpmAccountResponse](f, "DeleteNpmAccount", r)
}

func (f *fakeAdmin) ListMergeQueue(_ context.Context, r *connect.Request[apiv1.ListMergeQueueRequest]) (*connect.Response[apiv1.ListMergeQueueResponse], error) {
	return handle[apiv1.ListMergeQueueResponse](f, "ListMergeQueue", r)
}

func (f *fakeAdmin) AddToMergeQueue(_ context.Context, r *connect.Request[apiv1.AddToMergeQueueRequest]) (*connect.Response[apiv1.AddToMergeQueueResponse], error) {
	return handle[apiv1.AddToMergeQueueResponse](f, "AddToMergeQueue", r)
}

func (f *fakeAdmin) RemoveFromMergeQueue(_ context.Context, r *connect.Request[apiv1.RemoveFromMergeQueueRequest]) (*connect.Response[apiv1.RemoveFromMergeQueueResponse], error) {
	return handle[apiv1.RemoveFromMergeQueueResponse](f, "RemoveFromMergeQueue", r)
}

func (f *fakeAdmin) ListNpmAccounts(_ context.Context, r *connect.Request[apiv1.ListNpmAccountsRequest]) (*connect.Response[apiv1.ListNpmAccountsResponse], error) {
	return handle[apiv1.ListNpmAccountsResponse](f, "ListNpmAccounts", r)
}

func (f *fakeAdmin) DeleteSecret(_ context.Context, r *connect.Request[apiv1.DeleteSecretRequest]) (*connect.Response[apiv1.DeleteSecretResponse], error) {
	return handle[apiv1.DeleteSecretResponse](f, "DeleteSecret", r)
}

func (f *fakeAdmin) ListSecrets(_ context.Context, r *connect.Request[apiv1.ListSecretsRequest]) (*connect.Response[apiv1.ListSecretsResponse], error) {
	return handle[apiv1.ListSecretsResponse](f, "ListSecrets", r)
}

func (f *fakeAdmin) SetVariable(_ context.Context, r *connect.Request[apiv1.SetVariableRequest]) (*connect.Response[apiv1.SetVariableResponse], error) {
	return handle[apiv1.SetVariableResponse](f, "SetVariable", r)
}

func (f *fakeAdmin) DeleteVariable(_ context.Context, r *connect.Request[apiv1.DeleteVariableRequest]) (*connect.Response[apiv1.DeleteVariableResponse], error) {
	return handle[apiv1.DeleteVariableResponse](f, "DeleteVariable", r)
}

func (f *fakeAdmin) ListVariables(_ context.Context, r *connect.Request[apiv1.ListVariablesRequest]) (*connect.Response[apiv1.ListVariablesResponse], error) {
	return handle[apiv1.ListVariablesResponse](f, "ListVariables", r)
}

func (f *fakeAdmin) UpsertEnvironment(_ context.Context, r *connect.Request[apiv1.UpsertEnvironmentRequest]) (*connect.Response[apiv1.UpsertEnvironmentResponse], error) {
	return handle[apiv1.UpsertEnvironmentResponse](f, "UpsertEnvironment", r)
}

func (f *fakeAdmin) DeleteEnvironment(_ context.Context, r *connect.Request[apiv1.DeleteEnvironmentRequest]) (*connect.Response[apiv1.DeleteEnvironmentResponse], error) {
	return handle[apiv1.DeleteEnvironmentResponse](f, "DeleteEnvironment", r)
}

func (f *fakeAdmin) ListEnvironments(_ context.Context, r *connect.Request[apiv1.ListEnvironmentsRequest]) (*connect.Response[apiv1.ListEnvironmentsResponse], error) {
	return handle[apiv1.ListEnvironmentsResponse](f, "ListEnvironments", r)
}

func (f *fakeAdmin) ListOutbox(_ context.Context, r *connect.Request[apiv1.ListOutboxRequest]) (*connect.Response[apiv1.ListOutboxResponse], error) {
	return handle[apiv1.ListOutboxResponse](f, "ListOutbox", r)
}

func (f *fakeAdmin) RetryOutbox(_ context.Context, r *connect.Request[apiv1.RetryOutboxRequest]) (*connect.Response[apiv1.RetryOutboxResponse], error) {
	return handle[apiv1.RetryOutboxResponse](f, "RetryOutbox", r)
}

func (f *fakeAdmin) SetDecisionDoc(_ context.Context, r *connect.Request[apiv1.SetDecisionDocRequest]) (*connect.Response[apiv1.SetDecisionDocResponse], error) {
	return handle[apiv1.SetDecisionDocResponse](f, "SetDecisionDoc", r)
}

func (f *fakeAdmin) ClearDecisionDoc(_ context.Context, r *connect.Request[apiv1.ClearDecisionDocRequest]) (*connect.Response[apiv1.ClearDecisionDocResponse], error) {
	return handle[apiv1.ClearDecisionDocResponse](f, "ClearDecisionDoc", r)
}

func (f *fakeAdmin) ListDecisionDocs(_ context.Context, r *connect.Request[apiv1.ListDecisionDocsRequest]) (*connect.Response[apiv1.ListDecisionDocsResponse], error) {
	return handle[apiv1.ListDecisionDocsResponse](f, "ListDecisionDocs", r)
}

func (f *fakeAdmin) ListDecisionSyncs(_ context.Context, r *connect.Request[apiv1.ListDecisionSyncsRequest]) (*connect.Response[apiv1.ListDecisionSyncsResponse], error) {
	return handle[apiv1.ListDecisionSyncsResponse](f, "ListDecisionSyncs", r)
}

func (f *fakeAdmin) ReconcileDecisionSync(_ context.Context, r *connect.Request[apiv1.ReconcileDecisionSyncRequest]) (*connect.Response[apiv1.ReconcileDecisionSyncResponse], error) {
	return handle[apiv1.ReconcileDecisionSyncResponse](f, "ReconcileDecisionSync", r)
}

func (f *fakeAdmin) ResolveDecisionSync(_ context.Context, r *connect.Request[apiv1.ResolveDecisionSyncRequest]) (*connect.Response[apiv1.ResolveDecisionSyncResponse], error) {
	return handle[apiv1.ResolveDecisionSyncResponse](f, "ResolveDecisionSync", r)
}

// serveAdmin 启动假 Server，返回它的地址。
func serveAdmin(t *testing.T, f *fakeAdmin) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAdminServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// loggedIn 隔离环境变量，启动假 Server 并写入已登录的配置，返回配置路径、假 Server 和它的地址。
func loggedIn(t *testing.T) (cfg string, f *fakeAdmin, url string) {
	t.Helper()
	cfg = isolate(t)
	f = newFakeAdmin()
	url = serveAdmin(t, f)
	if err := saveConfig(cfg, cliConfig{Server: url, Token: testToken}); err != nil {
		t.Fatal(err)
	}
	return cfg, f, url
}

// wantRequest 检查 method 只收到一个请求且内容等于 want。
func wantRequest(t *testing.T, f *fakeAdmin, method string, want proto.Message) {
	t.Helper()
	got := f.requests(method)
	if len(got) != 1 {
		t.Fatalf("%s 收到 %d 个请求，期望 1 个；全部请求：%v", method, len(got), f.methods())
	}
	if !proto.Equal(got[0], want) {
		t.Errorf("%s 的请求 = {%s}\n期望 {%s}", method, prototext.Format(got[0]), prototext.Format(want))
	}
}

// wantNoCalls 检查假 Server 没有收到任何请求（用法错误应在发请求前发现）。
func wantNoCalls(t *testing.T, f *fakeAdmin) {
	t.Helper()
	if m := f.methods(); len(m) > 0 {
		t.Errorf("不应请求 Server，实际请求了 %v", m)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name       string
		file       string // 为空表示文件不存在
		envServer  string
		envToken   string
		want       cliConfig
		wantErrSub string
	}{
		{name: "文件不存在时为空配置", want: cliConfig{}},
		{name: "读取文件并去掉地址末尾的斜杠", file: `{"server":"https://k.example.com//","token":"t"}`, want: cliConfig{Server: "https://k.example.com", Token: "t"}},
		{name: "环境变量覆盖文件", file: `{"server":"https://a","token":"a"}`, envServer: "https://b/", envToken: "b", want: cliConfig{Server: "https://b", Token: "b"}},
		{name: "只有环境变量", envServer: "https://env", envToken: "tok", want: cliConfig{Server: "https://env", Token: "tok"}},
		{name: "文件损坏", file: "{", wantErrSub: "已损坏"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := isolate(t)
			t.Setenv("KILN_SERVER", tt.envServer)
			t.Setenv("KILN_ADMIN_TOKEN", tt.envToken)
			if tt.file != "" {
				if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadConfig(cfg)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("loadConfig() 错误 = %v，应包含 %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("loadConfig() = %+v，期望 %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigUnreadable(t *testing.T) {
	cfg := isolate(t)
	// 配置路径是目录：读取失败，错误中带上路径
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), cfg) {
		t.Errorf("loadConfig() 错误 = %v，应包含路径 %s", err, cfg)
	}
}

// 配置中有 admin token：目录和文件只允许本人访问。
func TestSaveConfigPermissions(t *testing.T) {
	t.Parallel()
	cfg := filepath.Join(t.TempDir(), "a", "b", "cli.json")
	if err := saveConfig(cfg, cliConfig{Server: "https://k", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{cfg: 0o600, filepath.Dir(cfg): 0o700} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s 的权限 = %o，期望 %o", path, got, want)
		}
	}
}

// 已有的配置文件权限更宽时，保存后收紧为 0600（WriteFile 不修改已有文件的权限）。
func TestSaveConfigTightensExisting(t *testing.T) {
	t.Parallel()
	cfg := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o644); err != nil { // 不受 umask 影响
		t.Fatal(err)
	}
	if err := saveConfig(cfg, cliConfig{Server: "https://k", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("%s 的权限 = %o，期望 600", cfg, got)
	}
}

// 写入 token 之前先收紧权限（先写入再收紧的话，中间其他用户可以读到 token）；收紧失败时不写入 token。
func TestWriteConfigFileTightensFirst(t *testing.T) {
	t.Parallel()
	f := &recordingFile{}
	if err := writeConfigFile(f, []byte(`{"token":"t"}`)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"chmod 600", "write", "close"}; !slices.Equal(f.ops, want) {
		t.Errorf("文件操作 = %v，期望 %v", f.ops, want)
	}

	f = &recordingFile{chmodErr: errors.New("operation not permitted")}
	err := writeConfigFile(f, []byte(`{"token":"t"}`))
	if err == nil || !strings.Contains(err.Error(), "设置配置文件的权限") {
		t.Errorf("收紧权限失败时的错误 = %v", err)
	}
	if want := []string{"chmod 600", "close"}; !slices.Equal(f.ops, want) {
		t.Errorf("收紧权限失败时的文件操作 = %v，期望 %v（不写入 token）", f.ops, want)
	}
}

// recordingFile 记录 writeConfigFile 对文件的操作。
type recordingFile struct {
	ops      []string
	chmodErr error
}

func (f *recordingFile) Chmod(mode os.FileMode) error {
	f.ops = append(f.ops, fmt.Sprintf("chmod %o", mode))
	return f.chmodErr
}

func (f *recordingFile) Write(b []byte) (int, error) {
	f.ops = append(f.ops, "write")
	return len(b), nil
}

func (f *recordingFile) Close() error {
	f.ops = append(f.ops, "close")
	return nil
}

func TestSaveConfigError(t *testing.T) {
	t.Parallel()
	// 父目录是普通文件：无法创建配置目录
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := saveConfig(filepath.Join(parent, "kiln", "cli.json"), cliConfig{})
	if err == nil || !strings.Contains(err.Error(), "创建配置目录") {
		t.Errorf("saveConfig() 错误 = %v，应提示无法创建配置目录", err)
	}
}

// 无法确定用户配置目录（没有设置 HOME）时不退回到相对路径：修复前 kiln login 把 admin token 写进当前目录的 .config/kiln/cli.json。
func TestDefaultConfigPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	if p := defaultConfigPath(); p != "" {
		t.Fatalf("defaultConfigPath() = %q，应为空", p)
	}
	t.Setenv("HOME", "relative/home")
	if p := defaultConfigPath(); p != "" {
		t.Fatalf("HOME 是相对路径时 defaultConfigPath() = %q，应为空", p)
	}
	t.Setenv("HOME", t.TempDir())
	if p := defaultConfigPath(); !filepath.IsAbs(p) || filepath.Base(p) != "cli.json" {
		t.Fatalf("defaultConfigPath() = %q，应为绝对路径", p)
	}

	// 没有配置路径：login 报错，不写任何文件；环境变量仍然可以代替 kiln login
	dir := t.TempDir()
	t.Chdir(dir)
	f := newFakeAdmin()
	url := serveAdmin(t, f)
	got := runApp(t, "", "", "login", "--server", url, "--token", testToken)
	if got.code != exitError || !strings.Contains(got.stderr, "无法确定配置文件的位置") {
		t.Errorf("退出码 = %d，stderr = %q；期望 1 并说明无法确定配置文件的位置", got.code, got.stderr)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("当前目录中不应写入文件：%v %v", entries, err)
	}
	t.Setenv("KILN_SERVER", url)
	t.Setenv("KILN_ADMIN_TOKEN", testToken)
	if got := runApp(t, "", "", "runner", "list"); got.code != exitOK {
		t.Errorf("用环境变量时退出码 = %d，stderr = %q", got.code, got.stderr)
	}
}

// 环境变量 KILN_SERVER、KILN_ADMIN_TOKEN 可以代替 kiln login。
func TestClientFromEnv(t *testing.T) {
	cfg := isolate(t)
	f := newFakeAdmin()
	url := serveAdmin(t, f)
	t.Setenv("KILN_SERVER", url+"/")
	t.Setenv("KILN_ADMIN_TOKEN", testToken)
	got := runApp(t, cfg, "", "runner", "list")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "ListRunners", &apiv1.ListRunnersRequest{})
}

func TestNotLoggedIn(t *testing.T) {
	tests := []struct {
		name string
		cfg  *cliConfig // nil 表示没有配置文件
		args []string
	}{
		{name: "没有配置文件", args: []string{"runs"}},
		{name: "只有地址没有 token", cfg: &cliConfig{Server: "https://k"}, args: []string{"runner", "list"}},
		{name: "只有 token 没有地址", cfg: &cliConfig{Token: "t"}, args: []string{"repo", "list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := isolate(t)
			if tt.cfg != nil {
				if err := saveConfig(cfg, *tt.cfg); err != nil {
					t.Fatal(err)
				}
			}
			got := runApp(t, cfg, "", tt.args...)
			if got.code != exitError || !strings.Contains(got.stderr, "还没有登录：kiln login --server") {
				t.Errorf("退出码 = %d，stderr = %q；期望 1 并提示 kiln login", got.code, got.stderr)
			}
		})
	}
}

func (f *fakeAdmin) GetAdminBinding(_ context.Context, r *connect.Request[apiv1.GetAdminBindingRequest]) (*connect.Response[apiv1.GetAdminBindingResponse], error) {
	return handle[apiv1.GetAdminBindingResponse](f, "GetAdminBinding", r)
}

func (f *fakeAdmin) SetTokenLogin(_ context.Context, r *connect.Request[apiv1.SetTokenLoginRequest]) (*connect.Response[apiv1.SetTokenLoginResponse], error) {
	return handle[apiv1.SetTokenLoginResponse](f, "SetTokenLogin", r)
}

func (f *fakeAdmin) UnbindAdmin(_ context.Context, r *connect.Request[apiv1.UnbindAdminRequest]) (*connect.Response[apiv1.UnbindAdminResponse], error) {
	return handle[apiv1.UnbindAdminResponse](f, "UnbindAdmin", r)
}

func (f *fakeAdmin) ListAutoTags(_ context.Context, r *connect.Request[apiv1.ListAutoTagsRequest]) (*connect.Response[apiv1.ListAutoTagsResponse], error) {
	return handle[apiv1.ListAutoTagsResponse](f, "ListAutoTags", r)
}

func (f *fakeAdmin) RetryAutoTag(_ context.Context, r *connect.Request[apiv1.RetryAutoTagRequest]) (*connect.Response[apiv1.RetryAutoTagResponse], error) {
	return handle[apiv1.RetryAutoTagResponse](f, "RetryAutoTag", r)
}

func (f *fakeAdmin) GetRunnerUpgrade(_ context.Context, r *connect.Request[apiv1.GetRunnerUpgradeRequest]) (*connect.Response[apiv1.GetRunnerUpgradeResponse], error) {
	return handle[apiv1.GetRunnerUpgradeResponse](f, "GetRunnerUpgrade", r)
}

func (f *fakeAdmin) SetRunnerUpgradeRegistry(_ context.Context, r *connect.Request[apiv1.SetRunnerUpgradeRegistryRequest]) (*connect.Response[apiv1.SetRunnerUpgradeRegistryResponse], error) {
	return handle[apiv1.SetRunnerUpgradeRegistryResponse](f, "SetRunnerUpgradeRegistry", r)
}

func (f *fakeAdmin) RequestRunnerUpgrade(_ context.Context, r *connect.Request[apiv1.RequestRunnerUpgradeRequest]) (*connect.Response[apiv1.RequestRunnerUpgradeResponse], error) {
	return handle[apiv1.RequestRunnerUpgradeResponse](f, "RequestRunnerUpgrade", r)
}

func (f *fakeAdmin) CancelRunnerUpgrade(_ context.Context, r *connect.Request[apiv1.CancelRunnerUpgradeRequest]) (*connect.Response[apiv1.CancelRunnerUpgradeResponse], error) {
	return handle[apiv1.CancelRunnerUpgradeResponse](f, "CancelRunnerUpgrade", r)
}

func (f *fakeAdmin) CheckRunnerUpgrade(_ context.Context, r *connect.Request[apiv1.CheckRunnerUpgradeRequest]) (*connect.Response[apiv1.CheckRunnerUpgradeResponse], error) {
	return handle[apiv1.CheckRunnerUpgradeResponse](f, "CheckRunnerUpgrade", r)
}

func (f *fakeAdmin) SyncAIDev(_ context.Context, r *connect.Request[apiv1.SyncAIDevRequest]) (*connect.Response[apiv1.SyncAIDevResponse], error) {
	return handle[apiv1.SyncAIDevResponse](f, "SyncAIDev", r)
}

func (f *fakeAdmin) PauseAIDev(_ context.Context, r *connect.Request[apiv1.PauseAIDevRequest]) (*connect.Response[apiv1.PauseAIDevResponse], error) {
	return handle[apiv1.PauseAIDevResponse](f, "PauseAIDev", r)
}

func (f *fakeAdmin) ListWorkItems(_ context.Context, r *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	return handle[apiv1.ListWorkItemsResponse](f, "ListWorkItems", r)
}

func (f *fakeAdmin) StopWorkItem(_ context.Context, r *connect.Request[apiv1.StopWorkItemRequest]) (*connect.Response[apiv1.StopWorkItemResponse], error) {
	return handle[apiv1.StopWorkItemResponse](f, "StopWorkItem", r)
}

func (f *fakeAdmin) RetryWorkItem(_ context.Context, r *connect.Request[apiv1.RetryWorkItemRequest]) (*connect.Response[apiv1.RetryWorkItemResponse], error) {
	return handle[apiv1.RetryWorkItemResponse](f, "RetryWorkItem", r)
}

// CLI 的重定向策略：admin token 只发给配置的 Server；https 不降级到 http。
func TestSafeRedirect(t *testing.T) {
	t.Parallel()
	newReq := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer admin-token")
		return r
	}
	via := []*http.Request{newReq("https://kiln.example.com/x")}

	if err := safeRedirect(newReq("http://kiln.example.com/x"), via); err == nil {
		t.Error("https → http 应被拒绝")
	}
	other := newReq("https://evil.example.com/x")
	if err := safeRedirect(other, via); err != nil {
		t.Fatal(err)
	}
	if h := other.Header.Get("Authorization"); h != "" {
		t.Errorf("跨 origin 仍带凭据：%q", h)
	}
	same := newReq("https://kiln.example.com/y")
	if err := safeRedirect(same, via); err != nil {
		t.Fatal(err)
	}
	if h := same.Header.Get("Authorization"); h != "Bearer admin-token" {
		t.Errorf("同一 origin 应保留凭据：%q", h)
	}
	// 默认端口归一化：https://host:443 与 https://host 是同一个 origin。
	explicit := newReq("https://kiln.example.com:443/y")
	if err := safeRedirect(explicit, via); err != nil {
		t.Fatal(err)
	}
	if h := explicit.Header.Get("Authorization"); h != "Bearer admin-token" {
		t.Errorf("默认端口归一化后应保留凭据：%q", h)
	}
	// 任意历史跳是 https 时不允许降级：原始是 http，中间升级到 https，再降回 http。
	upgraded := []*http.Request{newReq("http://kiln.example.com/x"), newReq("https://kiln.example.com/y")}
	if err := safeRedirect(newReq("http://kiln.example.com/z"), upgraded); err == nil {
		t.Error("历史跳为 https 时降级到 http 应被拒绝")
	}
	// 重定向次数上限。
	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = newReq("https://kiln.example.com/x")
	}
	if err := safeRedirect(newReq("https://kiln.example.com/y"), many); err == nil {
		t.Error("超过重定向上限应被拒绝")
	}
}
