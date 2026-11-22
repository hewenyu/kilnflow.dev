// Package gitcred 是 Server 按操作签发 git 凭据的登记（PLAN #33.13），AI 与 CI
// 共用代码、各自的 RPC 调用它。
//
// 每个操作（网络 git 的一次传输、连接器的一次准备或推送、Server 的一次交付写操作）由客户端随机生成 op_id，键是
// (领域, attempt, epoch, op_id)：
//   - 每个操作至多一枚 token：同键的重复签发返回同一枚（仓库和权限必须与第一次一致），签发在途时后到的请求等同一个结果；
//     不续签，token 到期后重放同一操作仍是那一枚。
//   - 关闭以 op_id 为准，是终态：关闭吊销登记中的 token（客户端没收到、Server 已拿到的也在内），之后同键的签发一律拒绝；
//     签发在途时被关闭，GitHub 返回的 token 立即吊销、不交出。关闭记录保留到 attempt 失效。
//   - 交出前先在登记中记下 token，再调用调用方给的最终检查（数据库中 attempt 的归属、epoch、状态、租约，Runner 未被吊销）；
//     不通过时关闭操作、吊销、不交出。检查通过之后才失效的，由失效处理（Invalidate）吊销。
//   - attempt 失效是终态：同步关闭它的全部操作、之后的签发一律拒绝；已登记的 token 由登记在后台吊销（每次失效一个总的
//     期限，不重试、不持久化），调用方不等 GitHub。Wait 等后台吊销结束（Server 关闭、测试）。
//   - 对账（Reconcile）补上漏掉的失效通知：领域的 sweeper 每个周期把登记中还没有失效的 attempt 交给自己的数据库查询，
//     已经结束或不存在的失效。
//   - GitHub 签发结果不明时不重试，这个操作以错误关闭（ErrUnknown），记 warn（过期时间是估计值）；确定没有签发时操作不关闭，
//     客户端可以用同一 op_id、同一范围再试。吊销失败只记 warn（带过期时间），不重试：token 到期自然失效。
//
// 登记只在内存中，不落盘；Server 重启后客户端带 token 的关闭仍能吊销，客户端没收到的 token 自然过期。token 不进日志，
// 日志用 Fingerprint。
package gitcred

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
)

const (
	// DefaultIssueTimeout 是向 GitHub 签发一枚 token 的时限（Config.IssueTimeout 为 0 时）。
	DefaultIssueTimeout = 30 * time.Second
	// DefaultRevokeTimeout 是吊销一枚 token 的时限（Config.RevokeTimeout 为 0 时）。
	DefaultRevokeTimeout = 30 * time.Second
	// DefaultCheckTimeout 是交出 token 前最终检查的时限（Config.CheckTimeout 为 0 时）。
	DefaultCheckTimeout = 10 * time.Second
	// DefaultInvalidateTimeout 是一次失效在后台吊销全部 token 的总时限（Config.InvalidateTimeout 为 0 时）。
	DefaultInvalidateTimeout = DefaultRevokeTimeout
	// DefaultRetention 是已失效 attempt 的记录保留时间（Config.Retention 为 0 时）：与 token 的有效期相同，之后登记中
	// 不会再有它的 token，拒绝签发靠调用方在数据库中的检查。
	DefaultRetention = gh.TokenLifetime
	// reconcileBatch 是对账时一次交给领域查询的 attempt 数上限。
	reconcileBatch = 500
)

var (
	// ErrClosed 表示操作已经关闭或 attempt 已经失效：不再签发。
	ErrClosed = errors.New("git 凭据的操作已经关闭或 attempt 已经结束，不再签发")
	// ErrScopeMismatch 表示同一操作的重复签发请求的仓库或权限与第一次不同。
	ErrScopeMismatch = errors.New("同一操作的重复签发请求的仓库或权限与第一次不同")
	// ErrUnknown 表示 GitHub 签发结果不明（请求已发出，没有可用的响应）：操作已经以错误关闭，重新执行即是新的操作。
	// 签发在途时操作已被关闭的，错误同时满足 ErrClosed 和 ErrUnknown。
	ErrUnknown = errors.New("GitHub 签发 token 的结果不明，这个操作已经关闭")
	// ErrCheckFailed 表示交出前的最终检查没有完成（超时、取消）：操作已经关闭、token 已吊销，重新执行即是新的操作。错误链
	// 中不含检查的 context 错误，调用方不会把它当成发起请求的客户端取消。
	ErrCheckFailed = errors.New("交出 git 凭据前的最终检查没有完成，这个操作已经关闭")
)

