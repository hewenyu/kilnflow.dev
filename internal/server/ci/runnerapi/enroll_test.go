package runnerapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Enroll：一次性 token 只能用一次、过期的不能用；Runner 进入 token 指定的 pool。
func TestService_Enroll(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	newToken := func(pool string, ttl time.Duration) string {
		tok, _, err := auth.CreateEnrollToken(ctx, h.db, pool, "", "", ttl)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	newKey := func() ed25519.PublicKey {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return pub
	}
	used := newToken("", time.Hour)
	if _, err := h.client.Enroll(ctx, connect.NewRequest(&runnerv1.EnrollRequest{Token: used, Name: "first", PublicKey: newKey()})); err != nil {
		t.Fatalf("第一次使用 token 应成功：%v", err)
	}

	tests := []struct {
		name     string
		token    string
		runner   string
		key      []byte
		wantPool string // 空表示应失败
		wantErr  string
	}{
		{"default pool", newToken("", time.Hour), "mac-1", newKey(), store.DefaultPool, ""},
		{"token 指定的 pool", newToken("gpu", time.Hour), "gpu-1", newKey(), "gpu", ""},
		{"token 已使用", used, "second", newKey(), "", "无效、已使用或已过期"},
		{"token 已过期", newToken("", -time.Second), "late", newKey(), "", "无效、已使用或已过期"},
		{"token 不存在", "kiln-enroll-unknown", "nobody", newKey(), "", "无效、已使用或已过期"},
		{"公钥长度不对", newToken("", time.Hour), "badkey", []byte("short"), "", "公钥无效"},
		{"缺少名称", newToken("", time.Hour), "", newKey(), "", "需要 Runner 名称"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.client.Enroll(ctx, connect.NewRequest(&runnerv1.EnrollRequest{
				Token: tc.token, Name: tc.runner, PublicKey: tc.key,
				State: &runnerv1.RunnerState{Version: "v1", Platforms: []string{"linux/arm64"}, Executors: []string{"oci"}},
			}))
			if tc.wantPool == "" {
				if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Enroll 错误 = %v，期望 PermissionDenied 且包含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enroll：%v", err)
			}
			if resp.Msg.Pool != tc.wantPool || resp.Msg.RunnerId == 0 {
				t.Fatalf("Enroll = %+v，期望 pool %s", resp.Msg, tc.wantPool)
			}
			var pool, state, version string
			var pub []byte
			var platforms []string
			if err := h.db.QueryRow(ctx, `SELECT c.pool, r.state, r.version, r.public_key, c.platforms FROM runners r JOIN ci_runners c ON c.runner_id=r.id WHERE r.id=$1`, resp.Msg.RunnerId).
				Scan(&pool, &state, &version, &pub, &platforms); err != nil {
				t.Fatal(err)
			}
			if pool != tc.wantPool || state != store.RunnerActive || version != "v1" || string(pub) != string(tc.key) || len(platforms) != 1 {
				t.Fatalf("runners 记录：pool=%s state=%s version=%s platforms=%v", pool, state, version, platforms)
			}
			var tokenRunner *int64
			if err := h.db.QueryRow(ctx, `SELECT runner_id FROM enrollment_tokens WHERE token_hash=$1 AND used_at IS NOT NULL`,
				auth.HashEnrollToken(tc.token)).Scan(&tokenRunner); err != nil || tokenRunner == nil || *tokenRunner != resp.Msg.RunnerId {
				t.Fatalf("token 应标记为已被这个 Runner 使用：%v %v", tokenRunner, err)
			}
		})
	}
}

