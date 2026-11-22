package decisioninbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// Service 把 AI 开发交付报告里的决策疑难点单向同步到仓库配置的 Outline「待拍板」清单。
//
// 这是 C19 的典型场景：写外部文档没有幂等键。入队先写意图（dev_decision_syncs，pending），投递最多尝试一次；
// 结果不明时只做只读查证，绝不重发。同一仓库的投递与对账由一把独立的 advisory lock 串行化，不阻塞管理员改配置。

// KindWake 是唤醒投递 worker 的 outbox 记录类型：只做一次非阻塞唤醒，不承载业务。
const KindWake = "decision_inbox.wake"

// 条目状态（dev_decision_syncs.state）：与迁移里的 CHECK 约束一一对应。
const (
	statePending   = "pending"
	stateAttempted = "attempted"
	stateDelivered = "delivered"
	stateCancelled = "cancelled"
	stateUnknown   = "unknown"
)

// last_error_code 的取值（Dashboard 与人工对账按它分类，只在这里定义一次）。
const (
	codeConfigChanged        = "config_changed"
	codeTargetReadonly       = "target_in_readonly_collection"
	codeReadRejected         = "read_rejected"
	codeReadFailed           = "read_failed"
	codeWriteRejected        = "write_rejected"
	codeDocConflict          = "document_conflict"
	codeRateLimited          = "rate_limited"
	codeTokenRotated         = "token_rotated"
	codeCanceledBeforeSend   = "canceled_before_send"
	codeWriterConfig         = "writer_config"
	codeUpdateOutcomeUnknown = "update_outcome_unknown"
	codeDocIDMismatch        = "doc_id_mismatch"
	codeDocNotWritable       = "doc_not_writable"
	codeDocWrongCollection   = "doc_wrong_collection"
	codeDocTooLarge          = "doc_too_large"
	codeDocNearLimit         = "doc_near_limit"
)

// 处理循环与退避。
const (
	// pollInterval 是兜底扫描的间隔：outbox 的唤醒之外，每过这么久再检查一次到期的条目。
	pollInterval = time.Minute
	// batchSize 是单次为同一仓库读取的条目上限。
	batchSize = 20
	// reconcileBackoffBase / reconcileBackoffMax 是临时失败（网络、5xx、限流之外的未知错误）的指数退避。
	reconcileBackoffBase = 30 * time.Second
	reconcileBackoffMax  = 15 * time.Minute
	// conflictRetries 是 CAS 冲突时在一轮处理内的最多重试次数；用尽后回到 pending 等下一轮。
	conflictRetries = 3
	// tokenRotationRetries 是同一目标轮换 token 后在一轮处理内的最多重载次数：管理员连续轮换时不能无限循环，
	// 用尽后回到 pending / 保持 attempted 等下一轮。与 conflictRetries 独立计数。
	tokenRotationRetries = 3
	// rateLimitMaxWait 是可信限流时等待时间的上限：尊重服务端的 Retry-After，只在异常大（超过一天）时封顶。
	rateLimitMaxWait = 24 * time.Hour
	// commitStatusTimeout 是收尾状态（delivered / cancelled / unknown / retry）落盘的时限。收尾不随调用方取消而中断，
	// 但必须有期限：行锁阻塞或连接卡死时不能无限期挂住 Run / Close。
	commitStatusTimeout = 15 * time.Second
	// opTimeout 是配置设置 / 清除 / 人工对账这类同步操作的时限（与 BaseCtx 联结，服务退出时立即取消）。
	opTimeout = 2 * time.Minute
	// maxDocToken 是「待决策清单」写 token 的长度上限。
	maxDocToken = 4096
	// maxErrorLen 是 last_error 保存的错误信息上限。
	maxErrorLen = 2000
)

// Outline 单篇文档的篇幅限制（UTF-16 code unit，对齐 Outline 的 validations.ts）。
const (
	// decisionDocWarnUnits 是 Outline 的 maxRecommendedLength：超过时提醒管理员准备轮转到新文档。
	decisionDocWarnUnits int64 = 250_000
	// decisionDocMaxUnits 是 Outline 的 maxLength（1500 * 1024）：超过时不可能写入，直接把条目取消。
	decisionDocMaxUnits int64 = 1_536_000
)

// WriterFunc 用给定的站点地址和受限写 token 构造文档写接口。
type WriterFunc func(baseURL, token string) (docprovider.DocumentWriter, error)

// Deps 是 Service 的依赖。
type Deps struct {
	DB        *store.DB
	MasterKey []byte
	Log       *slog.Logger
	// Writer 构造写接口（生产中是 Outline 客户端）。
	Writer WriterFunc
	// Track 登记一次进行中的同步操作（服务生命周期门闩，与规划、文档冻结共用）；为 nil 时不登记。
	Track func() (done func(), err error)
	// BaseCtx 是服务生命周期 context：退出时取消后台循环、入队和同步操作。
	BaseCtx context.Context
	// CommitTimeout 是收尾状态落盘的时限；为 0 时用 commitStatusTimeout（测试可注入更短的值）。
	CommitTimeout time.Duration
	// Outbox 写唤醒记录（复用 outbox 共用包）；为 nil 时用一个始终启用的 writer。
	Outbox *outbox.Writer
}

