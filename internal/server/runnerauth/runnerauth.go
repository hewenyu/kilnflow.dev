// Package runnerauth 是 CI Runner 与 AI 执行节点共用的身份机制（PLAN #9.3、#30.3）：用一次性 enroll token 注册、
// 签名换取 access token、校验 access token 和 Runner 的类型。领域特有的部分（CI 的 pool、平台，AI 的角色）由各领域在
// 注册时的钩子中写入自己的表（ci_runners、ai_runners），这个包只访问共用表（runners、enrollment_tokens、auth_nonces）。
package runnerauth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

const (
	AccessTokenTTL = 15 * time.Minute
	// AccessTokenAudience 是 Runner access token 的 aud，签发和校验都用它。
	AccessTokenAudience = "runner-api"
	// authClockSkew 是 Runner 签名时间戳与 Server 时钟允许的最大偏差。
	authClockSkew = time.Minute
	// minNonceLen、MaxNonceLen 是认证随机数允许的长度范围（字节）。
	minNonceLen = 16
	MaxNonceLen = 128
	// NonceRetention 是认证随机数的保存时间（必须长于 authClockSkew，才能防止重放）。清理由 CI 的 sweeper 执行。
	NonceRetention = 10 * time.Minute
)

// Enrolled 是注册的结果：Runner ID，以及由令牌决定的类型、CI Runner 的 pool 和 AI 执行节点的角色。
type Enrolled struct {
	ID   int64
	Name string
	Kind string
	Pool string
	Role string
	// Details 是注册请求中领域的信息（EnrollRequest.Details），原样交给钩子。
	Details any
}

// EnrollHook 在注册的事务中写入领域自己的 Runner 记录（ci_runners、ai_runners）。同名 Runner 重新注册时也会调用：
// 领域的记录与令牌不符（例如 AI 审计节点 用开发令牌重新注册）时返回 Reject 的错误，整个注册回滚。
type EnrollHook func(ctx context.Context, tx pgx.Tx, e Enrolled) error

// Auth 是 Runner 的注册与认证。
type Auth struct {
	db    *store.DB
	keys  *auth.Keys
	hooks map[string]EnrollHook
}

// New 返回 Auth。hooks 按 Runner 类型（store.RunnerKindCI、store.RunnerKindAI）给出领域的注册钩子。
func New(db *store.DB, keys *auth.Keys, hooks map[string]EnrollHook) *Auth {
	return &Auth{db: db, keys: keys, hooks: hooks}
}

// EnrollRequest 是一次注册请求。
type EnrollRequest struct {
	Token     string
	Name      string
	PublicKey []byte
	Version   string
	// Details 是领域的注册信息（例如 CI Runner 上报的平台、executor），这个包不解析，交给钩子。
	Details any
}

