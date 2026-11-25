package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// secret、变量和 environment 的管理（PLAN #16.4）。
// 写操作的公开方法各自开一个事务；需要在同一个事务中组合多个写入时（例如 Docker Hub 连接，见 docker.go）用 *Tx 版本。

// 设置的校验和前提错误：稳定的类型（sentinel 或带原始参数的类型），Dashboard 按它显示当前语言的说明，
// CLI 和日志用 Error() 的中文说明（与原来相同）。

// ErrNameEmpty 表示变量或 secret 的名称为空。
var ErrNameEmpty = errors.New("名称不能为空")

// InvalidNameError 表示变量或 secret 的名称含有字母、数字和 _ 以外的字符；Name 是原样的名称。
type InvalidNameError struct{ Name string }

func (e *InvalidNameError) Error() string {
	return fmt.Sprintf("名称 %q 只能包含字母、数字和 _", e.Name)
}

// ErrEnvNameRequired 表示没有给出 environment 名称。
var ErrEnvNameRequired = errors.New("需要 environment 名称")

// EnvNameSpaceError 表示 environment 名称以空白开头或结尾；Name 是原样的名称。
type EnvNameSpaceError struct{ Name string }

func (e *EnvNameSpaceError) Error() string {
	return fmt.Sprintf("environment 名称 %q 不能以空白开头或结尾", e.Name)
}

// PublishRepoError 表示 environment 允许发布的仓库没有写成 owner/name；Repo 是原样的输入。
type PublishRepoError struct{ Repo string }

func (e *PublishRepoError) Error() string {
	return "允许发布的仓库应写成 owner/name：" + e.Repo
}

// ErrEnvRequired 表示删除 environment 时作用域中没有指定 environment。
var ErrEnvRequired = errors.New("需要指定 environment")

// ValidName 检查变量或 secret 的名称：不能为空（ErrNameEmpty），只能包含字母、数字和 _（*InvalidNameError）。
// CLI 和 Dashboard 共用。
func ValidName(n string) error {
	if n == "" {
		return ErrNameEmpty
	}
	for _, c := range n {
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return &InvalidNameError{Name: n}
		}
	}
	return nil
}

// ValidRepoFullName 判断 s 是否写成了 owner/name（environment 允许发布的仓库，CLI 和 Dashboard 共用）：
// 恰好一个 /，两边都不为空，不含空白。发布时按全名比较（不区分大小写），其他写法永远不会匹配。
func ValidRepoFullName(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/") && !strings.ContainsFunc(s, unicode.IsSpace)
}

// inScope 在事务中解析作用域并执行 f。
func (s *Service) inScope(ctx context.Context, sc *apiv1.Scope, f func(tx pgx.Tx, ref *store.ScopeRef) error) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		ref, err := resolveScope(ctx, tx, sc)
		if err != nil {
			return err
		}
		return f(tx, ref)
	})
}

// resolveScope 解析作用域 sc（nil 表示没有指定）。
func resolveScope(ctx context.Context, q store.Querier, sc *apiv1.Scope) (*store.ScopeRef, error) {
	acc, repo, env := scopeOf(sc)
	return store.ResolveScope(ctx, q, acc, repo, env)
}

func (a *rpcAPI) SetSecret(ctx context.Context, req *connect.Request[apiv1.SetSecretRequest]) (*connect.Response[apiv1.SetSecretResponse], error) {
	if err := a.s.SetSecret(ctx, req.Msg.Scope, req.Msg.Name, req.Msg.Value, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.SetSecretResponse{}), nil
}

// SetSecret 加密保存（新建或覆盖）作用域 sc 中的 secret。值只写入密文，不进入审计日志和错误信息。
// 每次写入都取新的版本号（即使值相同），之前用旧版本做过的发布不再算作对当前 secret 的验证。
func (s *Service) SetSecret(ctx context.Context, sc *apiv1.Scope, name, value, actor string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return s.inScope(ctx, sc, func(tx pgx.Tx, ref *store.ScopeRef) error {
		return s.setSecretTx(ctx, tx, ref, name, value, actor)
	})
}