// Service 是「待拍板」同步的服务。
type Service struct {
	db        *store.DB
	masterKey []byte
	log       *slog.Logger
	writer    WriterFunc
	outbox    *outbox.Writer
	track     func() (func(), error)
	baseCtx   context.Context
	commit    time.Duration
	wake      chan struct{}
}

// New 返回 Service。
func New(d Deps) *Service {
	track := d.Track
	if track == nil {
		track = func() (func(), error) { return func() {}, nil }
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	baseCtx := d.BaseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	commit := d.CommitTimeout
	if commit <= 0 {
		commit = commitStatusTimeout
	}
	ob := d.Outbox
	if ob == nil {
		ob = outbox.NewWriter(true)
	}
	writer := d.Writer
	if writer == nil {
		writer = func(baseURL, token string) (docprovider.DocumentWriter, error) {
			return docprovider.NewOutline(docprovider.Config{BaseURL: baseURL, Token: token})
		}
	}
	return &Service{
		db: d.DB, masterKey: d.MasterKey, log: log, writer: writer, outbox: ob, track: track, baseCtx: baseCtx,
		commit: commit, wake: make(chan struct{}, 1),
	}
}

// linkOp 给一次同步操作加上时限，并让它在服务生命周期结束时立即取消（opGate.Close 与后台循环共用 BaseCtx）。
// 服务已经在退出时直接拒绝，不再开始新的操作。
func (s *Service) linkOp(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := s.baseCtx.Err(); err != nil {
		return nil, nil, err
	}
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	stop := context.AfterFunc(s.baseCtx, cancel)
	return octx, func() { stop(); cancel() }, nil
}

// statusCtx 返回收尾状态落盘用的 context：不随调用方取消而中断，但有有限期限。
func (s *Service) statusCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.commit)
}

// lockKey 是按仓库串行化「待拍板」投递与对账的 advisory lock 键。它独立于配置锁（store.DocBindingLockKey）：
// 慢的 Outline 写请求不会阻塞管理员改配置。
func lockKey(repoID int64) string { return fmt.Sprintf("kiln:decision-inbox:%d", repoID) }

// Wake 非阻塞地唤醒后台循环。可以在任何 goroutine 中调用（包括 outbox 投递）。
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run 处理到期的条目，直到 ctx 结束。启动时先扫描一次；没有到期条目时不访问 Outline。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	s.processDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		s.processDue(ctx)
	}
}

// processDue 找出有到期条目的仓库，逐个处理。
func (s *Service) processDue(ctx context.Context) {
	repos, err := store.QueryIDs(ctx, s.db, `SELECT DISTINCT repo_id FROM dev_decision_syncs
		WHERE state IN ('pending', 'attempted') AND next_retry_at <= now() ORDER BY repo_id`)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("读取待同步的决策条目", "err", err)
		}
		return
	}
	for _, repoID := range repos {
		if ctx.Err() != nil {
			return
		}
		if err := s.ProcessRepo(ctx, repoID); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("同步决策条目", "repo", repoID, "err", err)
		}
	}
}

// ProcessRepo 处理一个仓库到期的条目。同一仓库的投递与对账由会话级 advisory lock 串行化；锁内只用同一条连接做短事务，
// 不跨网络请求持有数据库事务，也不锁工作项行（避免与 RecordResult 的加锁顺序倒置）。
func (s *Service) ProcessRepo(ctx context.Context, repoID int64) error {
	done, err := s.track()
	if err != nil {
		return err
	}
	defer done()
	octx, cancel, err := s.linkOp(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return s.db.WithAdvisoryLock(octx, lockKey(repoID), func(ctx context.Context, conn *pgxpool.Conn) error {
		return s.processRepoLocked(ctx, conn, repoID)
	})
}

func (s *Service) processRepoLocked(ctx context.Context, conn *pgxpool.Conn, repoID int64) error {
	// 游标推进：处理失败、next_retry_at 没有前进时，同一批条目也不会在本轮被反复取到（消灭持锁空转的活锁）。
	var afterID int64
	for {
		items, err := s.dueItems(ctx, conn, repoID, afterID)
		if err != nil {
			return err
		}
		for i := range items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := s.processItem(ctx, conn, &items[i]); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.log.Warn("处理决策条目失败", "repo", repoID, "sync", items[i].ID, "err", err)
			}
		}
		if len(items) < batchSize {
			return nil
		}
		afterID = items[len(items)-1].ID
	}
}

// dueItem 是一条待处理的条目。
type dueItem struct {
	ID                 int64
	RepoID             int64
	TargetProvider     string
	TargetBaseURL      string
	TargetDocID        string
	TargetCollectionID string
	TargetGeneration   int64
	Marker             string
	EntryKind          string
	State              string
	Attempts           int
	PayloadMD          *string
	Payload            []byte
}

