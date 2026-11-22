package runner

import (
	"fmt"
	"strings"
	"sync"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 执行阶段与进度：Runner 在心跳中上报，Dashboard 据此说明 job 正在做什么；
// 上传产物时另外按节流间隔写日志，CLI 看日志也能判断任务是否仍在推进。

const (
	// progressLogInterval 是上传期间进度日志的最短间隔（不按数据块写日志）。
	progressLogInterval = 15 * time.Second
	// stallNotice 是多久没有进展时在进度日志中提示（只是提示，不判定失败）。
	stallNotice = 30 * time.Second
	// minSpeedWindow 是计算平均速度所需的最短时长，太短的速度没有参考价值。
	minSpeedWindow = 5 * time.Second
	// maxProgressText 是上报的说明和错误信息的长度上限（字节）。
	maxProgressText = 200
)

// progress 记录 attempt 当前的阶段与进度。并发安全：执行过程写，心跳和进度日志读。
type progress struct {
	now func() time.Time

	mu         sync.Mutex
	phase      runnerv1.Phase
	detail     string
	phaseStart time.Time
	// lastMove 是最近一次进展（阶段切换、发送了数据、对象确认）的时间。
	lastMove time.Time
	up       uploadCounters
}

// uploadCounters 是上传阶段的计数（字段含义见 runnerv1.Progress）。
type uploadCounters struct {
	filesTotal, filesDone, filesSkipped int
	bytesTotal, bytesDone, bytesSent    int64
	retries                             int
	lastErr                             string
	// current 是正在上传的对象（输出名称和工作区中的相对路径）；currentSize 是它的大小。
	current     string
	currentSize int64
	// sentAll 是当前对象全部发送完的时间（还在等 Server 确认）；没有发送完时为零值。
	sentAll time.Time
	start   time.Time
}

func newProgress() *progress {
	p := &progress{now: time.Now}
	p.set(runnerv1.Phase_PHASE_PREPARE, "")
	return p
}

// set 切换阶段（同一阶段只更新说明，不重新计时）。离开上传阶段时清空上传计数。
func (p *progress) set(phase runnerv1.Phase, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if phase != p.phase {
		p.phase, p.phaseStart = phase, now
		p.up = uploadCounters{}
	}
	p.detail = shorten.Text(detail, maxProgressText)
	p.lastMove = now
}

// snapshot 返回上报给 Server 的进度。
func (p *progress) snapshot() *runnerv1.Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	u := &p.up
	return &runnerv1.Progress{
		Phase:          p.phase,
		Detail:         p.detail,
		PhaseElapsedMs: now.Sub(p.phaseStart).Milliseconds(),
		IdleMs:         now.Sub(p.lastMove).Milliseconds(),
		FilesTotal:     runnerkit.ClampInt32(u.filesTotal),
		FilesDone:      runnerkit.ClampInt32(u.filesDone),
		FilesSkipped:   runnerkit.ClampInt32(u.filesSkipped),
		BytesTotal:     u.bytesTotal,
		BytesDone:      u.bytesDone,
		BytesSent:      u.bytesSent,
		Retries:        runnerkit.ClampInt32(u.retries),
		LastError:      u.lastErr,
	}
}

// planUpload 登记一批需要上传的对象：files、size 是本地的全部对象，skipped、skippedBytes 是 Server 已有、不需要上传的。
func (p *progress) planUpload(files int, size int64, skipped int, skippedBytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &p.up
	if u.start.IsZero() {
		u.start = p.now()
	}
	u.filesTotal += files - skipped
	u.bytesTotal += size - skippedBytes
	u.filesSkipped += skipped
}

// startObject 开始上传（或重试上传）一个对象：已发送的字节从 0 重新计数。
func (p *progress) startObject(name string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &p.up
	u.current, u.currentSize, u.bytesSent, u.sentAll = name, size, 0, time.Time{}
	p.lastMove = p.now()
}

// sent 累计当前对象已发送的字节（只是客户端写出，不代表 Server 已收到并校验）。
func (p *progress) sent(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &p.up
	u.bytesSent += n
	now := p.now()
	p.lastMove = now
	if u.bytesSent >= u.currentSize && u.sentAll.IsZero() {
		u.sentAll = now
	}
}

// failed 记录当前对象的一次失败（随后可能重试）。
func (p *progress) failed(err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up.retries++
	p.up.lastErr = shorten.Text(err, maxProgressText)
}

// confirmed 表示 Server 已确认收到并校验当前对象。
func (p *progress) confirmed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &p.up
	u.filesDone++
	u.bytesDone += u.currentSize
	u.current, u.currentSize, u.bytesSent, u.sentAll = "", 0, 0, time.Time{}
	u.retries, u.lastErr = 0, ""
	p.lastMove = p.now()
}

// uploadLine 返回一行上传进度日志，例如
// "上传中：已确认 0/1 个文件，已发送 45.2 MiB / 129.4 MiB（34%），平均 1.2 MiB/s；当前 dmg/Kiln.dmg"。
func (p *progress) uploadLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	u := &p.up
	moved := u.bytesDone + u.bytesSent
	var b strings.Builder
	fmt.Fprintf(&b, "上传中：已确认 %d/%d 个文件，已发送 %s / %s", u.filesDone, u.filesTotal, bytesize.Format(moved), bytesize.Format(u.bytesTotal))
	if u.bytesTotal > 0 {
		fmt.Fprintf(&b, "（%d%%）", percent(moved, u.bytesTotal))
	}
	if el := now.Sub(u.start); el >= minSpeedWindow && moved > 0 {
		fmt.Fprintf(&b, "，平均 %s/s", bytesize.Format(int64(float64(moved)/el.Seconds())))
	}
	if u.current != "" {
		fmt.Fprintf(&b, "；当前 %s", u.current)
		if !u.sentAll.IsZero() {
			fmt.Fprintf(&b, "，已全部发送，等待 Server 确认校验（%s）", now.Sub(u.sentAll).Round(time.Second))
		}
	}
	if u.retries > 0 {
		fmt.Fprintf(&b, "；第 %d 次重试，上次失败：%s", u.retries, u.lastErr)
	}
	if idle := now.Sub(p.lastMove); idle >= stallNotice && u.sentAll.IsZero() {
		fmt.Fprintf(&b, "；已 %s 没有新的进展（仍在等待，未判定失败）", idle.Round(time.Second))
	}
	return b.String()
}

// uploadDone 返回上传完成的日志。
func (p *progress) uploadDone() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &p.up
	el := p.now().Sub(u.start)
	s := fmt.Sprintf("产物上传完成：%d 个文件，%s，用时 %s", u.filesDone, bytesize.Format(u.bytesDone), el.Round(time.Millisecond))
	if el >= minSpeedWindow && u.bytesDone > 0 {
		s += fmt.Sprintf("，平均 %s/s", bytesize.Format(int64(float64(u.bytesDone)/el.Seconds())))
	}
	return s
}

// percent 返回 n/total 的百分比（向下取整，未完成时不显示 100%）。
func percent(n, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return min(n*100/total, 100)
}
