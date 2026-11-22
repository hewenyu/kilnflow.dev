package runnerkit

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// fakeLog 是 Server 上一个 attempt 的日志（与 Server 的 AppendLog 相同：按 offset 幂等追加，超过上限拒收）。
type fakeLog struct {
	mu          sync.Mutex
	data        []byte
	logLimit    int     // >0 时日志总长超过它的追加被拒收（返回原 offset）
	appendErrs  []error // 依次作为 AppendLog 的错误返回
	stale       bool    // attempt 已经不属于本 Runner
	appendCalls int
}

func newFakeLog() *fakeLog { return &fakeLog{} }

func (f *fakeLog) append(_ context.Context, off int64, data []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	if f.stale {
		return 0, ErrStale
	}
	if len(f.appendErrs) > 0 {
		err := f.appendErrs[0]
		f.appendErrs = f.appendErrs[1:]
		return 0, err
	}
	size := int64(len(f.data))
	if off > size {
		return 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("日志不连续"))
	}
	if off+int64(len(data)) <= size {
		return size, nil
	}
	data = data[size-off:]
	if f.logLimit > 0 && len(f.data)+len(data) > f.logLimit {
		return size, nil
	}
	f.data = append(f.data, data...)
	return int64(len(f.data)), nil
}

func (f *fakeLog) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.data)
}

// newTestShipper 创建向 fs 上传日志的 LogShipper。
func newTestShipper(_ *testing.T, fs *fakeLog) *LogShipper {
	return NewLogShipper(fs.append)
}

// secret 的原文、base64、URL 编码和多行 secret 的每一行都被脱敏；太短的值不处理（PLAN #16.3）。
func TestLogShipperMasksSecrets(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	l := newTestShipper(t, fs)
	const secret = "p@ss w/rd+42"
	l.AddSecret(secret)
	l.AddSecret("first-line-xyz\nsecond-line-abc")
	l.AddSecret("abc") // 太短，不脱敏
	lines := []string{
		"raw=" + secret,
		"b64=" + base64.StdEncoding.EncodeToString([]byte(secret)),
		"query=" + url.QueryEscape(secret),
		"path=" + url.PathEscape(secret),
		"multi=second-line-abc",
		"short=abc",
	}
	// secret 被拆在两次写入中也要脱敏（按整行处理）
	out := strings.Join(lines, "\n") + "\n"
	cut := strings.Index(out, "w/rd")
	if _, err := l.Write([]byte(out[:cut])); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte(out[cut:])); err != nil {
		t.Fatal(err)
	}
	l.Close()
	want := "raw=***\nb64=***\nquery=***\npath=***\nmulti=***\nshort=abc\n"
	if got := fs.log(); got != want {
		t.Errorf("日志 = %q，期望 %q", got, want)
	}
}

