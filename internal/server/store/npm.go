package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// 账号级 npm 配置（PLAN #27.2）：release.npm 发布到 @<scope>/<name>，registry、scope、token 每个账号一份。

// NpmAccount 是一个账号的 npm 配置（不含 token）。
type NpmAccount struct {
	OwnerID   int64
	Account   string
	Scope     string
	Registry  string
	Version   int64
	UpdatedBy string
	UpdatedAt time.Time
}

const npmAccountCols = `owner_id, account, scope, registry, version, updated_by, updated_at`

// npmTokenAAD 是 npm token 的附加认证数据：密文只能在同一个账号的这一行中解密。
func npmTokenAAD(ownerID int64) []byte { return secretbox.AAD("npm", ownerID, "token") }

// GetNpmAccount 返回账号的 npm 配置；没有配置时返回 nil。
func GetNpmAccount(ctx context.Context, q Querier, ownerID int64) (*NpmAccount, error) {
	rows, err := q.Query(ctx, `SELECT `+npmAccountCols+` FROM npm_accounts WHERE owner_id=$1`, ownerID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectOneRow(rows, pgx.RowToAddrOfStructByPos[NpmAccount])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// ListNpmAccounts 返回全部账号的 npm 配置，按账号名排序。
func ListNpmAccounts(ctx context.Context, q Querier) ([]NpmAccount, error) {
	rows, err := q.Query(ctx, `SELECT `+npmAccountCols+` FROM npm_accounts ORDER BY lower(account), owner_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[NpmAccount])
}

// PutNpmAccount 加密保存（新建或覆盖）账号的 npm 配置，每次写入都取新的版本号。参数由调用方校验。
func PutNpmAccount(ctx context.Context, q Execer, masterKey []byte, ownerID int64, account, scope, registry, token, actor string) error {
	sealed, err := secretbox.Seal(masterKey, npmTokenAAD(ownerID), []byte(token))
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO npm_accounts (owner_id, account, scope, registry, ciphertext, wrapped_dek, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (owner_id) DO UPDATE SET account=EXCLUDED.account, scope=EXCLUDED.scope, registry=EXCLUDED.registry,
		ciphertext=EXCLUDED.ciphertext, wrapped_dek=EXCLUDED.wrapped_dek, updated_by=EXCLUDED.updated_by, updated_at=now(),
		version=nextval('secret_versions')`, ownerID, account, scope, registry, sealed.Ciphertext, sealed.WrappedDEK, actor)
	return err
}

// DeleteNpmAccount 删除账号的 npm 配置，返回是否删除了一行。
func DeleteNpmAccount(ctx context.Context, q Execer, ownerID int64) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM npm_accounts WHERE owner_id=$1`, ownerID)
	return tag.RowsAffected() > 0, err
}

// NpmCredentials 是解密后的 npm 配置，用于发布。
type NpmCredentials struct {
	NpmAccount
	Token string
}

// ResolveNpmCredentials 读取并解密账号的 npm 配置；没有配置时返回 nil。配置和 token 来自同一次读取。
func ResolveNpmCredentials(ctx context.Context, q Querier, masterKey []byte, ownerID int64) (*NpmCredentials, error) {
	var c NpmCredentials
	var ciphertext, wrapped []byte
	err := q.QueryRow(ctx, `SELECT `+npmAccountCols+`, ciphertext, wrapped_dek FROM npm_accounts WHERE owner_id=$1`, ownerID).Scan(
		&c.OwnerID, &c.Account, &c.Scope, &c.Registry, &c.Version, &c.UpdatedBy, &c.UpdatedAt, &ciphertext, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := secretbox.Open(masterKey, npmTokenAAD(ownerID), secretbox.Sealed{Ciphertext: ciphertext, WrappedDEK: wrapped})
	if err != nil {
		return nil, fmt.Errorf("解密账号 %s 的 npm token：%w", c.Account, err)
	}
	c.Token = string(v)
	return &c, nil
}