func (s *Service) dueItems(ctx context.Context, q store.Querier, repoID, afterID int64) ([]dueItem, error) {
	rows, err := q.Query(ctx, `SELECT id, repo_id, target_provider, target_base_url, target_doc_id::text,
			target_collection_id, target_generation, marker, entry_kind, state, attempts, payload_md, payload
		FROM dev_decision_syncs
		WHERE repo_id=$1 AND id > $2 AND state IN ('pending', 'attempted') AND next_retry_at <= now()
		ORDER BY id ASC LIMIT $3`, repoID, afterID, batchSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueItem, error) {
		var it dueItem
		err := row.Scan(&it.ID, &it.RepoID, &it.TargetProvider, &it.TargetBaseURL, &it.TargetDocID,
			&it.TargetCollectionID, &it.TargetGeneration, &it.Marker, &it.EntryKind, &it.State, &it.Attempts,
			&it.PayloadMD, &it.Payload)
		return it, err
	})
}

func (s *Service) processItem(ctx context.Context, conn *pgxpool.Conn, it *dueItem) error {
	if it.State == stateAttempted {
		return s.processAttempted(ctx, conn, it)
	}
	return s.processPending(ctx, conn, it)
}

// sameTarget 判断当前配置是否仍指向条目入队时冻结的目标。
func sameTarget(d *store.DecisionDoc, it *dueItem) bool {
	return d != nil && d.Provider == it.TargetProvider &&
		store.NormalizeDocBaseURL(d.BaseURL) == store.NormalizeDocBaseURL(it.TargetBaseURL) && d.DocID == it.TargetDocID
}

// loadTarget 在短事务里按 FOR SHARE 读当前配置与 token，并检查只读绑定是否占用了目标集合。
// reason 非空时表示条目不能继续（配置被清除 / 轮换，或目标集合是只读集合）。
func (s *Service) loadTarget(ctx context.Context, conn *pgxpool.Conn, it *dueItem) (tok *store.DecisionDocToken, reason string, err error) {
	err = store.TxOn(ctx, conn, func(tx pgx.Tx) error {
		cfg, err := store.GetDecisionDocForShare(ctx, tx, it.RepoID)
		if err != nil {
			return err
		}
		if !sameTarget(cfg, it) {
			reason = codeConfigChanged
			return nil
		}
		b, err := store.GetDocBinding(ctx, tx, it.RepoID)
		if err != nil {
			return err
		}
		if b != nil && b.Provider == cfg.Provider && store.NormalizeDocBaseURL(b.BaseURL) == store.NormalizeDocBaseURL(cfg.BaseURL) &&
			b.CollectionID == cfg.CollectionID {
			reason = codeTargetReadonly
			return nil
		}
		tok, err = store.ResolveDecisionDocToken(ctx, tx, s.masterKey, it.RepoID)
		return err
	})
	return tok, reason, err
}

// terminalState 是鉴权拒绝之后可能落的终态。
type terminalState int

const (
	// terminalCancelled 表示确定没有写入（读取或写入被提供方拒绝）。
	terminalCancelled terminalState = iota
	// terminalUnknown 表示结果不明、无法证明写入是否生效。
	terminalUnknown
)

// resolveAuthRejection 在同一个短事务里核对配置代次，并在未轮换时把条目落终态。
//
// 鉴权拒绝之后必须原子地决定「轮换了还是真的拒绝」：普通 SELECT 与随后的条件更新之间有一个窗口，管理员在这期间
// 提交同一目标的新 token 时，普通读取看不到新代次，条目会被永久取消 / 转成 unknown。这里先对配置行加 FOR SHARE：
// 并发的 SetDecisionDoc / ClearDecisionDoc 事务（同样按 repo_decision_docs → dev_decision_syncs 的顺序加锁）没有
// 提交时，这里会等它提交，从而一定读到最新代次。确认没有轮换才在同一事务里落终态，事务内不发网络请求。
//
// rotated=true 表示同一目标轮换了 token：绝不修改 dev_decision_syncs，调用方换新凭据重来。
func (s *Service) resolveAuthRejection(ctx context.Context, conn *pgxpool.Conn, it *dueItem, usedGen int64,
	from string, state terminalState, code, msg string,
) (rotated bool, err error) {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	err = store.TxOn(ctx, conn, func(tx pgx.Tx) error {
		cfg, err := store.GetDecisionDocForShare(ctx, tx, it.RepoID)
		if err != nil {
			return err
		}
		if sameTarget(cfg, it) && cfg.Generation != usedGen {
			rotated = true
			return nil
		}
		sql := cancelStateSQL
		if state == terminalUnknown {
			sql = unknownStateSQL
		}
		_, err = tx.Exec(ctx, sql, it.ID, from, code, safeErrorText(msg))
		return err
	})
	return rotated, err
}

