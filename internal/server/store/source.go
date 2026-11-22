package store

// 请求的来源（runs.source、ai_requests.source）。CI 和 AI 共用这些值。
const (
	// SourceWebhook：GitHub webhook 事件。
	SourceWebhook = "webhook"
	// SourceReconcile：引用对账补上的事件（PLAN #7.3）。
	SourceReconcile = "reconcile"
	// SourceDashboard：在 Dashboard 中手动触发。
	SourceDashboard = "dashboard"
	// SourceCLI：用 kiln CLI 手动触发。
	SourceCLI = "cli"
	// SourceAutoTag：Server 自动打 tag 后直接规划的 tag push（PLAN #29.4）。
	SourceAutoTag = "autotag"
	// SourceAIDev：AI 开发的执行者规划的选题、返修（PLAN #26.11）。
	SourceAIDev = "aidev"
)
