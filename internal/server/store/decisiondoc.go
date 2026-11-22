package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// 「待决策清单」文档的配置：仓库用一份独立、受限的写 token 往一篇专用文档追加 AI 开发的决策疑难点。
// 它和只读的文档绑定（repo_doc_bindings）是两回事，token 用同样的信封加密，只写不读。

// DecisionDoc 是一个仓库的「待决策清单」配置（不含 token）。
type DecisionDoc struct {
	RepoID       int64
	Provider     string
	BaseURL      string
	DocID        string
	CollectionID string
	// Generation 是配置代次：新建时由序列生成，重设（含只轮换 token）时推进。
	Generation int64
	CreatedBy  string
	UpdatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// ObservedUnits 是最近一次按 UTF-16 code unit 量到的文档篇幅；超过推荐值时 WarningCode 记 doc_near_limit。
	ObservedUnits *int64
	ObservedAt    *time.Time
	WarningCode   string
	WarnedAt      *time.Time
}

// decisionDocCols 的顺序必须与 scanDecisionDoc 的扫描顺序一致。doc_id 以文本返回，避免调用方依赖 uuid 类型。
const decisionDocCols = `repo_id, provider, base_url, doc_id::text, collection_id, generation, created_by, updated_by,
	created_at, updated_at, observed_units, observed_at, warning_code, warned_at`

// DecisionDocTokenAAD 是「待决策清单」token 的附加认证数据：密文只能在同一个仓库的这个用途中解密，
// 不能与只读绑定的 token 互换。
func DecisionDocTokenAAD(repoID int64) []byte { return secretbox.AAD("doc", repoID, "decision_token") }

