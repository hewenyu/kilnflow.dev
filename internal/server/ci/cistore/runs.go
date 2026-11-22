package cistore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Run 是 runs 表中的一次 workflow 运行。
type Run struct {
	ID               int64
	RepoID           int64
	Workflow         string
	WorkflowName     string
	Event            string
	Ref              string
	SHA              string
	Source           string
	PRNumber         *int
	SenderLogin      string
	SenderIsBot      bool
	CurrentRerun     int
	ConcurrencyKey   *string
	CancelInProgress bool
	State            store.RunState
	ConfigError      *string
	Warnings         *string
	GitHubCtx        GitHubCtx
	Inputs           map[string]string
	CommitTime       int64
	Priority         int
	CreatedAt        time.Time
	FinishedAt       *time.Time
	// PurgedAt 是最近一次按保留策略清理过程数据（日志、产物）的时间（PLAN #10.3）；没有清理过时为 nil。
	// 在它之前创建的 job 的过程数据都已经清理（见 Purged、JobPurged）。
	PurgedAt *time.Time
}

// Purged 表示最近一次运行的过程数据已经清理（清理发生在这次运行结束之后）。
func (r *Run) Purged() bool {
	return r.PurgedAt != nil && r.FinishedAt != nil && !r.PurgedAt.Before(*r.FinishedAt)
}

// JobPurged 表示这个 run 中的 job j 的日志和产物已经清理（j 在最近一次清理之前创建）。
func (r *Run) JobPurged(j *Job) bool {
	return r.PurgedAt != nil && j.CreatedAt.Before(*r.PurgedAt)
}

const runCols = `id, repo_id, workflow, workflow_name, event, ref, sha, source, pr_number, sender_login, sender_is_bot,
	current_rerun, concurrency_key, cancel_in_progress, state, config_error, warnings, github_ctx, inputs, commit_time, priority, created_at, finished_at,
	purged_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var gctx, inputs []byte
	err := row.Scan(&r.ID, &r.RepoID, &r.Workflow, &r.WorkflowName, &r.Event, &r.Ref, &r.SHA, &r.Source, &r.PRNumber, &r.SenderLogin, &r.SenderIsBot,
		&r.CurrentRerun, &r.ConcurrencyKey, &r.CancelInProgress, &r.State, &r.ConfigError, &r.Warnings, &gctx, &inputs, &r.CommitTime, &r.Priority, &r.CreatedAt, &r.FinishedAt,
		&r.PurgedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(gctx, &r.GitHubCtx); err != nil {
		return nil, fmt.Errorf("run %d 的 github_ctx 无法解析：%w", r.ID, err)
	}
	if err := json.Unmarshal(inputs, &r.Inputs); err != nil {
		return nil, fmt.Errorf("run %d 的 inputs 无法解析：%w", r.ID, err)
	}
	return &r, nil
}

// GitHubCtx 是 run 的 github 上下文（PLAN #4.7），以 JSON 保存在 runs.github_ctx 中。
type GitHubCtx = workflow.GitHubContext

// GetRun 按 ID 读取 run；lock 为 true 时加行锁（FOR UPDATE）。不存在时返回的错误包装了 pgx.ErrNoRows。
func GetRun(ctx context.Context, q store.Querier, id int64, lock bool) (*Run, error) {
	sql := `SELECT ` + runCols + ` FROM runs WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanRun(q.QueryRow(ctx, sql, id))
	if err != nil {
		return nil, fmt.Errorf("读取 run %d：%w", id, err)
	}
	return r, nil
}

// ListRuns 返回最近的 limit 个 run 及其仓库全名（owner/name），按 ID 倒序。repoID 为 0 时列出全部仓库。
func ListRuns(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*Run, []string, error) {
	return QueryRuns(ctx, q, &RunFilter{RepoID: repoID, Limit: limit})
}

