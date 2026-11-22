package mergequeue

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 决策（PLAN #28.3 的决策表）：根据快照得出一个动作，不读写任何东西，便于表驱动测试。

// 更新 PR 分支的超时与次数：请求后超过 updateTimeout 新 head 还没到就重新请求，请求 maxUpdateTries 次仍没有完成时出队。
const (
	updateTimeout  = 10 * time.Minute
	maxUpdateTries = 3
)

// runInfo 是 PR head 的一个 pull_request run。
type runInfo struct {
	ID    int64
	Name  string
	State store.RunState
	// Source 是验证的来源（store.Verification.Source），Builtin 表示内建的验证（AI 审计）。
	Source  string
	Builtin bool
	// Jobs 是当前这次运行（rerun）的 job（只在配置了必需的 job 时读取）。
	Jobs []jobInfo
}

// jobInfo 是 run 中的一个 job。
type jobInfo struct {
	Name  string // job_name：workflow 中的 job ID，矩阵的各个变体相同
	State store.JobState
}

// satisfies 判断验证是否属于必需的验证 req：AI 审计只认内建的 AI 审计，其他验证按来源（workflow 文件名去掉
// .yml / .yaml）匹配。.kiln/workflows 中名为 ai-review.yml 的普通 workflow 不能冒充 AI 审计。
func (r *runInfo) satisfies(req mergecfg.Requirement) bool {
	return r.Source == req.Workflow && r.Builtin == (req.Workflow == mergecfg.AIReview)
}

// snapshot 是处理一个队列项时读到的状态。
type snapshot struct {
	Entry *Entry
	Pull  *forge.Pull
	// DefaultBranch 是仓库的默认分支。
	DefaultBranch string
	// ConfigErr 是 .kiln/merge.yml 的错误（为空表示配置有效）。
	ConfigErr string
	// BranchRemoval 表示 PR 的目标分支不在配置允许的合并分支中（BranchReason 是出队原因）：配置有效时才判断。
	BranchRemoval bool
	BranchReason  string
	// Parents 是 PR 当前 head 的父提交（只在 head 与记录不同、且正在更新分支时读取）。
	Parents []string
	// Processed 表示 Kiln 已经按 PR 的目标分支规划完这个 head（pr_plans 中有记录）。
	Processed bool
	// Runs 是这个 head 针对目标分支的 pull_request run。
	Runs []runInfo
	// Required 是默认分支的 .kiln/merge.yml 要求的验证。
	Required []mergecfg.Requirement
	// Behind 是 head 落后于目标分支的 commit 数。
	Behind int
	Now    time.Time
}

// 决定的动作。
type actionKind int

const (
	// actWait：等待（Status 是在等什么）。
	actWait actionKind = iota
	// actMerged：PR 已经合并，记为 merged。
	actMerged
	// actRemove：出队（Reason；Comment 为 true 时在 PR 上评论，DropLabel 为 true 时去掉 label）。
	actRemove
	// actAcceptHead：记录新的 head（Head），继续处理。
	actAcceptHead
	// actUpdate：请求更新 PR 分支。
	actUpdate
	// actMerge：合并。
	actMerge
)

// decision 是对一个队列项的决定。
type decision struct {
	Kind      actionKind
	Status    string
	Reason    string
	Comment   bool
	DropLabel bool
	Head      string
	// Removal、ConflictBase 是出队的类别和冲突时默认分支的 head（见 Entry）。
	Removal      string
	ConflictBase string
}

func wait(status string) decision { return decision{Kind: actWait, Status: status} }

// removeWithComment 出队，去掉 label 并在 PR 上评论原因（自动出队）。
func removeWithComment(reason string) decision {
	return decision{Kind: actRemove, Reason: reason, Comment: true, DropLabel: true}
}

