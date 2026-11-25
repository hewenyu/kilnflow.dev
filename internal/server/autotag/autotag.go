// Package autotag 实现自动打 tag（PLAN #29.4）：经过 PR 合入默认分支的 commit 由 Server 按 .kiln/merge.yml
// 的 tag: 打 tag，并直接规划这个 tag 的 push 事件（按 release.yml 构建和发布）。
//
// 记录（auto_tags）由合并队列（与合并结果同一事务）和 pull_request.closed webhook 写入（mqevents.RecordAutoTag）；
// 每个仓库只有一个执行者（Process，持有仓库级的 advisory lock），按记录的先后逐个处理。
//
// 不变量（PLAN #29.2）：一个 commit 最多一个 tag、一个 tag 名最多一个 commit（T1，auto_tags 的唯一约束）；打了就发布
// （T2，状态 tagged → planned 可以续做，规划按去重键幂等）；版本顺序等于提交顺序（T3，已有的最大版本 tag 所在的 commit
// 必须是这次 commit 的祖先）。创建 tag 之前先把 tag 名写进记录（tagging）：之后按名字确认，不依赖可能滞后的 tag 列表。
package autotag

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// auto_tags.state 的取值。
const (
	StatePending = "pending"
	StateTagging = "tagging"
	StateTagged  = "tagged"
	StatePlanned = "planned"
	StateSkipped = "skipped"
	StateFailed  = "failed"
)

// SkipCodeBranchNotRelease 是 auto_tags.skip_code 的取值：合并进的不是发版分支，不自动打 tag；这样的记录不在 Dashboard
// 的最近记录中显示（高频的开发分支合并不把正式版本挤出列表）。
const SkipCodeBranchNotRelease = mqevents.SkipCodeBranchNotRelease

// auditActor 是审计日志中自动打 tag 的操作者。
const auditActor = "autotag"

// maxSteps 是处理一条记录最多的状态转换次数（tag 名反复被别人占用时防止死循环）。
const maxSteps = 10

// maxReasonLen 是原因保存的上限。
const maxReasonLen = 1000

// sweepInterval 是兜底检查的间隔：有未完成记录的仓库每隔这么久处理一次（outbox 的重试之外）。
const sweepInterval = 10 * time.Minute

// inactiveReason 是仓库停用时放弃 pending 记录的原因。
var inactiveReason = Note{Code: ReasonRepoInactive}

// ErrBusy 表示另一次处理正在进行。
var ErrBusy = errors.New("自动打 tag 正在处理这个仓库，稍后重试")

// TagPush 是打出 tag 后要规划的 push 事件。
type TagPush struct {
	Repo     *store.Repo
	Tag, SHA string
	// Sender 是发布的真实发起者；SenderIsBot 表示它是机器人（AI 开发的可信实现 PR 由 Kiln 的 Bot 合并时仍是 true）。
	Sender      string
	SenderIsBot bool
}

// OriginKind 是一个 PR 的来源类别（DevOrigin.Kind）。由 AI 开发决定（OriginResolver），自动打 tag 自己做裁决，不依赖
// AI 领域的类型（PLAN #30.3）。
type OriginKind int

const (
	// OriginUnknown：没有可信的 AI 开发工作项归属。
	OriginUnknown OriginKind = iota
	// OriginImplementation：可信的 AI 开发实现 PR，机器人发起时也可以自动发布。
	OriginImplementation
	// OriginDesign：AI 开发工作项的设计 PR，不打版本 tag。
	OriginDesign
	// OriginRecording：工作项的 PR 创建归属还没有确认，等确认后重试。
	OriginRecording
	// OriginDenied：有归属但状态、证据或分支不满足自动发布。
	OriginDenied
)

// DevOrigin 是一个 PR 的来源及其依据（PLAN #29.4）。
type DevOrigin struct {
	Kind           OriginKind
	WorkItemID     int64
	Phase          string
	ImplRound      int
	BaseRef        string
	CreatorLogin   string
	DeliveryTaskID int64
	Issues         []int
	// Reason 是不自动发布的中文说明；ReasonCode 是同一个原因的原因码（ReasonOrigin*、ReasonDesignPR，由组装层从 AI 开发的
	// 原因码换过来；为空表示没有或者不认识）。
	Reason     string
	ReasonCode string
}

