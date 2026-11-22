package review

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 合并队列更新 PR 分支后沿用审计（PLAN #28.4）：队列把默认分支合并进 PR 分支产生的新 head，PR 自己的改动没有变化，
// 沿用旧 head 的审计结论（CI 照常全部重跑）。

// queueReuse 判断 PR 事件的新 head 能否沿用旧 head 的审计；能时把 tasks 改为沿用（按 key 一一对应）并返回来源请求，
// 否则不改、返回 nil。条件：事件由本 App 触发（synchronize）；合并队列记录了"已请求从旧 head X 更新"（或者已经接受了
// 这个新 head，记录中保留着 X）；新 head 的父提交是 X 和另一个 commit（默认分支）；X 针对同一目标分支的审计请求已经
// 成功；审计规则（policy_digest：agent、从 base 读取的审计说明、报告版本）与这次相同（PLAN #29.6）；每个任务可以执行的
// 平台与这次相同（一组平台中由哪台执行不影响沿用）。
func (p *Reviews) queueReuse(ctx context.Context, inst *gh.Inst, ev *Event, tasks []ai.NewTask) (*int64, error) {
	if !ev.FromSelf || ev.Event != eventPullRequest || ev.Action != "synchronize" || ev.PRNumber == 0 || p.updatedFrom == nil {
		return nil, nil
	}
	from, err := p.updatedFrom(ctx, p.db, ev.Repo.ID, ev.PRNumber, ev.SHA, ev.BaseRef)
	if err != nil || from == "" {
		return nil, err
	}
	c, err := inst.GetCommit(ctx, ev.Repo.FullName(), ev.SHA)
	if err != nil {
		return nil, fmt.Errorf("读取 commit %s：%w", ev.SHA, err)
	}
	if len(c.Parents) != 2 || c.Parents[0].SHA != from {
		return nil, nil
	}
	reqs, err := ai.QueryRequests(ctx, p.db, `type=$1 AND repo_id=$2 AND event=$3 AND sha=$4 AND state='succeeded'
		AND id IN (SELECT request_id FROM ai_review_requests WHERE pr_number=$5 AND base_ref=$6) ORDER BY id DESC LIMIT 1`,
		agentcfg.TaskReview, ev.Repo.ID, eventPullRequest, from, ev.PRNumber, ev.BaseRef)
	if err != nil || len(reqs) == 0 {
		return nil, err
	}
	prev, err := ai.RequestTasks(ctx, p.db, reqs[0].ID, false)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*ai.Task, len(prev))
	for _, t := range prev {
		if t.State != ai.TaskSucceeded {
			return nil, nil
		}
		byKey[t.Key] = t
	}
	src := make([]*ai.Task, len(tasks))
	oldBase := ""
	for i, t := range tasks {
		pt, ok := byKey[t.Key]
		if !ok || !samePlatforms(pt.Platforms, t.Platforms) {
			return nil, nil // 审计的平台变了（包括同一个 id 的一组平台变了）：照常审计
		}
		var old aispec.ReviewSpec
		cur, _ := t.Spec.(*aispec.ReviewSpec)
		if pt.DecodeSpec(&old) != nil || cur == nil || old.PolicyDigest == "" || old.PolicyDigest != cur.PolicyDigest {
			// 审计规则变了（例如默认分支改了审计说明）或者是没有记录摘要的旧审计：旧结论不再适用（PLAN #29.6）
			return nil, nil //nolint:nilerr // 旧 spec 解析不了时同样照常审计
		}
		src[i] = pt
		oldBase = cmp.Or(oldBase, old.BaseSHA)
	}
	for i := range tasks {
		// 沿用的范围要看得见：审计时的 base 之后合入目标分支的改动不在旧结论里，由 CI 验证
		tasks[i].ReuseFrom = src[i]
		tasks[i].ReuseReason = fmt.Sprintf("合并队列只把目标分支 %s（%s）合并进了 PR 分支，沿用 %s 的审计结论（任务 #%d）。审计时的 base 是 %s："+
			"之后合入目标分支的改动不在这次审计的范围内，由 CI 验证",
			ev.BaseRef, shorten.Hash(c.Parents[1].SHA), shorten.Hash(from), ai.ReuseSource(src[i]), cmp.Or(shorten.Hash(oldBase), "未记录"))
	}
	return &reqs[0].ID, nil
}

// samePlatforms 判断两个任务可以执行的平台是否相同（与顺序无关）。
func samePlatforms(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}
