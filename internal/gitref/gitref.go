// Package gitref 是 git ref 的前缀、零 commit 的哨兵值和触发事件的名称（CI、AI 和共用的 GitHub 读取代码共用，不含领域判断）。
package gitref

// git ref 的前缀。
const (
	BranchPrefix = "refs/heads/"
	TagPrefix    = "refs/tags/"
	PullPrefix   = "refs/pull/"
)

// ZeroSHA 是 GitHub 表示"不存在"的 commit（新建分支的 before、删除的 ref 的 after）。
const ZeroSHA = "0000000000000000000000000000000000000000"

// 触发事件的名称（github.event_name，也是 runs.event、ai_requests.event）。
const (
	EventPush             = "push"
	EventPullRequest      = "pull_request"
	EventWorkflowDispatch = "workflow_dispatch"
)
