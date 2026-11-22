package decisioninbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 入队：在 AI 开发结束的事务里，把有效的交付报告拆成条目并登记意图。这里绝不发起外部请求，也不锁工作项之外的资源。
// 任何失败都返回给调用方，由调用方回滚整个事务（交付报告与同步意图要么一起提交，要么都不提交）。

// EnqueueReportTx 在一次交付报告的同一个事务 tx 中登记要同步的条目，返回是否有新条目插入。
// 仓库没有配置「待决策清单」时什么也不做（也不写入唤醒记录）。
func (s *Service) EnqueueReportTx(ctx context.Context, tx pgx.Tx, in ReportInput) (bool, error) {
	if in.Report == nil {
		return false, nil
	}
	// 只处理仍归属这个任务的工作项：stale task（例如设计 PR 合并后开始了新轮次）晚到的报告不入队。
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dev_work_items WHERE id=$1 AND task_id=$2)`,
		in.WorkItemID, in.TaskID).Scan(&owned); err != nil {
		return false, fmt.Errorf("核对决策条目的工作项归属：%w", err)
	}
	if !owned {
		return false, nil
	}
	// 短事务里锁定当前配置（FOR SHARE）：没有配置时直接返回，不建意图、不写唤醒。
	cfg, err := store.GetDecisionDocForShare(ctx, tx, in.RepoID)
	if err != nil {
		return false, fmt.Errorf("读取待决策清单配置：%w", err)
	}
	if cfg == nil {
		return false, nil
	}
	sha, err := ReportSHA256(in.Raw)
	if err != nil {
		return false, err
	}
	payloads := entries(&in, time.Now())
	inserted := 0
	for idx := range payloads {
		p := &payloads[idx]
		var category *string
		if p.EntryKind == EntryDecision {
			c := p.Category
			category = &c
		}
		marker := Marker(in.RepoID, in.WorkItemID, in.TaskID, idx)
		tag, err := tx.Exec(ctx, `INSERT INTO dev_decision_syncs (repo_id, work_item_id, task_id, entry_idx,
				target_provider, target_base_url, target_doc_id, target_collection_id, target_generation,
				marker, entry_kind, category, report_sha256, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7::uuid,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (repo_id, task_id, entry_idx) DO NOTHING`,
			in.RepoID, in.WorkItemID, in.TaskID, idx, cfg.Provider, cfg.BaseURL, cfg.DocID, cfg.CollectionID,
			cfg.Generation, marker, string(p.EntryKind), category, sha, store.MustJSON(p))
		if err != nil {
			return false, fmt.Errorf("登记决策条目：%w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	if inserted == 0 {
		return false, nil
	}
	// 与入队同一事务写一条轻量唤醒记录：事务提交后 outbox worker 一定会唤醒投递循环，且处理函数只做非阻塞唤醒，
	// 不占用共用 outbox worker。复用 outbox 共用包，写法与其他领域一致。
	if err := s.outbox.Enqueue(ctx, tx, fmt.Sprintf("decision-inbox:%d", in.RepoID), KindWake, struct{}{}); err != nil {
		return false, fmt.Errorf("写入决策同步的唤醒记录：%w", err)
	}
	return true, nil
}