// RunFilter 是 run 列表的筛选条件和游标。零值的字段不参与筛选。
type RunFilter struct {
	// RepoID 只列出这个仓库的 run；RepoLike 只列出全名（owner/name）包含它的仓库的 run（不区分大小写）。
	RepoID   int64
	RepoLike string
	State    store.RunState
	// Workflow 匹配 workflow 文件名（可以省略 .yml / .yaml）或 workflow 的 name。
	Workflow string
	// Ref 匹配完整的 ref，或者分支名、tag 名（例如 main 匹配 refs/heads/main）。
	Ref string
	// Actor 只列出由这个 GitHub 账号发起（推送、开 PR、手动触发）的 run（不区分大小写）。
	Actor string
	// Before 只列出 ID 小于它的 run（下一页，更早）；After 只列出 ID 大于它的 run（上一页，更新）。
	// 两者都为 0 时从最新的 run 开始。
	Before, After int64
	Limit         int
}

// QueryRuns 按筛选条件返回 run 及其仓库全名，结果总是按 ID 倒序。设置了 After 时返回紧挨着 After 的、较新的 Limit 个 run。
func QueryRuns(ctx context.Context, q store.Querier, f *RunFilter) ([]*Run, []string, error) {
	var conds []string
	args := []any{f.Limit}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.RepoID != 0 {
		conds = append(conds, "r.repo_id="+arg(f.RepoID))
	}
	if f.RepoLike != "" {
		conds = append(conds, "strpos(lower(p.owner||'/'||p.name), lower("+arg(f.RepoLike)+")) > 0")
	}
	if f.State != "" {
		conds = append(conds, "r.state="+arg(string(f.State)))
	}
	if f.Workflow != "" {
		n := arg(f.Workflow)
		conds = append(conds, "(r.workflow IN ("+n+", "+n+"||'.yml', "+n+"||'.yaml') OR r.workflow_name="+n+")")
	}
	if f.Ref != "" {
		n := arg(f.Ref)
		conds = append(conds, "r.ref IN ("+n+", 'refs/heads/'||"+n+", 'refs/tags/'||"+n+")")
	}
	if f.Actor != "" {
		conds = append(conds, "lower(r.sender_login)=lower("+arg(f.Actor)+")")
	}
	// 上一页（After）按 ID 升序取紧挨着的 Limit 个，读出后再反转成倒序
	ascending := false
	switch {
	case f.Before > 0:
		conds = append(conds, "r.id < "+arg(f.Before))
	case f.After > 0:
		conds = append(conds, "r.id > "+arg(f.After))
		ascending = true
	}
	order := "DESC"
	if ascending {
		order = "ASC"
	}
	where := "true"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}
	cols := prefixCols(runCols, "r.")
	rows, err := q.Query(ctx, `SELECT `+cols+`, p.owner||'/'||p.name FROM runs r JOIN repos p ON p.id=r.repo_id WHERE `+where+` ORDER BY r.id `+order+` LIMIT $1`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var runs []*Run
	var names []string
	for rows.Next() {
		var name string
		r, err := scanRun(rowWithExtra{rows, &name})
		if err != nil {
			return nil, nil, err
		}
		runs = append(runs, r)
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if ascending {
		slices.Reverse(runs)
		slices.Reverse(names)
	}
	return runs, names, nil
}

// RunJobStates 返回每个 run 最新一次运行（current_rerun）中各 job 的状态，按 job ID 排序（运行列表的烧制条）。
// 没有 job 的 run 不在结果中。
func RunJobStates(ctx context.Context, q store.Querier, runIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT j.run_id, array_agg(j.state ORDER BY j.id) FROM jobs j
		JOIN runs r ON r.id=j.run_id AND j.rerun=r.current_rerun
		WHERE j.run_id = ANY($1) GROUP BY j.run_id`, runIDs)
	if err != nil {
		return nil, err
	}
	var id int64
	var states []string
	_, err = pgx.ForEachRow(rows, []any{&id, &states}, func() error {
		out[id] = slices.Clone(states)
		return nil
	})
	return out, err
}

// RunCounts 返回 since 之后创建的 run 按状态的数量（总览的统计）。
func RunCounts(ctx context.Context, q store.Querier, since time.Time) (map[store.RunState]int, error) {
	rows, err := q.Query(ctx, `SELECT state, count(*) FROM runs WHERE created_at >= $1 GROUP BY state`, since)
	if err != nil {
		return nil, err
	}
	out := map[store.RunState]int{}
	var state string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&state, &n}, func() error {
		out[store.RunState(state)] = n
		return nil
	})
	return out, err
}

// RunWorkflows 返回出现过的 workflow 文件名（最多 limit 个，按名称排序）；repoID 非 0 时只看这个仓库。
func RunWorkflows(ctx context.Context, q store.Querier, repoID int64, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT workflow FROM runs WHERE $1=0 OR repo_id=$1 ORDER BY 1 LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// rowWithExtra 在 scanRun 的字段之后再多扫描一列。
type rowWithExtra struct {
	pgx.Rows
	extra any
}

func (r rowWithExtra) Scan(dest ...any) error { return r.Rows.Scan(append(dest, r.extra)...) }

// prefixCols 给逗号分隔的列名加上表别名。
func prefixCols(cols, p string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = p + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}

// LockConcurrencyGroup 在当前事务中锁住一个并发组（PLAN #6.5），直到事务结束。
// 判断组内是否已有 active 的 run、以及激活排队的 run 都必须在持有这把锁时进行，
// 否则并行的事务可能让同一组出现两个 active 的 run。
func LockConcurrencyGroup(ctx context.Context, tx pgx.Tx, repoID int64, key string) error {
	return store.LockXact(ctx, tx, concurrencyLockKey(repoID, key))
}

// CountActiveInGroup 统计并发组中 active 的 run 数（不含 exclude）。调用方应持有并发组的锁。
// 状态直接写在 SQL 中而不是作为参数传入：部分索引 runs_concurrency 的条件依赖字面量。
func CountActiveInGroup(ctx context.Context, q store.Querier, repoID int64, key string, exclude int64) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repo_id=$1 AND concurrency_key=$2 AND state='active' AND id<>$3`,
		repoID, key, exclude).Scan(&n)
	return n, err
}

