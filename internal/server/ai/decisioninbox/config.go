package decisioninbox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 配置与人工对账：「待决策清单」的文档设置 / 清除，以及 unknown 条目的只读复查和人工结案。
// 这些操作由 admin（CLI / Dashboard）调用。

// uuidRe 匹配标准 UUID 文本（Outline 的文档 id 是 uuid）。
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrNotConfigured 表示仓库没有配置「待决策清单」。
var ErrNotConfigured = errors.New("仓库没有配置待决策清单")

// ErrNoToken 表示没有可用的写 token，无法只读查证。
var ErrNoToken = errors.New("当前配置已变更，无法用原 token 查证")

// errSyncStateChanged 表示持锁后重读到的状态已经不是预期的来源状态：排队等锁期间条目被并发改掉
// （后台处理转 unknown / cancelled、人工结案等），条件更新没有命中。绝不能把它当成成功。
var errSyncStateChanged = errors.New("条目状态已经变化，请刷新后重试")

// SetResult 是设置「待决策清单」的结果。
type SetResult struct {
	Doc *store.DecisionDoc
	// Cancelled / Unknown 是文档轮转时被取消 / 转为结果不明的旧条目数量；只轮换 token 时都为 0。
	Cancelled int64
	Unknown   int64
}

// ClearResult 是清除「待决策清单」的结果。
type ClearResult struct {
	Cancelled int64
	Unknown   int64
}

// SetDecisionDoc 设置或重设仓库的「待决策清单」：用新 token 只读校验目标文档，保存加密配置，并按轮转语义处理旧条目。
// 文档轮转（doc_id 或 base_url 变化）时把旧目标的 pending 取消、attempted 转为 unknown；只轮换 token 时不动队列。
func (s *Service) SetDecisionDoc(ctx context.Context, repoID int64, baseURL, docID, token, actor string) (*SetResult, error) {
	done, err := s.track()
	if err != nil {
		return nil, err
	}
	defer done()

	// UUID 大小写不敏感：统一存小写，避免同一篇文档用大写写法重设时被当成换文档、误取消 pending 条目。
	docID = strings.ToLower(strings.TrimSpace(docID))
	if !uuidRe.MatchString(docID) {
		return nil, errors.New("文档 id 不是合法的 UUID")
	}
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return nil, errors.New("写 token 为空")
	case len(token) > maxDocToken:
		return nil, fmt.Errorf("写 token 超过 %d 字节", maxDocToken)
	}
	baseURL = store.NormalizeDocBaseURL(baseURL)
	octx, cancel, err := s.linkOp(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var out *SetResult
	err = s.db.WithAdvisoryLock(octx, store.DocBindingLockKey(repoID), func(ctx context.Context, conn *pgxpool.Conn) error {
		// 读取仓库和只读绑定；未显式给出 base_url 时回退到只读绑定的地址。
		var repo *store.Repo
		var binding *store.DocBinding
		if err := store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			var err error
			if repo, err = store.GetRepo(ctx, tx, repoID); err != nil {
				return err
			}
			binding, err = store.GetDocBinding(ctx, tx, repoID)
			return err
		}); err != nil {
			return err
		}
		if baseURL == "" {
			if binding == nil {
				return errors.New("请提供文档系统地址（--base-url）")
			}
			baseURL = store.NormalizeDocBaseURL(binding.BaseURL)
		}
		if err := validateBaseURL(baseURL); err != nil {
			return err
		}

		w, err := s.writer(baseURL, token)
		if err != nil {
			return err
		}
		doc, err := w.GetDocument(ctx, docID)
		if err != nil {
			return fmt.Errorf("读取目标文档（检查地址、token 和权限）：%w", err)
		}
		if err := checkTargetDoc(doc, docID); err != nil {
			return err
		}

		out = &SetResult{}
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			// 与只读绑定使用同一个集合会让人分不清哪些内容允许 Kiln 写：拒绝。
			if b, err := store.GetDocBinding(ctx, tx, repoID); err != nil {
				return err
			} else if b != nil && b.Provider == "outline" && store.NormalizeDocBaseURL(b.BaseURL) == baseURL && b.CollectionID == doc.CollectionID {
				return errors.New("目标文档所在的集合已被只读文档绑定使用，请为待决策清单使用独立的非绑定集合")
			}
			old, err := store.GetDecisionDoc(ctx, tx, repoID)
			if err != nil {
				return err
			}
			if err := store.PutDecisionDoc(ctx, tx, s.masterKey, store.DecisionDoc{
				RepoID: repoID, Provider: "outline", BaseURL: baseURL, DocID: docID,
				CollectionID: doc.CollectionID, CreatedBy: actor, UpdatedBy: actor,
			}, token); err != nil {
				return err
			}
			rotated := old != nil && (old.DocID != docID || store.NormalizeDocBaseURL(old.BaseURL) != baseURL)
			if rotated {
				out.Cancelled, out.Unknown, err = rotateEntries(ctx, tx, old, repoID)
				if err != nil {
					return err
				}
			}
			if out.Doc, err = store.GetDecisionDoc(ctx, tx, repoID); err != nil {
				return err
			}
			return store.RecordConfigChange(ctx, tx, repo.OwnerID, actor, "doc.decision_set", map[string]any{
				"repo": repo.FullName(), "base_url": baseURL, "doc_id": docID, "collection_id": doc.CollectionID,
				"rotated": rotated, "cancelled": out.Cancelled, "unknown": out.Unknown,
			})
		})
	})
	return out, err
}

