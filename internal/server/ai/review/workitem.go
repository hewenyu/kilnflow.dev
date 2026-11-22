package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 工作项的 PR 上的审计（PLAN #26.11、#31.5）：审计带上上一轮审计的问题和开发给出的处置，逐个判断处置是否成立
// （"已修复"要在新的 head 上确认，"不采纳"要判断理由）；finding 以稳定 ID 引用。设计 PR 的审计方式按被审的 head 判断
// （I12，见 reviewPhase）。

// prWorkItem 是 PR 所属的工作项。
type prWorkItem struct {
	id int64
	// designPR 表示 PR 是工作项的设计 PR（按 PR 编号等于 design_pr 或正式分支是设计分支判断，不看工作项的当前阶段）。
	designPR bool
	// mergedPR、mergedSHA 是已合并的设计 PR 和合并得到的 commit（先设计的工作项在实现阶段时）。
	mergedPR  int
	mergedSHA string
	prior     []aispec.PriorFinding
	// devAgents 是开发过这个工作项的 Agent（dev_work_items.dev_agents）：审计不能再用它们（PLAN #33.7）。
	devAgents []string
}

// workItemReview 返回 PR pr（来源分支 headRef）所属的工作项和上一轮的问题与处置（不是工作项的 PR 时为 nil）。按 PR
// 编号（本轮的 PR、设计 PR）或正式分支关联（Server 建 PR 之后才记下编号，记下之前 opened 事件已经可能到达）。处置来自
// 工作项最近一次交付报告，问题的标题和位置来自那次返修任务的内容。
//
// 按编号和按分支找到的不是同一个工作项时用按编号找到的。还没记下编号的工作项 pr_number、design_pr 为 NULL，比较的结果
// 是 NULL（不是 false），在 DESC 中排在最前，所以排序键写成 IS TRUE（2026-11-17 代码审计 D2）。
func workItemReview(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (*prWorkItem, error) {
	if pr == 0 {
		return nil, nil
	}
	b, isBranch := devreport.ParseWorkItemBranch(headRef)
	var w prWorkItem
	var designPR *int
	err := q.QueryRow(ctx, `SELECT id, design_pr, design_sha, dev_agents FROM dev_work_items WHERE repo_id=$1
		AND (pr_number=$2 OR design_pr=$2 OR id=$3) ORDER BY (pr_number=$2 OR design_pr=$2) IS TRUE DESC, id DESC LIMIT 1`, repoID, pr, b.ID).
		Scan(&w.id, &designPR, &w.mergedSHA, &w.devAgents)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case designPR != nil && *designPR == pr, isBranch && b.ID == w.id && b.Phase == devreport.PhaseDesign:
		w.designPR, w.mergedSHA = true, ""
	case designPR != nil:
		w.mergedPR = *designPR
	default:
		w.mergedSHA = ""
	}
	if w.prior, err = priorFindings(ctx, q, w.id); err != nil {
		return nil, err
	}
	return &w, nil
}

// priorFindings 返回工作项上一轮审计的问题和开发给出的处置。
func priorFindings(ctx context.Context, q store.Querier, id int64) ([]aispec.PriorFinding, error) {
	var report, spec []byte
	err := q.QueryRow(ctx, `SELECT r.report, t.spec FROM dev_reports r JOIN ai_tasks t ON t.id=r.task_id
		WHERE r.work_item_id=$1 AND r.kind='delivery' ORDER BY r.created_at DESC LIMIT 1`, id).Scan(&report, &spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rep devreport.Delivery
	var ds aispec.DevelopSpec
	if json.Unmarshal(report, &rep) != nil || json.Unmarshal(spec, &ds) != nil {
		return nil, nil //nolint:nilerr // 保存时校验过；解析不了时不带上一轮（审计照常进行）
	}
	var prior []aispec.PriorFinding
	findings := map[string]aispec.DevFinding{}
	for _, f := range ds.Findings {
		findings[f.ID] = f
	}
	for _, d := range rep.Dispositions {
		f := findings[d.Finding]
		note := d.Reason
		if d.DuplicateOf != "" {
			note = "同 " + d.DuplicateOf + "：" + note
		}
		prior = append(prior, aispec.PriorFinding{ID: d.Finding, Title: f.Title, Path: f.Path, Line: f.Line, Disposition: d.Action, Note: note})
	}
	return prior, nil
}

// 审计方式（ReviewSpec.Phase，PLAN #31.5）。
const (
	phaseDesign    = devreport.PhaseDesign
	phaseImplement = devreport.PhaseImplement
)

// reviewPhase 返回工作项的 PR 在被审 head 上的审计方式和说明（I12）：设计 PR 的 head 相对 base 的完整改动只有 Markdown 时
// 按设计审计；有其他文件或文件列表不完整时按实现审计（包括人之后推送了代码的 head），说明写明原因。不是设计 PR 时按实现审计。
func reviewPhase(w *prWorkItem, files []string, complete bool) (phase, note string) {
	if !w.designPR {
		return phaseImplement, ""
	}
	if !complete {
		return phaseImplement, "这是 AI 开发的设计 PR，但改动的文件太多、无法确认只有 Markdown，按实现审计的完整标准审计"
	}
	var other []string
	for _, f := range files {
		if !devreport.IsMarkdown(f) {
			other = append(other, f)
		}
	}
	if len(other) > 0 {
		return phaseImplement, fmt.Sprintf("这是 AI 开发的设计 PR，但被审的 head 改了 Markdown 之外的文件（%s），按实现审计的完整标准审计",
			shorten.Text(strings.Join(other, "、"), maxPhaseNoteFiles))
	}
	return phaseDesign, ""
}

// maxPhaseNoteFiles 是审计方式说明中文件列表的长度上限。
const maxPhaseNoteFiles = 1000