// OriginResolver 判断一个 PR 的来源（AI 开发的实现，PLAN #29.4、#30.3）；必须使用调用方传入的 q。
type OriginResolver interface {
	Resolve(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (DevOrigin, error)
}

// Planner 规划 tag 的 push 事件，返回建出的 run（组装层用 CI 的规划器实现，PLAN #30.3）。重复规划按去重键不会建出第二个 run。
type Planner interface {
	PlanTagPush(ctx context.Context, ev TagPush) ([]int64, error)
}

// ForgeFunc 返回仓库的平台接口。
type ForgeFunc func(repo *store.Repo) forge.Repo

// Deps 是 Service 的依赖。
type Deps struct {
	DB *store.DB
	// Forge 为 nil（没有配置 GitHub App）时不处理。
	Forge   ForgeFunc
	Planner Planner
	Outbox  *outbox.Writer
	Hub     *notify.Hub
	Log     *slog.Logger
	// DevOrigin 判断 PR 是否来自可信的 AI 开发实现工作项（PLAN #30.3，由组装层用 aidev 实现）；为 nil 时视为未知来源。
	DevOrigin OriginResolver
}

// Service 是自动打 tag 的执行者。
type Service struct {
	db        *store.DB
	forge     ForgeFunc
	planner   Planner
	outbox    *outbox.Writer
	hub       *notify.Hub
	log       *slog.Logger
	devOrigin OriginResolver
}

// New 返回 Service。
func New(d Deps) *Service {
	return &Service{db: d.DB, forge: d.Forge, planner: d.Planner, outbox: d.Outbox, hub: d.Hub, log: d.Log, devOrigin: d.DevOrigin}
}

// Record 是 auto_tags 的一行。
type Record struct {
	ID          int64    `json:"id"`
	RepoID      int64    `json:"repo_id"`
	SHA         string   `json:"sha"`
	PR          int      `json:"pr"`
	Labels      []string `json:"labels"`
	MergedBy    string   `json:"merged_by"`
	SenderIsBot bool     `json:"sender_is_bot"`
	HeadRef     string   `json:"head_ref"`
	BaseRef     string   `json:"base_ref"`
	// DefaultBranchAtMerge 是合并那一刻仓库的默认分支（未知时为空）；省略 tag.branch 时它是发版分支。
	DefaultBranchAtMerge string `json:"default_branch_at_merge"`
	// SkipCode 是结构化的跳过原因码（SkipCodeBranchNotRelease 等；不是跳过时为空）。
	SkipCode   string `json:"skip_code"`
	WorkItemID *int64 `json:"work_item_id"`
	Source     string `json:"source"`
	State      string `json:"state"`
	Tag        string `json:"tag"`
	Reason     string `json:"reason"`
	// ReasonNote 是 Reason 的原因码和原始参数（Dashboard 按语言显示，见 notes.go）；升级前的旧记录原因码为空。
	ReasonNote Note      `json:"-"`
	Tries      int       `json:"tries"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// 以下是这次处理中认定的 AI 开发归属（审计用，不落库）。
	OriginPhase   string `json:"-"`
	OriginRound   int    `json:"-"`
	OriginIssues  []int  `json:"-"`
	OriginCreator string `json:"-"`
	// OriginWorkItemID 是来源判定认定的工作项编号（诊断用：归属未确认、设计、拒绝时不写进 work_item_id 可信列，审计里仍
	// 记下它便于排查）。
	OriginWorkItemID int64 `json:"-"`
}

const recordCols = `id, repo_id, sha, pr_number, labels, merged_by, sender_is_bot, head_ref, base_ref, default_branch_at_merge,
	skip_code, work_item_id, source, state, tag, reason, reason_code, reason_params, tries, created_at, updated_at`

func scanRecords(rows pgx.Rows) ([]*Record, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Record, error) {
		var x Record
		err := r.Scan(&x.ID, &x.RepoID, &x.SHA, &x.PR, &x.Labels, &x.MergedBy, &x.SenderIsBot, &x.HeadRef, &x.BaseRef,
			&x.DefaultBranchAtMerge, &x.SkipCode, &x.WorkItemID, &x.Source, &x.State, &x.Tag, &x.Reason, &x.ReasonNote.Code,
			&x.ReasonNote.Params, &x.Tries, &x.CreatedAt, &x.UpdatedAt)
		return &x, err
	})
}

// List 返回仓库最近的 limit 条记录（新的在前）：CLI 排查、诊断和 Retry 使用，包含不自动打 tag 的开发分支合并。
func List(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*Record, error) {
	rows, err := q.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 ORDER BY id DESC LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

// ListVisible 返回仓库最近的 limit 条记录（新的在前），在 SQL 的 LIMIT 之前排除因目标分支不是发版分支而跳过的记录：
// 高频的开发分支合并不把正式版本的记录挤出 Dashboard 的最近列表。CLI 用 List。
func ListVisible(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*Record, error) {
	rows, err := q.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 AND NOT (state='skipped' AND skip_code=$3)
		ORDER BY id DESC LIMIT $2`, repoID, limit, SkipCodeBranchNotRelease)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

// ForPR 返回 PR 合并后的记录（没有时为 nil）。
func ForPR(ctx context.Context, q store.Querier, repoID int64, pr int) (*Record, error) {
	rows, err := q.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 AND pr_number=$2 ORDER BY id DESC LIMIT 1`, repoID, pr)
	if err != nil {
		return nil, err
	}
	rs, err := scanRecords(rows)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return rs[0], nil
}

// Record 记下经过 PR 合入的 commit 并通知执行者（pull_request.closed webhook 用；合并队列在自己的事务中直接写）。
// 任何目标分支的合并都记下，是否发版由 decide 按发版分支裁决。
// 返回是否新记下（同一个 commit 已经记过时为 false）。
func (s *Service) Record(ctx context.Context, r mqevents.AutoTagRecord) (bool, error) {
	var added bool
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if added, err = mqevents.RecordAutoTag(ctx, tx, r); err != nil || !added {
			return err
		}
		return mqevents.EnqueueAutoTag(ctx, s.outbox, tx, r.RepoID)
	})
	if added && err == nil && s.hub != nil {
		s.hub.All()
	}
	return added, err
}

// ErrNotRetryable 表示记录不能重试（不存在，或者不是 failed / skipped）。
var ErrNotRetryable = errors.New("只能重试失败或跳过的记录")

// Retry 把失败或跳过的记录重新置为 pending（从头计算版本号，清掉上次尝试的名字和认定过的工作项；已经建好的 tag 在平台的
// 列表中会被认出来，列表滞后时创建同名 tag 得到"已存在"，按名字读到指向这个 commit 也会认出来）并通知执行者。
func (s *Service) Retry(ctx context.Context, repoID int64, sha, actor string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE auto_tags SET state='pending', tag='', reason='', reason_code='', reason_params='{}', skip_code='',
			work_item_id=NULL, updated_at=now() WHERE repo_id=$1 AND sha=$2 AND state IN ('failed','skipped')`, repoID, sha)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotRetryable
		}
		if err := store.Audit(ctx, tx, actor, "autotag.retry", map[string]any{"repo_id": repoID, "sha": sha}); err != nil {
			return err
		}
		return mqevents.EnqueueAutoTag(ctx, s.outbox, tx, repoID)
	})
	if err == nil && s.hub != nil {
		s.hub.All()
	}
	return err
}

// HandleOutbox 投递 outbox 的 autotag.repo 记录（注册到 outbox.Worker）。
func (s *Service) HandleOutbox(ctx context.Context, payload []byte) error {
	var p mqevents.RepoPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("自动打 tag 通知的 payload 无法解析：%w", err)
	}
	return s.Process(ctx, p.RepoID)
}

// Run 是兜底检查：每 sweepInterval 通知一次有未完成记录的仓库，直到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("自动打 tag 兜底检查", "err", err)
		}
	}
}

func (s *Service) sweep(ctx context.Context) error {
	ids, err := store.QueryIDs(ctx, s.db, `SELECT DISTINCT a.repo_id FROM auto_tags a JOIN repos r ON r.id=a.repo_id
		WHERE a.state IN ('pending','tagging','tagged') AND (r.active OR a.state='pending')`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := mqevents.EnqueueAutoTag(ctx, s.outbox, s.db, id); err != nil {
			return err
		}
	}
	return nil
}