// checkTargetDoc 校验用新 token 读到的目标文档：必须正是请求的那一篇、有修订号、在集合里，且已发布、未归档、未删除。
func checkTargetDoc(doc docprovider.Document, docID string) error {
	switch {
	case !strings.EqualFold(doc.ID, docID):
		return errors.New("outline 返回的文档 id 与请求不一致")
	case doc.Revision <= 0:
		return errors.New("目标文档没有有效的修订号")
	case doc.CollectionID == "":
		return errors.New("目标文档不在任何集合中")
	case !doc.Published || doc.Archived || doc.Deleted:
		return errors.New("目标文档必须是已发布、未归档、未删除的文档")
	}
	return nil
}

// rotateEntries 在文档轮转（换了 doc_id 或 base_url）时处理旧目标上未结束的条目：pending 不会再写入，取消；
// attempted 无法再查证，转为 unknown。delivered / unknown 保持不动。
func rotateEntries(ctx context.Context, tx pgx.Tx, old *store.DecisionDoc, repoID int64) (cancelled, unknown int64, err error) {
	tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='cancelled', next_retry_at=NULL,
		last_error_code='target_rotated', updated_at=now()
		WHERE repo_id=$1 AND target_provider=$2 AND target_base_url=$3 AND target_doc_id=$4::uuid AND state='pending'`,
		repoID, old.Provider, store.NormalizeDocBaseURL(old.BaseURL), old.DocID)
	if err != nil {
		return 0, 0, err
	}
	cancelled = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='unknown', next_retry_at=NULL,
		last_error_code='target_rotated_while_attempted', updated_at=now()
		WHERE repo_id=$1 AND target_provider=$2 AND target_base_url=$3 AND target_doc_id=$4::uuid AND state='attempted'`,
		repoID, old.Provider, store.NormalizeDocBaseURL(old.BaseURL), old.DocID)
	if err != nil {
		return 0, 0, err
	}
	unknown = tag.RowsAffected()
	return cancelled, unknown, nil
}

// ClearDecisionDoc 删除仓库的「待决策清单」配置：pending 取消、attempted 转为 unknown，delivered / unknown 不动。
func (s *Service) ClearDecisionDoc(ctx context.Context, repoID int64, actor string) (*ClearResult, error) {
	done, err := s.track()
	if err != nil {
		return nil, err
	}
	defer done()

	octx, cancel, err := s.linkOp(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out := &ClearResult{}
	err = s.db.WithAdvisoryLock(octx, store.DocBindingLockKey(repoID), func(ctx context.Context, conn *pgxpool.Conn) error {
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			repo, err := store.GetRepo(ctx, tx, repoID)
			if err != nil {
				return err
			}
			ok, err := store.DeleteDecisionDoc(ctx, tx, repoID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNotConfigured
			}
			if out.Cancelled, out.Unknown, err = endEntries(ctx, tx, repoID, "config_cleared", "config_cleared_while_attempted"); err != nil {
				return err
			}
			return store.RecordConfigChange(ctx, tx, repo.OwnerID, actor, "doc.decision_clear", map[string]any{
				"repo": repo.FullName(), "cancelled": out.Cancelled, "unknown": out.Unknown,
			})
		})
	})
	return out, err
}

