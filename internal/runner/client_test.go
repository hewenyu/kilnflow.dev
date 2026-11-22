package runner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// access token 在有效期内复用；Server 返回 Unauthenticated 时重新认证并重试一次。
func TestCallReauthenticates(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	ref := &runnerv1.AttemptRef{AttemptId: 1, Epoch: 1}
	fetch := func() error {
		_, err := runnerkit.Call(t.Context(), r.c, r.c.RPC().FetchSecrets, &runnerv1.FetchSecretsRequest{Ref: ref})
		return err
	}
	for range 2 {
		if err := fetch(); err != nil {
			t.Fatal(err)
		}
	}
	fs.mu.Lock()
	n := fs.authCalls
	fs.mu.Unlock()
	if n != 1 {
		t.Errorf("token 有效期内认证了 %d 次，期望 1 次", n)
	}
	fs.expireToken()
	if err := fetch(); err != nil {
		t.Fatalf("token 失效后应重新认证：%v", err)
	}
	fs.mu.Lock()
	n = fs.authCalls
	fs.mu.Unlock()
	if n != 2 {
		t.Errorf("认证了 %d 次，期望 2 次", n)
	}
}

// call 把 Server 约定的错误原因转换为 Runner 的错误。
func TestCallErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(fs *fakeServer)
		check func(err error) bool
	}{
		{
			name:  "吊销",
			setup: func(fs *fakeServer) { fs.revoked = true },
			check: func(err error) bool { return errors.Is(err, runnerkit.ErrRevoked) },
		},
		{
			name:  "epoch 过期",
			setup: func(fs *fakeServer) { fs.epochs[1] = 2 },
			check: func(err error) bool { return errors.Is(err, runnerkit.ErrStale) },
		},
		{
			name: "签名无效",
			setup: func(fs *fakeServer) {
				// Server 登记的是另一把公钥
				fs.pub = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
			},
			check: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "认证失败") && connect.CodeOf(err) == connect.CodeUnauthenticated
			},
		},
		{
			name:  "其他错误原样返回",
			setup: func(fs *fakeServer) { fs.secretsErr = connect.NewError(connect.CodeInternal, errors.New("boom")) },
			check: func(err error) bool { return connect.CodeOf(err) == connect.CodeInternal },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			fs.mu.Lock()
			c.setup(fs)
			fs.mu.Unlock()
			_, err := runnerkit.Call(t.Context(), r.c, r.c.RPC().FetchSecrets, &runnerv1.FetchSecretsRequest{Ref: &runnerv1.AttemptRef{AttemptId: 1, Epoch: 1}})
			if !c.check(err) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// retry：不可重试的错误和次数用完时立即返回，不等待。
func TestRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		err         error
		maxAttempts int
		cancelled   bool
		calls       int
	}{
		{name: "epoch 过期", err: runnerkit.ErrStale, calls: 1},
		{name: "已吊销", err: runnerkit.ErrRevoked, calls: 1},
		{name: "InvalidArgument", err: connect.NewError(connect.CodeInvalidArgument, errors.New("x")), calls: 1},
		{name: "NotFound", err: connect.NewError(connect.CodeNotFound, errors.New("x")), calls: 1},
		{name: "PermissionDenied", err: connect.NewError(connect.CodePermissionDenied, errors.New("x")), calls: 1},
		{name: "FailedPrecondition", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("x")), calls: 1},
		{name: "Unimplemented", err: connect.NewError(connect.CodeUnimplemented, errors.New("x")), calls: 1},
		{name: "次数用完", err: connect.NewError(connect.CodeUnavailable, errors.New("x")), maxAttempts: 1, calls: 1},
		{name: "ctx 已结束", err: errors.New("网络错误"), cancelled: true, calls: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			if c.cancelled {
				cancel()
			}
			defer cancel()
			calls := 0
			_, err := runnerkit.Retry(ctx, c.maxAttempts, func() (int, error) {
				calls++
				return 0, c.err
			})
			if !errors.Is(err, c.err) || calls != c.calls {
				t.Errorf("retry = %v，调用 %d 次，期望 %v、%d 次", err, calls, c.err, c.calls)
			}
		})
	}
	v, err := runnerkit.Retry(t.Context(), 3, func() (string, error) { return "ok", nil })
	if v != "ok" || err != nil {
		t.Errorf("成功时 retry = %q, %v", v, err)
	}
}

func TestClientURL(t *testing.T) {
	t.Parallel()
	c := runnerkit.NewClient("https://kiln.example.com/", 1, nil)
	cases := map[string]string{
		"/cas/x?sig=1":               "https://kiln.example.com/cas/x?sig=1",
		"https://r2.example.com/o":   "https://r2.example.com/o",
		"http://127.0.0.1:9000/blob": "http://127.0.0.1:9000/blob",
	}
	for in, want := range cases {
		if got := casURL(c, in); got != want {
			t.Errorf("casURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 预签名地址的上传和下载：HTTP 错误带状态码和正文。
func TestClientPutGet(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	c := runnerkit.NewClient(fs.URL(), 1, nil)
	data := []byte("blob content")
	d := cas.Digest(data)
	file := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	putURL := "/cas/" + d + "?sig=" + casSig(http.MethodPut, d)
	getURL := "/cas/" + d + "?sig=" + casSig(http.MethodGet, d)

	if err := casPut(t.Context(), c, "/cas/"+d+"?sig=bad", file, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403 bad signature") {
		t.Errorf("签名错误时 put = %v", err)
	}
	if err := casPut(t.Context(), c, putURL, filepath.Join(t.TempDir(), "missing"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("文件不存在时 put = %v", err)
	}
	if _, err := casGet(t.Context(), c, getURL); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("对象不存在时 get = %v", err)
	}
	if err := casPut(t.Context(), c, putURL, file, nil); err != nil {
		t.Fatal(err)
	}
	body, err := casGet(t.Context(), c, getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if b, err := io.ReadAll(body); err != nil || string(b) != string(data) {
		t.Errorf("get = %q, %v", b, err)
	}
	// 连不上 Server
	dead := runnerkit.NewClient("http://127.0.0.1:1", 1, nil)
	// 错误信息中不能带预签名地址（签名会进入日志和结果说明）
	if _, err := casGet(t.Context(), dead, "/cas/x?sig=SECRETSIG"); err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Errorf("连不上时 get 应报错且不带地址：%v", err)
	}
	if err := casPut(t.Context(), dead, "/cas/x?sig=SECRETSIG", file, nil); err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Errorf("连不上时 put 应报错且不带地址：%v", err)
	}
}

func TestReadErrorBody(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxErrorBody+100)
	if got := readErrorBody(strings.NewReader("  oops \n")); got != "oops" {
		t.Errorf("readErrorBody = %q", got)
	}
	if got := readErrorBody(strings.NewReader(long)); len(got) != maxErrorBody {
		t.Errorf("readErrorBody 读了 %d 字节，期望最多 %d", len(got), maxErrorBody)
	}
	if got := readErrorBody(errReader{}); got != "" {
		t.Errorf("读取失败时 readErrorBody = %q", got)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("读取失败") }
