package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
)

// lockedBuffer 是并发安全的日志缓冲区（HTTP 服务的 goroutine 写，测试读）。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestHandleCASServerErrors：Server 写入或读取对象失败（磁盘错误、数据库错误）时返回 500 并写日志，
// 响应中不含数据目录的路径和数据库的错误详情。
func TestHandleCASServerErrors(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	var logs lockedBuffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	// CAS 根目录是一个普通文件：创建对象目录、打开对象都失败
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.cas = &cas.FSStore{Root: blocker}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	content := []byte("hello kiln")
	digest := cas.Digest(content)

	code, body := do(t, http.MethodPut, srv.URL+s.keys.CASURL(auth.CASOpPut, digest, time.Minute), bytes.NewReader(content), nil)
	if code != http.StatusInternalServerError || strings.Contains(body, blocker) {
		t.Fatalf("写入失败：HTTP %d %q，期望 500 且不含路径", code, body)
	}
	if out := logs.String(); !strings.Contains(out, "CAS 写入对象失败") || !strings.Contains(out, blocker) {
		t.Fatalf("写入失败应记录日志（含详细信息）：%s", out)
	}
	code, body = do(t, http.MethodGet, srv.URL+s.keys.CASURL(auth.CASOpGet, digest, time.Minute), nil, nil)
	if code != http.StatusInternalServerError || strings.Contains(body, blocker) {
		t.Fatalf("读取失败：HTTP %d %q，期望 500 且不含路径", code, body)
	}

	// 数据库不可用：记录对象失败
	s.Close()
	code, body = do(t, http.MethodPut, srv.URL+s.keys.CASURL(auth.CASOpPut, digest, time.Minute), bytes.NewReader(content), nil)
	if code != http.StatusInternalServerError || strings.Contains(body, "closed") {
		t.Fatalf("数据库不可用：HTTP %d %q，期望 500 且不含错误详情", code, body)
	}
	if !strings.Contains(logs.String(), "CAS 记录对象失败") {
		t.Fatalf("数据库错误应记录日志：%s", logs.String())
	}
}

// TestCASPutError：上传失败按原因分类：请求体超过上限 413，读取请求体失败和 digest 不符 400，磁盘错误 500（只写日志）。
func TestCASPutError(t *testing.T) {
	t.Parallel()
	pathErr := &fs.PathError{Op: "open", Path: "/srv/kiln-data/cas/x", Err: fs.ErrPermission}
	tests := []struct {
		name         string
		readErr, err error
		status       int
		logged       bool
	}{
		{"超过上限", &http.MaxBytesError{Limit: 1}, &http.MaxBytesError{Limit: 1}, http.StatusRequestEntityTooLarge, false},
		{"请求体中断", io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, http.StatusBadRequest, false},
		{"digest 不符", nil, errors.New("内容的 digest 为 a，与声明的 b 不一致"), http.StatusBadRequest, false},
		{"磁盘错误", nil, fmt.Errorf("写入：%w", pathErr), http.StatusInternalServerError, true},
		{"改名失败", nil, &os.LinkError{Op: "rename", Old: "/srv/a", New: "/srv/b", Err: fs.ErrPermission}, http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil))}
			rec := httptest.NewRecorder()
			s.casPutError(rec, "sha256:x", tt.readErr, tt.err)
			if rec.Code != tt.status {
				t.Fatalf("HTTP %d %q，期望 %d", rec.Code, rec.Body.String(), tt.status)
			}
			if (logs.Len() > 0) != tt.logged {
				t.Errorf("记录了日志 %v，期望 %v：%s", logs.Len() > 0, tt.logged, logs.String())
			}
			if strings.Contains(rec.Body.String(), "/srv/") {
				t.Errorf("响应中有路径：%q", rec.Body.String())
			}
		})
	}
}

// TestStatusWriterUnwrap：请求日志的包装不妨碍 handler 用 http.ResponseController 设置写超时等（需要 Unwrap 找到底层的 ResponseWriter）。
func TestStatusWriterUnwrap(t *testing.T) {
	t.Parallel()
	errCh := make(chan error, 1)
	srv := httptest.NewServer(logRequests(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		errCh <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
	})))
	t.Cleanup(srv.Close)
	if code, _ := do(t, http.MethodGet, srv.URL+"/runs", nil, nil); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("经过请求日志的包装后 SetWriteDeadline = %v", err)
	}
}

// TestServeHTTPShutdown：退出时等进行中的请求自然结束；超过 grace 仍未结束的请求（例如 Runner 的长轮询）被取消，
// 服务在 timeout 之内正常退出（不返回错误）。修复前长轮询会让 Shutdown 等满 timeout 后返回 context deadline exceeded。
func TestServeHTTPShutdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		work      time.Duration // 请求自己结束需要的时间；0 表示一直等到 ctx 取消
		cancelled bool          // 请求的 ctx 是否应被取消
	}{
		{"短请求自然结束", 200 * time.Millisecond, false},
		{"长轮询被取消", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			addr := freeAddr(t)
			entered := make(chan struct{})
			result := make(chan error, 1)
			srv := &http.Server{Addr: addr, ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/work" {
					return
				}
				close(entered)
				if tt.work > 0 {
					time.Sleep(tt.work)
				} else {
					<-r.Context().Done()
				}
				result <- r.Context().Err()
				_, _ = io.WriteString(w, "done")
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- serveHTTP(ctx, srv, time.Second, 10*time.Second, func() {}, func() {}) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := tryGet(t.Context(), "http://"+addr+"/"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("服务没有启动")
				}
				time.Sleep(10 * time.Millisecond)
			}
			go func() { _, _ = tryGet(context.WithoutCancel(t.Context()), "http://"+addr+"/work") }()
			<-entered
			start := time.Now()
			cancel()
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("serveHTTP 返回 %v，期望 nil", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("serveHTTP 没有退出")
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("退出用了 %s", d)
			}
			if err := <-result; (err != nil) != tt.cancelled {
				t.Errorf("请求的 ctx 错误 = %v，期望被取消 %v", err, tt.cancelled)
			}
		})
	}
}

// TestServeHTTPStopping：开始优雅退出与监听失败时都先调用 stopping（停止接纳同步规划 / 文档冻结），再关停服务。
func TestServeHTTPStopping(t *testing.T) {
	t.Parallel()
	t.Run("ctx 结束", func(t *testing.T) {
		t.Parallel()
		addr := freeAddr(t)
		ctx, cancel := context.WithCancel(t.Context())
		var calls atomic.Int32
		started := make(chan struct{})
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
		errCh := make(chan error, 1)
		go func() {
			errCh <- serveHTTP(ctx, srv, time.Second, 10*time.Second, func() { close(started) }, func() { calls.Add(1) })
		}()
		<-started
		// ListenAndServe 的绑定在 started 之后完成：连上之后再取消，确保走的是正常退出路径。
		deadline := time.Now().Add(10 * time.Second)
		for {
			c, err := net.Dial("tcp", addr)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("服务没有启动")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("serveHTTP = %v，期望 nil", err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("stopping 调用 %d 次，应为 1", n)
		}
	})
	t.Run("监听失败", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		var calls atomic.Int32
		srv := &http.Server{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
		if err := serveHTTP(t.Context(), srv, time.Second, 10*time.Second, func() {}, func() { calls.Add(1) }); err == nil {
			t.Fatal("监听失败应返回错误")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("stopping 调用 %d 次，应为 1", n)
		}
	})
}