// currentWriter 用当前配置（同一目标的最新 token）构造写接口，只用于只读查证。
// gen 是这份凭据的代次，用于识别连续轮换；拿不到可用凭据时 ok=false（配置已变化），
// 写接口构造失败按错误返回，由调用方保持 attempted 等下一轮。
func (s *Service) currentWriter(ctx context.Context, conn *pgxpool.Conn, it *dueItem) (docprovider.DocumentWriter, int64, bool, error) {
	tok, reason, err := s.loadTarget(ctx, conn, it)
	if err != nil {
		return nil, 0, false, err
	}
	if reason != "" || tok == nil {
		return nil, 0, false, nil
	}
	w, err := s.writer(tok.BaseURL, tok.Token)
	if err != nil {
		return nil, 0, false, err
	}
	return w, tok.Generation, true, nil
}

// readFailureAction 是一次读取目标文档失败之后的处理。
type readFailureAction int

const (
	// readRequeued 表示已经安排好下一轮（取消或退避），本轮结束。
	readRequeued readFailureAction = iota
	// readRefetch 表示同一目标轮换了 token：换新凭据重新读取。
	readRefetch
)

// handleReadFailure 处理读取目标文档的失败。鉴权拒绝先原子判定是否轮换了 token（未轮换才取消），轮换了就重读；
// 其余永久拒绝取消；临时失败按指数退避保持在 pending。
func (s *Service) handleReadFailure(ctx context.Context, conn *pgxpool.Conn, it *dueItem, usedGen int64,
	rotationRound int, readErr error,
) (readFailureAction, error) {
	if docprovider.IsAuthRejected(readErr) {
		rotated, err := s.resolveAuthRejection(ctx, conn, it, usedGen, statePending, terminalCancelled,
			codeReadRejected, readErr.Error())
		if err != nil || !rotated {
			return readRequeued, err
		}
		if rotationRound < tokenRotationRetries {
			return readRefetch, nil
		}
		return readRequeued, s.retryPending(ctx, conn, it, it.Attempts, codeTokenRotated, readErr.Error())
	}
	if docprovider.PermanentReadError(readErr) {
		return readRequeued, s.cancel(ctx, conn, it, statePending, codeReadRejected, readErr.Error())
	}
	return readRequeued, s.retryPending(ctx, conn, it, it.Attempts, codeReadFailed, readErr.Error())
}

// processPending 尝试第一次写入（或冲突 / 限流 / 轮换后的重试）：先只读核对文档，再冻结 payload_md、把条目改成
// attempted，最后发起唯一一次写请求。CAS 冲突在一轮内有界重试（每次重读文档、复用已冻结的 payload_md）。
//
// 同一目标轮换 token（管理员换了一份写 token，旧凭据被撤销）不算失败：读取或写入遇到鉴权拒绝时用当前配置的
// 新凭据重来，绝不因为旧凭据失效把条目永久取消。轮换重载同样有界，用尽后回到 pending 等下一轮。
func (s *Service) processPending(ctx context.Context, conn *pgxpool.Conn, it *dueItem) error {
	conflictRound, rotationRound := 0, 0
	for {
		tok, reason, err := s.loadTarget(ctx, conn, it)
		if err != nil {
			return err
		}
		if reason != "" {
			return s.cancel(ctx, conn, it, statePending, reason, "")
		}
		if tok == nil {
			return s.cancel(ctx, conn, it, statePending, codeConfigChanged, "")
		}
		w, err := s.writer(tok.BaseURL, tok.Token)
		if err != nil {
			return s.retryPending(ctx, conn, it, it.Attempts, codeWriterConfig, err.Error())
		}

		doc, err := w.GetDocument(ctx, it.TargetDocID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			action, rerr := s.handleReadFailure(ctx, conn, it, tok.Generation, rotationRound, err)
			if rerr != nil {
				return rerr
			}
			if action == readRefetch {
				rotationRound++
				continue
			}
			return nil
		}
		if code := validateTargetDoc(doc, it); code != "" {
			return s.cancel(ctx, conn, it, statePending, code, "")
		}

		// 文档里已经有这个 marker（上次写入其实成功了，或有人手工贴过）：直接算送达，不再写。
		if hasMarker(doc.Content, it.Marker) {
			return s.delivered(ctx, conn, it, statePending, doc.Revision)
		}

		block, err := s.entryBlock(ctx, conn, it)
		if err != nil {
			return err
		}
		newText, multiple := InsertEntry(doc.Content, block)
		if multiple {
			s.log.Warn("待拍板清单里有多个 `## 待拍板` 标题，只用了第一个", "repo", it.RepoID, "sync", it.ID)
		}
		units := UTF16Units(newText)
		warning := ""
		if units > decisionDocWarnUnits {
			warning = codeDocNearLimit
			s.log.Warn("待拍板清单接近 Outline 的篇幅上限，请准备轮转到新文档",
				"repo", it.RepoID, "units", units, "warn", decisionDocWarnUnits, "max", decisionDocMaxUnits)
		}
		if err := store.UpdateDecisionDocSize(ctx, conn, it.RepoID, tok.Generation, units, warning); err != nil {
			return fmt.Errorf("记录文档篇幅：%w", err)
		}
		if units > decisionDocMaxUnits {
			return s.cancel(ctx, conn, it, statePending, codeDocTooLarge, "")
		}

		// 写前先提交 attempted：如果这个短事务的结果不明，绝不发写请求；若它实际已提交，下一轮按 attempted 只读查证。
		// 同一目标在这期间轮换了 token 时保持 pending，重新加载新凭据后再写。
		begin, err := s.beginAttempt(ctx, conn, it, tok.Generation, block, doc.Revision)
		if err != nil {
			return fmt.Errorf("登记写入尝试：%w", err)
		}
		if begin.reason != "" {
			return s.cancel(ctx, conn, it, statePending, begin.reason, "")
		}
		if begin.tokenRotated {
			if rotationRound < tokenRotationRetries {
				rotationRound++
				continue
			}
			return s.retryPending(ctx, conn, it, it.Attempts, codeTokenRotated, "")
		}
		if !begin.attempted {
			return nil
		}
		it.Attempts++
		if it.PayloadMD == nil {
			it.PayloadMD = &block
		}

		res, werr := w.UpdateDocument(ctx, docprovider.UpdateDocumentRequest{
			ID: it.TargetDocID, Text: newText, LastRevision: doc.Revision,
		})
		if werr == nil {
			return s.delivered(ctx, conn, it, stateAttempted, res.Revision)
		}
		effect := docprovider.EffectMayHaveApplied
		if we, ok := errors.AsType[*docprovider.WriteError](werr); ok {
			effect = we.Effect
		}
		if effect == docprovider.EffectMayHaveApplied {
			// 结果不明：绝不重发，只读查证一次。旧凭据被轮换时查证会换成新凭据，但仍然只读。
			return s.verify(ctx, conn, it, w, tok.Generation, werr.Error())
		}

		outcome, err := s.handleWriteFailure(ctx, conn, it, tok.Generation, werr, conflictRound, rotationRound)
		if err != nil {
			return err
		}
		if !outcome.retry {
			return nil
		}
		if outcome.rotated {
			rotationRound++
		} else {
			conflictRound++
		}
	}
}