// 没有换行的尾部在 Close 时发出；Printf 的行带 [kiln] 前缀。
func TestLogShipperFlushOnClose(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	l := newTestShipper(t, fs)
	l.Printf("准备 %d", 1)
	if _, err := l.Write([]byte("no newline")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	l.Close() // 重复 Close 无影响
	if got, want := fs.log(), "\x1b[36m[kiln]\x1b[0m 准备 1\nno newline"; got != want {
		t.Errorf("日志 = %q，期望 %q", got, want)
	}
}

// 超长的行（没有换行）不会一直缓存。
func TestLogShipperLongLine(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	l := newTestShipper(t, fs)
	long := strings.Repeat("y", maxPartialLine+1)
	if _, err := l.Write([]byte(long)); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	pending, partial := len(l.pending), len(l.partial)
	l.mu.Unlock()
	if pending != len(long) || partial != 0 {
		t.Errorf("超长的行应进入待发送：pending %d、partial %d", pending, partial)
	}
	l.Close()
	if got := fs.log(); got != long {
		t.Errorf("日志长度 %d，期望 %d", len(got), len(long))
	}
}

// Server 上已有的部分（例如上次发送成功但响应丢失）不会重复写入：按 offset 幂等追加。
func TestLogShipperIdempotentOffset(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	fs.data = []byte("line-1\n")
	l := newTestShipper(t, fs)
	if _, err := l.Write([]byte("line-1\nline-2\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := fs.log(); got != "line-1\nline-2\n" {
		t.Errorf("日志 = %q", got)
	}
}

// Close 时发送失败会重试。
func TestLogShipperRetryOnClose(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	fs.appendErrs = []error{connect.NewError(connect.CodeUnavailable, errors.New("暂时不可用"))}
	l := newTestShipper(t, fs)
	if _, err := l.Write([]byte("final line\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := fs.log(); got != "final line\n" {
		t.Errorf("日志 = %q", got)
	}
}

// Server 拒收（日志达到上限）或 epoch 过期时丢弃剩余日志，不再重试。
func TestLogShipperDiscards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(fs *fakeLog)
	}{
		{name: "达到上限", setup: func(fs *fakeLog) { fs.logLimit = 4 }},
		{name: "epoch 过期", setup: func(fs *fakeLog) { fs.stale = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeLog()
			c.setup(fs)
			l := newTestShipper(t, fs)
			if _, err := l.Write([]byte("too long for the server\n")); err != nil {
				t.Fatal(err)
			}
			l.Close()
			fs.mu.Lock()
			calls := fs.appendCalls
			fs.mu.Unlock()
			l.mu.Lock()
			pending := len(l.pending)
			l.mu.Unlock()
			if fs.log() != "" || pending != 0 {
				t.Errorf("日志 = %q，剩余 %d 字节，期望全部丢弃", fs.log(), pending)
			}
			if calls > 1 {
				t.Errorf("AppendLog 调用了 %d 次，丢弃后不应重试", calls)
			}
		})
	}
}

// secret 被没有换行的分段发送（定时发送尾部、超长的行）切开时，各段拼起来也不能还原出 secret：
// 可能是 secret 开头的尾部留到后续输出到达后一起脱敏，只有 Close 时才全部发出。
func TestLogShipperMasksAcrossPartialFlush(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-synthetic/secret+value"
	forms := map[string]string{
		"raw":    secret,
		"base64": base64.StdEncoding.EncodeToString([]byte(secret)),
		"query":  url.QueryEscape(secret),
		"path":   url.PathEscape(secret),
	}
	for name, form := range forms {
		for _, flush := range []string{"定时", "超长的行"} {
			t.Run(name+"/"+flush, func(t *testing.T) {
				t.Parallel()
				fs := newFakeLog()
				l := newTestShipper(t, fs)
				l.AddSecret(secret)
				prefix := "x="
				if flush == "超长的行" {
					prefix = strings.Repeat("y", maxPartialLine-len(form)/2)
				}
				// 分三次写入，每次之后都按对应的方式发送尾部
				cuts := []int{len(form) / 3, 2 * len(form) / 3}
				parts := []string{prefix + form[:cuts[0]], form[cuts[0]:cuts[1]], form[cuts[1]:] + " tail"}
				for _, p := range parts {
					if _, err := l.Write([]byte(p)); err != nil {
						t.Fatal(err)
					}
					if flush == "定时" {
						l.flushPartial(false)
					}
					l.send(false)
				}
				l.Close()
				got := fs.log()
				if strings.Contains(got, form) || strings.Contains(got, secret) {
					t.Fatalf("日志中出现了 secret：%q", got)
				}
				if want := prefix + "*** tail"; got != want {
					t.Errorf("日志 = %q，期望 %q", shortLog(got), shortLog(want))
				}
			})
		}
	}
}

// secret 能与自身重叠、输出一直是首尾相接的出现（没有换行）时，缓冲不会无限增长，日志照常发出，
// 发出的内容中也没有完整的 secret。
func TestLogShipperOverlappingSecretBounded(t *testing.T) {
	t.Parallel()
	const secret = "aaaaaaaa"
	for _, flush := range []string{"定时", "超长的行"} {
		t.Run(flush, func(t *testing.T) {
			t.Parallel()
			fs := newFakeLog()
			l := newTestShipper(t, fs)
			l.AddSecret(secret)
			chunk := []byte(strings.Repeat("a", 4<<10))
			for range 256 { // 共 1 MiB
				if _, err := l.Write(chunk); err != nil {
					t.Fatal(err)
				}
				if flush == "定时" {
					l.flushPartial(false)
				}
				l.mu.Lock()
				n := len(l.partial)
				l.mu.Unlock()
				if n > maxPartialLine+len(chunk) {
					t.Fatalf("缓冲增长到 %d 字节", n)
				}
				l.send(false)
			}
			if _, err := l.Write([]byte(" tail\n")); err != nil {
				t.Fatal(err)
			}
			l.Close()
			got := fs.log()
			if strings.Contains(got, secret) {
				t.Fatalf("日志中出现了 secret：%q", shortLog(got))
			}
			if !strings.HasSuffix(got, " tail\n") || !strings.Contains(got, "***") {
				t.Errorf("日志 = %q", shortLog(got))
			}
		})
	}
}

// 没有 secret 可能被切开时，定时发送照常发出全部尾部。
func TestLogShipperPartialFlushWithoutSecret(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	l := newTestShipper(t, fs)
	l.AddSecret("synthetic-secret")
	if _, err := l.Write([]byte("progress 42% syn")); err != nil {
		t.Fatal(err)
	}
	l.flushPartial(false)
	l.mu.Lock()
	pending, partial := string(l.pending), string(l.partial)
	l.mu.Unlock()
	if pending != "progress 42% " || partial != "syn" {
		t.Errorf("pending %q、partial %q，期望只保留可能是 secret 开头的 %q", pending, partial, "syn")
	}
	// 后续输出与 secret 不符：保留的部分照常发出
	if _, err := l.Write([]byte("c done")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := fs.log(); got != "progress 42% sync done" {
		t.Errorf("日志 = %q", got)
	}
}

// shortLog 截短很长的日志，便于在失败信息中查看。
func shortLog(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:20] + "…" + s[len(s)-60:]
}

// 多行 secret 被换行处的写入、定时发送或 Close 切开时，上传的日志中也不能出现完整的 secret；
// 每行都不足最短长度的多行 secret 也一样。
func TestLogShipperMasksMultilineAcrossNewline(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		secret string
		parts  []string
		want   string
	}{
		{"短行", "abc\ndef", []string{"prefix=abc\n", "def\n"}, "prefix=***\n"},
		{"短行逐字节", "abc\ndef", strings.Split("x=abc\ndef y\n", ""), "x=*** y\n"},
		{"长行", "first-line-xyz\nsecond-line-abc", []string{"k=first-line-xyz\n", "second-line-abc\nok\n"}, "k=***\nok\n"},
		{"拆在行中间", "abc\ndef", []string{"v=ab", "c\nd", "ef\n"}, "v=***\n"},
		{"之后不是 secret", "abc\ndef", []string{"v=abc\n", "dez\n"}, "v=abc\ndez\n"},
	}
	for _, tc := range cases {
		for _, flush := range []string{"无", "定时"} {
			t.Run(tc.name+"/"+flush, func(t *testing.T) {
				t.Parallel()
				fs := newFakeLog()
				l := newTestShipper(t, fs)
				l.AddSecret(tc.secret)
				for _, p := range tc.parts {
					if _, err := l.Write([]byte(p)); err != nil {
						t.Fatal(err)
					}
					if flush == "定时" {
						l.flushPartial(false)
					}
					l.send(false)
				}
				l.Close()
				got := fs.log()
				if strings.Contains(got, tc.secret) {
					t.Fatalf("日志中出现了 secret：%q", got)
				}
				if got != tc.want {
					t.Errorf("日志 = %q，期望 %q", got, tc.want)
				}
			})
		}
	}
}