// endEntries 在配置被清除时收尾未结束的条目：pending 取消、attempted 转 unknown。
func endEntries(ctx context.Context, tx pgx.Tx, repoID int64, cancelCode, unknownCode string) (cancelled, unknown int64, err error) {
	tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='cancelled', next_retry_at=NULL,
		last_error_code=$2, updated_at=now() WHERE repo_id=$1 AND state='pending'`, repoID, cancelCode)
	if err != nil {
		return 0, 0, err
	}
	cancelled = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='unknown', next_retry_at=NULL,
		last_error_code=$2, updated_at=now() WHERE repo_id=$1 AND state='attempted'`, repoID, unknownCode)
	if err != nil {
		return 0, 0, err
	}
	unknown = tag.RowsAffected()
	return cancelled, unknown, nil
}

// ReconcileResult 是只读复查的结果。
type ReconcileResult struct {
	Sync *Sync
	// Found 表示文档里找到了条目的精确 marker（已经送达）。
	Found bool
}

// ReconcileSync 对 unknown（或 attempted）的条目做一次只读查证：调用 GetDocument，绝不调用 UpdateDocument。
// 找到精确 marker 转 delivered；没找到时 unknown 保持、attempted 转 unknown 并返回明确的提示。
//
// 锁外那次读取只用于取仓库锁键和快速拒绝终态；权威判断必须在拿到 advisory lock 之后重读最新行：
// 排队等锁期间后台 ProcessRepo 或人工结案可能已经改了状态。状态推进用锁内重读到的状态做条件更新并强制
// 检查影响行数（RowsAffected==1），影响 0 行说明重读之后又被改掉，绝不能返回 Found=true。
func (s *Service) ReconcileSync(ctx context.Context, id int64, actor string) (*ReconcileResult, error) {
	done, err := s.track()
	if err != nil {
		return nil, err
	}
	defer done()
	// 先建立带期限、随服务生命周期取消的 context，再查数据库：连池被占满或行锁被占用时也能被 opGate 取消。
	octx, cancel, err := s.linkOp(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	sync, err := GetSync(octx, s.db, id)
	if err != nil {
		return nil, err
	}
	if sync.State != stateUnknown && sync.State != stateAttempted {
		return nil, fmt.Errorf("只有 unknown / attempted 的条目可以复查，当前是 %s", sync.State)
	}
	res := &ReconcileResult{}
	err = s.db.WithAdvisoryLock(octx, lockKey(sync.RepoID), func(ctx context.Context, conn *pgxpool.Conn) error {
		// 拿锁后重读：等锁期间条目可能已经从 attempted 变成 unknown，或被人工结案为 delivered / cancelled。
		cur, err := GetSync(ctx, conn, id)
		if err != nil {
			return err
		}
		if cur.State != stateUnknown && cur.State != stateAttempted {
			return fmt.Errorf("条目状态已经变化（当前 %s），请刷新后重试", cur.State)
		}
		var tok *store.DecisionDocToken
		var reason string
		if err := store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			cfg, err := store.GetDecisionDocForShare(ctx, tx, cur.RepoID)
			if err != nil {
				return err
			}
			if cfg == nil || cfg.Provider != cur.provider() || store.NormalizeDocBaseURL(cfg.BaseURL) != cur.targetBaseURL() ||
				cfg.DocID != cur.TargetDocID {
				reason = codeConfigChanged
				return nil
			}
			tok, err = store.ResolveDecisionDocToken(ctx, tx, s.masterKey, cur.RepoID)
			return err
		}); err != nil {
			return err
		}
		if reason != "" || tok == nil {
			return ErrNoToken
		}
		w, err := s.writer(tok.BaseURL, tok.Token)
		if err != nil {
			return err
		}
		// 用锁内重读到的目标（doc id、marker）查证，避免用等锁前的旧记录。
		doc, err := w.GetDocument(ctx, cur.TargetDocID)
		if err != nil {
			return fmt.Errorf("读取目标文档：%w", err)
		}
		found := hasMarker(doc.Content, cur.Marker)
		// 状态推进与审计放在同一个短事务里：条件更新使用锁内重读到的状态，RowsAffected=0 表示重读之后
		// 状态又被并发改掉（例如配置轮转，走的是另一把锁），这时绝不能当成送达成功。
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			switch {
			case found:
				tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='delivered', delivered_revision=$3,
					delivered_at=now(), next_retry_at=NULL, last_error_code='', last_error='', updated_at=now()
					WHERE id=$1 AND state=$2`, id, cur.State, doc.Revision)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return errSyncStateChanged
				}
				// 只有确实推进到 delivered 才报告找到；与 CLI 的“已记为 delivered”文案严格一致。
				res.Found = true
			case cur.State == stateAttempted:
				// 没有找到 marker：不能证明写入生效，也不能再写，转成 unknown 交给人对账。
				tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state='unknown', next_retry_at=NULL,
					last_error_code='reconcile_not_found', last_error='文档中没有找到条目', updated_at=now()
					WHERE id=$1 AND state='attempted'`, id)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return errSyncStateChanged
				}
			}
			if err := store.Audit(ctx, tx, actor, "doc.decision_reconcile", map[string]any{
				"sync": id, "repo_id": cur.RepoID, "found": found,
			}); err != nil {
				return err
			}
			// 在同一把锁内读回最新状态返回，保证返回值与数据库、CLI 文案一致。
			res.Sync, err = GetSync(ctx, tx, id)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ResolveSync 人工对账结案：只允许 unknown -> delivered / cancelled，必须写明原因。它只解决通知投递记录，
