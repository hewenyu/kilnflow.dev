package workflow

import (
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// Dir 是仓库中 workflow 文件所在的目录（PLAN #4.1）。
const Dir = ".kiln/workflows"

// 触发事件的名称：on: 中的键，也是 github.event_name。
const (
	EventPush             = gitref.EventPush
	EventPullRequest      = gitref.EventPullRequest
	EventWorkflowDispatch = gitref.EventWorkflowDispatch
	EventSchedule         = "schedule"
)

// git ref 的前缀。
const (
	RefPrefixBranch = gitref.BranchPrefix
	RefPrefixTag    = gitref.TagPrefix
	RefPrefixPull   = gitref.PullPrefix
)

// github.ref_type 的取值。
const (
	RefTypeBranch = "branch"
	RefTypeTag    = "tag"
)

// PullRef 返回 PR head commit 的 ref（PR 构建 head commit，不构建 GitHub 生成的合并提交）。
func PullRef(number int) string {
	return RefPrefixPull + strconv.Itoa(number) + "/head"
}

// RefInfo 返回 ref 对应的 github.ref_type 和 github.ref_name。PR 的 ref（refs/pull/<n>/head）按分支处理，
// ref_name 为 "<n>/head"，与 GitHub Actions 一致。
func RefInfo(ref string) (refType, refName string) {
	if name, ok := strings.CutPrefix(ref, RefPrefixTag); ok {
		return RefTypeTag, name
	}
	if name, ok := strings.CutPrefix(ref, RefPrefixPull); ok {
		return RefTypeBranch, name
	}
	return RefTypeBranch, strings.TrimPrefix(ref, RefPrefixBranch)
}

// GitHubContext 是 github 上下文（PLAN #4.7）。Server 以 JSON 保存在 runs.github_ctx 中，JSON 字段名不能修改。
type GitHubContext struct {
	EventName       string `json:"event_name"`
	SHA             string `json:"sha"`
	Ref             string `json:"ref"`
	RefName         string `json:"ref_name"`
	RefType         string `json:"ref_type"`
	Repository      string `json:"repository"`
	RepositoryOwner string `json:"repository_owner"`
	HeadRef         string `json:"head_ref"`
	BaseRef         string `json:"base_ref"`
	Actor           string `json:"actor"`
}

// Map 返回表达式求值使用的形式（PlanContext.GitHub，键与 JSON 字段名相同）。所有字段总是存在，
// 所以不论事件类型，workflow 都可以引用任何一个字段。
func (g *GitHubContext) Map() map[string]any {
	return map[string]any{
		"event_name":       g.EventName,
		"sha":              g.SHA,
		"ref":              g.Ref,
		"ref_name":         g.RefName,
		"ref_type":         g.RefType,
		"repository":       g.Repository,
		"repository_owner": g.RepositoryOwner,
		"head_ref":         g.HeadRef,
		"base_ref":         g.BaseRef,
		"actor":            g.Actor,
	}
}