// setSecretTx 在事务 tx 中写入作用域 ref 中的 secret（名称已校验）并记录配置变更。
func (s *Service) setSecretTx(ctx context.Context, tx pgx.Tx, ref *store.ScopeRef, name, value, actor string) error {
	sealed, err := secretbox.Seal(s.cfg.MasterKey, secretbox.AAD(string(ref.Kind), ref.ID, name), []byte(value))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secrets (scope_kind, scope_id, name, ciphertext, wrapped_dek, updated_by) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (scope_kind, scope_id, name) DO UPDATE SET ciphertext=EXCLUDED.ciphertext, wrapped_dek=EXCLUDED.wrapped_dek,
		updated_by=EXCLUDED.updated_by, updated_at=now(), version=nextval('secret_versions')`, ref.Kind, ref.ID, name, sealed.Ciphertext, sealed.WrappedDEK, actor); err != nil {
		return err
	}
	return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "secret.set", map[string]any{"scope": ref.Label, "name": name})
}

func (a *rpcAPI) DeleteSecret(ctx context.Context, req *connect.Request[apiv1.DeleteSecretRequest]) (*connect.Response[apiv1.DeleteSecretResponse], error) {
	if err := a.s.DeleteSecret(ctx, req.Msg.Scope, req.Msg.Name, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.DeleteSecretResponse{}), nil
}

// DeleteSecret 删除作用域 sc 中的 secret（不存在时什么也不做）。
func (s *Service) DeleteSecret(ctx context.Context, sc *apiv1.Scope, name, actor string) error {
	return s.inScope(ctx, sc, func(tx pgx.Tx, ref *store.ScopeRef) error {
		return deleteSecretTx(ctx, tx, ref, name, actor)
	})
}

// deleteSecretTx 在事务 tx 中删除作用域 ref 中的 secret 并记录配置变更。
func deleteSecretTx(ctx context.Context, tx pgx.Tx, ref *store.ScopeRef, name, actor string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE scope_kind=$1 AND scope_id=$2 AND name=$3`, ref.Kind, ref.ID, name); err != nil {
		return err
	}
	return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "secret.delete", map[string]any{"scope": ref.Label, "name": name})
}

func (a *rpcAPI) ListSecrets(ctx context.Context, req *connect.Request[apiv1.ListSecretsRequest]) (*connect.Response[apiv1.ListSecretsResponse], error) {
	s := a.s
	ref, err := resolveScope(ctx, s.db, req.Msg.Scope)
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	metas, err := store.ListSecretMeta(ctx, s.db, ref)
	if err != nil {
		return nil, err
	}
	list := make([]*apiv1.SecretInfo, 0, len(metas))
	for _, m := range metas {
		// 与原来 SQL 中 extract(epoch ...)::bigint 一致：四舍五入到秒
		list = append(list, &apiv1.SecretInfo{Name: m.Name, UpdatedAt: m.UpdatedAt.Round(time.Second).Unix()})
	}
	return connect.NewResponse(&apiv1.ListSecretsResponse{Secrets: list}), nil
}

func (a *rpcAPI) SetVariable(ctx context.Context, req *connect.Request[apiv1.SetVariableRequest]) (*connect.Response[apiv1.SetVariableResponse], error) {
	if err := a.s.SetVariable(ctx, req.Msg.Scope, req.Msg.Name, req.Msg.Value, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.SetVariableResponse{}), nil
}

// SetVariable 保存（新建或覆盖）作用域 sc 中的变量。
func (s *Service) SetVariable(ctx context.Context, sc *apiv1.Scope, name, value, actor string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return s.inScope(ctx, sc, func(tx pgx.Tx, ref *store.ScopeRef) error {
		return setVariableTx(ctx, tx, ref, name, value, actor)
	})
}

// setVariableTx 在事务 tx 中写入作用域 ref 中的变量（名称已校验）并记录配置变更。
func setVariableTx(ctx context.Context, tx pgx.Tx, ref *store.ScopeRef, name, value, actor string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO variables (scope_kind, scope_id, name, value) VALUES ($1,$2,$3,$4)
		ON CONFLICT (scope_kind, scope_id, name) DO UPDATE SET value=EXCLUDED.value`, ref.Kind, ref.ID, name, value); err != nil {
		return err
	}
	return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "variable.set", map[string]any{"scope": ref.Label, "name": name, "value": value})
}

func (a *rpcAPI) DeleteVariable(ctx context.Context, req *connect.Request[apiv1.DeleteVariableRequest]) (*connect.Response[apiv1.DeleteVariableResponse], error) {
	if err := a.s.DeleteVariable(ctx, req.Msg.Scope, req.Msg.Name, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.DeleteVariableResponse{}), nil
}

// DeleteVariable 删除作用域 sc 中的变量（不存在时什么也不做）。
func (s *Service) DeleteVariable(ctx context.Context, sc *apiv1.Scope, name, actor string) error {
	return s.inScope(ctx, sc, func(tx pgx.Tx, ref *store.ScopeRef) error {
		return deleteVariableTx(ctx, tx, ref, name, actor)
	})
}

// deleteVariableTx 在事务 tx 中删除作用域 ref 中的变量并记录配置变更。
func deleteVariableTx(ctx context.Context, tx pgx.Tx, ref *store.ScopeRef, name, actor string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM variables WHERE scope_kind=$1 AND scope_id=$2 AND name=$3`, ref.Kind, ref.ID, name); err != nil {
		return err
	}
	return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "variable.delete", map[string]any{"scope": ref.Label, "name": name})
}

