package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// Environment、变量、secret 的三级作用域（PLAN #16.4）。

// Environment 是 environments 表中的一个 environment（账号级或仓库级）。
type Environment struct {
	ID                  int64
	OwnerID             int64
	RepoID              *int64
	Name                string
	RefPolicy           []string
	AllowedPublishRepos []string
}

const envCols = `id, owner_id, repo_id, name, ref_policy, allowed_publish_repos`

func scanEnv(row pgx.Row) (*Environment, error) {
	var e Environment
	var policy []byte
	if err := row.Scan(&e.ID, &e.OwnerID, &e.RepoID, &e.Name, &policy, &e.AllowedPublishRepos); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(policy, &e.RefPolicy); err != nil {
		return nil, fmt.Errorf("environment %s 的 ref 策略无法解析：%w", e.Name, err)
	}
	return &e, nil
}

// LookupEnvironment 查找 environment：仓库级优先，整体取代账号级（不合并）。不存在时返回 nil。
func LookupEnvironment(ctx context.Context, q Querier, repoID int64, name string) (*Environment, error) {
	e, err := scanEnv(q.QueryRow(ctx, `SELECT `+envCols+` FROM environments e
		WHERE lower(name)=lower($2) AND (repo_id=$1 OR (repo_id IS NULL AND owner_id=(SELECT owner_id FROM repos WHERE id=$1)))
		ORDER BY repo_id NULLS LAST LIMIT 1`, repoID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// RefAllowed 判断 ref 是否满足 environment 的 ref 策略（空表示不限制）。
func (e *Environment) RefAllowed(ref string) bool {
	if len(e.RefPolicy) == 0 {
		return true
	}
	for _, p := range e.RefPolicy {
		if ok, _ := doublestar.Match(p, ref); ok {
			return true
		}
	}
	return false
}

// VariablesFor 返回合并后的变量：environment > 仓库 > 账号。
func VariablesFor(ctx context.Context, q Querier, repo *Repo, env *Environment) (map[string]string, error) {
	out := map[string]string{}
	for _, sc := range scopesFor(repo, env) {
		rows, err := q.Query(ctx, `SELECT name, value FROM variables WHERE scope_kind=$1 AND scope_id=$2`, sc.kind, sc.id)
		if err != nil {
			return nil, err
		}
		vars, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Variable])
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			out[v.Name] = v.Value
		}
	}
	return out, nil
}

// Variable 是作用域中的一个变量。
type Variable struct{ Name, Value string }

// ListVariables 返回作用域 ref 中的变量，按名称排序。
func ListVariables(ctx context.Context, q Querier, ref *ScopeRef) ([]Variable, error) {
	rows, err := q.Query(ctx, `SELECT name, value FROM variables WHERE scope_kind=$1 AND scope_id=$2 ORDER BY name`, ref.Kind, ref.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Variable])
}

// SecretMeta 是 secret 的元数据（不含值）。Version 是 secrets.version：每次写入都不同，用来判断发布用的是不是这一版。
type SecretMeta struct {
	Name, UpdatedBy string
	UpdatedAt       time.Time
	Version         int64
}

// ListSecretMeta 返回作用域 ref 中 secret 的元数据，按名称排序。只查询元数据，不读取密文。
func ListSecretMeta(ctx context.Context, q Querier, ref *ScopeRef) ([]SecretMeta, error) {
	rows, err := q.Query(ctx, `SELECT name, updated_by, updated_at, version FROM secrets WHERE scope_kind=$1 AND scope_id=$2 ORDER BY name`, ref.Kind, ref.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SecretMeta])
}

// scope 是一个作用域（类型和 ID）。
type scope struct {
	kind ScopeKind
	id   int64
}

// scopesFor 返回 job 可见的作用域，优先级从低到高：账号、仓库、environment。
func scopesFor(repo *Repo, env *Environment) []scope {
	scopes := []scope{{ScopeOwner, repo.OwnerID}, {ScopeRepo, repo.ID}}
	if env != nil {
		scopes = append(scopes, scope{ScopeEnvironment, env.ID})
	}
	return scopes
}

// SecretsFor 用 masterKey 解密 job 引用的 secret：environment > 仓库 > 账号。
func SecretsFor(ctx context.Context, q Querier, masterKey []byte, repo *Repo, env *Environment, names []string) (map[string]string, error) {
	resolved, err := ResolveSecrets(ctx, q, masterKey, repo, env, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resolved))
	for name, s := range resolved {
		out[name] = s.Value
	}
	return out, nil
}

// ResolvedSecret 是解密后的 secret 和它的版本（secrets.version：每次写入都不同、全局唯一）。
type ResolvedSecret struct {
	Value   string
	Version int64
}