// writeFailureOutcome 是「确定没生效」的写失败之后的处理结果。
type writeFailureOutcome struct {
	// retry 表示条目已经回到 pending，可以在本轮立即重试。
	retry bool
	// rotated 表示这次重试是因为同一目标轮换了 token（而不是 CAS 冲突），两者独立计数。
	rotated bool
}

// handleWriteFailure 处理「确定没生效」的写失败：CAS 冲突与可信限流按原有语义重试；鉴权拒绝在同一目标轮换了
// token 时换新凭据重来，绝不能因为旧凭据被撤销而取消条目；发送前的本地取消退回 pending 并结束本轮（不是提供方拒绝）；
// 其余永久拒绝取消条目。
// conflictRound / rotationRound 是本轮已经重试的次数，用尽后推迟到下一轮。
func (s *Service) handleWriteFailure(ctx context.Context, conn *pgxpool.Conn, it *dueItem, usedGen int64, werr error,
	conflictRound, rotationRound int,
) (writeFailureOutcome, error) {
	switch {
	case docprovider.IsDocumentConflict(werr):
		// 文档在读取之后被编辑过：确定没有写入。回到 pending，下一轮重读文档、复用冻结的 payload_md 再写。
		if err := s.backToPending(ctx, conn, it, codeDocConflict, werr.Error()); err != nil {
			return writeFailureOutcome{}, err
		}
		if conflictRound+1 >= conflictRetries {
			return writeFailureOutcome{}, s.retryPending(ctx, conn, it, it.Attempts, codeDocConflict, werr.Error())
		}
		return writeFailureOutcome{retry: true}, nil
	case docprovider.IsRateLimited(werr):
		wait := reconcileBackoff(it.Attempts)
		if we, ok := errors.AsType[*docprovider.WriteError](werr); ok && we.RetryAfter > 0 {
			wait = min(we.RetryAfter, rateLimitMaxWait)
		}
		return writeFailureOutcome{}, s.backToPendingWait(ctx, conn, it, wait, codeRateLimited, werr.Error())
	case docprovider.IsAuthRejected(werr):
		// 同一目标轮换了 token、旧凭据被撤销：写请求确定没生效。原子判定（配置行 FOR SHARE 与落终态同一事务），
		// 未轮换才取消；轮换了就保留冻结的 payload_md 用新凭据重来。
		rotated, err := s.resolveAuthRejection(ctx, conn, it, usedGen, stateAttempted, terminalCancelled,
			codeWriteRejected, werr.Error())
		if err != nil {
			return writeFailureOutcome{}, err
		}
		if !rotated {
			return writeFailureOutcome{}, nil
		}
		if rotationRound >= tokenRotationRetries {
			return writeFailureOutcome{}, s.backToPendingWait(ctx, conn, it, reconcileBackoff(it.Attempts), codeTokenRotated, werr.Error())
		}
		if err := s.backToPending(ctx, conn, it, codeTokenRotated, werr.Error()); err != nil {
			return writeFailureOutcome{}, err
		}
		return writeFailureOutcome{retry: true, rotated: true}, nil
	case canceledBeforeSend(werr):
		// 请求在发送前就被本地取消：确定没有写，绝不是提供方的永久拒绝。退回 pending 并保留冻结的
		// payload_md（下一轮用同一份正文重发）；本轮不再尝试，调用方的取消应立即中止该仓库后续条目。
		if err := s.backToPending(ctx, conn, it, codeCanceledBeforeSend, werr.Error()); err != nil {
			return writeFailureOutcome{}, err
		}
		if err := ctx.Err(); err != nil {
			return writeFailureOutcome{}, err
		}
		return writeFailureOutcome{}, nil
	default:
		// 提供方确定拒绝（参数无效、鉴权失败、文档不存在等）：不再重试。
		return writeFailureOutcome{}, s.cancel(ctx, conn, it, stateAttempted, codeWriteRejected, werr.Error())
	}
}