func lockKey(repoID int64) string { return fmt.Sprintf("kiln:autotag:%d", repoID) }

// Process 处理仓库 repoID 未完成的记录（按 id 顺序）。返回错误表示需要稍后重试。可以重复调用。
//
// 分两段：持有仓库锁时打 tag（decide、ensureTag，推进到 tagged）；释放锁、归还连接之后再规划 tagged 的记录。
// 规划（Planner.PlanTagPush）从连接池借自己的连接，持有锁的连接不归还就规划时，连接池上限为 1（pool_max_conns=1）会一直
// 等待（独立审查发现）。规划按去重键幂等，记为 planned 是条件更新，两次处理同时规划同一条记录也只建一个 run（T2）。
func (s *Service) Process(ctx context.Context, repoID int64) error {
	if s.forge == nil {
		return nil
	}
	repo, err := s.tagLocked(ctx, repoID)
	if repo == nil || !repo.Active {
		return err
	}
	return errors.Join(err, s.planTagged(ctx, repo))
}

// tagLocked 持有仓库锁处理 pending、tagging 的记录（推进到 tagged 或结束），返回仓库（读取仓库之前失败时为 nil）。
func (s *Service) tagLocked(ctx context.Context, repoID int64) (_ *store.Repo, err error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	unlock, locked, err := store.TryAdvisoryLock(ctx, conn, lockKey(repoID))
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrBusy
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil {
			err = errors.Join(err, fmt.Errorf("释放自动打 tag 的锁：%w", uerr))
		}
	}()
	// 持有锁期间的查询和事务都用这个连接（连接池上限为 1 时再借一个连接会一直等待）
	repo, err := store.GetRepo(ctx, conn, repoID)
	if err != nil {
		return nil, err
	}
	p := &pass{s: s, conn: conn, repo: repo}
	if !repo.Active {
		// 还没开始建 tag 的（pending）放弃，并在同一个事务里清掉这些 commit 的待裁决候选（候选只对 pending 有意义，
		// 停用期间留下会被 Retry 误用）；tagging（已经请求创建、结果可能不明，tag 可能已经建好）和 tagged 保留，
		// 仓库恢复后按名字确认、照常规划发布（T2，独立审查发现：以前 tagging 也放弃，建好的 tag 永远不发布）
		err := store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE auto_tags SET state='failed', reason=$2, reason_code=$3, reason_params=$4, updated_at=now()
				WHERE repo_id=$1 AND state='pending'`, repoID, inactiveReason.Text(), inactiveReason.Code, inactiveReason.Params); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM auto_tag_candidates WHERE repo_id=$1`, repoID)
			return err
		})
		return repo, err
	}
	p.f = s.forge(repo)
	rows, err := conn.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 AND state IN ('pending','tagging') ORDER BY id`, repoID)
	if err != nil {
		return repo, err
	}
	recs, err := scanRecords(rows)
	if err != nil {
		return repo, err
	}
	for _, r := range recs {
		// 前一条没有完成（暂时失败）时不处理后面的：后面的 commit 更新，先打它会让旧 commit 拿不到 tag（T3）
		if err := p.process(ctx, r); err != nil {
			return repo, err
		}
	}
	return repo, nil
}

// planTagged 规划仓库中已经建好 tag（tagged）的记录（PLAN #29.4 第 6 步），不持有仓库锁，用连接池。
// 一条失败时记下次数和原因，继续规划后面的（tag 已经建好，各自的发布互不依赖）。
func (s *Service) planTagged(ctx context.Context, repo *store.Repo) error {
	rows, err := s.db.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE repo_id=$1 AND state='tagged' ORDER BY id`, repo.ID)
	if err != nil {
		return err
	}
	recs, err := scanRecords(rows)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		if err := s.plan(ctx, repo, r); err != nil {
			n := note(ReasonRetrying, NoteParams{Detail: err.Error()})
			if _, uerr := s.db.Exec(ctx, `UPDATE auto_tags SET tries=tries+1, reason=$2, reason_code=$3, reason_params=$4, updated_at=now()
				WHERE id=$1 AND state='tagged'`, r.ID, shorten.Text(n.Text(), maxReasonLen), n.Code, n.Params); uerr != nil {
				err = errors.Join(err, uerr)
			}
			errs = append(errs, fmt.Errorf("自动打 tag（%s，PR #%d）：%w", shorten.Hash(r.SHA), r.PR, err))
		}
	}
	return errors.Join(errs...)
}

// pass 是一次处理。
type pass struct {
	s    *Service
	conn *pgxpool.Conn
	repo *store.Repo
	f    forge.Repo
	// cfg、cfgErr 是默认分支的 .kiln/merge.yml（第一次需要时读取）。
	cfg    *mergecfg.Config
	cfgErr string
	loaded bool
	// occupied 是这次处理中按名字读到、已经指向其他 commit 的 tag（名字 → commit）：tag 列表可能滞后、还看不到它们，
	// 重新计算时要算进去，否则会反复算出同一个名字。
	occupied map[string]string
}

// permanentError 是重试没有意义的失败（配置错误、平台拒绝）：记为 failed 后继续处理后面的记录。
type permanentError struct{ note Note }

func (e *permanentError) Error() string { return e.note.Text() }