// 回归测试：Runner 没有上报状态（或者上报的平台、executor 为空）时也能注册和领取。
// 之前 nil 切片被写成 NULL，违反 ci_runners.platforms / executors 的 NOT NULL 约束，注册失败、Acquire 返回内部错误。
func TestService_Enroll_withoutState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok, _, err := auth.CreateEnrollToken(t.Context(), h.db, "", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client.Enroll(t.Context(), connect.NewRequest(&runnerv1.EnrollRequest{Token: tok, Name: "bare", PublicKey: pub}))
	if err != nil {
		t.Fatalf("没有状态时注册应成功：%v", err)
	}
	r := &testRunner{h: h, id: resp.Msg.RunnerId, priv: priv, state: &runnerv1.RunnerState{}}
	ar, err := h.client.Authenticate(t.Context(), connect.NewRequest(r.authRequest(time.Now(), randNonce())))
	if err != nil {
		t.Fatal(err)
	}
	r.token = ar.Msg.AccessToken
	h.pendingJob()
	r.mustAcquire(1, 0) // 没有上报平台：领不到 job，但不是错误
}

// 同名 Runner 用新 token 重新注册（例如重装了机器）：沿用同一条记录，换成新公钥，恢复为 active 并解除 drain，
// 旧私钥不能再认证。
func TestService_Enroll_reenrollSameName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner(withPool("gpu"))
	h.exec(`UPDATE runners SET state='revoked', drain=true WHERE id=$1`, r.id)

	tok, _, err := auth.CreateEnrollToken(t.Context(), h.db, "", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client.Enroll(t.Context(), connect.NewRequest(&runnerv1.EnrollRequest{Token: tok, Name: r.name, PublicKey: pub}))
	if err != nil {
		t.Fatalf("重新注册：%v", err)
	}
	if resp.Msg.RunnerId != r.id || resp.Msg.Pool != store.DefaultPool {
		t.Fatalf("重新注册应沿用 ID %d 并进入新 token 的 pool：%+v", r.id, resp.Msg)
	}
	var state string
	var drain bool
	if err := h.db.QueryRow(t.Context(), `SELECT state, drain FROM runners WHERE id=$1`, r.id).Scan(&state, &drain); err != nil {
		t.Fatal(err)
	}
	if state != store.RunnerActive || drain {
		t.Fatalf("重新注册后应为 active 且不 drain：%s %v", state, drain)
	}
	if _, err := h.client.Authenticate(t.Context(), connect.NewRequest(r.authRequest(time.Now(), randNonce()))); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("旧私钥不应再能认证：%v", err)
	}
	r.priv = priv
	if _, err := h.client.Authenticate(t.Context(), connect.NewRequest(r.authRequest(time.Now(), randNonce()))); err != nil {
		t.Fatalf("新私钥应能认证：%v", err)
	}
}

