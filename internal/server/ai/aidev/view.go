package aidev

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 查看工作项（Dashboard、CLI 共用）：工作项、它的 Issue、选题和交付报告（附适用范围）、评论意图的状态、attempt 分支。

// View 是列表中的一个工作项。
type View struct {
	*WorkItem
	Repo   string      `json:"repo"`
	Issues []WorkIssue `json:"issues"`
}

// Report 是一份选题或交付报告和它的适用范围（PLAN #26.14：Runner / Agent 版本、模型，拿不到时为空，显示为"未知"）。
type Report struct {
	TaskID        int64           `json:"task_id"`
	Kind          string          `json:"kind"`
	Report        json.RawMessage `json:"report"`
	RunnerVersion string          `json:"runner_version"`
	AgentVersion  string          `json:"agent_version"`
	Model         string          `json:"model"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Comment 是一条评论意图和它的状态（C19：unknown 表示结果不明，不自动重发）。
type Comment struct {
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Error     string    `json:"error"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Branch 是一个 attempt 分支（交付或 checkpoint，保留时间到了删除）。
type Branch struct {
	Branch    string     `json:"branch"`
	SHA       string     `json:"sha"`
	Kind      string     `json:"kind"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
}

// Detail 是一个工作项的全部记录。
type Detail struct {
	View
	Reports  []Report  `json:"reports"`
	Comments []Comment `json:"comments"`
	Branches []Branch  `json:"branches"`
}

// ListViews 返回工作项（repoID 为 0 时是全部仓库），新的在前，最多 limit 个。分页在主实体层（一次查询主表 JOIN repos 取仓库名），
// 再用一次批量查询取所有工作项的 Issue 并分组，避免按工作项逐个查询（N+1）。
func ListViews(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*View, error) {
	rows, err := q.Query(ctx, `SELECT `+workItemCols+`, (SELECT owner || '/' || name FROM repos WHERE id=repo_id)
		FROM dev_work_items WHERE ($1=0 OR repo_id=$1) ORDER BY id DESC LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*View, error) {
		w := &WorkItem{}
		v := &View{WorkItem: w}
		return v, r.Scan(append(workItemDests(w), &v.Repo)...)
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]int64, len(out))
	byID := make(map[int64]*View, len(out))
	for i, v := range out {
		ids[i] = v.ID
		byID[v.ID] = v
	}
	irows, err := q.Query(ctx, `SELECT work_item_id, issue_number, title, digest, active, closed, resolution
		FROM dev_work_item_issues WHERE work_item_id = ANY($1) ORDER BY work_item_id, issue_number`, ids)
	if err != nil {
		return nil, err
	}
	var wiID int64
	var issue WorkIssue
	if _, err := pgx.ForEachRow(irows, []any{&wiID, &issue.Number, &issue.Title, &issue.Digest, &issue.Active, &issue.Closed, &issue.Resolution}, func() error {
		if v := byID[wiID]; v != nil {
			v.Issues = append(v.Issues, issue)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func viewOf(ctx context.Context, q store.Querier, w *WorkItem) (*View, error) {
	v := &View{WorkItem: w}
	if err := q.QueryRow(ctx, `SELECT owner || '/' || name FROM repos WHERE id=$1`, w.RepoID).Scan(&v.Repo); err != nil {
		return nil, err
	}
	var err error
	v.Issues, err = workIssues(ctx, q, w.ID)
	return v, err
}

// GetDetail 返回工作项 id 的全部记录；不存在时返回 ErrNotFound。
func GetDetail(ctx context.Context, q store.Querier, id int64) (*Detail, error) {
	w, err := GetWorkItem(ctx, q, id)
	if err != nil {
		return nil, err
	}
	v, err := viewOf(ctx, q, w)
	if err != nil {
		return nil, err
	}
	d := &Detail{View: *v}
	rows, err := q.Query(ctx, `SELECT task_id, kind, report, runner_version, agent_version, model, created_at FROM dev_reports
		WHERE work_item_id=$1 ORDER BY created_at, kind DESC`, id)
	if err != nil {
		return nil, err
	}
	if d.Reports, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Report]); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT number, kind, state, error, updated_at FROM dev_comments WHERE work_item_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	if d.Comments, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Comment]); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT branch, sha, kind, created_at, deleted_at FROM dev_branches WHERE work_item_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	d.Branches, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Branch])
	return d, err
}

// TaskReports 返回 AI 开发任务保存的报告（选题、交付）和它关联的工作项（没有时为 nil）。没有领取的选题（没有选中、
// dry_run、领取失败）不属于任何工作项，报告只能按任务查看。
func TaskReports(ctx context.Context, q store.Querier, taskID int64) (reports []Report, workItem *int64, err error) {
	rows, err := q.Query(ctx, `SELECT task_id, kind, report, runner_version, agent_version, model, created_at FROM dev_reports
		WHERE task_id=$1 ORDER BY created_at, kind DESC`, taskID)
	if err != nil {
		return nil, nil, err
	}
	if reports, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Report]); err != nil {
		return nil, nil, err
	}
	err = q.QueryRow(ctx, `SELECT max(work_item_id) FROM dev_reports WHERE task_id=$1`, taskID).Scan(&workItem)
	return reports, workItem, err
}

// Paused 返回仓库是否暂停了 AI 开发。
func Paused(ctx context.Context, q store.Querier, repoID int64) (bool, error) {
	st, err := readRepoState(ctx, q, repoID)
	if err != nil {
		return false, err
	}
	return st.paused, nil
}