// process 推进一条记录直到建好 tag（tagged）或结束（skipped / failed）。暂时失败时返回错误（记下次数和原因）。
func (p *pass) process(ctx context.Context, r *Record) error {
	for range maxSteps {
		var err error
		switch r.State {
		case StatePending:
			err = p.decide(ctx, r)
		case StateTagging:
			err = p.ensureTag(ctx, r)
		default: // tagged（释放锁后由 planTagged 规划）、planned、skipped、failed
			return nil
		}
		if err == nil {
			continue
		}
		if pe, ok := errors.AsType[*permanentError](err); ok {
			return p.finishNote(ctx, r, StateFailed, pe.note, "autotag.failed")
		}
		if forge.IsRejected(err) {
			return p.finishNote(ctx, r, StateFailed, note(ReasonPermission, NoteParams{Detail: err.Error()}), "autotag.failed")
		}
		n := note(ReasonRetrying, NoteParams{Detail: err.Error()})
		if _, uerr := p.conn.Exec(ctx, `UPDATE auto_tags SET tries=tries+1, reason=$2, reason_code=$3, reason_params=$4, updated_at=now()
			WHERE id=$1`, r.ID, shorten.Text(n.Text(), maxReasonLen), n.Code, n.Params); uerr != nil {
			err = errors.Join(err, uerr)
		}
		return fmt.Errorf("自动打 tag（%s，PR #%d）：%w", shorten.Hash(r.SHA), r.PR, err)
	}
	return fmt.Errorf("自动打 tag（%s）超过 %d 步（tag 名反复被占用？）", shorten.Hash(r.SHA), maxSteps)
}

// loadConfig 读取默认分支的 .kiln/merge.yml（一次处理只读一次）。配置有错误时 cfg 保持默认值，只记 cfgErr。
func (p *pass) loadConfig(ctx context.Context) error {
	if p.loaded {
		return nil
	}
	data, err := p.f.ReadFile(ctx, p.repo.DefaultBranch, mergecfg.Path)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", mergecfg.Path, err)
	}
	p.cfg, p.loaded = mergecfg.Default(), true
	if data != nil {
		cfg, err := mergecfg.Parse(data)
		if err != nil {
			p.cfgErr = err.Error()
			return nil //nolint:nilerr // 配置错误记在 cfgErr 中，由 decide 转成失败（不是读取失败，不需要重试）
		}
		p.cfg = cfg
	}
	return nil
}