func scanDecisionDoc(row pgx.Row) (*DecisionDoc, error) {
	var d DecisionDoc
	err := row.Scan(&d.RepoID, &d.Provider, &d.BaseURL, &d.DocID, &d.CollectionID, &d.Generation,
		&d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.ObservedUnits, &d.ObservedAt, &d.WarningCode, &d.WarnedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDecisionDoc 返回仓库的「待决策清单」配置；没有配置时返回 nil。
func GetDecisionDoc(ctx context.Context, q Querier, repoID int64) (*DecisionDoc, error) {
	return scanDecisionDoc(q.QueryRow(ctx, `SELECT `+decisionDocCols+` FROM repo_decision_docs WHERE repo_id=$1`, repoID))
}

// GetDecisionDocForShare 与 GetDecisionDoc 相同，但对这一行加 FOR SHARE：入队在短事务里锁定当前配置，避免与配置变更
// （重设 / 清除）交错读到一半旧一半新的目标。必须在事务中调用。
func GetDecisionDocForShare(ctx context.Context, q Querier, repoID int64) (*DecisionDoc, error) {
	return scanDecisionDoc(q.QueryRow(ctx, `SELECT `+decisionDocCols+` FROM repo_decision_docs WHERE repo_id=$1 FOR SHARE`, repoID))
}

// ListDecisionDocs 返回全部仓库的「待决策清单」配置，按仓库 ID 排序。
func ListDecisionDocs(ctx context.Context, q Querier) ([]DecisionDoc, error) {
	rows, err := q.Query(ctx, `SELECT `+decisionDocCols+` FROM repo_decision_docs ORDER BY repo_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DecisionDoc, error) {
		var d DecisionDoc
		err := row.Scan(&d.RepoID, &d.Provider, &d.BaseURL, &d.DocID, &d.CollectionID, &d.Generation,
			&d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt,
			&d.ObservedUnits, &d.ObservedAt, &d.WarningCode, &d.WarnedAt)
		return d, err
	})
}

// PutDecisionDoc 加密保存（新建或覆盖）仓库的「待决策清单」配置。更新时保留第一次写入的 created_by / created_at，
// 推进 generation；当 doc_id 或 base_url 变化（文档轮转）时清空旧的观测篇幅与告警。参数由调用方校验。
func PutDecisionDoc(ctx context.Context, q Execer, masterKey []byte, d DecisionDoc, token string) error {
	sealed, err := secretbox.Seal(masterKey, DecisionDocTokenAAD(d.RepoID), []byte(token))
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO repo_decision_docs (repo_id, provider, base_url, doc_id, collection_id,
			token_ciphertext, token_wrapped_dek, created_by, updated_by)
		VALUES ($1,$2,$3,$4::uuid,$5,$6,$7,$8,$8)
		ON CONFLICT (repo_id) DO UPDATE SET provider=EXCLUDED.provider, base_url=EXCLUDED.base_url,
		doc_id=EXCLUDED.doc_id, collection_id=EXCLUDED.collection_id,
		token_ciphertext=EXCLUDED.token_ciphertext, token_wrapped_dek=EXCLUDED.token_wrapped_dek,
		generation=nextval('repo_decision_docs_generation_seq'), updated_by=EXCLUDED.updated_by, updated_at=now(),
		observed_units=CASE WHEN repo_decision_docs.doc_id<>EXCLUDED.doc_id OR repo_decision_docs.base_url<>EXCLUDED.base_url
			THEN NULL ELSE repo_decision_docs.observed_units END,
		observed_at=CASE WHEN repo_decision_docs.doc_id<>EXCLUDED.doc_id OR repo_decision_docs.base_url<>EXCLUDED.base_url
			THEN NULL ELSE repo_decision_docs.observed_at END,
		warning_code=CASE WHEN repo_decision_docs.doc_id<>EXCLUDED.doc_id OR repo_decision_docs.base_url<>EXCLUDED.base_url
			THEN '' ELSE repo_decision_docs.warning_code END,
		warned_at=CASE WHEN repo_decision_docs.doc_id<>EXCLUDED.doc_id OR repo_decision_docs.base_url<>EXCLUDED.base_url
			THEN NULL ELSE repo_decision_docs.warned_at END`,
		d.RepoID, d.Provider, d.BaseURL, d.DocID, d.CollectionID, sealed.Ciphertext, sealed.WrappedDEK, d.CreatedBy)
	return err
}

// DeleteDecisionDoc 删除仓库的「待决策清单」配置，返回是否删除了一行。
func DeleteDecisionDoc(ctx context.Context, q Execer, repoID int64) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM repo_decision_docs WHERE repo_id=$1`, repoID)
	return tag.RowsAffected() > 0, err
}

// DecisionDocToken 是解密后的「待决策清单」配置，用于访问写接口。
type DecisionDocToken struct {
	DecisionDoc
	Token string // 解密后的 token，不写入日志、不返回给前端
}

// ResolveDecisionDocToken 读取并解密仓库的「待决策清单」配置；没有配置时返回 nil。配置信息和 token 来自同一次读取。
func ResolveDecisionDocToken(ctx context.Context, q Querier, masterKey []byte, repoID int64) (*DecisionDocToken, error) {
	var d DecisionDocToken
	var ciphertext, wrapped []byte
	err := q.QueryRow(ctx, `SELECT `+decisionDocCols+`, token_ciphertext, token_wrapped_dek
		FROM repo_decision_docs WHERE repo_id=$1`, repoID).Scan(
		&d.RepoID, &d.Provider, &d.BaseURL, &d.DocID, &d.CollectionID, &d.Generation,
		&d.CreatedBy, &d.UpdatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.ObservedUnits, &d.ObservedAt, &d.WarningCode, &d.WarnedAt, &ciphertext, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := secretbox.Open(masterKey, DecisionDocTokenAAD(repoID), secretbox.Sealed{Ciphertext: ciphertext, WrappedDEK: wrapped})
	if err != nil {
		return nil, fmt.Errorf("解密仓库 %d 的待决策清单 token：%w", repoID, err)
	}
	d.Token = string(v)
	return &d, nil
}

// UpdateDecisionDocSize 记下最近一次量到的文档篇幅，并在 warningCode 非空时记录告警。只更新当前代次的配置：
// 配置在读取之后被重设 / 轮转时代次已经变化，不把旧文档的篇幅记到新配置上。
func UpdateDecisionDocSize(ctx context.Context, q Execer, repoID, generation, units int64, warningCode string) error {
	_, err := q.Exec(ctx, `UPDATE repo_decision_docs SET observed_units=$3, observed_at=now(),
		warning_code=CASE WHEN $4::text<>'' THEN $4 ELSE warning_code END,
		warned_at=CASE WHEN $4::text<>'' THEN now() ELSE warned_at END
		WHERE repo_id=$1 AND generation=$2`, repoID, generation, units, warningCode)
	return err
}