// ResolveSecrets 与 SecretsFor 相同，另外返回每个值所在那一行的版本，用来记录发布实际用的是哪一版 secret。
// 值和版本来自同一次读取，不会因为中间被替换而对不上。
//
// secret 使用信封加密（PLAN #16.3）：每个 secret 一个数据密钥，数据密钥由 master key 加密。
func ResolveSecrets(ctx context.Context, q Querier, masterKey []byte, repo *Repo, env *Environment, names []string) (map[string]ResolvedSecret, error) {
	out := map[string]ResolvedSecret{}
	if len(names) == 0 {
		return out, nil
	}
	type sealedSecret struct {
		Name                   string
		Ciphertext, WrappedDEK []byte
		Version                int64
	}
	for _, sc := range scopesFor(repo, env) {
		rows, err := q.Query(ctx, `SELECT name, ciphertext, wrapped_dek, version FROM secrets WHERE scope_kind=$1 AND scope_id=$2 AND name = ANY($3)`, sc.kind, sc.id, names)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sealedSecret])
		if err != nil {
			return nil, err
		}
		for _, sec := range list {
			aad := secretbox.AAD(string(sc.kind), sc.id, sec.Name)
			v, err := secretbox.Open(masterKey, aad, secretbox.Sealed{Ciphertext: sec.Ciphertext, WrappedDEK: sec.WrappedDEK})
			if err != nil {
				return nil, fmt.Errorf("解密 secret %s：%w", sec.Name, err)
			}
			out[sec.Name] = ResolvedSecret{Value: string(v), Version: sec.Version}
		}
	}
	return out, nil
}

// ScopeRef 是解析后的作用域。
type ScopeRef struct {
	OwnerID int64
	Kind    ScopeKind
	ID      int64
	Label   string
}

// ErrEnvironmentNotFound 表示作用域中指定的 environment 不存在（账号或仓库存在）。
var ErrEnvironmentNotFound = errors.New("environment 不存在")

// envNotFoundError 是 ResolveScope 找不到 environment 时的错误：说明文字给 CLI 和 Dashboard 直接显示，errors.Is 匹配 ErrEnvironmentNotFound。
type envNotFoundError struct{ name string }

func (e *envNotFoundError) Error() string {
	return fmt.Sprintf("environment %s 不存在（先用 kiln env set 创建）", e.name)
}

func (e *envNotFoundError) Unwrap() error { return ErrEnvironmentNotFound }

// ResolveScope 把 CLI / Dashboard 传来的作用域（账号登录名、owner/name、environment 名）解析为 ID。
// 仓库不存在时返回的错误包装了 ErrRepoNotFound，environment 不存在时包装了 ErrEnvironmentNotFound。
func ResolveScope(ctx context.Context, q Querier, account, repo, env string) (*ScopeRef, error) {
	var ownerID int64
	var found *Repo
	if repo != "" {
		r, err := FindRepo(ctx, q, repo)
		if err != nil {
			return nil, err
		}
		found = r
		ownerID = r.OwnerID
		account = r.Owner
	} else {
		if account == "" {
			return nil, errors.New("需要指定账号（--account）或仓库（--repo）")
		}
		err := q.QueryRow(ctx, `SELECT account_id FROM installations WHERE lower(account_login)=lower($1) AND NOT deleted ORDER BY id LIMIT 1`, account).Scan(&ownerID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = q.QueryRow(ctx, `SELECT owner_id FROM repos WHERE lower(owner)=lower($1) LIMIT 1`, account).Scan(&ownerID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("账号 %s 不在 Kiln 中（App 是否已安装？）", account)
		}
		if err != nil {
			return nil, err
		}
	}
	if env == "" {
		if found != nil {
			return &ScopeRef{OwnerID: ownerID, Kind: ScopeRepo, ID: found.ID, Label: found.FullName()}, nil
		}
		return &ScopeRef{OwnerID: ownerID, Kind: ScopeOwner, ID: ownerID, Label: account}, nil
	}
	var repoID *int64
	if found != nil {
		repoID = &found.ID
	}
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM environments WHERE owner_id=$1 AND repo_id IS NOT DISTINCT FROM $2 AND lower(name)=lower($3)`, ownerID, repoID, env).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &envNotFoundError{name: env}
	}
	if err != nil {
		return nil, err
	}
	label := account + "/" + env
	if found != nil {
		label = found.FullName() + "/" + env
	}
	return &ScopeRef{OwnerID: ownerID, Kind: ScopeEnvironment, ID: id, Label: label}, nil
}