// setState 把记录推进到 state（tag 为新的 tag 名，why 为原因，零值表示没有）。进入这些状态时清掉结构化的跳过原因码，并在
// 同一个事务中清掉这个 commit 的待裁决候选（离开 pending 之后候选不再有意义，绝不残留）。
func (p *pass) setState(ctx context.Context, r *Record, state, tag string, why Note) error {
	reason := shorten.Text(why.Text(), maxReasonLen)
	err := store.TxOn(ctx, p.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, tag=$3, reason=$4, reason_code=$5, reason_params=$6, skip_code='',
			updated_at=now() WHERE id=$1`, r.ID, state, tag, reason, why.Code, why.Params); err != nil {
			return err
		}
		return clearCandidates(ctx, tx, r.RepoID, r.SHA)
	})
	if err != nil {
		return err
	}
	r.State, r.Tag, r.Reason, r.ReasonNote, r.SkipCode = state, tag, reason, why, ""
	return nil
}

// clearCandidates 删掉这个 commit 的全部待裁决候选（mqevents.RecordAutoTag 在记录还是 pending 时从其他分支合入留下的）。
// 离开 pending 之前由 decide 按候选重新裁决，之后必须清空：master 正常发布后残留的 dev 候选不会再被用到。
func clearCandidates(ctx context.Context, q store.Execer, repoID int64, sha string) error {
	_, err := q.Exec(ctx, `DELETE FROM auto_tag_candidates WHERE repo_id=$1 AND sha=$2`, repoID, sha)
	return err
}

// autoTagCandidate 是 auto_tag_candidates 的一行：记录还是 pending 时同一个 commit 从另一个分支合入的事实。
type autoTagCandidate struct {
	PR                   int
	Labels               []string
	MergedBy             string
	SenderIsBot          bool
	HeadRef              string
	BaseRef              string
	DefaultBranchAtMerge string
	Source               string
}

// candidateOutcome 是 resolveCandidates 的结果。
type candidateOutcome int

const (
	// candidateMoved 表示记录在事务里已经不是 pending（状态被别处推进了），什么也没做。
	candidateMoved candidateOutcome = iota
	// candidatePromoted 表示找到了能发版的候选，已经在事务中写回记录并清空候选。
	candidatePromoted
	// candidateSkipped 表示没有能发版的候选，已经在事务中记为 skipped、清空候选并写审计。
	candidateSkipped
)

// resolveCandidates 在记录还是 pending、而它自己的目标分支不是发版分支时，按同一个 commit 的候选重新裁决（PLAN #29.4）：
// 候选是记录还在 pending 时同一个 commit 从另一个分支的合入（mqevents.RecordAutoTag 写入）。锁父行、读候选、晋级或跳过、
// 清候选在同一个事务中完成，与 RecordAutoTag 串行化：既不会在"查空之后、清候选之前"误删刚提交的候选，也不会丢掉发版
// 分支上的合并事实。逐条按候选自己的默认分支快照（default_branch_at_merge）裁决：默认分支改过名时，迟到的候选仍按合并
// 当时的默认分支判断它合入的是不是发版分支。
func (p *pass) resolveCandidates(ctx context.Context, r *Record, mergeDefault string) (candidateOutcome, error) {
	var (
		outcome  candidateOutcome
		promoted autoTagCandidate
		skipNote Note
	)
	err := store.TxOn(ctx, p.conn, func(tx pgx.Tx) error {
		// 重试时从头开始：上一次尝试的内存结果作废
		outcome, promoted, skipNote = candidateMoved, autoTagCandidate{}, Note{}
		// 第一句先锁父行（与 mqevents.RecordAutoTag 的 FOR UPDATE 串行化）
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM auto_tags WHERE id=$1 FOR UPDATE`, r.ID).Scan(&state); err != nil {
			return err
		}
		if state != StatePending {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT pr_number, labels, merged_by, sender_is_bot, head_ref, base_ref,
			default_branch_at_merge, source FROM auto_tag_candidates WHERE repo_id=$1 AND sha=$2 ORDER BY id`, r.RepoID, r.SHA)
		if err != nil {
			return err
		}
		cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (autoTagCandidate, error) {
			var c autoTagCandidate
			err := row.Scan(&c.PR, &c.Labels, &c.MergedBy, &c.SenderIsBot, &c.HeadRef, &c.BaseRef, &c.DefaultBranchAtMerge, &c.Source)
			return c, err
		})
		if err != nil {
			return err
		}
		for _, c := range cands {
			// 按候选自己的默认分支快照判断它合入的是不是发版分支
			candDefault := cmp.Or(c.DefaultBranchAtMerge, p.repo.DefaultBranch)
			if _, _, skip := branchVerdict(c.BaseRef, p.cfg.Tag, candDefault); skip {
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE auto_tags SET pr_number=$2, labels=$3, merged_by=$4, sender_is_bot=$5, head_ref=$6,
				base_ref=$7, default_branch_at_merge=$8, source=$9, work_item_id=NULL, tag='', reason='', reason_code='', reason_params='{}',
				skip_code='', updated_at=now() WHERE id=$1 AND state='pending'`,
				r.ID, c.PR, c.Labels, c.MergedBy, c.SenderIsBot, c.HeadRef, c.BaseRef, c.DefaultBranchAtMerge, c.Source)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			if err := clearCandidates(ctx, tx, r.RepoID, r.SHA); err != nil {
				return err
			}
			promoted, outcome = c, candidatePromoted
			return nil
		}
		// 没有能发版的候选：同一个事务里记为跳过并清空候选（绝不残留会被误用的候选）
		why, _, _ := branchVerdict(r.BaseRef, p.cfg.Tag, mergeDefault)
		reason := why.Text()
		if _, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, reason=$3, reason_code=$5, reason_params=$6, skip_code=$4,
			updated_at=now() WHERE id=$1`,
			r.ID, StateSkipped, shorten.Text(reason, maxReasonLen), SkipCodeBranchNotRelease, why.Code, why.Params); err != nil {
			return err
		}
		if err := clearCandidates(ctx, tx, r.RepoID, r.SHA); err != nil {
			return err
		}
		detail := auditFields(r)
		detail["reason"] = reason
		if err := store.Audit(ctx, tx, auditActor, "autotag.skipped", detail); err != nil {
			return err
		}
		skipNote, outcome = why, candidateSkipped
		return nil
	})
	if err != nil {
		return candidateMoved, err
	}
	switch outcome {
	case candidatePromoted:
		r.PR, r.Labels, r.MergedBy, r.SenderIsBot, r.HeadRef = promoted.PR, promoted.Labels, promoted.MergedBy, promoted.SenderIsBot, promoted.HeadRef
		r.BaseRef, r.DefaultBranchAtMerge, r.Source = promoted.BaseRef, promoted.DefaultBranchAtMerge, promoted.Source
		r.WorkItemID, r.Tag, r.Reason, r.ReasonNote, r.SkipCode = nil, "", "", Note{}, ""
	case candidateSkipped:
		r.State, r.Reason, r.ReasonNote, r.SkipCode = StateSkipped, skipNote.Text(), skipNote, SkipCodeBranchNotRelease
	case candidateMoved:
		// 记录在事务里已经不是 pending：重新读到真实的记录，交给 process 按状态继续（理论上不会发生）
		if err := p.reload(ctx, r); err != nil {
			return candidateMoved, err
		}
	}
	return outcome, nil
}

// reload 用数据库中的最新值刷新内存中的记录（记录被别处推进时用）。
func (p *pass) reload(ctx context.Context, r *Record) error {
	rows, err := p.conn.Query(ctx, `SELECT `+recordCols+` FROM auto_tags WHERE id=$1`, r.ID)
	if err != nil {
		return err
	}
	rs, err := scanRecords(rows)
	if err != nil {
		return err
	}
	if len(rs) == 1 {
		*r = *rs[0]
	}
	return nil
}

// finishNote 与 finish 相同，中文说明是原因码的说明。
func (p *pass) finishNote(ctx context.Context, r *Record, state string, why Note, action string) error {
	return p.finish(ctx, r, state, why.Text(), why, action)
}

// finish 把记录结束为 state（skipped / failed）并写审计日志：reason 是中文说明，why 是同一个原因的原因码（零值表示没有）。
// r.SkipCode 是结构化的跳过原因码（不跳过时为空）。
func (p *pass) finish(ctx context.Context, r *Record, state, reason string, why Note, action string) error {
	// 写审计之前只读补齐来源详情：tagging 崩溃恢复后内存中的来源字段已经丢失（例如恢复后创建 tag 被平台明确拒绝、转到
	// failed）。只用于审计，补齐失败只记 warn，不改变状态转移（T2 续做）。
	p.s.enrichOrigin(ctx, p.conn, r)
	return store.TxOn(ctx, p.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, reason=$3, reason_code=$5, reason_params=$6, skip_code=$4,
			updated_at=now() WHERE id=$1`, r.ID, state, shorten.Text(reason, maxReasonLen), r.SkipCode, why.Code, why.Params); err != nil {
			return err
		}
		// 离开 pending 时清掉这个 commit 的待裁决候选（没有能发版的候选时，这里和记 skipped 在同一个事务中完成）
		if err := clearCandidates(ctx, tx, r.RepoID, r.SHA); err != nil {
			return err
		}
		r.State, r.Reason, r.ReasonNote = state, reason, why
		detail := auditFields(r)
		detail["reason"] = reason
		return store.Audit(ctx, tx, auditActor, action, detail)
	})
}

// auditFields 返回审计日志中的发布依据（PLAN #29.7）：哪一次合并、打的 tag、真实的发起者是不是机器人、来源，以及认定的
// AI 开发工作项（有的话；可信列为空时用诊断用的编号）。阶段、轮次、Issue 只在这次处理的来源判定之后有值。
func auditFields(r *Record) map[string]any {
	m := map[string]any{
		"repo_id": r.RepoID, "sha": r.SHA, "pr": r.PR, "tag": r.Tag, "merged_by": r.MergedBy,
		"sender_is_bot": r.SenderIsBot, "source": r.Source,
	}
	id := r.OriginWorkItemID
	if r.WorkItemID != nil {
		id = *r.WorkItemID
	}
	if id > 0 {
		issues := r.OriginIssues
		if issues == nil {
			issues = []int{}
		}
		m["work_item_id"] = id
		m["phase"] = r.OriginPhase
		m["impl_round"] = r.OriginRound
		m["issues"] = issues
		m["creator"] = r.OriginCreator
	}
	return m
}