func concurrencyLockKey(repoID int64, key string) string {
	return "concurrency:" + strconv.FormatInt(repoID, 10) + ":" + key
}

// 加锁顺序（PLAN #6.1）：所有可能结束 job、推进或结束 run 的事务都按同一顺序加锁，否则并发时会互相等待（死锁）：
//
//  1. 并发组的 advisory lock（LockConcurrencyGroup），按 (repo_id, concurrency_key) 排序；
//  2. run 的行锁，按 ID 升序；
//  3. job 的行锁，按 ID 升序（RerunJobs 加锁时按 ID 排序）。
//
// 结束 run 时要锁并发组（释放并发组、激活排队的 run），推进 run 时要锁 run 的全部 job，所以从 job 出发的路径
// （Runner 提交结果、租约过期、超时、发布结束）必须先用 LockRunsOfJob 锁住并发组和 run，再锁 job。
// 已经持有的锁再次获取不会等待（行锁和事务级 advisory lock 都可重入），所以内层的 AdvanceRun 可以照常调用 LockRuns。

// LockRuns 按加锁顺序锁住 run 所在的并发组和 run 本身（不存在的 ID 忽略）。
func LockRuns(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	type group struct {
		RepoID int64
		Key    string
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT repo_id, concurrency_key FROM runs WHERE id = ANY($1) AND concurrency_key IS NOT NULL
		ORDER BY repo_id, concurrency_key`, ids)
	if err != nil {
		return err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowToStructByPos[group])
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := LockConcurrencyGroup(ctx, tx, g.RepoID, g.Key); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT id FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	return err
}

// LockRunsOfJob 在锁 job 之前按加锁顺序锁住 job 所在的 run，以及跟随它的 job 所在的 run（job 结束后都要推进），
// 返回已锁定的 run ID（升序）。调用方把这份集合交给推进步骤（advanceAfterFinish），推进时就按这份集合在事务内按升序
// 推进，不再在持有 job 行锁之后重新查询、反向锁住动态新增的 follower run（那会违反 cistore.LockRuns 的加锁顺序）。
// 调用锁定之后才挂上的 follower run 由 sweepStuckRuns 兜底推进。
func LockRunsOfJob(ctx context.Context, tx pgx.Tx, jobID int64) ([]int64, error) {
	ids, err := store.QueryIDs(ctx, tx, `SELECT run_id FROM jobs WHERE id=$1 AND run_id IS NOT NULL
		UNION SELECT run_id FROM jobs WHERE follows_job_id=$1 AND run_id IS NOT NULL`, jobID)
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)
	if err := LockRuns(ctx, tx, ids); err != nil {
		return nil, err
	}
	return ids, nil
}