// Domain 是操作所属的领域：AI 与 CI 的 attempt 编号各自独立。
type Domain string

// 领域。
const (
	DomainAI Domain = "ai"
	DomainCI Domain = "ci"
)

// Attempt 标识一个 attempt。
type Attempt struct {
	Domain Domain
	ID     int64
}

// Key 标识一个操作：attempt、领取的 epoch 和客户端生成的 op_id。
type Key struct {
	Attempt
	Epoch int64
	Op    string
}

func (k Key) valid() bool { return k.Domain != "" && k.ID > 0 && k.Op != "" }

// Scope 是一枚 token 的范围：installation 下的一个仓库和权限（例如 {"contents": "read"}）。
type Scope struct {
	Installation int64
	RepoID       int64
	// Repository 是仓库的 owner/name，只用于日志。
	Repository  string
	Permissions map[string]string
}

func (s Scope) valid() bool { return s.Installation > 0 && s.RepoID > 0 && len(s.Permissions) > 0 }

func (s Scope) equal(o Scope) bool {
	return s.Installation == o.Installation && s.RepoID == o.RepoID && maps.Equal(s.Permissions, o.Permissions)
}

// Check 是交出 token 之前的最终检查（由 AI、CI 各自提供）：在数据库中确认 attempt 的归属、epoch、状态、租约未过期，
// Runner 未被吊销。返回的错误原样交给调用方。
type Check func(ctx context.Context) error

// GitHub 是登记用到的 GitHub 接口（*github.Client 实现了它）。CreateToken 的错误按 github.NotApplied 区分确定没有签发
// 和结果不明。
type GitHub interface {
	CreateToken(ctx context.Context, installationID int64, repoIDs []int64, perms map[string]string) (*gh.AccessToken, error)
	RevokeToken(ctx context.Context, installationID int64, token string) error
}

// Config 是登记的配置；为 0 的项取默认值。
type Config struct {
	// IssueTimeout 是向 GitHub 签发一枚 token 的时限。签发不随发起请求的客户端取消：客户端没收到响应时用同一 op_id 重试，
	// 拿到的仍是这一枚。
	IssueTimeout time.Duration
	// RevokeTimeout 是吊销一枚 token 的时限。
	RevokeTimeout time.Duration
	// CheckTimeout 是最终检查的时限。最终检查不随发起请求的客户端取消：客户端断开不是 attempt 失效的证据，不能因此关闭操作。
	CheckTimeout time.Duration
	// InvalidateTimeout 是一次失效在后台吊销这个 attempt 全部 token 的总时限（不按 token 数累乘）。
	InvalidateTimeout time.Duration
	// Retention 是已失效 attempt 的记录保留时间。
	Retention time.Duration
}

// Deps 是登记的依赖。
type Deps struct {
	GitHub GitHub
	Log    *slog.Logger
	// Now 为 nil 时用 time.Now。
	Now func() time.Time
}

// Registry 是签发登记，并发安全。
type Registry struct {
	cfg Config
	gh  GitHub
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	attempts map[Attempt]*attemptState
	// expired 是已失效的 attempt，按失效时间排序，保留 cfg.Retention 后从 attempts 中删除。
	expired []expiry
	// pending 是在后台吊销中的失效批数；idle 在 pending 由正数变为 0 时关闭（Wait）。
	pending int
	idle    chan struct{}
}

// attemptState 是一个 attempt 的操作。
type attemptState struct {
	ops map[opKey]*op
	// invalid 为真时 attempt 已经失效（终态），ops 已经清空。
	invalid bool
}

type opKey struct {
	epoch int64
	id    string
}

type expiry struct {
	attempt Attempt
	at      time.Time
}

// op 是一个操作。done 关闭之后 tok、err 不再改变；closed、waiters 由 Registry.mu 保护。
type op struct {
	scope Scope
	done  chan struct{}
	// tok 是 GitHub 签发的 token（签发成功、在途时没有被关闭）。
	tok *gh.AccessToken
	// err 是签发的错误（确定没有签发、结果不明、在途时被关闭）。
	err error
	// closed 为真时操作已经关闭（终态）。由 false 改为 true 的一方负责吊销 tok。
	closed bool
	// notIssued 为真时 GitHub 确定没有签发：同范围的下一次请求重新签发（新的 op 替换它），范围仍按 scope 绑定。
	notIssued bool
	// waiters 是签发在途时加入等待的请求数。
	waiters int
}