func (a *rpcAPI) ListVariables(ctx context.Context, req *connect.Request[apiv1.ListVariablesRequest]) (*connect.Response[apiv1.ListVariablesResponse], error) {
	s := a.s
	ref, err := resolveScope(ctx, s.db, req.Msg.Scope)
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	vars, err := store.ListVariables(ctx, s.db, ref)
	if err != nil {
		return nil, err
	}
	list := make([]*apiv1.Variable, 0, len(vars))
	for _, v := range vars {
		list = append(list, &apiv1.Variable{Name: v.Name, Value: v.Value})
	}
	return connect.NewResponse(&apiv1.ListVariablesResponse{Variables: list}), nil
}

// emptyIfNil 把 nil 切片换成空切片（写入 NOT NULL 的数组和 JSON 列）。
func emptyIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (a *rpcAPI) UpsertEnvironment(ctx context.Context, req *connect.Request[apiv1.UpsertEnvironmentRequest]) (*connect.Response[apiv1.UpsertEnvironmentResponse], error) {
	// 旧版 CLI 的 --reviewer、--wait 写在已删除的字段 5、6 中，这里是未知字段。静默忽略会让人误以为设置了审批，所以拒绝。
	if e := req.Msg.Environment; e != nil && len(e.ProtoReflect().GetUnknown()) > 0 {
		return nil, invalid(errRemovedEnvFields)
	}
	if err := a.s.UpsertEnvironment(ctx, req.Msg.Environment, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.UpsertEnvironmentResponse{}), nil
}

// errRemovedEnvFields 是旧版 CLI 传入已移除的审批字段时的错误。
var errRemovedEnvFields = errors.New("environment 已不支持审批人和等待时间（--reviewer、--wait），Kiln 只做 CI；请升级 kiln CLI，只用 --ref 和 --publish-to")

// UpsertEnvironment 新建或整体覆盖一个 environment 的配置（e.Repo 非空时为仓库级，否则为 e.Account 的账号级）。
// 挂在 environment 上的变量和 secret 不受影响。
func (s *Service) UpsertEnvironment(ctx context.Context, e *apiv1.Environment, actor string) error {
	if err := validEnvironment(e); err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error { return upsertEnvironmentTx(ctx, tx, e, actor) })
}

// validEnvironment 检查 environment 的名称和允许发布的仓库。
func validEnvironment(e *apiv1.Environment) error {
	if e == nil || strings.TrimSpace(e.Name) == "" {
		return ErrEnvNameRequired
	}
	if strings.TrimSpace(e.Name) != e.Name {
		// workflow 中按名称查找 environment，首尾的空白会让它永远找不到
		return &EnvNameSpaceError{Name: e.Name}
	}
	for _, r := range e.AllowedPublishRepos {
		if !ValidRepoFullName(r) {
			return &PublishRepoError{Repo: r}
		}
	}
	return nil
}