// decidePull 根据 PR 本身的状态决定（PLAN #28.3 决策表的前几行）。返回 false 表示 PR 本身没有问题，需要继续看验证结果。
// 不在队首的项也用它检查（它们不会处于"正在更新分支"）。
func decidePull(s *snapshot) (decision, bool) {
	p, e := s.Pull, s.Entry
	switch {
	case p.State == forge.PullMerged:
		return decision{Kind: actMerged}, true
	case e.Action == actionRemoving:
		// 上一次自动出队没有完成（例如评论失败）：按记下的原因继续（关闭的 PR 不评论）
		return decision{
			Kind: actRemove, Reason: e.Reason, Comment: true, DropLabel: p.HasLabel(Label), Removal: e.Removal, ConflictBase: e.ConflictBase,
		}, true
	case p.State == forge.PullClosed:
		// 关闭的 PR 不评论；去掉 label，重新打开时不会被当成仍在队列中
		return decision{Kind: actRemove, Reason: "PR 已关闭", DropLabel: p.HasLabel(Label)}, true
	case !p.HasLabel(Label):
		return decision{Kind: actRemove, Reason: "去掉了 " + Label + " label"}, true
	case p.Fork:
		return removeWithComment("fork 的 PR 不能合并"), true
	case p.Draft:
		return removeWithComment("PR 是 draft"), true
	case e.HeadSHA == "":
		return decision{Kind: actAcceptHead, Head: p.HeadSHA}, true
	case p.HeadSHA != e.HeadSHA:
		// 队列更新分支产生的新 head：父提交是记录的 head 和更新时目标分支上的 commit。更新期间 PR 改投了目标分支时不接受
		// （action_base / base_ref 与当前目标分支不一致）：dev 更新出来的合并提交不能混进 main，按推送了新 commit 出队。
		// action_base 为空的是升级前的旧行，退回按 base_ref（也为空时放行）兼容判断。
		if e.Action == actionUpdating && len(s.Parents) == 2 && s.Parents[0] == e.ActionHead && e.ActionHead == e.HeadSHA &&
			(e.ActionBase == "" || e.ActionBase == p.BaseRef) && (e.BaseRef == "" || e.BaseRef == p.BaseRef) {
			return decision{Kind: actAcceptHead, Head: p.HeadSHA}, true
		}
		return removeWithComment("入队后推送了新的 commit，确认后重新加上 " + Label + " 即可再次入队"), true
	}
	return decision{}, false
}

// requiredRuns 返回必需的验证 r 对应的 run。
func requiredRuns(s *snapshot, r mergecfg.Requirement) []*runInfo {
	var out []*runInfo
	for i := range s.Runs {
		if s.Runs[i].satisfies(r) {
			out = append(out, &s.Runs[i])
		}
	}
	return out
}

// requiredJobs 返回 runs 中名为 name 的 job（矩阵的全部变体）。
func requiredJobs(runs []*runInfo, name string) []jobInfo {
	var out []jobInfo
	for _, r := range runs {
		for _, j := range r.Jobs {
			if j.Name == name {
				out = append(out, j)
			}
		}
	}
	return out
}

// missingRequired 返回没有对应 run（或 run 中没有对应 job）的必需验证。
func missingRequired(s *snapshot) []string {
	var out []string
	for _, r := range s.Required {
		runs := requiredRuns(s, r)
		if len(runs) == 0 || (r.Job != "" && len(requiredJobs(runs, r.Job)) == 0) {
			out = append(out, r.String())
		}
	}
	return out
}

// unsuccessfulRequired 返回没有成功的必需验证（run 全部结束后调用）：run 必须成功（跳过的 run 不算），
// 必需的 job 的每个变体都必须成功（命中缓存、沿用结果都算成功；跳过不算）。
func unsuccessfulRequired(s *snapshot) []string {
	var out []string
	for _, r := range s.Required {
		runs := requiredRuns(s, r)
		ok := true
		for _, run := range runs {
			ok = ok && run.State == store.RunSucceeded
		}
		if r.Job != "" {
			for _, j := range requiredJobs(runs, r.Job) {
				ok = ok && j.State == store.JobSucceeded
			}
		}
		if !ok {
			out = append(out, r.String())
		}
	}
	return out
}