// decide 是 pending 的处理（PLAN #29.4 第 1–4 步）：读配置、判断 PR 的来源、检查 label、检查发布授权——这些都在认出已经
// 建好的 tag、计算版本号之前：已有版本 tag 的 commit 被第三方 Bot 合并、或者来自设计 PR 时，Retry 也必须拒绝（否则"已有
// tag 就认下来"会绕过授权）。之后检查提交顺序、计算版本号，先写下 tag 名（tagging）。
func (p *pass) decide(ctx context.Context, r *Record) error {
	if err := p.loadConfig(ctx); err != nil {
		return err
	}
	if p.cfgErr != "" {
		return &permanentError{note(ReasonConfigError, NoteParams{Detail: p.cfgErr})}
	}
	// 先按目标分支裁决是否发版（PLAN #29.4）：不合入发版分支的合并正常打 tag 之前就跳过，不进来源判定、不检查
	// label、不算版本号。合并那一刻的默认分支是省略 tag.branch 时的发版分支（默认分支改过名时也按合并当时判断）。
	mergeDefault := cmp.Or(r.DefaultBranchAtMerge, p.repo.DefaultBranch)
	// 记录还是 pending 时同一个 commit 从另一个分支合入（候选）：当前分支不是发版分支时按候选重新裁决，找到能发版的那一条
	// 就用它继续（例如乱序到达的 dev webhook 不能把还没处理的 master 合并盖掉）。锁父行、读候选、晋级或跳过、清候选在
	// 同一个事务中完成：mqevents.RecordAutoTag 也在同一行上 FOR UPDATE，两者串行化（并发测试见 branches_test.go）。
	// 没有 tag: 配置时不在这里晋级（无论如何都不会打 tag），交给下面的 finish 记下"没有 tag:"并清候选。
	if _, code, skip := branchVerdict(r.BaseRef, p.cfg.Tag, mergeDefault); skip && code == SkipCodeBranchNotRelease && p.cfg.Tag != nil {
		outcome, err := p.resolveCandidates(ctx, r, mergeDefault)
		if err != nil {
			return err
		}
		switch outcome {
		case candidateSkipped:
			// 没有能发版的候选：事务里已经记为 skipped 并清空候选
			return nil
		case candidatePromoted:
			mergeDefault = cmp.Or(r.DefaultBranchAtMerge, p.repo.DefaultBranch)
		case candidateMoved:
			// 记录已经不是 pending（不应发生：持有仓库锁时只有这里改变状态）：按重新读到的状态继续
			return nil
		}
	}
	if p.cfg.Tag == nil {
		if r.BaseRef != "" && r.BaseRef != mergeDefault {
			r.SkipCode = SkipCodeBranchNotRelease
		}
		return p.finishNote(ctx, r, StateSkipped, Note{Code: ReasonNoTagConfig}, "autotag.skipped")
	}
	if why, code, skip := releaseBranchSkip(r, p.cfg.Tag, mergeDefault); skip {
		r.SkipCode = code
		return p.finishNote(ctx, r, StateSkipped, why, "autotag.skipped")
	}
	origin := DevOrigin{Kind: OriginUnknown}
	if p.s.devOrigin != nil {
		o, err := p.s.devOrigin.Resolve(ctx, p.conn, r.RepoID, r.PR, r.HeadRef, r.BaseRef)
		if err != nil {
			// 判断不出来时保持 pending，由 outbox 重试：不能把查询失败伪装成策略拒绝
			return fmt.Errorf("判断 PR #%d 的来源：%w", r.PR, err)
		}
		origin = o
		r.OriginPhase, r.OriginRound, r.OriginIssues, r.OriginCreator = o.Phase, o.ImplRound, o.Issues, o.CreatorLogin
		r.OriginWorkItemID = o.WorkItemID
	}
	// 只有可信的实现 PR 才把工作项写进 work_item_id 可信列（恢复保护、审计关联用）；未知、归属未确认、设计、拒绝都不写，
	// 有旧值时清回 NULL。诊断用的编号留在审计说明和原因里。
	if err := p.setWorkItem(ctx, r, trustedWorkItem(origin)); err != nil {
		return err
	}
	bump, skip := BumpFor(r.Labels)
	if skip {
		return p.finishNote(ctx, r, StateSkipped, Note{Code: ReasonSkipLabel, Params: NoteParams{Label: LabelSkip}}, "autotag.skipped")
	}
	if origin.Kind == OriginDesign {
		reason, why := originNote(origin, Note{Code: ReasonDesignPR, Params: NoteParams{WorkItem: origin.WorkItemID}})
		return p.finish(ctx, r, StateSkipped, reason, why, "autotag.skipped")
	}
	if ok, reason, why := authorizeRelease(r, origin); !ok {
		return p.finish(ctx, r, StateSkipped, reason, why, "autotag.skipped")
	}
	prefix := p.cfg.Tag.PrefixOr()
	tags, err := p.knownTags(ctx, prefix)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(tags))
	var own []string
	for name, commit := range tags {
		names = append(names, name)
		if commit == r.SHA {
			own = append(own, name)
		}
	}
	if len(own) > 0 {
		// 这个 commit 已经有版本 tag（例如上一次创建成功、还没记下就崩溃了）：认下来，照常规划发布（T2）
		return p.setState(ctx, r, StateTagged, Latest(prefix, own), Note{Code: ReasonAlreadyTagged})
	}
	if latest := Latest(prefix, names); latest != "" {
		behind, err := p.f.Behind(ctx, tags[latest], r.SHA)
		if err != nil {
			return fmt.Errorf("比较 %s 与 %s：%w", latest, shorten.Hash(r.SHA), err)
		}
		if behind > 0 {
			return p.finishNote(ctx, r, StateSkipped, Note{Code: ReasonVersionOrder, Params: NoteParams{Tag: latest}}, "autotag.skipped")
		}
	}
	next := Next(prefix, p.cfg.Tag.Initial, names, bump)
	if !IsVersionTag(prefix, next) {
		// 递增后超出版本号的范围（某个数字超过 9 位）：之后的计算认不出它，会反复算出同一个名字
		return &permanentError{Note{Code: ReasonVersionOverflow, Params: NoteParams{Tag: next}}}
	}
	return p.setState(ctx, r, StateTagging, next, Note{})
}