// Enroll 用一次性 token 注册 Runner。同名 Runner 已存在时更新它的公钥（重新注册同一台机器）；同名的 Runner 是另一种类型时
// 拒绝（不能用 AI 令牌覆盖 CI Runner）。领域的钩子在同一事务中写入领域的记录。
func (a *Auth) Enroll(ctx context.Context, req EnrollRequest) (Enrolled, error) {
	var e Enrolled
	if len(req.PublicKey) != ed25519.PublicKeySize {
		return e, Reject("公钥无效")
	}
	if req.Name == "" {
		return e, Reject("需要 Runner 名称")
	}
	e.Name, e.Details = req.Name, req.Details
	err := a.db.Tx(ctx, func(tx pgx.Tx) error {
		var tid int64
		err := tx.QueryRow(ctx, `SELECT id, pool, kind, role FROM enrollment_tokens WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now() FOR UPDATE`,
			auth.HashEnrollToken(req.Token)).Scan(&tid, &e.Pool, &e.Kind, &e.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return Reject("enroll token 无效、已使用或已过期")
		}
		if err != nil {
			return err
		}
		hook := a.hooks[e.Kind]
		if hook == nil {
			return fmt.Errorf("没有 %s Runner 的注册钩子", e.Kind)
		}
		var existingKind string
		err = tx.QueryRow(ctx, `SELECT kind FROM runners WHERE name=$1 FOR UPDATE`, req.Name).Scan(&existingKind)
		switch {
		case err == nil && existingKind != e.Kind:
			return Reject("名称 %s 已被一台%s使用，不能用%s的令牌重新注册；请换一个名称", req.Name, KindLabel(existingKind), KindLabel(e.Kind))
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// ON CONFLICT 的 WHERE 再检查一次类型：同名的 Runner 可能在上面的检查之后才由并发的注册创建（行锁锁不住还不存在的行）
		err = tx.QueryRow(ctx, `INSERT INTO runners (name, kind, public_key, version, state, last_seen_at) VALUES ($1,$2,$3,$4,'active',now())
			ON CONFLICT (name) DO UPDATE SET public_key=EXCLUDED.public_key, version=EXCLUDED.version, state='active', drain=false, last_seen_at=now()
			  WHERE runners.kind=EXCLUDED.kind
			RETURNING id`, req.Name, e.Kind, req.PublicKey, req.Version).Scan(&e.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Reject("名称 %s 已被另一种 Runner 使用，不能用%s的令牌重新注册；请换一个名称", req.Name, KindLabel(e.Kind))
		}
		if err != nil {
			return err
		}
		if err := hook(ctx, tx, e); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET used_at=now(), runner_id=$2 WHERE id=$1`, tid, e.ID); err != nil {
			return err
		}
		return store.Audit(ctx, tx, "runner:"+req.Name, "runner.enroll", map[string]any{"runner_id": e.ID, "pool": e.Pool, "kind": e.Kind, "role": e.Role})
	})
	return e, err
}

// KindLabel 返回 Runner 类型的显示名称（前后带空格，用在句子中）。
func KindLabel(kind string) string {
	if kind == store.RunnerKindAI {
		return " AI 执行节点 "
	}
	return " CI Runner "
}

// Authenticate 校验 Runner 的签名并签发 access token。
func (a *Auth) Authenticate(ctx context.Context, runnerID, ts int64, nonce string, sig []byte) (string, time.Time, error) {
	if d := time.Since(time.Unix(ts, 0)); d > authClockSkew || d < -authClockSkew {
		return "", time.Time{}, Reject("时间戳与 Server 相差超过 %d 秒，请检查 Runner 的时钟", int64(authClockSkew/time.Second))
	}
	if len(nonce) < minNonceLen || len(nonce) > MaxNonceLen {
		return "", time.Time{}, Reject("随机数无效")
	}
	var pub []byte
	var state string
	err := a.db.QueryRow(ctx, `SELECT public_key, state FROM runners WHERE id=$1`, runnerID).Scan(&pub, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrRevoked
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if state != store.RunnerActive {
		return "", time.Time{}, ErrRevoked
	}
	// 签名的消息格式是 Runner 与 Server 之间的协议，必须与 Runner 端（internal/runnerkit）签名时的格式一致
	msg := fmt.Sprintf("kiln-runner-auth:%d:%d:%s", runnerID, ts, nonce)
	if !ed25519.Verify(pub, []byte(msg), sig) {
		return "", time.Time{}, Reject("签名无效")
	}
	tag, err := a.db.Exec(ctx, `INSERT INTO auth_nonces (runner_id, nonce) VALUES ($1,$2) ON CONFLICT DO NOTHING`, runnerID, nonce)
	if err != nil {
		return "", time.Time{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", time.Time{}, Reject("随机数已使用过")
	}
	exp := time.Now().Add(AccessTokenTTL)
	return a.keys.SignToken(auth.Claims{Sub: runnerID, Exp: exp.Unix(), Aud: AccessTokenAudience}), exp, nil
}

// Rejection 是注册或认证被拒绝的原因（请求本身不合格：token、公钥、签名、时间戳、随机数）。
// 其他错误（数据库等）是 Server 的内部错误，不能当作拒绝返回给 Runner：否则 ErrorInterceptor 不会记录它们，
// Runner 也会把暂时的故障当作 token 无效（PermissionDenied 不重试）。
type Rejection struct{ msg string }

func (r *Rejection) Error() string { return r.msg }

// Reject 返回一个 Rejection。
func Reject(format string, args ...any) error { return &Rejection{msg: fmt.Sprintf(format, args...)} }

// IsRejection 判断 err 是不是 Rejection。
func IsRejection(err error) bool {
	_, ok := errors.AsType[*Rejection](err)
	return ok
}

var (
	// ErrRevoked 表示 Runner 已被吊销或不存在。
	ErrRevoked = errors.New("runner 已被吊销或不存在")
	// errUnauthenticated 表示请求没有携带有效的 access token。
	errUnauthenticated = errors.New("未认证")
)

// Caller 是一次请求的 Runner。
type Caller struct {
	ID   int64
	Kind string
}

// runnerFromHeader 校验 access token，并检查 Runner 状态（吊销立即生效）。
func (a *Auth) runnerFromHeader(ctx context.Context, h http.Header) (Caller, error) {
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok {
		return Caller{}, fmt.Errorf("%w：缺少 access token", errUnauthenticated)
	}
	c, err := a.keys.VerifyToken(tok, AccessTokenAudience)
	if err != nil {
		return Caller{}, fmt.Errorf("%w：%w", errUnauthenticated, err)
	}
	var state, kind string
	err = a.db.QueryRow(ctx, `SELECT state, kind FROM runners WHERE id=$1`, c.Sub).Scan(&state, &kind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != store.RunnerActive) {
		return Caller{}, ErrRevoked
	}
	if err != nil {
		return Caller{}, fmt.Errorf("读取 Runner 状态：%w", err)
	}
	return Caller{ID: c.Sub, Kind: kind}, nil
}

// Authorize 校验请求的 access token，返回调用的 Runner。认证失败已转换为 Connect 错误码；其他错误（读取 Runner 状态失败）
// 原样返回，由 ErrorInterceptor 记录并转换为 CodeInternal。
func (a *Auth) Authorize(ctx context.Context, h http.Header) (Caller, error) {
	c, err := a.runnerFromHeader(ctx, h)
	switch {
	case err == nil:
		return c, nil
	case errors.Is(err, ErrRevoked):
		return Caller{}, rpcerr.RunnerRevoked()
	case errors.Is(err, errUnauthenticated):
		return Caller{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return Caller{}, err
}

// AuthorizeKind 与 Authorize 相同，但只接受类型为 kind 的 Runner（AI 执行节点调 CI 的执行接口、CI Runner 调 AI 的服务都拒绝，
// PLAN #33.6）。
func (a *Auth) AuthorizeKind(ctx context.Context, h http.Header, kind string) (int64, error) {
	c, err := a.Authorize(ctx, h)
	if err != nil {
		return 0, err
	}
	if c.Kind != kind {
		return 0, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("这个接口只接受%s，调用方是%s", KindLabel(kind), KindLabel(c.Kind)))
	}
	return c.ID, nil
}
