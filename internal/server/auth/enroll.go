package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/randtoken"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Runner 注册令牌（PLAN #9.3）：一次性 enroll token，数据库中只保存哈希。
// 管理侧（CLI、Dashboard）创建令牌，Runner API 在注册时用 HashEnrollToken 查找令牌。

// HashEnrollToken 返回 enroll token 在数据库中保存的哈希。
func HashEnrollToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// CreateEnrollToken 生成一次性 enroll token（只保存哈希），pool 为空时使用 default，kind 为空时是 CI Runner 的令牌
// （store.RunnerKindCI；注册出来的 Runner 类型和 AI 执行节点的角色由令牌决定，PLAN #26.7）。
// role 只用于 AI 执行节点，为空时是审计。返回 token 和过期时间。
func CreateEnrollToken(ctx context.Context, db store.Execer, pool, kind, role string, ttl time.Duration) (string, time.Time, error) {
	if pool == "" {
		pool = store.DefaultPool
	}
	if kind == "" {
		kind = store.RunnerKindCI
	}
	if !store.ValidRunnerKind(kind) {
		return "", time.Time{}, fmt.Errorf("无效的 Runner 类型 %q（ci 或 ai）", kind)
	}
	role, err := store.RunnerRole(kind, role)
	if err != nil {
		return "", time.Time{}, err
	}
	tok := "kiln-enroll-" + randtoken.New(24)
	exp := time.Now().Add(ttl)
	if _, err := db.Exec(ctx, `INSERT INTO enrollment_tokens (token_hash, pool, kind, role, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		HashEnrollToken(tok), pool, kind, role, exp); err != nil {
		// 没有写入的令牌不能用，不返回它
		return "", time.Time{}, fmt.Errorf("写入注册令牌：%w", err)
	}
	return tok, exp, nil
}