// releaseBranchSkip 判断这次合并能不能按 tag.branch 发版（PLAN #29.4）：不是发版分支时返回跳过的原因和原因码。
// 省略 tag.branch 时发版分支是合并那一刻的默认分支；升级前的旧记录没有目标分支，省略 tag.branch 的放行（当时按默认
// 分支准入），显式配置了 tag.branch 的跳过并说明原因。
func releaseBranchSkip(r *Record, t *mergecfg.TagConfig, mergeDefault string) (why Note, code string, skip bool) {
	return branchVerdict(r.BaseRef, t, mergeDefault)
}

// branchVerdict 按目标分支 baseRef 判断能不能发版（PLAN #29.4），供决定跳过和按候选重新裁决共用：省略 tag.branch（包括
// 没有配置 tag:，此时由调用方另行决定是否需要 tag）时发版分支是合并那一刻的默认分支；旧记录没有目标分支时放行。不是
// 发版分支时返回原因和 SkipCodeBranchNotRelease；显式配置 tag.branch 而记录没有目标分支时跳过，但没有原因码（无法确认）。
func branchVerdict(baseRef string, t *mergecfg.TagConfig, mergeDefault string) (why Note, code string, skip bool) {
	if t != nil && t.Branch != nil {
		if baseRef == "" {
			return Note{Code: ReasonBranchUnknown, Params: NoteParams{Release: *t.Branch}}, "", true
		}
		if baseRef != *t.Branch {
			return Note{Code: ReasonNotTagBranch, Params: NoteParams{Branch: baseRef, Release: *t.Branch}}, SkipCodeBranchNotRelease, true
		}
		return Note{}, "", false
	}
	if baseRef == "" || baseRef == mergeDefault {
		return Note{}, "", false
	}
	return Note{Code: ReasonNotDefaultBranch, Params: NoteParams{Branch: baseRef, Release: mergeDefault}}, SkipCodeBranchNotRelease, true
}

// authorizeRelease 判断这条记录是否可以自动发布（PLAN #29.4）：人是发起者时总是允许；机器人（或者未知）是发起者时，
// 只有可信的 AI 开发实现工作项才允许——Kiln 的 Bot 合并 AI 开发的实现 PR 属于这种情况。返回不允许的原因。
//
// 例外：AI 开发工作项的 PR 创建归属正在核验（创建 PR 的响应丢失、合并事件先到）时，无论人类还是机器人合并都先跳过——归属
// 核验完成、补记之后可以 Retry。否则设计 PR 被人类合并、归属还没核验时会被误打 tag 并发布。
func authorizeRelease(r *Record, origin DevOrigin) (bool, string, Note) {
	if origin.Kind == OriginRecording {
		reason, why := originNote(origin, Note{Code: ReasonOriginPending})
		return false, reason, why
	}
	if !r.SenderIsBot && r.MergedBy != "" {
		return true, "", Note{}
	}
	if origin.Kind == OriginImplementation {
		return true, "", Note{}
	}
	if origin.Kind == OriginDenied && origin.Reason != "" {
		reason, why := originNote(origin, Note{})
		return false, reason, why
	}
	why := Note{Code: ReasonUnknownSender}
	if r.MergedBy != "" {
		why = Note{Code: ReasonBotSender, Params: NoteParams{Login: r.MergedBy}}
	}
	return false, why.Text(), why
}

// trustedWorkItem 返回要持久化到 auto_tags.work_item_id 的可信工作项：只有可信的 AI 开发实现 PR 才记（恢复保护、审计关联
// 用）；其他来源（未知、归属未确认、设计、拒绝）返回 0，不写可信列。
func trustedWorkItem(o DevOrigin) int64 {
	if o.Kind == OriginImplementation && o.WorkItemID > 0 {
		return o.WorkItemID
	}
	return 0
}

// setWorkItem 记下认定的 AI 开发工作项（审计关联），之后的状态更新都带着它；id 为 0 时清回 NULL（不写入不可信的工作项）。
func (p *pass) setWorkItem(ctx context.Context, r *Record, id int64) error {
	switch {
	case id == 0:
		if r.WorkItemID == nil {
			return nil
		}
		if _, err := p.conn.Exec(ctx, `UPDATE auto_tags SET work_item_id=NULL, updated_at=now() WHERE id=$1`, r.ID); err != nil {
			return err
		}
		r.WorkItemID = nil
		return nil
	case r.WorkItemID != nil && *r.WorkItemID == id:
		return nil
	}
	if _, err := p.conn.Exec(ctx, `UPDATE auto_tags SET work_item_id=$2, updated_at=now() WHERE id=$1`, r.ID, id); err != nil {
		return err
	}
	r.WorkItemID = &id
	return nil
}

// enrichOrigin 在写 autotag.tagged / autotag.planned 审计之前，按工作项归属只读补齐阶段、轮次、Issue 和创建者（重查数据库：
// tagging / tagged 崩溃恢复后内存中的来源详情已经丢失）。只用于审计，查询出错只记 warn 日志，绝不重新否决已经授权的
// 记录（T2 续做）。必须使用调用方传入的 q（持有仓库锁的连接，连接池上限为 1 时再借连接会死锁）。
func (s *Service) enrichOrigin(ctx context.Context, q store.Querier, r *Record) {
	if r.WorkItemID == nil || s.devOrigin == nil || r.OriginPhase != "" {
		return
	}
	o, err := s.devOrigin.Resolve(ctx, q, r.RepoID, r.PR, r.HeadRef, r.BaseRef)
	if err != nil {
		s.log.Warn("自动打 tag：补齐审计中的来源详情失败", "repo_id", r.RepoID, "pr", r.PR, "err", err)
		return
	}
	r.OriginPhase, r.OriginRound, r.OriginIssues, r.OriginCreator = o.Phase, o.ImplRound, o.Issues, o.CreatorLogin
}