// New 返回登记。
func New(cfg Config, d Deps) *Registry {
	if cfg.IssueTimeout == 0 {
		cfg.IssueTimeout = DefaultIssueTimeout
	}
	if cfg.RevokeTimeout == 0 {
		cfg.RevokeTimeout = DefaultRevokeTimeout
	}
	if cfg.CheckTimeout == 0 {
		cfg.CheckTimeout = DefaultCheckTimeout
	}
	if cfg.InvalidateTimeout == 0 {
		cfg.InvalidateTimeout = DefaultInvalidateTimeout
	}
	if cfg.Retention == 0 {
		cfg.Retention = DefaultRetention
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{cfg: cfg, gh: d.GitHub, log: d.Log, now: now, attempts: map[Attempt]*attemptState{}}
}

// Fingerprint 返回 token 在日志中的指纹（SHA-256 的前缀），不能还原 token。
func Fingerprint(token string) string { return redact.Fingerprint(token) }

// Issue 为操作 key 签发 scope 范围的 token，交出前调用 check。同键的重复请求返回同一枚 token（scope 必须相同，否则
// ErrScopeMismatch）；操作已经关闭、attempt 已经失效时返回 ErrClosed；GitHub 结果不明时返回 ErrUnknown（操作随之关闭）；
// GitHub 确定没有签发时返回它的错误（github.NotApplied 为真，操作不关闭，只能以同一 scope 重试）；check 不通过时关闭
// 操作、吊销 token，返回 check 的错误。返回的是副本。
//
// 调用约定：调用方必须先在数据库中校验 attempt 的归属、epoch、状态和租约（以及仓库在授权范围内），通过后才调用 Issue；
// check 是登记之后的第二次检查。登记不是鉴权：已失效 attempt 的记录只保留 Retention 作为惰性清理，过期或 Server 重启后
// 登记中没有它，拒绝签发只能靠调用方的数据库校验。
//
// check 和向 GitHub 的签发都用不随 ctx 取消的有界 context：ctx 被取消（客户端断开）时这个调用返回 ctx 的错误、不交出
// token，但不关闭操作，未取消的同键重放仍拿到同一枚。
func (r *Registry) Issue(ctx context.Context, key Key, scope Scope, check Check) (*gh.AccessToken, error) {
	if !key.valid() || !scope.valid() || check == nil {
		return nil, fmt.Errorf("签发 git 凭据的参数不完整：%+v %+v", key, scope)
	}
	scope.Permissions = maps.Clone(scope.Permissions)
	o, leader, err := r.join(key, scope)
	if err != nil {
		return nil, err
	}
	if leader {
		r.create(ctx, key, o)
	} else {
		select {
		case <-o.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if o.err != nil {
		return nil, o.err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.CheckTimeout)
	err = check(cctx)
	cancel()
	if err != nil {
		r.closeOp(ctx, key, o, "最终检查不通过")
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// 检查自己的时限到了（不是发起请求的客户端取消）：断开错误链，调用方按终态的内部错误处理
			return nil, fmt.Errorf("%w：%s", ErrCheckFailed, err.Error())
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		// 发起请求的客户端已经取消：这次不交出，操作保持打开
		return nil, err
	}
	r.mu.Lock()
	closed := o.closed
	r.mu.Unlock()
	if closed {
		// 最终检查之后、交出之前被关闭或失效：关闭的一方已经吊销
		return nil, ErrClosed
	}
	tok := *o.tok
	return &tok, nil
}

// join 找到或登记操作 key；leader 为真时由调用方向 GitHub 签发。
func (r *Registry) join(key Key, scope Scope) (o *op, leader bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	a := r.attempt(key.Attempt)
	if a.invalid {
		return nil, false, ErrClosed
	}
	k := opKey{key.Epoch, key.Op}
	if o = a.ops[k]; o != nil {
		switch {
		case o.closed:
			return nil, false, ErrClosed
		case !o.scope.equal(scope):
			return nil, false, ErrScopeMismatch
		case o.notIssued:
			// 上一次确定没有签发：同范围重新签发
			o = &op{scope: scope, done: make(chan struct{})}
			a.ops[k] = o
			return o, true, nil
		}
		select {
		case <-o.done:
		default:
			o.waiters++
		}
		return o, false, nil
	}
	o = &op{scope: scope, done: make(chan struct{})}
	a.ops[k] = o
	return o, true, nil
}

// create 向 GitHub 签发操作 o 的 token，按结果更新 o 并结束等待（关闭 o.done）。签发不随 ctx 取消，以 IssueTimeout 为限。
func (r *Registry) create(ctx context.Context, key Key, o *op) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.IssueTimeout)
	defer cancel()
	tok, err := r.gh.CreateToken(cctx, o.scope.Installation, []int64{o.scope.RepoID}, o.scope.Permissions)
	var late *gh.AccessToken
	r.mu.Lock()
	switch {
	case err == nil && o.closed:
		// 签发在途时被关闭或失效：不交出，立即吊销
		o.err, late = ErrClosed, tok
	case err == nil:
		o.tok = tok
	case o.closed && gh.NotApplied(err):
		// 已经关闭的操作优先报告关闭：不能给出可以同键重试的信号
		o.err = ErrClosed
	case o.closed:
		o.err = fmt.Errorf("%w；%w：%w", ErrClosed, ErrUnknown, err)
	case gh.NotApplied(err):
		// 确定没有签发：同一 op_id 可以以同一范围再试（join）
		o.err, o.notIssued = fmt.Errorf("签发 git 凭据：%w", err), true
	default:
		o.err, o.closed = fmt.Errorf("%w：%w", ErrUnknown, err), true
	}
	close(o.done)
	r.mu.Unlock()
	if err == nil {
		r.log.Info("签发 git 凭据", r.attrs(key, o.scope, tok.Token, tok.ExpiresAt)...)
	}
	switch {
	case late != nil:
		r.revoke(ctx, revocation{key, o.scope, late.Token, late.ExpiresAt}, "签发在途时操作已经关闭")
	case err != nil && !gh.NotApplied(err):
		r.log.Warn("GitHub 签发 git 凭据的结果不明，不重试，操作以错误关闭；可能有一枚谁也没拿到的 token，到期自然失效",
			append(r.scopeAttrs(key, o.scope), "expires_at", r.now().Add(gh.TokenLifetime), "expires_at_estimated", true, "err", err)...)
	}
}

// Close 关闭操作 key（终态）并吊销登记中的 token。token 是客户端收到的 token（可以为空），登记中没有这枚 token 时
// （Server 重启丢了登记、attempt 已经失效）用 installation 补吊销。操作不存在时登记一条关闭记录，之后同键的签发被拒绝。
// 吊销失败只记 warn。
//
// attempt 已经失效（Invalidate）之后带 token 的 Close 不建任何记录，只对这枚 token 再发一次吊销：失效时已经吊销过的，
// GitHub 返回 401，按已经失效处理（RevokeToken 返回 nil），无害。
//
// 调用约定：只在数据库中确认 attempt 仍在执行（归属、epoch 一致、状态未结束）时调用 Close；已经结束的 attempt 用
// RevokeEnded，否则 Server 重启或失效记录清除之后的 Close 会留下一条不会被清除的关闭记录。
func (r *Registry) Close(ctx context.Context, key Key, installation int64, token string) error {
	if !key.valid() {
		return fmt.Errorf("关闭 git 凭据的操作的参数不完整：%+v", key)
	}
	var revoke []revocation
	r.mu.Lock()
	r.prune()
	a := r.attempt(key.Attempt)
	k := opKey{key.Epoch, key.Op}
	o := a.ops[k]
	switch {
	case a.invalid:
	case o == nil:
		o = &op{done: make(chan struct{}), err: ErrClosed, closed: true}
		close(o.done)
		a.ops[k] = o
	default:
		if rv, ok := r.closeLocked(key, o); ok {
			revoke = append(revoke, rv)
		}
	}
	if token != "" && (o == nil || o.tok == nil || o.tok.Token != token) {
		revoke = append(revoke, revocation{key: key, scope: Scope{Installation: installation}, token: token})
	}
	r.mu.Unlock()
	for _, rv := range revoke {
		r.revoke(ctx, rv, "操作关闭")
	}
	return nil
}

// RevokeEnded 是调用方已在数据库中确认 attempt 已经结束（不在执行中）时的补吊销：吊销登记中操作 key 的 token 和客户端
// 带来的 token（可以为空，用 installation 吊销），不新建任何登记记录。登记中这个 attempt 还是活动的（失效处理还没有
// 运行），只关闭这一个操作；attempt 的其余操作由 Invalidate 处理。吊销失败只记 warn。
func (r *Registry) RevokeEnded(ctx context.Context, key Key, installation int64, token string) error {
	return r.closeExisting(ctx, key, installation, token, "attempt 已结束后的补吊销")
}

// Discard 关闭已经登记的操作 key 并吊销它的 token，不新建记录：Issue 返回 token 之后、交出之前调用方自己的步骤失败
// （例如写审计日志失败）时用。操作不存在（attempt 已经失效、记录已经清除）时什么都不做。吊销失败只记 warn。
func (r *Registry) Discard(ctx context.Context, key Key) error {
	return r.closeExisting(ctx, key, 0, "", "交出前的步骤失败")
}

// closeExisting 关闭登记中已有的操作 key 并吊销它的 token，再吊销 token（不是登记中那一枚时，用 installation）；不新建记录。
func (r *Registry) closeExisting(ctx context.Context, key Key, installation int64, token, reason string) error {
	if !key.valid() {
		return fmt.Errorf("吊销 git 凭据的参数不完整：%+v", key)
	}
	var revoke []revocation
	r.mu.Lock()
	r.prune()
	var o *op
	if a := r.attempts[key.Attempt]; a != nil {
		o = a.ops[opKey{key.Epoch, key.Op}]
	}
	if o != nil {
		if rv, ok := r.closeLocked(key, o); ok {
			revoke = append(revoke, rv)
		}
	}
	if token != "" && (o == nil || o.tok == nil || o.tok.Token != token) {
		revoke = append(revoke, revocation{key: key, scope: Scope{Installation: installation}, token: token})
	}
	r.mu.Unlock()
	for _, rv := range revoke {
		r.revoke(ctx, rv, reason)
	}
	return nil
}

// Invalidate 使 attempt 失效（终态）：同步关闭它的全部操作，返回之后同一 attempt 的签发一律拒绝；已登记的 token 在后台
// 吊销（并发，共用 InvalidateTimeout 的总时限，不随 ctx 取消），调用方不等 GitHub，需要等吊销结束时调用 Wait。调用方在
// attempt 结束的事务提交之后调用（完成、取消、超时、sweeper 判定租约过期）。吊销失败只记 warn。
func (r *Registry) Invalidate(ctx context.Context, attempt Attempt) {
	var revoke []revocation
	r.mu.Lock()
	r.prune()
	a := r.attempt(attempt)
	if a.invalid {
		r.mu.Unlock()
		return
	}
	for k, o := range a.ops {
		if rv, ok := r.closeLocked(Key{attempt, k.epoch, k.id}, o); ok {
			revoke = append(revoke, rv)
		}
	}
	a.ops, a.invalid = nil, true
	r.expired = append(r.expired, expiry{attempt, r.now()})
	if len(revoke) == 0 {
		r.mu.Unlock()
		return
	}
	if r.pending == 0 {
		r.idle = make(chan struct{})
	}
	r.pending++
	r.mu.Unlock()
	go r.revokeBatch(context.WithoutCancel(ctx), revoke, "attempt 结束")
}

// Live 返回 ids 中在数据库里仍在执行（还没有结束）的 attempt：领域查询自己的表，只读。
type Live func(ctx context.Context, ids []int64) ([]int64, error)

// Reconcile 补上漏掉的失效通知（领域的 sweeper 每个周期调用一次）：domain 中有记录、还没有失效的 attempt 分批（每批最多
// reconcileBatch 个）交给 live，不在结果中的（已经结束或不存在）调用 Invalidate。结束 attempt 的事务已经提交、调用方却
// 收到错误（提交确认丢失）或在通知之前出错时，领域之后不会再选中这个 attempt，由这里在下一个周期补上。只做"已结束 →
// 失效"这一个方向：live 返回的不动。查询失败时返回错误，没有处理的批次等下一个周期；没有记录时不调用 live。返回交给
// Invalidate 的 attempt 数。
func (r *Registry) Reconcile(ctx context.Context, domain Domain, live Live) (int, error) {
	n := 0
	for batch := range slices.Chunk(r.holding(domain), reconcileBatch) {
		alive, err := live(ctx, batch)
		if err != nil {
			return n, err
		}
		running := make(map[int64]bool, len(alive))
		for _, id := range alive {
			running[id] = true
		}
		for _, id := range batch {
			if !running[id] {
				r.log.Info("git 凭据对账：attempt 已经结束，使它失效", "domain", domain, "attempt", id)
				r.Invalidate(ctx, Attempt{domain, id})
				n++
			}
		}
	}
	return n, nil
}

// holding 返回 domain 中有记录、还没有失效的 attempt（升序）。
func (r *Registry) holding(domain Domain) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []int64
	for a, st := range r.attempts {
		if a.Domain == domain && !st.invalid {
			ids = append(ids, a.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// revokeBatch 在 InvalidateTimeout 的总时限内并发吊销 revoke，结束后减少 pending。
func (r *Registry) revokeBatch(ctx context.Context, revoke []revocation, reason string) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.InvalidateTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, rv := range revoke {
		wg.Go(func() { r.revokeWithin(ctx, rv, reason) })
	}
	wg.Wait()
	r.mu.Lock()
	if r.pending--; r.pending == 0 {
		close(r.idle)
	}
	r.mu.Unlock()
}

// Wait 等待后台吊销（Invalidate 发起的）全部结束，最多等到 ctx 结束（返回 ctx 的错误）。Server 关闭时有界地等待，
// 测试据此确定性地等到吊销完成。
func (r *Registry) Wait(ctx context.Context) error {
	r.mu.Lock()
	if r.pending == 0 {
		r.mu.Unlock()
		return nil
	}
	idle := r.idle
	r.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// revocation 是操作 key 要吊销的一枚 token。expires 为零时不知道过期时间（客户端带来的 token）。
type revocation struct {
	key     Key
	scope   Scope
	token   string
	expires time.Time
}

// closeOp 关闭操作 o（终态）并吊销它的 token。
func (r *Registry) closeOp(ctx context.Context, key Key, o *op, reason string) {
	r.mu.Lock()
	rv, ok := r.closeLocked(key, o)
	r.mu.Unlock()
	if ok {
		r.revoke(ctx, rv, reason)
	}
}

// closeLocked 把 o 标为关闭；o 由打开变为关闭且已有 token 时返回要吊销的 token。调用方持有 r.mu。
func (r *Registry) closeLocked(key Key, o *op) (revocation, bool) {
	if o.closed {
		return revocation{}, false
	}
	o.closed = true
	if o.tok == nil {
		// 签发在途（create 看到 closed 后吊销）或没有签发
		return revocation{}, false
	}
	return revocation{key: key, scope: o.scope, token: o.tok.Token, expires: o.tok.ExpiresAt}, true
}

// revoke 吊销 token，不随 ctx 取消，以 RevokeTimeout 为限；失败只记 warn。
func (r *Registry) revoke(ctx context.Context, rv revocation, reason string) {
	r.revokeWithin(context.WithoutCancel(ctx), rv, reason)
}

// revokeWithin 吊销 token，以 ctx 和 RevokeTimeout 中先到的为限；失败只记 warn。
func (r *Registry) revokeWithin(ctx context.Context, rv revocation, reason string) {
	rctx, cancel := context.WithTimeout(ctx, r.cfg.RevokeTimeout)
	defer cancel()
	attrs := append(r.attrs(rv.key, rv.scope, rv.token, rv.expires), "reason", reason)
	if err := r.gh.RevokeToken(rctx, rv.scope.Installation, rv.token); err != nil {
		r.log.Warn("吊销 git 凭据失败，不重试，到期自然失效", append(attrs, "err", err)...)
		return
	}
	r.log.Info("吊销 git 凭据", attrs...)
}

// attempt 返回 attempt 的状态，没有时新建。调用方持有 r.mu。
func (r *Registry) attempt(a Attempt) *attemptState {
	st := r.attempts[a]
	if st == nil {
		st = &attemptState{ops: map[opKey]*op{}}
		r.attempts[a] = st
	}
	return st
}

// prune 删除失效超过 Retention 的 attempt。调用方持有 r.mu。
func (r *Registry) prune() {
	now := r.now()
	n := 0
	for _, e := range r.expired {
		if now.Sub(e.at) < r.cfg.Retention {
			break
		}
		if st := r.attempts[e.attempt]; st != nil && st.invalid {
			delete(r.attempts, e.attempt)
		}
		n++
	}
	r.expired = r.expired[n:]
}

// scopeAttrs 是日志中操作和范围的字段。
func (r *Registry) scopeAttrs(key Key, scope Scope) []any {
	return []any{
		"domain", key.Domain, "attempt", key.ID, "epoch", key.Epoch, "op", key.Op,
		"repository", scope.Repository, "permissions", scope.Permissions,
	}
}

// attrs 是日志中操作、范围、token 指纹和过期时间的字段（过期时间未知时不记）。
func (r *Registry) attrs(key Key, scope Scope, token string, expires time.Time) []any {
	attrs := append(r.scopeAttrs(key, scope), "token_fingerprint", Fingerprint(token))
	if !expires.IsZero() {
		attrs = append(attrs, "expires_at", expires)
	}
	return attrs
}