// upsertEnvironmentTx 在事务 tx 中写入 environment（已经过 validEnvironment 检查）并记录配置变更。
func upsertEnvironmentTx(ctx context.Context, tx pgx.Tx, e *apiv1.Environment, actor string) error {
	ref, err := resolveScope(ctx, tx, &apiv1.Scope{Account: e.Account, Repo: e.Repo})
	if err != nil {
		return err
	}
	var repoID *int64
	if ref.Kind == store.ScopeRepo {
		repoID = &ref.ID
	}
	policy, publish := emptyIfNil(e.RefPolicy), emptyIfNil(e.AllowedPublishRepos)
	// environment 名称不区分大小写（与 ResolveScope、LookupEnvironment 一致）：同一作用域中已有只是大小写不同的，
	// 更新它（保留原来的写法），而不是再建一个查找时无法区分的同名 environment
	tag, err := tx.Exec(ctx, `UPDATE environments SET ref_policy=$4, allowed_publish_repos=$5
		WHERE owner_id=$1 AND repo_id IS NOT DISTINCT FROM $2 AND lower(name)=lower($3)`,
		ref.OwnerID, repoID, e.Name, store.MustJSON(policy), publish)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// 上面的 UPDATE 与这里之间可能有并发的同名（仅大小写不同）创建：由大小写不敏感的唯一索引
		// environments_scope_name_ci 原子合并（冲突时更新已有行，保留它原来的写法，不改 name）。
		if _, err := tx.Exec(ctx, `INSERT INTO environments (owner_id, repo_id, name, ref_policy, allowed_publish_repos)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (owner_id, repo_id, lower(name)) DO UPDATE SET ref_policy=EXCLUDED.ref_policy,
		allowed_publish_repos=EXCLUDED.allowed_publish_repos`,
			ref.OwnerID, repoID, e.Name, store.MustJSON(policy), publish); err != nil {
			return err
		}
	}
	return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "environment.upsert", map[string]any{
		"scope": ref.Label, "name": e.Name, "ref_policy": policy, "allowed_publish_repos": publish,
	})
}

func (a *rpcAPI) DeleteEnvironment(ctx context.Context, req *connect.Request[apiv1.DeleteEnvironmentRequest]) (*connect.Response[apiv1.DeleteEnvironmentResponse], error) {
	if err := a.s.DeleteEnvironment(ctx, req.Msg.Scope, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.DeleteEnvironmentResponse{}), nil
}

// DeleteEnvironment 删除 sc 指定的 environment，连同挂在它上面的变量和 secret。
func (s *Service) DeleteEnvironment(ctx context.Context, sc *apiv1.Scope, actor string) error {
	return s.inScope(ctx, sc, func(tx pgx.Tx, ref *store.ScopeRef) error {
		if ref.Kind != store.ScopeEnvironment {
			return ErrEnvRequired
		}
		for _, sql := range []string{
			`DELETE FROM variables WHERE scope_kind=$2 AND scope_id=$1`,
			`DELETE FROM secrets WHERE scope_kind=$2 AND scope_id=$1`,
		} {
			if _, err := tx.Exec(ctx, sql, ref.ID, store.ScopeEnvironment); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM environments WHERE id=$1`, ref.ID); err != nil {
			return err
		}
		return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "environment.delete", map[string]any{"scope": ref.Label})
	})
}

func (a *rpcAPI) ListEnvironments(ctx context.Context, req *connect.Request[apiv1.ListEnvironmentsRequest]) (*connect.Response[apiv1.ListEnvironmentsResponse], error) {
	list, err := a.s.ListEnvironments(ctx, req.Msg.Account)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.ListEnvironmentsResponse{Environments: list}), nil
}

// ListEnvironments 返回 environment 列表；account 非空时只返回该账号的。
// 账号名取自该账号的 installation，与 ResolveScope 一致优先取没有删除的（账号改名后重新安装时旧的 installation 已删除）。
func (s *Service) ListEnvironments(ctx context.Context, account string) ([]*apiv1.Environment, error) {
	rows, err := s.db.Query(ctx, `SELECT e.name, coalesce(i.account_login, ''), coalesce(r.owner||'/'||r.name, ''), e.ref_policy,
		e.allowed_publish_repos
		FROM environments e LEFT JOIN repos r ON r.id=e.repo_id
		LEFT JOIN LATERAL (SELECT account_login FROM installations WHERE account_id=e.owner_id ORDER BY deleted, id LIMIT 1) i ON true
		WHERE $1='' OR lower(i.account_login)=lower($1) ORDER BY 2, 3, 1`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*apiv1.Environment, error) {
		var e apiv1.Environment
		var policy []byte
		if err := row.Scan(&e.Name, &e.Account, &e.Repo, &policy, &e.AllowedPublishRepos); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(policy, &e.RefPolicy); err != nil {
			return nil, fmt.Errorf("environment %s 的 ref 策略无法解析：%w", e.Name, err)
		}
		return &e, nil
	})
}
