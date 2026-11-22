package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// planned 是一个事件新建的 CI run 和 AI 审计请求的数量。
type planned struct{ runs, reviews int }

// total 返回新建的总数。
func (p planned) total() int { return p.runs + p.reviews }

// String 返回事件的处理结果（没有新建 AI 审计请求时不提）。
func (p planned) String() string {
	s := fmt.Sprintf("新建 %d 个 run", p.runs)
	if p.reviews > 0 {
		s += fmt.Sprintf("、%d 个 AI 审计请求", p.reviews)
	}
	return s
}

// planEvent 规划事件：CI 的 workflow，PR 事件还有 AI 审计（PLAN #30.6）。两边互不阻断：一方出错（例如配置错误、读取失败）
// 不影响另一方，出错时整个事件稍后重试，已经建成的按去重键跳过。PR 事件两边都规划完成后写 pr_plans。
func (i *Ingester) planEvent(ctx context.Context, ev *planner.Request) (planned, error) {
	ids, err := i.planner.PlanEvent(ctx, ev)
	n := planned{runs: len(ids)}
	if ev.Event != workflow.EventPullRequest {
		return n, err
	}
	if i.reviews != nil {
		id, aiErr := i.reviews.PlanEvent(ctx, reviewEvent(ev))
		if id != 0 {
			n.reviews++
		}
		if aiErr != nil {
			err = errors.Join(err, fmt.Errorf("规划 AI 审计：%w", aiErr))
		}
	}
	if err != nil {
		return n, err
	}
	return n, i.markPRPlanned(ctx, ev)
}

// reviewEvent 把 PR 事件转换为 AI 审计的规划输入。
func reviewEvent(ev *planner.Request) *review.Event {
	return &review.Event{
		Repo: ev.Repo, Event: ev.Event, Ref: ev.Ref, SHA: ev.SHA, Action: ev.Action, PRNumber: ev.PRNumber, BaseRef: ev.BaseRef,
		BaseSHA: ev.BaseSHA, HeadRef: ev.HeadRef, Sender: ev.SenderLogin, FromSelf: ev.FromSelf, Source: ev.Source,
	}
}

// markPRPlanned 记录 PR 的 head 已经按目标分支规划完成（pr_plans，合并队列据此判断，PLAN #28.3、#30.3）。
// 只增不改：乱序到达的旧 head 事件不会影响新 head 的记录。写入失败时事件稍后重试（按去重键不会重复规划）。
// 新记下时同一事务中记为自动入队的候选（PLAN #29.5）：验证可能在规划完成之前就已经结束并提名过，当时执行者看到规划没有
// 完成会丢掉候选，重试的规划不再新建、也就不会再提名。目标分支是否允许自动合并由执行者按 .kiln/merge.yml 裁决
// （PLAN #28.5），这里不按分支过滤。
func (i *Ingester) markPRPlanned(ctx context.Context, ev *planner.Request) error {
	if ev.PRNumber == 0 {
		return nil
	}
	nominated := false
	err := i.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO pr_plans (repo_id, pr_number, sha, base_ref) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			ev.Repo.ID, ev.PRNumber, ev.SHA, ev.BaseRef)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		nominated = true
		return mqevents.NominateAutoMerge(ctx, i.outbox, tx, ev.Repo.ID, ev.PRNumber)
	})
	if err != nil {
		return fmt.Errorf("记录 PR #%d 的规划：%w", ev.PRNumber, err)
	}
	if nominated {
		i.hub.All()
	}
	return nil
}
