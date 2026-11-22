package store

import "context"

// Verification 是 PR head 的一个验证：CI 的 pull_request run，或者 AI 审计请求（PLAN #30.3）。CI、AI 各自提供，合并领域
// 据此判断能否合并；只统计 event=pull_request、base 为默认分支的验证。
type Verification struct {
	ID int64
	// Name 是显示名称（workflow 的 name，AI 审计为"AI 审计"）。
	Name string
	// Source 是验证的来源，也是 .kiln/merge.yml 中 required 的写法：CI 为 .kiln/workflows 中的文件名去掉 .yml / .yaml，
	// AI 审计为 ai-review（mergecfg.AIReview）。
	Source string
	// Builtin 表示内建的验证（AI 审计）：.kiln/workflows 中同名的 workflow 不能冒充它。
	Builtin bool
	// State 是验证的状态：queued | active | succeeded | failed | cancelled | skipped（CI run 与 AI 请求的取值相同）。
	State string
	// Jobs 是验证的 job（CI 为当前这次运行的 job，AI 审计为各平台的任务，名称为 review），只在要求时读取。
	Jobs []VerificationJob
}

// VerificationJob 是验证中的一个 job。
type VerificationJob struct {
	Name  string
	State string
}

// VerificationSource 读取 PR pr 的 head 针对 baseRef 的验证。合并领域在同一个 REPEATABLE READ 事务中调用全部来源，
// 保持单一快照。
type VerificationSource func(ctx context.Context, q Querier, repoID int64, pr int, head, baseRef string, withJobs bool) ([]Verification, error)
