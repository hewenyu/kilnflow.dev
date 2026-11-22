package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdminBinding 是管理员绑定的 GitHub 账号（admin_binding 表，只有一行）。
type AdminBinding struct {
	GitHubID    int64
	GitHubLogin string
	// TokenLogin 表示 Dashboard 是否还允许用 admin token 登录。
	TokenLogin bool
	BoundBy    string
	BoundAt    time.Time
	UpdatedAt  time.Time
}

// GetAdminBinding 返回管理员绑定的 GitHub 账号；还没有绑定时返回 nil。
func GetAdminBinding(ctx context.Context, q Querier) (*AdminBinding, error) {
	var b AdminBinding
	err := q.QueryRow(ctx, `SELECT github_id, github_login, token_login, bound_by, bound_at, updated_at FROM admin_binding`).
		Scan(&b.GitHubID, &b.GitHubLogin, &b.TokenLogin, &b.BoundBy, &b.BoundAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// PutAdminBinding 绑定（或更换绑定）GitHub 账号。更换时 admin token 登录的开关保持不变。
func PutAdminBinding(ctx context.Context, q Execer, githubID int64, login, actor string) error {
	_, err := q.Exec(ctx, `INSERT INTO admin_binding (github_id, github_login, bound_by) VALUES ($1,$2,$3)
		ON CONFLICT (singleton) DO UPDATE SET github_id=$1, github_login=$2, bound_by=$3, bound_at=now(), updated_at=now()`,
		githubID, login, actor)
	return err
}

// DeleteAdminBinding 解除绑定，返回是否删除了绑定。没有绑定时 admin token 登录总是开启。
func DeleteAdminBinding(ctx context.Context, q Execer) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM admin_binding`)
	return tag.RowsAffected() > 0, err
}

// SetAdminTokenLogin 设置 Dashboard 是否允许 admin token 登录，返回是否有绑定（没有绑定时不修改）。
func SetAdminTokenLogin(ctx context.Context, q Execer, enabled bool) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE admin_binding SET token_login=$1, updated_at=now()`, enabled)
	return tag.RowsAffected() > 0, err
}