// 以换行结尾、不可能是 secret 开头的行立即进入待发送，不等定时发送。
func TestLogShipperCompleteLinesPending(t *testing.T) {
	t.Parallel()
	fs := newFakeLog()
	l := newTestShipper(t, fs)
	l.AddSecret("abc\ndef")
	if _, err := l.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	pending, partial := string(l.pending), string(l.partial)
	l.mu.Unlock()
	if pending != "line one\nline two\n" || partial != "" {
		t.Errorf("pending %q、partial %q", pending, partial)
	}
	l.Close()
}

// 待发送日志有上限：Server 长期不可用时丢弃放不进的数据，不会无限占用内存；恢复后补上说明。
func TestLogShipperPendingBudget(t *testing.T) {
	t.Parallel()
	l := &LogShipper{} // 只测试缓冲逻辑，不启动发送协程
	l.appendPendingLocked(make([]byte, maxPendingBytes+1024))
	if len(l.pending) != maxPendingBytes || !l.dropped {
		t.Fatalf("pending = %d, dropped = %v", len(l.pending), l.dropped)
	}
	l.appendPendingLocked([]byte("more"))
	if len(l.pending) != maxPendingBytes {
		t.Fatalf("超过上限后仍继续增长：%d", len(l.pending))
	}
	// 腾出空间后可以继续放
	l.pending = l.pending[:maxPendingBytes-8]
	l.appendPendingLocked([]byte("12345678"))
	if len(l.pending) != maxPendingBytes {
		t.Fatalf("pending = %d", len(l.pending))
	}
	// 恢复后实际发送时，超过上限丢弃过输出的说明要补在最后（dropped 在发送成功后清除）。
	var sent []byte
	l.appendLog = func(_ context.Context, off int64, data []byte) (int64, error) {
		sent = append(sent, data...)
		return off + int64(len(data)), nil
	}
	l.send(false)
	if !bytes.HasSuffix(sent, []byte(dropNotice)) {
		t.Fatalf("恢复后发送的内容尾部 = %q，应补发 dropNotice", sent[max(0, len(sent)-len(dropNotice)-16):])
	}
	if l.dropped {
		t.Fatal("dropNotice 发出后 dropped 应清除")
	}
}

