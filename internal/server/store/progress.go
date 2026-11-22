package store

// 进度阶段（AttemptProgress.Phase）。
const (
	PhasePrepare  = "prepare"
	PhaseStep     = "step"
	PhaseUpload   = "upload"
	PhaseCleanup  = "cleanup"
	PhaseComplete = "complete"
)

// AttemptProgress 是 Runner 在心跳中上报的执行阶段与进度（attempts.progress）。
// 时间都是 Unix 秒，由 Server 按收到心跳的时间换算（不依赖 Runner 的时钟）。
type AttemptProgress struct {
	// Phase 是 prepare、step、upload、cleanup、complete 之一；Detail 是一句话的说明。
	Phase  string
	Detail string
	// Step 是正在执行的 step 序号（从 0 起；-1 表示还没开始执行 step）。
	Step int
	// PhaseStartedAt 是阶段开始的时间，MovedAt 是最近一次进展的时间，ReportedAt 是最近一次上报的时间。
	PhaseStartedAt, MovedAt, ReportedAt int64
	// 上传阶段：需要上传的对象数与字节数（不含 Server 已有的 FilesSkipped 个）、Server 已确认的对象数与字节数、
	// 正在上传的对象已发送的字节数（客户端写出，不代表 Server 已确认）。
	FilesTotal, FilesDone, FilesSkipped int
	BytesTotal, BytesDone, BytesSent    int64
	// Retries 是当前对象的重试次数，LastError 是最近一次失败的原因。
	Retries   int
	LastError string
}
