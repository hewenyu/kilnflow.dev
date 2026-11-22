package aidev

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 仓库级串行（PLAN #26.9 I14、C23）：一个仓库按完整的工作项串行——从领取到合并或终止（包括等待 CI、审计、合并，返修，
// 受阻，设计合并后的实现，部分完成后的下一轮）都占住仓库。占用者是仓库中未结束的工作项中编号最小的那个；只有占用者
// 能开始新的阶段或轮次，仓库中有未结束的工作项时不建新的工作项。规则只有这一个定义（Occupancy），选题门槛、领取事务、
// 实现规划和展示都用它。正常运行时仓库中至多一个未结束的工作项；升级前留下的多个工作项按编号依次成为占用者
// （#26.9 升级收敛）：其余的已有 PR 照常验证、返修、合并，只是不开始新的阶段或轮次。
//
// 占用不等于占着执行槽位：等待 CI、审计、合并、受阻的工作项没有任务（task_id 为空），开发机的空闲槽位按执行中和
// 排队中的任务计算（freeDevSlots），可以去做别的仓库；独立审计按审计的调度执行，不受影响。

// Occupancy 是仓库的 AI 开发占用。Occupant 为 nil 表示没有被占用（可以选题）；Waiting 是排在占用者之后的未结束的
// 工作项（升级前留下的，按编号）。
type Occupancy struct {
	Occupant *WorkItem `json:"occupant"`
	Waiting  []int64   `json:"waiting"`
}

// occupancy 返回仓库 repoID 的占用。
func occupancy(ctx context.Context, q store.Querier, repoID int64) (*Occupancy, error) {
	list, err := queryWorkItems(ctx, q, `repo_id=$1 AND active`, repoID)
	if err != nil || len(list) == 0 {
		return &Occupancy{}, err
	}
	o := &Occupancy{Occupant: list[0], Waiting: []int64{}}
	for _, w := range list[1:] {
		o.Waiting = append(o.Waiting, w.ID)
	}
	return o, nil
}

// RepoOccupancy 返回仓库的 AI 开发占用（Dashboard、CLI 展示）。
func RepoOccupancy(ctx context.Context, q store.Querier, repoID int64) (*Occupancy, error) {
	return occupancy(ctx, q, repoID)
}

// Why 返回仓库被占用、不能开始新的工作项的原因（没有被占用时为空）：占用者、它的阶段和状态、它在等什么。
func (o *Occupancy) Why() string {
	w := o.Occupant
	if w == nil {
		return ""
	}
	why := fmt.Sprintf("工作项 #%d 进行中（%s / %s", w.ID, w.Phase, w.State)
	if w.Reason != "" {
		why += "：" + w.Reason
	}
	why += "）；同一个仓库按工作项串行，它合并或终止之后才开始下一项"
	if len(o.Waiting) > 0 {
		why += "；之后依次是 " + workItemRefs(o.Waiting)
	}
	return why
}

// waitingReason 返回不是占用者的工作项 w 不开始新的阶段或轮次时的原因。
func (o *Occupancy) waitingReason() string {
	return fmt.Sprintf("等工作项 #%d 结束后开始实现（同一个仓库按工作项串行）", o.Occupant.ID)
}

// workItemRefs 返回工作项编号的列表（"#1 #2"）。
func workItemRefs(ids []int64) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "#"+strconv.FormatInt(id, 10))
	}
	return strings.Join(out, " ")
}

// claimLockKey 是仓库领取的事务级 advisory lock 的键：领取事务在它下面判断仓库是否被占用，并发的领取只有一个能建出
// 工作项（I14）。只有领取取这把锁，不与其他路径的锁交错。
func claimLockKey(repoID int64) string { return fmt.Sprintf("kiln:aidev:claim:%d", repoID) }

// lockClaim 在事务 tx 中取仓库 repoID 的领取锁（事务结束时释放），返回之后读到的占用。
func lockClaim(ctx context.Context, tx pgx.Tx, repoID int64) (*Occupancy, error) {
	if err := store.LockXact(ctx, tx, claimLockKey(repoID)); err != nil {
		return nil, err
	}
	return occupancy(ctx, tx, repoID)
}
