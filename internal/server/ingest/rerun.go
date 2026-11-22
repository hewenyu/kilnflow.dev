package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// GitHub 上的 Re-run：check_run.rerequested 和 check_suite.rerequested。

func (i *Ingester) handleCheckRunRerequest(ctx context.Context, delivery string, repo *store.Repo, payload []byte, sender string) (string, error) {
	var p struct {
		CheckRun struct {
			ExternalID string `json:"external_id"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析，忽略：" + err.Error())
	}
	return i.handleRerequest(ctx, delivery, repo, p.CheckRun.ExternalID, sender)
}

// handleCheckSuiteRerequest 处理 Check Suite 上的 "Re-run all checks"：重跑该 commit 上已结束的 run（按 ID 顺序）。
// 中途被限流时整个事件稍后重试；已经由本 delivery 重跑过的 run 由 planner 按 delivery 去重，不会再次重跑，
// 即使那次重跑在冷却期间已经结束、run 又回到了终态。
func (i *Ingester) handleCheckSuiteRerequest(ctx context.Context, delivery string, repo *store.Repo, payload []byte, sender string) (string, error) {
	var p struct {
		CheckSuite struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析，忽略：" + err.Error())
	}
	ids, err := store.QueryIDs(ctx, i.db, `SELECT id FROM runs WHERE repo_id=$1 AND sha=$2 AND state NOT IN ('active','queued','skipped') ORDER BY id`,
		repo.ID, p.CheckSuite.HeadSHA)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		req := planner.RerunRequest{RunID: id, Mode: store.RerunFailed, Actor: githubActor(sender), DeliveryID: delivery}
		if _, err := i.planner.Rerun(ctx, req); err != nil {
			// 限流时整个事件稍后重试，已经处理过的 run 按 delivery 跳过
			if _, ok := gh.RateLimited(err); ok {
				return "", err
			}
			i.log.Warn("重跑", "run", id, "err", err)
		}
	}
	reviews := 0
	if i.reviews != nil {
		// AI 审计（PLAN #30.6）：每个请求族最新的一次，同一个 delivery 至多重跑一次
		if reviews, err = i.reviews.RerunHead(ctx, repo.ID, p.CheckSuite.HeadSHA, githubActor(sender), delivery); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("重跑 %d 个 run", len(ids))
	if reviews > 0 {
		msg += fmt.Sprintf("、%d 个 AI 审计请求", reviews)
	}
	return msg, nil
}

// rerunFailed 处理重跑失败：GitHub 限流时返回错误，等恢复后重试事件；其他原因（例如 run 还没有结束）只记录，不重试。
func rerunFailed(err error) (string, error) {
	if _, ok := gh.RateLimited(err); ok {
		return "", err
	}
	return dropEvent("重跑失败：" + err.Error())
}

// handleRerequest 处理 Check Run 上的 "Re-run"：job 的 Check Run 重跑这个 job，run 的汇总 Check Run 重跑失败的 job。
// external_id 指向的 run 必须属于事件的仓库 repo，否则丢弃（不让一个仓库的事件重跑另一个仓库的 run）。
// 重跑本身失败（例如 run 还没有结束）时只记录原因，不重试事件；事件重试时按 delivery 去重，不会重复重跑。
func (i *Ingester) handleRerequest(ctx context.Context, delivery string, repo *store.Repo, externalID, sender string) (string, error) {
	// AI 的 Check Run（ai-task:<id>、ai-request:<id>，PLAN #30.5）
	if i.reviews != nil {
		at, err := ai.ResolveExternalID(externalID)
		switch {
		case err == nil:
			return i.rerequestAI(ctx, delivery, repo, at, sender)
		case !errors.Is(err, ai.ErrUnknownExternalID):
			return dropEvent("external_id 无效：" + externalID)
		}
	}
	target, err := cicheck.ParseExternalID(externalID)
	if errors.Is(err, cicheck.ErrUnknownExternalID) {
		return "", nil
	}
	if err != nil {
		return dropEvent("external_id 无效：" + externalID)
	}
	actor := githubActor(sender)
	if target.JobID == 0 {
		var ok bool
		if err := i.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1 AND repo_id=$2)`, target.RunID, repo.ID).Scan(&ok); err != nil {
			return "", err
		}
		if !ok {
			return dropEvent("找不到 run")
		}
		r, err := i.planner.Rerun(ctx, planner.RerunRequest{RunID: target.RunID, Mode: store.RerunFailed, Actor: actor, DeliveryID: delivery})
		if err != nil {
			return rerunFailed(err)
		}
		return fmt.Sprintf("重跑 run #%d 失败的 job（rerun %d）", target.RunID, r), nil
	}
	var runID int64
	err = i.db.QueryRow(ctx, `SELECT j.run_id FROM jobs j JOIN runs r ON r.id=j.run_id WHERE j.id=$1 AND r.repo_id=$2`,
		target.JobID, repo.ID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dropEvent("找不到 job")
	}
	if err != nil {
		return "", err
	}
	r, err := i.planner.Rerun(ctx, planner.RerunRequest{RunID: runID, Mode: store.RerunJob, JobID: target.JobID, Actor: actor, DeliveryID: delivery})
	if err != nil {
		return rerunFailed(err)
	}
	return fmt.Sprintf("重跑 run #%d 的 job #%d（rerun %d）", runID, target.JobID, r), nil
}

// rerequestAI 处理 AI 审计的 Check Run 上的 "Re-run"：任务的 Check Run 重跑这个任务，请求的 Check Run 重跑没有成功的任务。
func (i *Ingester) rerequestAI(ctx context.Context, delivery string, repo *store.Repo, target ai.CheckTarget, sender string) (string, error) {
	r, found, err := i.reviews.Rerequest(ctx, repo.ID, target, githubActor(sender), delivery)
	if !found && err == nil {
		return dropEvent("找不到 AI 审计")
	}
	if err != nil {
		return rerunFailed(err)
	}
	return fmt.Sprintf("重跑 AI 审计（请求 #%d）", r.ID), nil
}