// Authenticate：签名、时间偏差、随机数长度和重放、Runner 状态。
func TestService_Authenticate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	r := h.enrollRunner()
	revoked := h.enrollRunner()
	h.exec(`UPDATE runners SET state='revoked' WHERE id=$1`, revoked.id)
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	replayed := randNonce()
	if _, err := h.client.Authenticate(ctx, connect.NewRequest(r.authRequest(time.Now(), replayed))); err != nil {
		t.Fatalf("第一次认证：%v", err)
	}

	now := time.Now()
	tests := []struct {
		name    string
		req     *runnerv1.AuthenticateRequest
		code    connect.Code // 0 表示应成功
		wantErr string
	}{
		{"有效", r.authRequest(now, randNonce()), 0, ""},
		{"时间偏差在一分钟内", r.authRequest(now.Add(-50*time.Second), randNonce()), 0, ""},
		{"时间戳太旧", r.authRequest(now.Add(-2*time.Minute), randNonce()), connect.CodeUnauthenticated, "时间戳"},
		{"时间戳在未来", r.authRequest(now.Add(2*time.Minute), randNonce()), connect.CodeUnauthenticated, "时间戳"},
		{"随机数太短", r.authRequest(now, "short"), connect.CodeUnauthenticated, "随机数无效"},
		{"随机数太长", r.authRequest(now, strings.Repeat("n", runnerauth.MaxNonceLen+1)), connect.CodeUnauthenticated, "随机数无效"},
		{"随机数重放", r.authRequest(now, replayed), connect.CodeUnauthenticated, "随机数已使用过"},
		{"签名错误", func() *runnerv1.AuthenticateRequest {
			x := *r
			x.priv = otherKey
			return x.authRequest(now, randNonce())
		}(), connect.CodeUnauthenticated, "签名无效"},
		{"Runner 不存在", func() *runnerv1.AuthenticateRequest {
			req := r.authRequest(now, randNonce())
			req.RunnerId = revoked.id + 1000
			return req
		}(), connect.CodePermissionDenied, rpcerr.ReasonRunnerRevoked},
		{"已吊销", revoked.authRequest(now, randNonce()), connect.CodePermissionDenied, rpcerr.ReasonRunnerRevoked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.client.Authenticate(ctx, connect.NewRequest(tc.req))
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("Authenticate：%v", err)
				}
				if exp := time.Unix(resp.Msg.ExpiresAt, 0); time.Until(exp) < runnerauth.AccessTokenTTL-time.Minute || time.Until(exp) > runnerauth.AccessTokenTTL {
					t.Fatalf("access token 应在约 %v 后过期，实际 %v", runnerauth.AccessTokenTTL, time.Until(exp))
				}
				c, err := h.keys.VerifyToken(resp.Msg.AccessToken, runnerauth.AccessTokenAudience)
				if err != nil || c.Sub != r.id {
					t.Fatalf("签发的 token 应属于 Runner %d：%+v %v", r.id, c, err)
				}
				return
			}
			if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Authenticate 错误 = %v，期望 %v 且包含 %q", err, tc.code, tc.wantErr)
			}
		})
	}
}

// access token 的校验：每个需要认证的接口都先校验 token，再检查 Runner 状态（吊销立即生效）。
func TestService_authorize(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	revoked := h.enrollRunner()
	h.exec(`UPDATE runners SET state='revoked' WHERE id=$1`, revoked.id)
	sign := func(sub int64, aud string, ttl time.Duration) string {
		return h.keys.SignToken(auth.Claims{Sub: sub, Exp: time.Now().Add(ttl).Unix(), Aud: aud})
	}
	other := auth.NewKeys([]byte("other-key"), nil, "")

	tests := []struct {
		name   string
		header string // 完整的 Authorization 头；空表示不设置
		code   connect.Code
		reason string
	}{
		{"有效", "Bearer " + r.token, 0, ""},
		{"缺少 token", "", connect.CodeUnauthenticated, "缺少 access token"},
		{"不是 Bearer", "Basic " + r.token, connect.CodeUnauthenticated, "缺少 access token"},
		{"其他密钥签发", "Bearer " + other.SignToken(auth.Claims{Sub: r.id, Exp: time.Now().Add(time.Minute).Unix(), Aud: runnerauth.AccessTokenAudience}), connect.CodeUnauthenticated, "签名无效"},
		{"用途不符", "Bearer " + sign(r.id, "dashboard", time.Minute), connect.CodeUnauthenticated, "用途不符"},
		{"已过期", "Bearer " + sign(r.id, runnerauth.AccessTokenAudience, -time.Second), connect.CodeUnauthenticated, "已过期"},
		{"Runner 已吊销", "Bearer " + revoked.token, connect.CodePermissionDenied, rpcerr.ReasonRunnerRevoked},
		{"Runner 不存在", "Bearer " + sign(revoked.id+1000, runnerauth.AccessTokenAudience, time.Minute), connect.CodePermissionDenied, rpcerr.ReasonRunnerRevoked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := connect.NewRequest(&runnerv1.AcquireRequest{State: r.state})
			if tc.header != "" {
				req.Header().Set("Authorization", tc.header)
			}
			_, err := h.client.Acquire(t.Context(), req)
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("Acquire：%v", err)
				}
				return
			}
			if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Acquire 错误 = %v，期望 %v 且包含 %q", err, tc.code, tc.reason)
			}
		})
	}
}