// canceledBeforeSend 判断「确定没生效」的写错误是否是本地取消（发送前就被调用方取消 / 超时）。
// 这不是提供方的拒绝：条目必须退回 pending 保留冻结正文，绝不能落 cancelled 终态。
// 只有 *WriteError 会带 context 哨兵（docprovider.WriteError.Unwrap），裸的 context.Canceled 不在此列：
// 它的效果不明，仍要按 C19 只读查证。
func canceledBeforeSend(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// validateTargetDoc 校验读取到的文档仍是可写的目标：id 一致、已发布未归档未删除、仍在目标集合。
// 返回非空的错误码表示不能写入。
func validateTargetDoc(doc docprovider.Document, it *dueItem) string {
	switch {
	case doc.ID != it.TargetDocID:
		return codeDocIDMismatch
	case !doc.Published || doc.Archived || doc.Deleted:
		return codeDocNotWritable
	case doc.CollectionID != it.TargetCollectionID:
		return codeDocWrongCollection
	}
	return ""
}

// entryBlock 返回条目的 Markdown 块：已经冻结过（payload_md 非空）时原样复用，否则渲染并（只在首次尝试前）补齐
// 工作项当前的 PR 号。
func (s *Service) entryBlock(ctx context.Context, conn *pgxpool.Conn, it *dueItem) (string, error) {
	if it.PayloadMD != nil && *it.PayloadMD != "" {
		return *it.PayloadMD, nil
	}
	var p Payload
	if err := json.Unmarshal(it.Payload, &p); err != nil {
		return "", fmt.Errorf("解析决策条目 %d 的 payload：%w", it.ID, err)
	}
	s.backfillPR(ctx, conn, &p)
	return RenderEntry(it.Marker, p), nil
}

// backfillPR 在首次冻结 payload_md 之前，用工作项当前的信息补齐 PR 号（绝不调用 GitHub）：
// 只有工作项仍处于同一个阶段、同一轮时才采用它的 pr_number；设计阶段的条目还可以采用 design_pr。
func (s *Service) backfillPR(ctx context.Context, conn *pgxpool.Conn, p *Payload) {
	var phase string
	var implRound int
	var pr, designPR *int
	err := conn.QueryRow(ctx, `SELECT phase, impl_round, pr_number, design_pr FROM dev_work_items WHERE id=$1`, p.WorkItemID).
		Scan(&phase, &implRound, &pr, &designPR)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			s.log.Warn("补齐决策条目的 PR 号", "work_item", p.WorkItemID, "err", err)
		}
		return
	}
	if phase == p.Phase && implRound == p.ImplRound && pr != nil {
		p.PRNumber = pr
	}
	if p.Phase == devreport.PhaseDesign && designPR != nil {
		p.DesignPR = designPR
	}
}

// beginOutcome 是 beginAttempt 的结果。
type beginOutcome struct {
	// attempted 表示这一行拿到了（pending -> attempted），可以发起写请求。
	attempted bool
	// reason 非空表示配置已变化，条目不能继续（取消）。
	reason string
	// tokenRotated 表示同一目标轮换了 token：条目保持 pending，应重新加载新凭据后重试。
	tokenRotated bool
}

