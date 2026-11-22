package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestHashEnrollToken(t *testing.T) {
	t.Parallel()
	h := HashEnrollToken("kiln-enroll-abc")
	if len(h) != 64 || h != HashEnrollToken("kiln-enroll-abc") {
		t.Fatalf("HashEnrollToken = %q，应为稳定的 64 位 hex", h)
	}
	if h == HashEnrollToken("kiln-enroll-abd") {
		t.Fatal("不同 token 的哈希不应相同")
	}
}

// CreateEnrollToken 生成一次性 token：数据库中只有哈希，pool 为空时为 default，过期时间为 now+ttl。
func TestCreateEnrollToken(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	tests := []struct {
		name     string
		pool     string
		wantPool string
	}{
		{"默认 pool", "", store.DefaultPool},
		{"指定 pool", "gpu", "gpu"},
	}
	seen := map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			before := time.Now()
			tok, exp, err := CreateEnrollToken(ctx, db, tc.pool, "", "", 30*time.Minute)
			if err != nil {
				t.Fatalf("CreateEnrollToken：%v", err)
			}
			if !strings.HasPrefix(tok, "kiln-enroll-") || len(tok) < len("kiln-enroll-")+32 {
				t.Fatalf("token 格式不对：%q", tok)
			}
			if seen[tok] {
				t.Fatal("每次应生成不同的 token")
			}
			seen[tok] = true
			if d := exp.Sub(before); d < 30*time.Minute || d > 31*time.Minute {
				t.Fatalf("过期时间应约为 30 分钟后，实际 %v", d)
			}
			var pool string
			var stored time.Time
			var used *time.Time
			err = db.QueryRow(ctx, `SELECT pool, expires_at, used_at FROM enrollment_tokens WHERE token_hash=$1`, HashEnrollToken(tok)).Scan(&pool, &stored, &used)
			if err != nil {
				t.Fatalf("按哈希应能找到 token：%v", err)
			}
			if pool != tc.wantPool || used != nil || stored.Sub(exp).Abs() > time.Millisecond {
				t.Fatalf("保存的记录 pool=%q used=%v expires=%v，期望 pool=%q、未使用、expires=%v", pool, used, stored, tc.wantPool, exp)
			}
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM enrollment_tokens WHERE token_hash=$1`, tok).Scan(&n); err != nil || n != 0 {
				t.Fatalf("数据库中不应保存 token 原文：%d %v", n, err)
			}
		})
	}
}

// failingExecer 的 Exec 总是失败。
type failingExecer struct{}

func (failingExecer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("db down")
}

// 写入失败时不返回令牌（没有写入数据库的令牌不能用）。
func TestCreateEnrollTokenWriteFails(t *testing.T) {
	t.Parallel()
	tok, exp, err := CreateEnrollToken(t.Context(), failingExecer{}, "", "", "", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "db down") || tok != "" || !exp.IsZero() {
		t.Fatalf("CreateEnrollToken = %q %v %v，期望出错且不返回令牌", tok, exp, err)
	}
}

// 令牌保存 AI 执行节点的角色：AI 为空时是审计；CI 不能有角色；无效的角色不写入。
func TestCreateEnrollTokenRole(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	ctx := t.Context()
	for _, tc := range []struct{ kind, role, want string }{
		{store.RunnerKindCI, "", ""},
		{store.RunnerKindAI, "", store.RunnerRoleReview},
		{store.RunnerKindAI, store.RunnerRoleDevelopment, store.RunnerRoleDevelopment},
	} {
		tok, _, err := CreateEnrollToken(ctx, db, "", tc.kind, tc.role, time.Minute)
		if err != nil {
			t.Fatalf("%s/%s：%v", tc.kind, tc.role, err)
		}
		var role string
		if err := db.QueryRow(ctx, `SELECT role FROM enrollment_tokens WHERE token_hash=$1`, HashEnrollToken(tok)).Scan(&role); err != nil || role != tc.want {
			t.Errorf("%s/%s：role = %q %v，应为 %q", tc.kind, tc.role, role, err, tc.want)
		}
	}
	for _, tc := range []struct{ kind, role string }{{store.RunnerKindCI, store.RunnerRoleReview}, {store.RunnerKindAI, "admin"}} {
		if tok, _, err := CreateEnrollToken(ctx, db, "", tc.kind, tc.role, time.Minute); err == nil || tok != "" {
			t.Errorf("%s/%s 应被拒绝：%q %v", tc.kind, tc.role, tok, err)
		}
	}
}
