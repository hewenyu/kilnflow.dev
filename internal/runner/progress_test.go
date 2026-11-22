package runner

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// fakeClock 是可以手动拨动的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestProgress() (*progress, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := &progress{now: c.now}
	p.set(runnerv1.Phase_PHASE_PREPARE, "")
	return p, c
}

const mib = 1 << 20

// 上传进度：Server 已有的对象不计入；已发送和已确认分开计数；重试从 0 重新计数，不重复累计；
// 全部发送后还没确认时说明在等 Server 确认。
func TestProgressUpload(t *testing.T) {
	t.Parallel()
	p, clock := newTestProgress()
	clock.add(3 * time.Second)
	p.set(runnerv1.Phase_PHASE_UPLOAD, "上传产物到 Kiln Server")
	// 3 个对象共 101 MiB，其中 1 MiB 的那个 Server 已有
	p.planUpload(3, 101*mib, 1, 1*mib)
	p.startObject("dmg/Kiln.dmg", 80*mib)
	clock.add(10 * time.Second)
	p.sent(40 * mib)

	s := p.snapshot()
	if s.Phase != runnerv1.Phase_PHASE_UPLOAD || s.FilesTotal != 2 || s.FilesSkipped != 1 || s.BytesTotal != 100*mib ||
		s.BytesSent != 40*mib || s.BytesDone != 0 || s.PhaseElapsedMs != 10_000 || s.IdleMs != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	line := p.uploadLine()
	for _, want := range []string{"已确认 0/2 个文件", "40.0 MiB / 100.0 MiB（40%）", "平均 4.0 MiB/s", "当前 dmg/Kiln.dmg"} {
		if !strings.Contains(line, want) {
			t.Errorf("进度日志 %q 中没有 %q", line, want)
		}
	}

	// 失败后重试：已发送从 0 开始，不累计失败那次发送的字节
	p.failed("HTTP 502")
	p.startObject("dmg/Kiln.dmg", 80*mib)
	p.sent(10 * mib)
	if s := p.snapshot(); s.BytesSent != 10*mib || s.Retries != 1 || s.LastError != "HTTP 502" {
		t.Fatalf("重试后 snapshot = %+v", s)
	}
	if line := p.uploadLine(); !strings.Contains(line, "10.0 MiB / 100.0 MiB（10%）") || !strings.Contains(line, "第 1 次重试，上次失败：HTTP 502") {
		t.Errorf("重试后进度日志 = %q", line)
	}
}

// 全部发送、还没确认时不算完成，说明在等确认；长时间没有进展时提示（不判定失败）；离开上传阶段时清空计数。
func TestProgressUploadWaits(t *testing.T) {
	t.Parallel()
	p, clock := newTestProgress()
	clock.add(3 * time.Second)
	p.set(runnerv1.Phase_PHASE_UPLOAD, "上传产物到 Kiln Server")
	p.planUpload(3, 101*mib, 1, 1*mib)
	p.startObject("dmg/Kiln.dmg", 80*mib)
	clock.add(10 * time.Second)
	p.sent(80 * mib)
	clock.add(40 * time.Second)
	if s := p.snapshot(); s.FilesDone != 0 || s.BytesDone != 0 {
		t.Fatalf("没有确认就计入了完成：%+v", s)
	}
	line := p.uploadLine()
	if !strings.Contains(line, "已全部发送，等待 Server 确认校验（40s）") {
		t.Errorf("等待确认时进度日志 = %q", line)
	}
	if strings.Contains(line, "没有新的进展") {
		t.Errorf("等待确认时不应提示没有进展：%q", line)
	}

	p.confirmed()
	if s := p.snapshot(); s.FilesDone != 1 || s.BytesDone != 80*mib || s.BytesSent != 0 || s.Retries != 0 || s.LastError != "" {
		t.Fatalf("确认后 snapshot = %+v", s)
	}

	// 第二个对象长时间没有进展：提示，但不判定失败
	p.startObject("dmg/SHA256SUMS", 20*mib)
	p.sent(mib)
	clock.add(45 * time.Second)
	if line := p.uploadLine(); !strings.Contains(line, "已 45s 没有新的进展（仍在等待，未判定失败）") || !strings.Contains(line, "（81%）") {
		t.Errorf("没有进展时进度日志 = %q", line)
	}
	if s := p.snapshot(); s.IdleMs != 45_000 {
		t.Errorf("idle_ms = %d", s.IdleMs)
	}
	p.sent(19 * mib)
	p.confirmed()
	if done := p.uploadDone(); !strings.Contains(done, "产物上传完成：2 个文件，100.0 MiB，用时 1m35s") {
		t.Errorf("完成日志 = %q", done)
	}

	// 离开上传阶段时清空计数
	p.set(runnerv1.Phase_PHASE_CLEANUP, "清理")
	if s := p.snapshot(); s.FilesTotal != 0 || s.BytesTotal != 0 || s.PhaseElapsedMs != 0 {
		t.Errorf("切换阶段后 snapshot = %+v", s)
	}
}

