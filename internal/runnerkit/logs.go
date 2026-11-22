package runnerkit

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// minSecretLen 是需要脱敏的 secret 的最短长度（太短的值替换后会误伤正常输出）。
	minSecretLen = 4
	// maxPartialLine 是没有换行时缓存的最大长度，超过后直接发送。
	maxPartialLine = 16 << 10
	// kickThreshold 是待发送数据达到多少时立即发送，而不等下一个周期。
	kickThreshold = 64 << 10
	// maxAppendChunk 是一次 AppendLog 的最大字节数。
	maxAppendChunk = 1 << 20
	// appendTimeout 是一次 AppendLog 的超时。
	appendTimeout = 30 * time.Second
	// finalSendRetries 是 Close 时发送剩余日志的重试次数。
	finalSendRetries = 5
	// sendRetryDelay 是发送失败后的重试间隔。
	sendRetryDelay = time.Second
	// shipInterval 是定期发送的间隔；partialFlushTicks 个周期后发送没有换行的尾部。
	shipInterval      = time.Second
	partialFlushTicks = 2
	// maxPendingBytes 是等待发送的日志上限：Server 长时间不可用时丢弃新的输出，避免把 Runner 的内存耗光。
	maxPendingBytes = 64 << 20
	// dropNotice 是日志因超过上限被丢弃时补在末尾的说明。
	dropNotice = "\n[kiln] 日志过多、Server 长时间没有接收，部分输出已丢弃\n"
)

// AppendFunc 把日志 data 追加到 Server 上 offset 处，返回 Server 已经持有的日志长度。attempt 已经不属于本 Runner 时
// 返回 ErrStale（剩余日志丢弃）。
type AppendFunc func(ctx context.Context, offset int64, data []byte) (committed int64, err error)

// LogShipper 把任务的输出脱敏后按 offset 追加到 Server（幂等，失败时重传）。
type LogShipper struct {
	appendLog AppendFunc

	mu      sync.Mutex
	pending []byte // 已脱敏、等待发送
	partial []byte // 还不能安全发出的尾部：没有换行的部分，或可能与后续输出拼成 secret 的部分（可含换行）
	sent    int64
	secrets [][]byte
	// dropped 表示有输出因为没有发送空间被丢弃（下一次发送成功后补上说明）。
	dropped bool
	kick    chan struct{}
	closed  chan struct{}
	stopped chan struct{}
}

// NewLogShipper 开始在后台发送日志，用 appendLog 追加；用完调用 Close。
func NewLogShipper(appendLog AppendFunc) *LogShipper {
	l := &LogShipper{appendLog: appendLog, kick: make(chan struct{}, 1), closed: make(chan struct{}), stopped: make(chan struct{})}
	go l.loop()
	return l
}

// AddSecret 登记需要脱敏的值：原文、base64 和 URL 编码形式（PLAN #16.3）。
func (l *LogShipper) AddSecret(v string) {
	if len(v) < minSecretLen {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	forms := map[string]bool{
		v: true, base64.StdEncoding.EncodeToString([]byte(v)): true,
		base64.RawStdEncoding.EncodeToString([]byte(v)): true, url.QueryEscape(v): true, url.PathEscape(v): true,
	}
	for line := range strings.SplitSeq(v, "\n") {
		if len(strings.TrimSpace(line)) >= minSecretLen {
			forms[line] = true
		}
	}
	for f := range forms {
		l.secrets = append(l.secrets, []byte(f))
	}
	// 长的先替换
	slices.SortFunc(l.secrets, func(a, b []byte) int { return cmp.Compare(len(b), len(a)) })
}

// MaskString 返回脱敏后的 s，用于不经过日志的文本（例如上报给 Server 的错误信息）。
func (l *LogShipper) MaskString(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.mask([]byte(s)))
}

func (l *LogShipper) mask(b []byte) []byte {
	for _, s := range l.secrets {
		if bytes.Contains(b, s) {
			b = bytes.ReplaceAll(b, s, []byte("***"))
		}
	}
	return b
}