// 不改变 confirmed_decision 的判定，也不影响 PR 的 kiln:hold 门禁。
func (s *Service) ResolveSync(ctx context.Context, id int64, targetState, note, actor string) (*Sync, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, errors.New("人工结案必须写明原因（--note）")
	}
	if targetState != stateDelivered && targetState != stateCancelled {
		return nil, errors.New("只能结案为 delivered 或 cancelled")
	}
	done, err := s.track()
	if err != nil {
		return nil, err
	}
	defer done()
	// 同 ReconcileSync：先建立受服务生命周期约束的 context，再查数据库。
	octx, cancel, err := s.linkOp(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	sync, err := GetSync(octx, s.db, id)
	if err != nil {
		return nil, err
	}
	if sync.State != stateUnknown {
		return nil, fmt.Errorf("只有 unknown 的条目可以人工结案，当前是 %s", sync.State)
	}
	err = s.db.WithAdvisoryLock(octx, lockKey(sync.RepoID), func(ctx context.Context, conn *pgxpool.Conn) error {
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			// 拿锁后重读：排队等锁期间条目可能已被后台处理或另一次结案改掉，权威判断用最新状态。
			cur, err := GetSync(ctx, tx, id)
			if err != nil {
				return err
			}
			if cur.State != stateUnknown {
				return fmt.Errorf("条目状态已经变化（当前 %s），请刷新后重试", cur.State)
			}
			tag, err := tx.Exec(ctx, `UPDATE dev_decision_syncs SET state=$2, next_retry_at=NULL,
				delivered_at=CASE WHEN $2='delivered' THEN coalesce(delivered_at, now()) ELSE delivered_at END,
				resolved_by=$3, resolved_at=now(), resolution_note=$4, updated_at=now()
				WHERE id=$1 AND state='unknown'`, id, targetState, actor, note)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errSyncStateChanged
			}
			return store.Audit(ctx, tx, actor, "doc.decision_resolve", map[string]any{
				"sync": id, "repo_id": cur.RepoID, "state": targetState, "note": note,
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return GetSync(octx, s.db, id)
}

// Sync 是一条决策同步记录（admin 列表与命令输出的数据）。
type Sync struct {
	ID                int64
	RepoID            int64
	WorkItemID        int64
	TaskID            int64
	EntryIdx          int
	EntryKind         string
	Category          string
	ConfirmedDecision bool
	Marker            string
	TargetDocID       string
	State             string
	Attempts          int
	LastErrorCode     string
	LastError         string
	CreatedAt         time.Time
	AttemptedAt       *time.Time
	DeliveredAt       *time.Time
	ResolvedBy        string
	ResolutionNote    string
	// 目标来源（入队时冻结），用于复查时与当前配置比较；不对 CLI 输出。
	targetProviderValue string
	targetBaseURLValue  string
}

// provider 返回条目入队时冻结的提供方。
func (s *Sync) provider() string { return s.targetProviderValue }

// targetBaseURL 返回条目入队时冻结的站点地址。
func (s *Sync) targetBaseURL() string { return s.targetBaseURLValue }

const syncCols = `s.id, s.repo_id, s.work_item_id, s.task_id, s.entry_idx, s.entry_kind, coalesce(s.category,''),
	s.confirmed_decision, s.marker, s.target_doc_id::text, s.state, s.attempts, s.last_error_code, s.last_error,
	s.created_at, s.attempted_at, s.delivered_at, coalesce(s.resolved_by,''), coalesce(s.resolution_note,''),
	s.target_provider, s.target_base_url`

func scanSync(row pgx.Row) (*Sync, error) {
	var v Sync
	err := row.Scan(&v.ID, &v.RepoID, &v.WorkItemID, &v.TaskID, &v.EntryIdx, &v.EntryKind, &v.Category,
		&v.ConfirmedDecision, &v.Marker, &v.TargetDocID, &v.State, &v.Attempts, &v.LastErrorCode, &v.LastError,
		&v.CreatedAt, &v.AttemptedAt, &v.DeliveredAt, &v.ResolvedBy, &v.ResolutionNote,
		&v.targetProviderValue, &v.targetBaseURLValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// GetSync 返回一条同步记录；不存在时返回错误。
func GetSync(ctx context.Context, q store.Querier, id int64) (*Sync, error) {
	v, err := scanSync(q.QueryRow(ctx, `SELECT `+syncCols+` FROM dev_decision_syncs s WHERE s.id=$1`, id))
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("决策同步记录 %d 不存在", id)
	}
	return v, nil
}

// ListSyncs 返回同步记录，可按仓库和状态筛选，按 ID 倒序，最多 limit 条。
func ListSyncs(ctx context.Context, q store.Querier, repoID *int64, state string, limit int) ([]Sync, error) {
	rows, err := q.Query(ctx, `SELECT `+syncCols+` FROM dev_decision_syncs s
		WHERE ($1::bigint IS NULL OR s.repo_id=$1) AND ($2='' OR s.state=$2)
		ORDER BY s.id DESC LIMIT $3`, repoID, state, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Sync, error) {
		var v Sync
		err := row.Scan(&v.ID, &v.RepoID, &v.WorkItemID, &v.TaskID, &v.EntryIdx, &v.EntryKind, &v.Category,
			&v.ConfirmedDecision, &v.Marker, &v.TargetDocID, &v.State, &v.Attempts, &v.LastErrorCode, &v.LastError,
			&v.CreatedAt, &v.AttemptedAt, &v.DeliveredAt, &v.ResolvedBy, &v.ResolutionNote,
			&v.targetProviderValue, &v.targetBaseURLValue)
		return v, err
	})
}

// validateBaseURL 校验文档系统地址：必须是 http/https 且有主机名。
func validateBaseURL(baseURL string) error {
	if baseURL == "" {
		return errors.New("文档系统地址为空")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("文档系统地址无效：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("文档系统地址必须是 http 或 https")
	}
	if u.Host == "" {
		return errors.New("文档系统地址缺少主机名")
	}
	return nil
}
