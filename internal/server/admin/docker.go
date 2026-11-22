package admin

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Docker Hub 连接（DEPLOY #7.1）：每个账号一份，存放在账号级 environment DockerEnvName 中的两个变量和一个 secret。
// Dashboard 把它当成一个"连接"整体保存和删除：全部写入在一个事务中完成，中途失败不会留下半套配置。

const (
	// DockerEnvName 是存放 Docker Hub 连接的账号级 environment。
	DockerEnvName = "docker-release"
	// DockerUserVar、DockerNamespaceVar 是 Docker Hub 用户名和镜像命名空间的变量。
	DockerUserVar      = "DOCKERHUB_USERNAME"
	DockerNamespaceVar = "DOCKERHUB_NAMESPACE"
	// DockerTokenSecret 是访问令牌的 secret。
	DockerTokenSecret = "DOCKERHUB_TOKEN" //nolint:gosec // secret 的名称，不是凭据
)

// DockerConnection 是一次保存 Docker Hub 连接要写入的内容。只写入有变化的部分。
type DockerConnection struct {
	Account string
	// Environment 是已有的连接 environment 的名称（大小写以保存的为准）；为空表示还没有，按 RefPolicy 新建 DockerEnvName。
	Environment string
	RefPolicy   []string
	// Username、Namespace 为 nil 表示不修改。
	Username, Namespace *string
	// Token 为空表示不修改访问令牌。
	Token string
}

// SaveDockerConnection 在一个事务中保存账号的 Docker Hub 连接：连接 environment 不存在时先创建，
// 再写入要修改的用户名、命名空间和访问令牌。配置变更和审计与分别调用 UpsertEnvironment、SetVariable、SetSecret 相同。
func (s *Service) SaveDockerConnection(ctx context.Context, c *DockerConnection, actor string) error {
	env := c.Environment
	var create *apiv1.Environment
	if env == "" {
		env = DockerEnvName
		create = &apiv1.Environment{Name: DockerEnvName, Account: c.Account, RefPolicy: c.RefPolicy}
		if err := validEnvironment(create); err != nil {
			return err
		}
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if create != nil {
			if err := upsertEnvironmentTx(ctx, tx, create, actor); err != nil {
				return fmt.Errorf("创建 environment %s：%w", DockerEnvName, err)
			}
		}
		ref, err := store.ResolveScope(ctx, tx, c.Account, "", env)
		if err != nil {
			return err
		}
		for _, v := range []struct {
			name  string
			value *string
		}{{DockerUserVar, c.Username}, {DockerNamespaceVar, c.Namespace}} {
			if v.value == nil {
				continue
			}
			if err := setVariableTx(ctx, tx, ref, v.name, *v.value, actor); err != nil {
				return fmt.Errorf("保存变量 %s：%w", v.name, err)
			}
		}
		if c.Token != "" {
			if err := s.setSecretTx(ctx, tx, ref, DockerTokenSecret, c.Token, actor); err != nil {
				return fmt.Errorf("保存访问令牌：%w", err)
			}
		}
		return nil
	})
}

// DeleteDockerConnection 在一个事务中删除账号的 Docker Hub 连接（用户名、命名空间和访问令牌），
// 保留连接 environment env 及其 ref 策略和其他变量、secret。
func (s *Service) DeleteDockerConnection(ctx context.Context, account, env, actor string) error {
	return s.inScope(ctx, &apiv1.Scope{Account: account, Environment: env}, func(tx pgx.Tx, ref *store.ScopeRef) error {
		for _, name := range []string{DockerUserVar, DockerNamespaceVar} {
			if err := deleteVariableTx(ctx, tx, ref, name, actor); err != nil {
				return err
			}
		}
		return deleteSecretTx(ctx, tx, ref, DockerTokenSecret, actor)
	})
}
