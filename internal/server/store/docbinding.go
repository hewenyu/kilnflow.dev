package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// 仓库的文档绑定：仓库可以绑定一个 Outline collection，token 与 secret / npm 一样用信封加密，
// 只写不读。

// DocBinding 是一个仓库的文档绑定（不含 token）。
type DocBinding struct {
	RepoID         int64
	Provider       string
	BaseURL        string
	CollectionID   string
	CollectionName string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Generation 是绑定代次：新建时由序列生成，重绑（覆盖）时推进到下一个值。
	Generation int64
	// UpdatedBy 是最近一次保存配置的操作者；历史行没有这个记录，为 nil（显示"未记录"，不回填成 CreatedBy）。
	UpdatedBy *string
	// 显式只读连接验证的结果。没有任何验证时 CheckStatus 为 nil，其余为 NULL / 空。
	CheckStartedAt *time.Time
	CheckedAt      *time.Time
	CheckedBy      *string
	// CheckStatus 是 ok / empty / failed；nil 表示配置已保存、尚未验证。
	CheckStatus *string
	// CheckErrorCode 是固定枚举的错误码（不含远端原始信息）。
	CheckErrorCode string
	// CheckDocCount 是列举到的文档篇数；列举没有完成时为 nil（不默认 0）。
	CheckDocCount *int
	// CheckSampleDocID 是抽查了正文的文档 id（空集合或失败时为空）。
	CheckSampleDocID string
	// CollectionURL / ObservedCollectionName 是验证时从 Outline collections.list 观测到的文档集地址与名称。
	CollectionURL          string
	ObservedCollectionName string
}

// docBindingCols 的顺序必须与 DocBinding 的字段顺序一致（按位置扫描）。
const docBindingCols = `repo_id, provider, base_url, collection_id, collection_name, created_by, created_at, updated_at, generation,
	updated_by, check_started_at, checked_at, checked_by, check_status, check_error_code, check_doc_count, check_sample_doc_id,
	collection_url, observed_collection_name`

// NormalizeDocBaseURL 规范化文档系统地址，用于比较和保存：去掉首尾空白与结尾斜杠，scheme 与 host 转小写，
// 移除 http :80 / https :443 默认端口。这样 https://outline.example.com、https://OUTLINE.example.com:443/ 和
// https://outline.example.com/ 视为同一个站点，"只读绑定与待决策清单不能落在同一集合"的检查无法用写法差异绕过。
// 无法解析时退回只去尾部斜杠的原文（由调用方另行校验）。
func NormalizeDocBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		// JoinHostPort 会给 IPv6 字面量补方括号。
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		// Hostname() 剥掉了 IPv6 的方括号，去掉默认端口后要补回来，否则 http://[::1]:80 会变成非法的 http://::1。
		host = "[" + host + "]"
	}
	u.Host = host
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// docTokenAAD 是文档绑定 token 的附加认证数据：密文只能在同一个仓库的这一行中解密。
func docTokenAAD(repoID int64) []byte { return secretbox.AAD("doc", repoID, "token") }