// percent 向下取整，不超过 100；总数为 0 时为 0。
func TestPercent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ n, total, want int64 }{{0, 0, 0}, {5, 0, 0}, {999, 1000, 99}, {1000, 1000, 100}, {2000, 1000, 100}} {
		if got := percent(c.n, c.total); got != c.want {
			t.Errorf("percent(%d, %d) = %d，期望 %d", c.n, c.total, got, c.want)
		}
	}
}

// 执行时：心跳上报阶段；构建结束后立即说明在收集和上传产物；上传失败会重试并写明原因（不带预签名地址）；
// 提交结果前补发一次"正在提交结果"的心跳。
func TestExecuteUploadProgress(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	content := strings.Repeat("x", 4096)
	big := cas.Digest([]byte(content))
	var mu sync.Mutex
	failures := 0
	fs.putHook = func(d string) int {
		mu.Lock()
		defer mu.Unlock()
		if d == big && failures == 0 {
			failures++
			return http.StatusBadGateway
		}
		return 0
	}
	spec := hostJob(1, bash("build", "mkdir -p dist && printf '"+content+"' > dist/app.bin"))
	spec.Outputs = []*runnerv1.OutputSpec{{Name: "dist", Path: "dist/"}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, want := range []string{
		"构建完成，正在收集产物（1 个输出），之后上传到 Kiln Server",
		"输出 dist：1 个文件，4.0 KiB",
		"上传产物到 Kiln Server：1 个文件，4.0 KiB",
		"上传 dist/app.bin 失败（第 1/4 次）：上传失败：HTTP 502 injected failure",
		"产物上传完成：1 个文件，4.0 KiB",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("日志中没有 %q：\n%s", want, logTail(log))
		}
	}
	if strings.Contains(log, "sig=") {
		t.Errorf("日志中出现了预签名地址：\n%s", logTail(log))
	}
	hbs := fs.heartbeatCalls()
	if len(hbs) == 0 {
		t.Fatal("没有心跳")
	}
	last := hbs[len(hbs)-1].GetProgress()
	if last.GetPhase() != runnerv1.Phase_PHASE_COMPLETE {
		t.Errorf("提交结果前最后一次心跳的阶段 = %s，期望 PHASE_COMPLETE", last.GetPhase())
	}
}

// Server 已有全部产物时不上传，日志说明跳过的原因，不出现上传进度。
func TestExecuteUploadAllCached(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.putBlob([]byte("a"))
	spec := hostJob(1, bash("build", "mkdir -p dist && printf a > dist/a.txt"))
	spec.Outputs = []*runnerv1.OutputSpec{{Name: "dist", Path: "dist/"}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if !strings.Contains(log, "Server 已有全部 1 个文件（1 B），无需上传") {
		t.Errorf("日志中没有跳过上传的说明：\n%s", logTail(log))
	}
	if strings.Contains(log, "产物上传完成") || strings.Contains(log, "上传中：") {
		t.Errorf("没有上传时不应有上传进度：\n%s", logTail(log))
	}
}

// 没有声明输出的 job 不说"收集产物"。
func TestExecuteNoOutputs(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	got, log := runJob(t, r, fs, hostJob(1, bash("build", "true")))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if strings.Contains(log, "收集产物") || strings.Contains(log, "上传产物") {
		t.Errorf("没有输出时不应说明上传产物：\n%s", logTail(log))
	}
}
