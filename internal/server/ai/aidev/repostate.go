package aidev

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 仓库的 AI 开发状态（ai_repo_state，PLAN #30.4）：暂停、选题轮次、交给选题的任务池状态、任务池变化和同步的时间、读到的
// 开发 Agent 和平台。没有行时都是默认值；写入时按需建行（INSERT … ON CONFLICT），只建在存在的仓库上。

// repoState 是一个仓库的 AI 开发状态。
type repoState struct {
	paused   bool
	seq      int
	examined string
	agent    string
	// platforms 是读到的开发平台（Runner 平台）；还没读过时为 nil。
	platforms []string
	syncedAt  *time.Time
}

// readRepoState 读取仓库的 AI 开发状态；仓库不存在时返回 ErrRepoNotFound。
func readRepoState(ctx context.Context, q store.Querier, repoID int64) (*repoState, error) {
	var st repoState
	err := q.QueryRow(ctx, `SELECT coalesce(s.dev_paused, false), coalesce(s.dev_select_seq, 0), coalesce(s.dev_examined, ''),
		coalesce(s.dev_agent, ''), s.dev_platforms, s.dev_synced_at FROM repos r LEFT JOIN ai_repo_state s ON s.repo_id=r.id WHERE r.id=$1`, repoID).
		Scan(&st.paused, &st.seq, &st.examined, &st.agent, &st.platforms, &st.syncedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRepoNotFound
	}
	return &st, err
}

// readRepoStates 批量读取多个仓库的 AI 开发状态，返回 id → 状态（不存在的仓库不在结果中）。供一次处理多个仓库时避免逐个查询。
func readRepoStates(ctx context.Context, q store.Querier, repoIDs []int64) (map[int64]*repoState, error) {
	out := make(map[int64]*repoState, len(repoIDs))
	if len(repoIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT r.id, coalesce(s.dev_paused, false), coalesce(s.dev_select_seq, 0), coalesce(s.dev_examined, ''),
		coalesce(s.dev_agent, ''), s.dev_platforms, s.dev_synced_at FROM repos r LEFT JOIN ai_repo_state s ON s.repo_id=r.id
		WHERE r.id = ANY($1)`, repoIDs)
	if err != nil {
		return nil, err
	}
	var id int64
	var st repoState
	if _, err := pgx.ForEachRow(rows, []any{&id, &st.paused, &st.seq, &st.examined, &st.agent, &st.platforms, &st.syncedAt}, func() error {
		s := st
		out[id] = &s
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// setExamined 记下交给选题的任务池状态（没有变化就不再选题）。
func setExamined(ctx context.Context, q store.Execer, repoID int64, fingerprint string) error {
	_, err := q.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_examined) VALUES ($1,$2)
		ON CONFLICT (repo_id) DO UPDATE SET dev_examined=EXCLUDED.dev_examined`, repoID, fingerprint)
	return err
}

// setDevTarget 记下从默认分支读到的开发 Agent 和平台（development.platform 的 Runner 平台）。
func setDevTarget(ctx context.Context, q store.Execer, repoID int64, agent string, platforms []string) error {
	_, err := q.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_agent, dev_platforms) VALUES ($1,$2,$3)
		ON CONFLICT (repo_id) DO UPDATE SET dev_agent=EXCLUDED.dev_agent, dev_platforms=EXCLUDED.dev_platforms`, repoID, agent, platforms)
	return err
}

// setPaused 暂停或恢复仓库的 AI 开发；仓库不存在时返回 ErrRepoNotFound。
func setPaused(ctx context.Context, q store.Execer, repoID int64, paused bool) error {
	tag, err := q.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_paused) SELECT id, $2 FROM repos WHERE id=$1
		ON CONFLICT (repo_id) DO UPDATE SET dev_paused=EXCLUDED.dev_paused`, repoID, paused)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRepoNotFound
	}
	return nil
}