// GetDocBinding 返回仓库的文档绑定；没有绑定时返回 nil。
func GetDocBinding(ctx context.Context, q Querier, repoID int64) (*DocBinding, error) {
	rows, err := q.Query(ctx, `SELECT `+docBindingCols+` FROM repo_doc_bindings WHERE repo_id=$1`, repoID)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectOneRow(rows, pgx.RowToAddrOfStructByPos[DocBinding])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ListDocBindings 返回全部仓库的文档绑定，按仓库 ID 排序。
func ListDocBindings(ctx context.Context, q Querier) ([]DocBinding, error) {
	rows, err := q.Query(ctx, `SELECT `+docBindingCols+` FROM repo_doc_bindings ORDER BY repo_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DocBinding])
}

// PutDocBinding 加密保存（新建或覆盖）仓库的文档绑定。更新时保留第一次写入的 created_by / created_at，
// 更新 updated_by / updated_at，推进绑定代次，并清空全部验证结果与观测字段（即使只是轮换 token：旧验证对应的是旧连接）。
// 参数由调用方校验。
func PutDocBinding(ctx context.Context, q Execer, masterKey []byte, b DocBinding, token string) error {
	sealed, err := secretbox.Seal(masterKey, docTokenAAD(b.RepoID), []byte(token))
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO repo_doc_bindings (repo_id, provider, base_url, collection_id, collection_name,
			token_ciphertext, token_wrapped_dek, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (repo_id) DO UPDATE SET provider=EXCLUDED.provider, base_url=EXCLUDED.base_url,
		collection_id=EXCLUDED.collection_id, collection_name=EXCLUDED.collection_name,
		token_ciphertext=EXCLUDED.token_ciphertext, token_wrapped_dek=EXCLUDED.token_wrapped_dek,
		updated_by=EXCLUDED.updated_by,
		generation=nextval('repo_doc_bindings_generation_seq'), updated_at=now(),
		check_started_at=NULL, checked_at=NULL, checked_by=NULL, check_status=NULL, check_error_code='',
		check_doc_count=NULL, check_sample_doc_id='', collection_url='', observed_collection_name=''`,
		b.RepoID, b.Provider, b.BaseURL, b.CollectionID, b.CollectionName, sealed.Ciphertext, sealed.WrappedDEK,
		b.CreatedBy, b.UpdatedBy)
	return err
}

// DocCheckResult 是一次显式只读连接验证的结果（不含 token 与远端原始信息）。
type DocCheckResult struct {
	StartedAt time.Time
	CheckedAt time.Time
	CheckedBy string
	// Status 是 ok / empty / failed。
	Status string
	// ErrorCode 是固定枚举的错误码（Status=failed 时非空）。
	ErrorCode string
	// DocCount 是列举到的篇数；列举没有完成时为 nil（不默认 0）。
	DocCount *int
	// SampleDocID 是抽查了正文的文档 id。
	SampleDocID string
	// CollectionURL / ObservedCollectionName 是 collections.list 观测到的文档集地址与名称。
	CollectionURL          string
	ObservedCollectionName string
}

// SaveDocBindingCheck 保存一次验证结果。只更新与这次验证对应的绑定代次（generation）；绑定在验证期间被重绑或解绑时
// 影响 0 行、返回 false，绝不把旧连接的结果写到新配置上。它不修改 updated_at / updated_by / generation。
func SaveDocBindingCheck(ctx context.Context, q Execer, repoID, generation int64, r DocCheckResult) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE repo_doc_bindings
		SET check_started_at=$3, checked_at=$4, checked_by=$5, check_status=$6, check_error_code=$7,
			check_doc_count=$8, check_sample_doc_id=$9, collection_url=$10, observed_collection_name=$11
		WHERE repo_id=$1 AND generation=$2`,
		repoID, generation, r.StartedAt, r.CheckedAt, r.CheckedBy, r.Status, r.ErrorCode,
		r.DocCount, r.SampleDocID, r.CollectionURL, r.ObservedCollectionName)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteDocBinding 删除仓库的文档绑定，返回是否删除了一行。
func DeleteDocBinding(ctx context.Context, q Execer, repoID int64) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM repo_doc_bindings WHERE repo_id=$1`, repoID)
	return tag.RowsAffected() > 0, err
}

// DocBindingToken 是解密后的文档绑定，用于访问文档系统。
type DocBindingToken struct {
	DocBinding
	Token string // 解密后的 token，不写入日志、不返回给前端
}

// ResolveDocToken 读取并解密仓库的文档绑定；没有绑定时返回 nil。绑定信息和 token 来自同一次读取。
func ResolveDocToken(ctx context.Context, q Querier, masterKey []byte, repoID int64) (*DocBindingToken, error) {
	var t DocBindingToken
	var ciphertext, wrapped []byte
	err := q.QueryRow(ctx, `SELECT `+docBindingCols+`, token_ciphertext, token_wrapped_dek FROM repo_doc_bindings WHERE repo_id=$1`, repoID).Scan(
		&t.RepoID, &t.Provider, &t.BaseURL, &t.CollectionID, &t.CollectionName,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.Generation,
		&t.UpdatedBy, &t.CheckStartedAt, &t.CheckedAt, &t.CheckedBy, &t.CheckStatus, &t.CheckErrorCode, &t.CheckDocCount,
		&t.CheckSampleDocID, &t.CollectionURL, &t.ObservedCollectionName, &ciphertext, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := secretbox.Open(masterKey, docTokenAAD(repoID), secretbox.Sealed{Ciphertext: ciphertext, WrappedDEK: wrapped})
	if err != nil {
		return nil, fmt.Errorf("解密仓库 %d 的文档 token：%w", repoID, err)
	}
	t.Token = string(v)
	return &t, nil
}
