package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
)

// fakeServer 是只实现注册和认证的 RunnerService，用于测试子命令与 Server 的交互。
type fakeServer struct {
	runnerv1connect.UnimplementedRunnerServiceHandler

	mu      sync.Mutex
	revoked bool // Authenticate 返回 PermissionDenied（Runner 已被吊销）
	enrolls []*runnerv1.EnrollRequest
}

// newFakeServer 启动假的 Server，返回它和地址。
func newFakeServer(t *testing.T) (*fakeServer, string) {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.Handle(runnerv1connect.NewRunnerServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeServer) Enroll(_ context.Context, req *connect.Request[runnerv1.EnrollRequest]) (*connect.Response[runnerv1.EnrollResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrolls = append(f.enrolls, req.Msg)
	if req.Msg.Token != "good-token" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("注册令牌无效"))
	}
	return connect.NewResponse(&runnerv1.EnrollResponse{RunnerId: 7, Pool: "default"}), nil
}

func (f *fakeServer) Authenticate(context.Context, *connect.Request[runnerv1.AuthenticateRequest]) (*connect.Response[runnerv1.AuthenticateResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("已吊销"))
	}
	return connect.NewResponse(&runnerv1.AuthenticateResponse{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).Unix()}), nil
}

// dataDir 创建数据目录并设为 KILN_RUNNER_DIR；config.toml 关闭 docker，避免探测本机的 docker。
func dataDir(t *testing.T, extraConfig string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KILN_RUNNER_DIR", dir)
	t.Setenv("KILN_LOG_FORMAT", "")
	t.Setenv("KILN_DEBUG", "")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("docker = \"-\"\n"+extraConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runCmd 执行 kiln-runner 并返回退出码和输出。
func runCmd(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}