// 连续发送很多块之后遇到临时失败，仍然按 Close 的重试次数重试（attempt 在成功后重新计次）。
func TestLogShipperRetriesAfterManyChunks(t *testing.T) {
	t.Parallel()
	const chunks = 7 // 大于 finalSendRetries
	total := int64(maxAppendChunk * chunks)
	var mu sync.Mutex
	var sent int64
	calls := 0
	l := &LogShipper{
		pending: make([]byte, total),
		appendLog: func(_ context.Context, _ int64, data []byte) (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == chunks {
				return sent, errors.New("传输错误")
			}
			sent += int64(len(data))
			return sent, nil
		},
	}
	l.send(true)
	if sent != total {
		t.Fatalf("发送了 %d/%d 字节，失败后应继续重试（调用 %d 次）", sent, total, calls)
	}
}

// Server 持有的日志比 Runner 认为的少（offset 对不上）时丢弃待发送的数据，不无限重发。
func TestLogShipperServerBehind(t *testing.T) {
	t.Parallel()
	l := &LogShipper{
		pending:   []byte("hello"),
		sent:      100,
		appendLog: func(context.Context, int64, []byte) (int64, error) { return 50, nil },
	}
	done := make(chan struct{})
	go func() {
		l.send(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("offset 对不上时 send 没有返回")
	}
	if l.pending != nil {
		t.Fatalf("pending = %q，应丢弃", l.pending)
	}
}
