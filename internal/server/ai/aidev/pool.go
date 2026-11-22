package aidev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 任务池（PLAN #26.11）：有 kiln:ready 的打开的 Issue 在 Server 中的镜像。人在 GitHub 上打、去 label，issues webhook 逐个
// 到达时更新任务池；webhook 可能丢失，Dashboard 的"同步"按钮（kiln ai dev sync）从 GitHub 全量同步一次。选题的候选
// 从任务池中取，不在规划请求时列出：Runner 准备好后才向 Server 要（越晚确定越完整，批量打 label 时不会只看到一部分）。

const (
	// candidateLimit 是一次选题最多交给 Agent 的候选数（先排除不可选的，再按进入任务池的先后取）。
	candidateLimit = 50
	// syncLimit 是手动同步最多读取的条数（GitHub 的列表中含 PR）。
	syncLimit = 1000
	// issueBodyLimit 是候选快照中 Issue 正文的长度上限（字节，按字符截断）。
	issueBodyLimit = 16 << 10
	// poolSettle 是任务池静默多久之后才给出候选：在 GitHub 上一次勾选多个 Issue 打 label 时 webhook 陆续到达，
	// 等它们到齐（Runner 准备 clone 和测试服务的时间通常已经够了，这里兜底）。
	poolSettle = 20 * time.Second
	// maxSettleWait 是一次选题最多等任务池静默的时间（任务池一直在变时不无限等下去）。
	maxSettleWait = 2 * time.Minute
)

// poolIssue 是任务池中的一行。
type poolIssue struct {
	InPool    bool
	Digest    string
	Labels    []string
	UpdatedAt time.Time
}

// mirror 按 Issue 的当前内容更新任务池（在事务 tx 中）：打开且有 kiln:ready 的在任务池中，否则不在。只接受不比已记录的
// 更旧的内容（webhook 可能乱序到达；同一秒内的按到达顺序）：更旧时 stale 为真，什么也不改。changed 是会影响选题的变化
// （进出任务池、内容、kiln:skip）。
func mirror(ctx context.Context, tx pgx.Tx, repoID int64, is *forge.IssueContent) (changed, stale bool, err error) {
	in := is.Open && is.HasLabel(LabelReady)
	var old poolIssue
	err = tx.QueryRow(ctx, `SELECT in_pool, digest, labels, issue_updated_at FROM dev_pool WHERE repo_id=$1 AND issue_number=$2 FOR UPDATE`,
		repoID, is.Number).Scan(&old.InPool, &old.Digest, &old.Labels, &old.UpdatedAt)
	exists := err == nil
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, false, err
	case is.UpdatedAt.Before(old.UpdatedAt):
		return false, true, nil
	}
	if !exists && !in {
		return false, false, nil
	}
	digest := IssueDigest(is.Title, is.Body)
	labels := labelsOf(is)
	var rows int64
	if in {
		// ON CONFLICT 的 WHERE 保证版本单调：并发事务先插入了更新的内容时，这一条不覆盖它（RowsAffected 为 0，按 stale 处理）。
		tag, e := tx.Exec(ctx, `INSERT INTO dev_pool (repo_id, issue_number, in_pool, title, body, labels, url, digest, issue_updated_at)
			VALUES ($1,$2,true,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (repo_id, issue_number) DO UPDATE SET in_pool=true, title=EXCLUDED.title, body=EXCLUDED.body, labels=EXCLUDED.labels,
			url=EXCLUDED.url, digest=EXCLUDED.digest, issue_updated_at=EXCLUDED.issue_updated_at, updated_at=now(),
			ready_at=CASE WHEN dev_pool.in_pool THEN dev_pool.ready_at ELSE now() END
			WHERE dev_pool.issue_updated_at <= EXCLUDED.issue_updated_at`,
			repoID, is.Number, is.Title, cutText(is.Body, issueBodyLimit), labels, is.URL, digest, is.UpdatedAt)
		if e != nil {
			return false, false, e
		}
		rows = tag.RowsAffected()
	} else {
		tag, e := tx.Exec(ctx, `UPDATE dev_pool SET in_pool=false, labels=$3, issue_updated_at=$4, updated_at=now()
			WHERE repo_id=$1 AND issue_number=$2 AND issue_updated_at <= $4`,
			repoID, is.Number, labels, is.UpdatedAt)
		if e != nil {
			return false, false, e
		}
		rows = tag.RowsAffected()
	}
	if rows == 0 {
		// 没有写入：另一个事务已经写入了更新的内容（版本比这次的新）
		return false, true, nil
	}
	changed = !exists || old.InPool != in || (in && (old.Digest != digest || slices.Contains(old.Labels, LabelSkip) != is.HasLabel(LabelSkip)))
	return changed, false, nil
}

