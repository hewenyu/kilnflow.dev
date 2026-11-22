package aidev

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 规划开发请求（PLAN #26.8、#30.6）：一次选题或一轮返修是一个请求，其中一个开发任务。内容（选题的轮次、返修的问题）由
// 执行者准备好。同一个仓库按工作项串行（I14，serial.go），不同仓库的开发请求并行：选题由选题轮次和仓库占用串行，同一个
// 工作项的返修在按工作项的并发组中排队（只留最新的），不取消进行中的。

// dedupePrefix 是开发请求去重键的前缀（与迁移前开发 run 的 dedupe_key 相同，迁移后的去重一致）。
const dedupePrefix = aispec.EventDevelop

// taskKey 是开发请求中唯一任务的 key。
const taskKey = "develop"

// fixGroup 返回工作项返修的并发组。
func fixGroup(workItem int64) string { return fmt.Sprintf("fix:work-item:%d", workItem) }

// fixKey 返回工作项第 round 轮返修的去重键。
func fixKey(repoID, workItem int64, round int) string {
	return fmt.Sprintf("%s:%d:fix:%d:%d", dedupePrefix, repoID, workItem, round)
}

// devRequest 是要规划的一次开发执行。
type devRequest struct {
	repo *store.Repo
	// ref、sha 是起点：选题为默认分支和它的 head，返修为正式分支和期望的 head。
	ref, sha string
	// platforms 是可以执行的平台（development.platform 的 Runner 平台，至少一个）。
	platforms []string
	// dedupeKey 是请求的去重键：同一个键只建一个请求（重复的事件、执行者重试不会重复派单）。
	dedupeKey string
	sender    string
	timeout   int // 分钟
	spec      *aispec.DevelopSpec
}

// plan 建开发请求和它的任务，返回请求和任务；同一个去重键的请求已经存在时返回已有的。
func (s *Service) plan(ctx context.Context, req *devRequest) (*ai.Request, *ai.Task, error) {
	if s.gh != nil {
		if err := repodir.Complete(ctx, s.db, s.gh, req.repo); err != nil {
			return nil, nil, err
		}
	}
	d := req.spec
	mode := "选题"
	switch d.Mode {
	case aispec.DevFix:
		mode = fmt.Sprintf("工作项 #%d 返修第 %d 轮", d.WorkItemID, d.Round)
	case aispec.DevImplement:
		mode = fmt.Sprintf("工作项 #%d 第 %d 轮实现", d.WorkItemID, d.ImplRound)
	}
	nr := &ai.NewRequest{
		Type: agentcfg.TaskDevelop, RepoID: req.repo.ID, Event: aispec.EventDevelop, Ref: req.ref, SHA: req.sha, Trigger: d.Mode,
		Source: store.SourceAIDev, Sender: req.sender, DedupeKey: req.dedupeKey,
		Tasks: []ai.NewTask{{
			Key: taskKey, Platforms: req.platforms, Agent: d.Agent,
			DisplayName: fmt.Sprintf("%s · %s", d.Agent, mode), Spec: d, TimeoutMinutes: req.timeout,
		}},
		Detail: func(ctx context.Context, tx pgx.Tx, id int64) error {
			_, err := tx.Exec(ctx, `INSERT INTO ai_dev_requests (request_id, mode, work_item_id, round) VALUES ($1,$2,nullif($3, 0),$4)`,
				id, d.Mode, d.WorkItemID, d.Round)
			return err
		},
	}
	if d.Mode == aispec.DevFix {
		nr.GroupKey = fixGroup(d.WorkItemID)
	}
	planned, err := s.core.Plan(ctx, nr)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := ai.RequestTasks(ctx, s.db, planned.Request.ID, false)
	if err != nil {
		return nil, nil, err
	}
	if len(tasks) == 0 {
		return nil, nil, fmt.Errorf("AI 开发请求 %s 没有建成任务", req.dedupeKey)
	}
	if f := s.afterPlan; f != nil {
		f(tasks[0])
	}
	return planned.Request, tasks[0], nil
}