// notPlannedStatus 是等待 Kiln 按目标分支规划 head 时的状态。
func notPlannedStatus(baseRef string) string {
	return "等待 Kiln 按 " + baseRef + " 规划这个 commit 的验证（接入 Kiln 或升级到合并队列之前打开的 PR 需要推送新的 commit，或关闭后重新打开）"
}

// decide 决定一项的下一步动作（PLAN #28.3）。
func decide(s *snapshot) decision {
	if d, ok := decidePull(s); ok {
		return d
	}
	e := s.Entry
	// 配置错误时一律等待（PLAN #28.5）：不按允许的分支出队合法 PR，也不因为正在更新分支就继续发请求——配置修好之前
	// 不改变队列，也不写平台。
	if s.ConfigErr != "" {
		return wait("配置错误：" + s.ConfigErr)
	}
	if e.Action == actionUpdating {
		if e.ActionAt == nil {
			if e.ActionTries >= maxUpdateTries {
				// 计了次数的等待（平台回答冲突、PR 的可合并状态却一直没有确认）用完：出队，不交给 AI 开发
				return removeWithComment(fmt.Sprintf("更新 PR 分支 %d 次都没有完成（%s）", e.ActionTries, e.Status))
			}
			// 上一次请求暂时失败（结果未知）或者在等平台计算可合并状态，新 head 也没有到：重新请求
			return decision{Kind: actUpdate}
		}
		if s.Now.Sub(*e.ActionAt) < updateTimeout {
			return wait("正在更新 PR 分支，等待新的 commit")
		}
		if e.ActionTries >= maxUpdateTries {
			return removeWithComment(fmt.Sprintf("更新 PR 分支 %d 次都没有完成", e.ActionTries))
		}
		return decision{Kind: actUpdate}
	}
	if !s.Processed {
		return wait(notPlannedStatus(s.Pull.BaseRef))
	}
	var failed []string
	done := 0
	for _, r := range s.Runs {
		switch {
		case !r.State.Terminal():
		case r.State == store.RunSucceeded || r.State == store.RunSkipped:
			done++
		default:
			done++
			failed = append(failed, fmt.Sprintf("%s（run #%d，%s）", r.Name, r.ID, r.State))
		}
	}
	if len(failed) > 0 {
		return removeWithComment("验证没有通过：" + strings.Join(failed, "、"))
	}
	// 规划完成后 run 和 job 都已确定（与 run 在同一事务中创建）：没有任何 run、缺少必需的验证时不会再出现，直接出队
	// 跳过的 run（例如 paths 不匹配）不算验证：PR 可以改自己 head 中的过滤条件让 run 被跳过
	if !slices.ContainsFunc(s.Runs, func(r runInfo) bool { return r.State != store.RunSkipped }) {
		return removeWithComment("这个 commit 没有任何验证（没有 pull_request 的 run，或者都被跳过了，例如 PR 删掉了 .kiln/workflows 中的 workflow、改了 paths）：合并队列不合并没有经过验证的 PR")
	}
	if missing := missingRequired(s); len(missing) > 0 {
		return removeWithComment(fmt.Sprintf("缺少必需的验证：%s（默认分支的 %s 要求；PR 不能删除或改名它们）", strings.Join(missing, "、"), mergecfg.Path))
	}
	if done < len(s.Runs) {
		return wait(fmt.Sprintf("等待验证：%d/%d 个 run 已结束", done, len(s.Runs)))
	}
	if bad := unsuccessfulRequired(s); len(bad) > 0 {
		return removeWithComment(fmt.Sprintf("必需的验证没有成功：%s（默认分支的 %s 要求）", strings.Join(bad, "、"), mergecfg.Path))
	}
	if s.Behind > 0 {
		return decision{Kind: actUpdate}
	}
	return decision{Kind: actMerge}
}