func labelsOf(is *forge.IssueContent) []string {
	if is.Labels == nil {
		return []string{}
	}
	return is.Labels
}

// poolChanged 记下任务池变化的时间（给出候选前等它静默）。调用方必须在全部 dev_pool 行更新完成之后再调用一次，
// 让所有路径都严格遵守 dev_pool（按 issue_number 升序）-> ai_repo_state 的锁序（与领取的 claimTx / reoffer 一致）。
func poolChanged(ctx context.Context, tx pgx.Tx, repoID int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_pool_changed_at) VALUES ($1, now())
		ON CONFLICT (repo_id) DO UPDATE SET dev_pool_changed_at=EXCLUDED.dev_pool_changed_at`, repoID)
	return err
}

// SyncResult 是一次手动同步的结果。
type SyncResult struct {
	// Listed 是 GitHub 上打开的、有 kiln:ready 的 Issue 数；Complete 为假表示超过 syncLimit 没有读完（没读到的不移出任务池）。
	Listed   int
	Complete bool
	// Changed 是任务池中有变化（加入、移出、内容变了）的 Issue 数。
	Changed int
	// Candidates 是同步后可选的 Issue 数。
	Candidates int
}

// SyncPool 从 GitHub 全量同步仓库的任务池（PLAN #26.11，显式操作）：列出全部打开的、有 kiln:ready 的 Issue，加入或更新；
// 任务池中有、GitHub 上已经没有的移出（只移出同步开始之前的内容：同步期间到达的 webhook 更新）。同步后清除"已经选过"
// 的记录，任务池没有变化也重新判断一次（例如刚开启开发）。
func (s *Service) SyncPool(ctx context.Context, repoID int64, actor string) (*SyncResult, error) {
	if s.forge == nil {
		return nil, errors.New(whyNoForge)
	}
	repo, err := store.GetRepo(ctx, s.db, repoID)
	if err != nil {
		return nil, err
	}
	var started time.Time
	if err := s.db.QueryRow(ctx, `SELECT now()`).Scan(&started); err != nil {
		return nil, err
	}
	list, complete, err := s.forge(repo).ListIssues(ctx, LabelReady, syncLimit)
	if err != nil {
		return nil, fmt.Errorf("列出 %s 的 Issue：%w", LabelReady, err)
	}
	res := &SyncResult{Listed: len(list), Complete: complete}
	// 按编号依次锁任务池的行（与领取的 checkPool 相同的加锁顺序，不会互相等待）
	slices.SortFunc(list, func(a, b forge.IssueContent) int { return a.Number - b.Number })
	var changed, candidates int
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		// 先用任务池的独立锁串行化更新 dev_pool 的入口，再按编号依次锁任务池的行；ai_repo_state 放在全部 dev_pool
		// 更新之后统一写，保持 dev_pool -> ai_repo_state 的锁序，消除跨表锁序倒置的死锁。
		if err := store.LockXact(ctx, tx, poolLockKey(repoID)); err != nil {
			return err
		}
		// 每次尝试（store.DB.Tx 遇到可重试的错误会重来）都从零计数：上一次尝试累计的不能留下。
		changed, candidates = 0, 0
		listed := make([]int, 0, len(list))
		for i := range list {
			listed = append(listed, list[i].Number)
			moved, _, err := mirror(ctx, tx, repoID, &list[i])
			if err != nil {
				return err
			}
			if moved {
				changed++
			}
		}
		if complete {
			tag, err := tx.Exec(ctx, `UPDATE dev_pool SET in_pool=false, updated_at=now()
				WHERE repo_id=$1 AND in_pool AND NOT (issue_number = ANY($2)) AND issue_updated_at < $3`, repoID, listed, started)
			if err != nil {
				return err
			}
			changed += int(tag.RowsAffected())
		}
		// 全部 dev_pool 更新完成之后才写 ai_repo_state，统一锁序。
		if changed > 0 {
			if err := poolChanged(ctx, tx, repoID); err != nil {
				return err
			}
		}
		if err := markWaitsUnchecked(ctx, tx, repoID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_synced_at) VALUES ($1, now())
			ON CONFLICT (repo_id) DO UPDATE SET dev_synced_at=EXCLUDED.dev_synced_at, dev_examined='', dev_agent='', dev_platforms=NULL`, repoID); err != nil {
			return err
		}
		st, err := readPool(ctx, tx, repoID)
		if err != nil {
			return err
		}
		candidates = len(st.candidates)
		if err := store.Audit(ctx, tx, actor, "aidev.sync", map[string]any{
			"repo_id": repoID, "listed": res.Listed, "complete": complete, "changed": changed,
		}); err != nil {
			return err
		}
		return s.Enqueue(ctx, tx, repoID)
	})
	if err != nil {
		return nil, err
	}
	// 只在事务成功提交后写回结果（重试时 counts 已在闭包内重新开始）。
	res.Changed, res.Candidates = changed, candidates
	s.log.Info("AI 开发：已同步任务池", "repo", repo.FullName(), "listed", res.Listed, "complete", complete, "changed", res.Changed, "actor", actor)
	s.notify()
	return res, nil
}