// knownTags 返回带前缀的版本 tag（名字 → commit）：平台上的，加上 Kiln 自己记下的（平台的列表可能滞后，PLAN #29.4）。
func (p *pass) knownTags(ctx context.Context, prefix string) (map[string]string, error) {
	list, err := p.f.Tags(ctx, prefix, func(name string) bool { return IsVersionTag(prefix, name) })
	if err != nil {
		return nil, fmt.Errorf("列出 tag：%w", err)
	}
	out := make(map[string]string, len(list))
	for _, t := range list {
		out[t.Name] = t.Commit
	}
	rows, err := p.conn.Query(ctx, `SELECT tag, sha FROM auto_tags WHERE repo_id=$1 AND state IN ('tagged','planned') AND tag<>''`, p.repo.ID)
	if err != nil {
		return nil, err
	}
	type row struct{ Tag, SHA string }
	own, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	for _, o := range own {
		if _, ok := out[o.Tag]; !ok && IsVersionTag(prefix, o.Tag) {
			out[o.Tag] = o.SHA
		}
	}
	for name, commit := range p.occupied {
		if IsVersionTag(prefix, name) {
			out[name] = commit
		}
	}
	return out, nil
}

// ensureTag 是 tagging 的处理（第 5 步）：按名字读 tag——指向这个 commit 就完成；不存在就创建（名字被占用会失败，
// 不会建出第二个）；指向别处（期间有人手动建了）就回到 pending 重新计算。机器人发起、又没有已落库的可信工作项归属的
// 记录不创建 tag（防止未经授权的异常记录绕过 decide）。
func (p *pass) ensureTag(ctx context.Context, r *Record) error {
	if r.SenderIsBot && r.WorkItemID == nil {
		return p.finishNote(ctx, r, StateFailed, unauthorizedBotReason, "autotag.failed")
	}
	commit, err := p.f.TagCommit(ctx, r.Tag)
	if err != nil {
		return fmt.Errorf("读取 tag %s：%w", r.Tag, err)
	}
	switch commit {
	case r.SHA:
		return p.tagged(ctx, r)
	case "":
		err := p.f.CreateTag(ctx, r.Tag, r.SHA)
		switch {
		case err == nil:
			p.s.log.Info("自动打 tag：已创建", "repo", p.repo.FullName(), "tag", r.Tag, "sha", r.SHA, "pr", r.PR)
			return p.tagged(ctx, r)
		case errors.Is(err, forge.ErrTagExists):
			// 刚好被建了（可能是我们自己上一次的请求）：下一步按名字再读一次
			return nil
		case forge.IsRejected(err):
			return err // 没有权限：process 记为 failed 并提示需要的权限
		case !forge.Uncertain(err) && !forge.RateLimited(err):
			// 平台明确拒绝（例如 commit 不存在、名字不合法）：重试没有意义
			return &permanentError{note(ReasonTagRejected, NoteParams{Tag: r.Tag, Detail: err.Error()})}
		}
		// 结果不明（响应丢失、超时）：保持 tagging，下一次按名字读取确认，不会建出第二个 tag
		return fmt.Errorf("创建 tag %s：%w", r.Tag, err)
	}
	if p.occupied == nil {
		p.occupied = map[string]string{}
	}
	p.occupied[r.Tag] = commit
	p.s.log.Info("自动打 tag：tag 名已被占用，重新计算", "repo", p.repo.FullName(), "tag", r.Tag, "commit", commit)
	return p.setState(ctx, r, StatePending, "", Note{Code: ReasonTagOccupied, Params: NoteParams{Tag: r.Tag, SHA: commit}})
}

// tagged 记下 tag 已经存在且指向这个 commit。写审计之前只读补齐来源详情（tagging 崩溃恢复后内存中的详情已经丢失）。
func (p *pass) tagged(ctx context.Context, r *Record) error {
	p.s.enrichOrigin(ctx, p.conn, r)
	return store.TxOn(ctx, p.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, updated_at=now() WHERE id=$1`, r.ID, StateTagged); err != nil {
			return err
		}
		if err := clearCandidates(ctx, tx, r.RepoID, r.SHA); err != nil {
			return err
		}
		r.State = StateTagged
		return store.Audit(ctx, tx, auditActor, "autotag.tagged", auditFields(r))
	})
}

// unauthorizedBotReason 是机器人发起、又没有已落库的可信 AI 开发实现工作项归属时的失败原因（恢复保护）。
var unauthorizedBotReason = Note{Code: ReasonBotUnauthorized}

// plan 是 tagged 的处理（第 6 步）：规划这个 tag 的 push 事件。发送者是真实的发布发起者（人，或者 Kiln 的 Bot——AI 开发
// 的可信实现 PR 由 Bot 合并，规划时保留 SenderIsBot，由可信自动 tag 的规则放行发布）；重复规划按去重键不会建出第二个
// run（T2）；只有把 tagged 改成 planned 的那一次写审计日志。机器人发起、又没有已落库的可信工作项归属的记录不规划发布
// （防止未经授权的异常记录绕过 decide）。
func (s *Service) plan(ctx context.Context, repo *store.Repo, r *Record) error {
	// 审计之前只读补齐来源详情（tagged 崩溃恢复后内存中的详情已经丢失），用连接池（规划本身就不持有仓库锁）。
	s.enrichOrigin(ctx, s.db, r)
	if r.SenderIsBot && r.WorkItemID == nil {
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			why := unauthorizedBotReason
			tag, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, reason=$3, reason_code=$5, reason_params=$6, updated_at=now()
				WHERE id=$1 AND state=$4`, r.ID, StateFailed, why.Text(), StateTagged, why.Code, why.Params)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			r.State, r.Reason, r.ReasonNote = StateFailed, why.Text(), why
			detail := auditFields(r)
			detail["reason"] = why.Text()
			return store.Audit(ctx, tx, auditActor, "autotag.failed", detail)
		})
	}
	ids, err := s.planner.PlanTagPush(ctx, TagPush{Repo: repo, Tag: r.Tag, SHA: r.SHA, Sender: r.MergedBy, SenderIsBot: r.SenderIsBot})
	if err != nil {
		return fmt.Errorf("规划 tag %s 的发布：%w", r.Tag, err)
	}
	s.log.Info("自动打 tag：已规划发布", "repo", repo.FullName(), "tag", r.Tag, "runs", ids)
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE auto_tags SET state=$2, reason='', reason_code='', reason_params='{}', updated_at=now()
			WHERE id=$1 AND state=$3`, r.ID, StatePlanned, StateTagged)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		r.State = StatePlanned
		detail := auditFields(r)
		detail["runs"] = ids
		return store.Audit(ctx, tx, auditActor, "autotag.planned", detail)
	})
}