// Write 实现 io.Writer（step 的 stdout/stderr）。
func (l *LogShipper) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.partial = append(l.partial, p...)
	if i := bytes.LastIndexByte(l.partial, '\n'); i >= 0 {
		// 完整的行也只发出安全边界之前的部分：多行 secret 可能跨过换行，
		// 行尾若是某个 secret 的开头（例如 "abc\n" 之于 "abc\ndef"），要留到后续输出到达后再一起脱敏
		if k := l.safeBoundary(l.partial[:i+1]); k > 0 {
			l.appendPendingLocked(l.mask(l.partial[:k]))
			l.partial = append([]byte(nil), l.partial[k:]...)
		}
	}
	// 超长的行也要发出去（保留可能是 secret 开头的尾部）
	if len(l.partial) > maxPartialLine {
		l.flushPartialLocked(false)
	}
	big := len(l.pending) > kickThreshold
	l.mu.Unlock()
	if big {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// Printf 写一行 Runner 自己的信息。
func (l *LogShipper) Printf(format string, args ...any) {
	fmt.Fprintf(l, "\x1b[36m[kiln]\x1b[0m "+format+"\n", args...)
}

// flushPartial 把没有换行的尾部脱敏后放入待发送。all 为 false 时保留可能与后续输出拼成 secret 的尾部
// （只有 Close 时才全部发出），否则分两次发送的两段各自都不含完整的 secret，拼起来却是明文。
func (l *LogShipper) flushPartial(all bool) {
	l.mu.Lock()
	l.flushPartialLocked(all)
	l.mu.Unlock()
}

func (l *LogShipper) flushPartialLocked(all bool) {
	k := len(l.partial)
	if !all {
		k = l.safeBoundary(l.partial)
	}
	if k == 0 {
		return
	}
	l.appendPendingLocked(l.mask(l.partial[:k]))
	l.partial = append([]byte(nil), l.partial[k:]...)
}

// appendPendingLocked 把已脱敏的数据放入待发送。超过 maxPendingBytes 时只放得进的部分并记下丢弃
// （Server 长时间不可用时不能无限占用内存）；丢弃发生在末尾，已发送的 offset 仍然连续。调用方持有 mu。
func (l *LogShipper) appendPendingLocked(b []byte) {
	if room := maxPendingBytes - len(l.pending); len(b) > room {
		l.dropped = true
		b = b[:max(room, 0)]
	}
	l.pending = append(l.pending, b...)
}

// safeBoundary 返回 b 中可以安全发出的前缀长度 k：b[:k] 脱敏后发出，b[k:] 留到后续输出到达后再处理。
// 任何 secret（包括 base64、URL 编码形式）的出现都不会跨过 k：既不会有完整出现跨过 k，
// 也不会有从 k 之前开始、延伸到 b 末尾之后（可能由下一次写入补齐）的出现。
//
// 例外：secret 能与自身或其他 secret 重叠（例如 "aaaaaaaa"）、输出又一直是首尾相接的出现时，满足上面条件的 k
// 可能退到很靠前甚至 0，缓冲会无限增长。这时改为把 k 推到跨过它的完整出现的末尾，只保留可能是 secret 开头的尾部：
// 发出的部分中不会有完整的 secret，最多与后续输出拼出 secret 的片段（与 mask 对重叠出现的处理一致）。
func (l *LogShipper) safeBoundary(b []byte) int {
	tail := len(b)
	// 从 i 开始到末尾是某个 secret 的真前缀：后续输出可能补齐它
	for _, s := range l.secrets {
		for i := max(0, len(b)-len(s)+1); i < tail; i++ {
			if bytes.HasPrefix(s, b[i:]) {
				tail = i
				break
			}
		}
	}
	// k 落在某个完整出现的中间时，移到它的开头（这一出现留到下次与尾部一起脱敏）
	k := tail
	for moved := true; moved && k > 0; {
		moved = false
		for _, s := range l.secrets {
			if a, ok := l.straddling(b, k, s); ok {
				k = a
				moved = true
			}
		}
	}
	if len(b)-k <= maxPartialLine {
		return k
	}
	// 留下的太多：改为把 k 推到跨过它的完整出现的末尾
	k = tail
	for moved := true; moved; {
		moved = false
		for _, s := range l.secrets {
			if a, ok := l.straddling(b, k, s); ok {
				k = a + len(s)
				moved = true
			}
		}
	}
	return k
}

// straddling 返回 s 在 b 中跨过 k（从 k 之前开始、在 k 之后结束）的第一个完整出现的开头。
func (l *LogShipper) straddling(b []byte, k int, s []byte) (int, bool) {
	lo := max(0, k-len(s)+1)
	if j := bytes.Index(b[lo:min(len(b), k+len(s)-1)], s); j >= 0 && lo+j < k {
		return lo + j, true
	}
	return 0, false
}

func (l *LogShipper) loop() {
	defer close(l.stopped)
	t := time.NewTicker(shipInterval)
	defer t.Stop()
	idle := 0
	for {
		closing := false
		select {
		case <-t.C:
		case <-l.kick:
		case <-l.closed:
			closing = true
		}
		l.mu.Lock()
		hasPartial := len(l.partial) > 0
		l.mu.Unlock()
		if hasPartial {
			idle++
			if idle >= partialFlushTicks || closing {
				l.flushPartial(closing)
				idle = 0
			}
		}
		l.send(closing)
		if closing {
			return
		}
	}
}

// send 发送 pending 中的数据。final 为 true 时失败会重试几次，否则留到下个周期。
func (l *LogShipper) send(final bool) {
	retries := 0
	if final {
		retries = finalSendRetries
	}
	for attempt := 0; ; attempt++ {
		l.mu.Lock()
		data := l.pending
		if len(data) > maxAppendChunk {
			data = data[:maxAppendChunk]
		}
		off := l.sent
		l.mu.Unlock()
		if len(data) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), appendTimeout)
		committed, err := l.appendLog(ctx, off, data)
		cancel()
		if err != nil {
			if errors.Is(err, ErrStale) {
				// attempt 已不属于本 Runner，剩余日志没有意义
				l.mu.Lock()
				l.pending = nil
				l.mu.Unlock()
				return
			}
			if attempt >= retries {
				return
			}
			time.Sleep(sendRetryDelay)
			continue
		}
		l.mu.Lock()
		discarded := false
		switch {
		case committed < off:
			// Server 持有的日志比我们认为的少（offset 对不上）：继续发送只会一直失败，丢弃待发送的数据
			l.pending = nil
			discarded = true
		case committed > l.sent:
			n := min(committed-l.sent, int64(len(l.pending)))
			l.pending = l.pending[n:]
			l.sent += n
		case committed == off && len(data) > 0:
			// Server 拒收（达到上限），丢弃
			l.pending = nil
			discarded = true
		}
		// 之前因为超过上限丢弃过输出：发送成功后补上说明。刚刚因为 offset 对不上 / 被拒收清空了 pending 时，
		// 这条说明也发不出去，不再追加（避免多一次必定失败的 RPC）。
		if l.dropped {
			l.dropped = false
			if !discarded {
				l.appendPendingLocked([]byte(dropNotice))
			}
		}
		l.mu.Unlock()
		attempt = -1 // 成功发送（或丢弃）后重新计次：后面的失败仍然可以重试
	}
}

// Close 发送剩余日志。
func (l *LogShipper) Close() {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	<-l.stopped
}