// beginAttempt 在短事务里重新核对配置、冻结 payload_md，并把 pending 改成 attempted。usedGen 是发起写入时手上凭据的
// 代次：事务里读到同一目标却换了代次，说明 token 刚被轮换，绝不能拿旧凭据去写。返回 attempted=false 表示没有
// 拿到这一行（已被别的处理改掉）；reason 非空表示配置已变化。
func (s *Service) beginAttempt(ctx context.Context, conn *pgxpool.Conn, it *dueItem, usedGen int64, block string,
	revision int64,
) (beginOutcome, error) {
	var out beginOutcome
	err := store.TxOn(ctx, conn, func(tx pgx.Tx) error {
		cfg, err := store.GetDecisionDocForShare(ctx, tx, it.RepoID)
		if err != nil {
			return err
		}
		if !sameTarget(cfg, it) {
			out.reason = codeConfigChanged
			return nil
		}
		if cfg.Generation != usedGen {
			out.tokenRotated = true
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='attempted',
			payload_md=coalesce(payload_md, $2), attempts=attempts+1, attempted_revision=$3, attempted_at=now(),
			next_retry_at=now() + make_interval(secs => $4), last_error_code='', last_error='', updated_at=now()
			WHERE id=$1 AND state='pending'`, it.ID, block, revision, int64(reconcileBackoff(it.Attempts).Seconds()))
		if err != nil {
			return err
		}
		out.attempted = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return beginOutcome{}, err
	}
	return out, nil
}

// processAttempted 处理上次结果不明（或进程崩溃时正在写入）的条目：只读查证，绝不再写。
func (s *Service) processAttempted(ctx context.Context, conn *pgxpool.Conn, it *dueItem) error {
	tok, reason, err := s.loadTarget(ctx, conn, it)
	if err != nil {
		return err
	}
	if reason != "" || tok == nil {
		// 配置被清除或轮换到新文档：手上没有旧文档的写 token，无法查证。
		return s.unknown(ctx, conn, it, stateAttempted, codeUpdateOutcomeUnknown, "配置已变更，无法查证是否已写入")
	}
	w, err := s.writer(tok.BaseURL, tok.Token)
	if err != nil {
		return s.retryAttempted(ctx, conn, it, codeWriterConfig, err.Error())
	}
	return s.verifyWith(ctx, conn, it, w, tok.Generation, codeUpdateOutcomeUnknown)
}

// verify 在写请求结果不明之后立即尝试一次只读查证。ctx 已取消时保留 attempted 等下一轮。
// usedGen 是发起这次写请求时手上凭据的代次：查证遇到鉴权拒绝且 token 已轮换时换新凭据再读，绝不重发写请求。
func (s *Service) verify(ctx context.Context, conn *pgxpool.Conn, it *dueItem, w docprovider.DocumentWriter, usedGen int64,
	msg string,
) error {
	if ctx.Err() != nil {
		return s.retryAttempted(ctx, conn, it, codeUpdateOutcomeUnknown, msg)
	}
	return s.verifyWith(ctx, conn, it, w, usedGen, codeUpdateOutcomeUnknown)
}

// verifyWith 只读读取目标文档，按精确 marker 判断写入是否生效。读取临时失败保持 attempted 退避；永久失败转 unknown。
//
// 同一目标轮换了 token、旧凭据被撤销时，用当前配置的凭据再只读查证，最多重载 tokenRotationRetries 次（连续轮换
// A→B→C 时每次都换到最新代次）；预算用尽保持 attempted 等下一轮，绝不转 unknown。这只是换一份凭据读，绝不重发写请求。
func (s *Service) verifyWith(ctx context.Context, conn *pgxpool.Conn, it *dueItem, w docprovider.DocumentWriter, usedGen int64,
	code string,
) error {
	for rotationRound := 0; ; rotationRound++ {
		doc, err := w.GetDocument(ctx, it.TargetDocID)
		if err == nil {
			if hasMarker(doc.Content, it.Marker) {
				return s.delivered(ctx, conn, it, stateAttempted, doc.Revision)
			}
			// 远端读到了文档但没有 marker：无法证明写入生效，也不能再写（可能用户删掉了这一条）。
			return s.unknown(ctx, conn, it, stateAttempted, code, "文档中没有找到条目")
		}
		if ctx.Err() != nil || !docprovider.PermanentReadError(err) {
			return s.retryAttempted(ctx, conn, it, code, err.Error())
		}
		if !docprovider.IsAuthRejected(err) {
			return s.unknown(ctx, conn, it, stateAttempted, code, err.Error())
		}
		// 旧凭据被撤销：原子地判断是轮换了 token 还是真的拒绝；只有真的拒绝才转 unknown。
		rotated, rerr := s.resolveAuthRejection(ctx, conn, it, usedGen, stateAttempted, terminalUnknown, code, err.Error())
		if rerr != nil {
			return s.retryAttempted(ctx, conn, it, code, rerr.Error())
		}
		if !rotated {
			return nil
		}
		if rotationRound >= tokenRotationRetries {
			// 预算用尽：保持 attempted，等下一轮只读查证，绝不把不明结果打成终态。
			return s.retryAttempted(ctx, conn, it, code, err.Error())
		}
		fresh, gen, ok, ferr := s.currentWriter(ctx, conn, it)
		if ferr != nil {
			return s.retryAttempted(ctx, conn, it, code, ferr.Error())
		}
		if !ok {
			// 配置已变更（换文档 / 清除 / 目标进入只读集合），无法用当前配置查证。
			return s.unknown(ctx, conn, it, stateAttempted, code, "配置已变更，无法查证是否已写入")
		}
		w, usedGen = fresh, gen
	}
}

// delivered 把条目改成终态 delivered（只从预期的状态转移）。
func (s *Service) delivered(ctx context.Context, conn *pgxpool.Conn, it *dueItem, from string, revision int64) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, `UPDATE dev_decision_syncs SET state='delivered', delivered_revision=$3, delivered_at=now(),
		next_retry_at=NULL, last_error_code='', last_error='', updated_at=now() WHERE id=$1 AND state=$2`,
		it.ID, from, revision)
	return err
}

// 终态收尾语句：按 id 与来源状态条件更新。普通确定性拒绝与鉴权拒绝的原子判定共用同一份 SQL（单一定义来源）。
const (
	cancelStateSQL = `UPDATE dev_decision_syncs SET state='cancelled', next_retry_at=NULL, last_error_code=$3,
		last_error=$4, updated_at=now() WHERE id=$1 AND state=$2`
	unknownStateSQL = `UPDATE dev_decision_syncs SET state='unknown', next_retry_at=NULL, last_error_code=$3,
		last_error=$4, updated_at=now() WHERE id=$1 AND state=$2`
)

// cancel 把条目改成终态 cancelled。
func (s *Service) cancel(ctx context.Context, conn *pgxpool.Conn, it *dueItem, from, code, msg string) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, cancelStateSQL, it.ID, from, code, safeErrorText(msg))
	return err
}

// unknown 把条目改成终态 unknown（永不自动回到 pending，只能人工对账）。
func (s *Service) unknown(ctx context.Context, conn *pgxpool.Conn, it *dueItem, from, code, msg string) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, unknownStateSQL, it.ID, from, code, safeErrorText(msg))
	return err
}

// retryPending 保持 pending，按指数退避安排下一次处理。
func (s *Service) retryPending(ctx context.Context, conn *pgxpool.Conn, it *dueItem, attempts int, code, msg string) error {
	return s.retryState(ctx, conn, it, statePending, attempts, code, msg)
}

// retryAttempted 保持 attempted，按指数退避安排下一次只读查证。
func (s *Service) retryAttempted(ctx context.Context, conn *pgxpool.Conn, it *dueItem, code, msg string) error {
	return s.retryState(ctx, conn, it, stateAttempted, it.Attempts, code, msg)
}

func (s *Service) retryState(ctx context.Context, conn *pgxpool.Conn, it *dueItem, state string, attempts int, code, msg string) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, `UPDATE dev_decision_syncs SET next_retry_at=now() + make_interval(secs => $3),
		last_error_code=$4, last_error=$5, updated_at=now() WHERE id=$1 AND state=$2`,
		it.ID, state, int64(reconcileBackoff(attempts).Seconds()), code, safeErrorText(msg))
	return err
}

// backToPending 把 attempted 改回 pending（确定没有写入时）：立即重试，保留已经冻结的 payload_md。
func (s *Service) backToPending(ctx context.Context, conn *pgxpool.Conn, it *dueItem, code, msg string) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, `UPDATE dev_decision_syncs SET state='pending', next_retry_at=now(), last_error_code=$2,
		last_error=$3, updated_at=now() WHERE id=$1 AND state='attempted'`, it.ID, code, safeErrorText(msg))
	return err
}

// backToPendingWait 把 attempted 改回 pending 并推迟到 wait 之后（可信限流）。
func (s *Service) backToPendingWait(ctx context.Context, conn *pgxpool.Conn, it *dueItem, wait time.Duration, code, msg string) error {
	ctx, cancel := s.statusCtx(ctx)
	defer cancel()
	_, err := conn.Exec(ctx, `UPDATE dev_decision_syncs SET state='pending', next_retry_at=now() + make_interval(secs => $2),
		last_error_code=$3, last_error=$4, updated_at=now() WHERE id=$1 AND state='attempted'`,
		it.ID, waitSeconds(wait), code, safeErrorText(msg))
	return err
}

// waitSeconds 把等待时间换算成 make_interval 的秒数，至少 1 秒：服务端给的 Retry-After 可能小于一秒，
// 取整成 0 会让条目立刻再次被处理。
func waitSeconds(d time.Duration) int64 {
	return max(int64(math.Ceil(d.Seconds())), 1)
}

// reconcileBackoff 返回第 attempts 次失败后的等待时间：指数增长，封顶 reconcileBackoffMax。
func reconcileBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	d := reconcileBackoffBase << min(attempts, 8)
	return min(d, reconcileBackoffMax)
}

// bearerRe 匹配错误文本里可能被远端回显的 Authorization 头。
var bearerRe = regexp.MustCompile(`(?i)(bearer\s+)\S+`)

// safeErrorText 是写入 last_error 之前的最后一道防线：远端说明可能回显 Authorization，任何 Bearer 凭据都替换掉，
// 去掉 Postgres 文本不能保存的 NUL 字节，并按长度截断（不切开多字节字符）。docprovider 已经不透传远端 message，
// 这里再保证不会因为别的原因把凭据落库、或者写入非法字节让数据库报 22021。
func safeErrorText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = bearerRe.ReplaceAllString(s, "${1}[已隐藏]")
	return shorten.Text(s, maxErrorLen)
}