// poolState 是仓库任务池的当前状态：可选的 Issue（最多 candidateLimit 个）和未结束的工作项。
type poolState struct {
	candidates []aispec.DevIssue
	// workItems 是未结束的工作项（按编号）。
	workItems []int64
}

// fingerprint 是任务池状态的摘要：可选的 Issue（编号和内容）、未结束的工作项。交给选题时记下（repos.ai_dev_examined），
// 没有变化就不再选题——Agent 全部没选、dry_run 时不会反复选同一批；有工作项结束（可能解开了"与进行中的工作项冲突"）、
// 有 Issue 进出任务池或内容变了时重新选题。
func (p *poolState) fingerprint() string {
	h := sha256.New()
	for _, c := range p.candidates {
		fmt.Fprintf(h, "issue %d %s\n", c.Number, c.Digest)
	}
	for _, id := range p.workItems {
		fmt.Fprintf(h, "work %d\n", id)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// readPool 读取仓库任务池的当前状态。可选的 Issue：在任务池中、没有 kiln:skip、不在未结束的工作项中、不在跳过记录中
// （或者内容已经变了），按进入任务池的先后。先排除再取前 candidateLimit 个：排除的 Issue 再多也不会挡住后面的。
func readPool(ctx context.Context, q store.Querier, repoID int64) (*poolState, error) {
	rows, err := q.Query(ctx, `SELECT p.issue_number, p.title, p.body, p.labels, p.digest, p.url FROM dev_pool p
		WHERE p.repo_id=$1 AND p.in_pool AND NOT ($2 = ANY(p.labels))
		  AND NOT EXISTS (SELECT 1 FROM dev_work_item_issues i WHERE i.repo_id=p.repo_id AND i.issue_number=p.issue_number AND i.active)
		  AND NOT EXISTS (SELECT 1 FROM dev_issue_skips k WHERE k.repo_id=p.repo_id AND k.issue_number=p.issue_number AND k.digest=p.digest)
		ORDER BY p.ready_at, p.issue_number LIMIT $3`, repoID, LabelSkip, candidateLimit)
	if err != nil {
		return nil, err
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (aispec.DevIssue, error) {
		var d aispec.DevIssue
		err := r.Scan(&d.Number, &d.Title, &d.Body, &d.Labels, &d.Digest, &d.URL)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	ids, err := store.QueryIDs(ctx, q, `SELECT id FROM dev_work_items WHERE repo_id=$1 AND active ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	return &poolState{candidates: cands, workItems: ids}, nil
}

// PoolIssue 是任务池中的一个 Issue（Dashboard 展示）。
type PoolIssue struct {
	Number int
	Title  string
	URL    string
	// Status 是 Issue 现在的情况：candidate（可选）、claimed（在未结束的工作项中）、skip_label（有 kiln:skip）、
	// waiting（等待依赖的 Issue 关闭，PLAN #31.6）、skipped（跳过记录：未选、无需改代码、被终止，内容不变就不再选）。
	Status   string
	WorkItem *int64
	Reason   string
	// WaitingOn 是还没关闭的依赖（waiting 时）。
	WaitingOn []int
	ReadyAt   time.Time
}

// 任务池中 Issue 的情况（PoolIssue.Status）。
const (
	PoolCandidate = "candidate"
	PoolClaimed   = "claimed"
	PoolSkipLabel = "skip_label"
	PoolWaiting   = "waiting"
	PoolSkipped   = "skipped"
)

// PoolView 是仓库的任务池（Dashboard 展示）。
type PoolView struct {
	Issues []PoolIssue
	// SyncedAt 是上次全量同步的时间，没有同步过时为 nil。
	SyncedAt *time.Time
}

// poolViewLimit 是 Dashboard 展示的任务池 Issue 数上限。
const poolViewLimit = 200

// GetPool 返回仓库的任务池（在任务池中的 Issue，按进入的先后）。
func GetPool(ctx context.Context, q store.Querier, repoID int64) (*PoolView, error) {
	var v PoolView
	st, err := readRepoState(ctx, q, repoID)
	if err != nil {
		return nil, err
	}
	v.SyncedAt = st.syncedAt
	rows, err := q.Query(ctx, `SELECT p.issue_number, p.title, p.url, p.ready_at, $2 = ANY(p.labels),
		(SELECT i.work_item_id FROM dev_work_item_issues i WHERE i.repo_id=p.repo_id AND i.issue_number=p.issue_number AND i.active),
		coalesce(k.reason, ''), coalesce(k.waiting_on, '{}')
		FROM dev_pool p LEFT JOIN dev_issue_skips k ON k.repo_id=p.repo_id AND k.issue_number=p.issue_number AND k.digest=p.digest
		WHERE p.repo_id=$1 AND p.in_pool ORDER BY p.ready_at, p.issue_number LIMIT $3`, repoID, LabelSkip, poolViewLimit)
	if err != nil {
		return nil, err
	}
	v.Issues, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PoolIssue, error) {
		var p PoolIssue
		var skipLabel bool
		if err := r.Scan(&p.Number, &p.Title, &p.URL, &p.ReadyAt, &skipLabel, &p.WorkItem, &p.Reason, &p.WaitingOn); err != nil {
			return p, err
		}
		switch {
		case p.WorkItem != nil:
			p.Status = PoolClaimed
		case skipLabel:
			p.Status = PoolSkipLabel
		case len(p.WaitingOn) > 0:
			p.Status = PoolWaiting
			p.Reason = waitingNote(p.WaitingOn) + "：" + p.Reason
		case p.Reason != "":
			p.Status = PoolSkipped
		default:
			p.Status = PoolCandidate
		}
		return p, nil
	})
	return &v, err
}

// InProgress 是一个未结束的工作项（选题时交给 Agent，避开会改到同一处的 Issue）。
type InProgress struct {
	ID     int64
	State  string
	Issues []aispec.DevIssue
	Plan   string
}

// inProgress 返回仓库未结束的工作项。
func inProgress(ctx context.Context, q store.Querier, repoID int64) ([]InProgress, error) {
	list, err := queryWorkItems(ctx, q, `repo_id=$1 AND active`, repoID)
	if err != nil {
		return nil, err
	}
	out := make([]InProgress, 0, len(list))
	for _, w := range list {
		is, err := workIssues(ctx, q, w.ID)
		if err != nil {
			return nil, err
		}
		ip := InProgress{ID: w.ID, State: w.State, Plan: w.Plan}
		for _, i := range is {
			ip.Issues = append(ip.Issues, aispec.DevIssue{Number: i.Number, Title: i.Title})
		}
		out = append(out, ip)
	}
	return out, nil
}

// selectKey 返回仓库第 seq 轮选题的请求去重键。
func selectKey(repoID int64, seq int) string {
	return dedupePrefix + ":" + strconv.FormatInt(repoID, 10) + ":select:" + strconv.Itoa(seq)
}

// endTurn 结束仓库第 seq 轮选题（领取成功、没有领取、选题请求在领取前结束）：下一台空闲的开发机可以开始下一轮。
// 只在轮次仍是 seq 时前进（重复调用、晚到的旧请求不会多跳一轮）。
func endTurn(ctx context.Context, q store.Execer, repoID int64, seq int) error {
	// 没有行时轮次是 0（readRepoState），插入下一轮；有行时只在轮次仍是 seq 时前进
	_, err := q.Exec(ctx, `INSERT INTO ai_repo_state (repo_id, dev_select_seq) VALUES ($1, $2+1)
		ON CONFLICT (repo_id) DO UPDATE SET dev_select_seq=ai_repo_state.dev_select_seq+1 WHERE ai_repo_state.dev_select_seq=$2`, repoID, seq)
	return err
}
