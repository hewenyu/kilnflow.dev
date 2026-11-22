package httpretry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/httpretry"
)

func TestRetryable(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]bool{
		http.StatusOK: false, http.StatusNotFound: false, http.StatusConflict: false, http.StatusForbidden: false,
		http.StatusTooManyRequests: true, http.StatusInternalServerError: true, http.StatusBadGateway: true, http.StatusServiceUnavailable: true,
	} {
		if got := httpretry.Retryable(status); got != want {
			t.Errorf("Retryable(%d) = %v，期望 %v", status, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"5", 5 * time.Second, true},
		{" 60 ", time.Minute, true},
		{"61", 0, false},
		{"-1", 0, false},
		{"", 0, false},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, false},
		// 乘以 time.Second 会溢出成负数的值不能被当成"立即重试"
		{"99999999999999", 0, false},
	}
	for _, tt := range tests {
		if got, ok := httpretry.RetryAfter(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("RetryAfter(%q) = %v, %v，期望 %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSleep(t *testing.T) {
	t.Parallel()
	if err := httpretry.Sleep(t.Context(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v", err)
	}
	if err := httpretry.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("Sleep(1ms) = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := httpretry.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消时 Sleep(0) = %v", err)
	}
	start := time.Now()
	if err := httpretry.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消时 Sleep(1h) = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("取消后应立即返回")
	}
}

// readCounter 记录读取的字节数。
type readCounter struct {
	r    io.Reader
	read int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	small := &readCounter{r: strings.NewReader("short body")}
	httpretry.Discard(small)
	if small.read != len("short body") {
		t.Errorf("小的响应体应读完：读了 %d 字节", small.read)
	}
	// 超大的响应体只读到上限，不会一直读下去
	huge := &readCounter{r: io.LimitReader(zeros{}, 1<<30)}
	httpretry.Discard(huge)
	if huge.read != 1<<20 {
		t.Errorf("超大的响应体应只读 1 MiB：读了 %d 字节", huge.read)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
