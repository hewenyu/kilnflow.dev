package cistore

import (
	"context"
	"fmt"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 排队顺序（PLAN #14.2）：pending 的 job 按 (priority DESC, created_at, queued_at, id) 排队，这里是它唯一的定义。候选分页
// （scheduler 的 Cursor）、发布 job 的领取、Dashboard 的调度解释都用它，索引 jobs_pending 的列与它一致。
//   - priority：显式优先级，最先比较；
//   - created_at（提交时间）：job 所属那次提交（run 的首次规划，或一次重跑）的时间。同一次规划、同一次重跑的 job 在同一个事务
//     中插入（planner.insertJobs），created_at 是事务时间 now()，彼此相同，之后不再改变；没有别的路径插入 job。所以后继 job
//     就绪、基础设施重试或撤销预留回到 pending 时仍排在更晚提交的 run 前面；重跑插入新的 job 行，按重跑的时间排队；
//   - queued_at：进入 pending 的时间，同一次提交内部先就绪的先；
//   - id：使顺序成为全序。

// queueTieCols 是同一 priority 内依次比较的列（都是升序）。
const queueTieCols = `created_at, queued_at, id`

// QueueOrderSQL 是排队顺序的 ORDER BY 列表。
const QueueOrderSQL = `priority DESC, ` + queueTieCols

// QueueKey 是一个排队中的 job 在排队顺序中的位置。
type QueueKey struct {
	Priority int
	// Submitted 是提交时间（jobs.created_at）。
	Submitted time.Time
	QueuedAt  time.Time
	ID        int64
}

// QueueKey 返回 job 在排队顺序中的位置；没有 queued_at（没有排过队）时 ok 为 false。
func (j *Job) QueueKey() (k QueueKey, ok bool) {
	if j.QueuedAt == nil {
		return k, false
	}
	return QueueKey{Priority: j.Priority, Submitted: j.CreatedAt, QueuedAt: *j.QueuedAt, ID: j.ID}, true
}

// QueueKeyArgs 是 QueueKey.Args 的参数个数。
const QueueKeyArgs = 4

// Args 返回 QueueAfterSQL、QueueBeforeSQL 的参数（QueueKeyArgs 个）。
func (k QueueKey) Args() []any { return []any{k.Priority, k.Submitted, k.QueuedAt, k.ID} }

// QueueAfterSQL 返回"排在 k 之后"的条件，k 是从 $n 开始的参数（QueueKey.Args）。
func QueueAfterSQL(n int) string { return queueCmpSQL(n, "<", ">") }

// QueueBeforeSQL 返回"排在 k 之前"的条件，参数同 QueueAfterSQL。
func QueueBeforeSQL(n int) string { return queueCmpSQL(n, ">", "<") }

// queueCmpSQL 是 QueueAfterSQL、QueueBeforeSQL 的共同形式：priority 用 prio 比较（降序），其余列整体做行比较（升序）。
func queueCmpSQL(n int, prio, tie string) string {
	return fmt.Sprintf(`(priority %[5]s $%[1]d::int OR (priority = $%[1]d::int AND (`+queueTieCols+`) %[6]s ($%[2]d::timestamptz, $%[3]d::timestamptz, $%[4]d::bigint)))`,
		n, n+1, n+2, n+3, prio, tie)
}

// QueuePosition 是排队中的 job 前面还有多少个待领取的 job（调度解释，PLAN #14.2）。
type QueuePosition struct {
	// Ahead 是排在它前面、与它 platform 和 executor 相同的普通 job 数（不论资源、pool、能力是否匹配同一个 Runner）。
	Ahead int
	// FirstRunID 是其中排在最前面的 job 所属的 run；Ahead 为 0 时为 0。
	FirstRunID int64
}

// QueueAhead 按排队顺序统计排在 j 前面、与它 platform 和 executor 相同的 pending 普通 job。j 不在排队（不是 pending 的普通
// job，或没有 queued_at）时返回 nil。
func QueueAhead(ctx context.Context, q store.Querier, j *Job) (*QueuePosition, error) {
	k, ok := j.QueueKey()
	if !ok || j.State != store.JobPending || j.Kind != workflow.JobKindNormal {
		return nil, nil
	}
	// state='pending' 写成字面量：部分索引 jobs_pending 依赖字面量才能命中。参数依次为 platform、executor、k
	ahead := `FROM jobs WHERE state='pending' AND kind='normal' AND platform=$1 AND executor_kind=$2 AND ` + QueueBeforeSQL(3)
	var p QueuePosition
	var first *int64
	err := q.QueryRow(ctx, `SELECT count(*), (SELECT run_id `+ahead+` ORDER BY `+QueueOrderSQL+` LIMIT 1) `+ahead,
		append([]any{j.Platform, j.Executor}, k.Args()...)...).Scan(&p.Ahead, &first)
	if err != nil {
		return nil, fmt.Errorf("统计 job %d 前面的排队：%w", j.ID, err)
	}
	if first != nil {
		p.FirstRunID = *first
	}
	return &p, nil
}
