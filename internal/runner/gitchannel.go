package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 按操作签发的源码凭据（PLAN #8.4）：
//   - 缓存缺失时 fetchSource 为这一次 fetch 开一个操作，fetch 进程退出后吊销（source.go）；
//   - step 中的 git 经每个 job 一个的本地凭据通道（unix socket）：git 的每次 HTTP(S) 传输由传输包装（kiln-runner 以
//     git-remote-http(s) 的名字运行，gitremote.go）在这里开一个操作并保持连接，credential helper（kiln-runner
//     git-credential）带着操作 id 来要凭据，第一次要时向 Server 签发这个仓库的只读 token，连接关闭时吊销。
//
// 一个操作只签一枚 token、只覆盖一个仓库，不续签；token 只在 Runner 进程和 git 进程的内存中，不进 step 环境、不落盘。

// errServerTooOld 是 Server 不认识按操作签发的 RPC（旧版本，返回 Unimplemented）时的错误：新 Runner 不退回到旧的
// FetchCredentials。
//
//nolint:staticcheck // ST1005：Server 是名字，这句话原样显示在 job 的结果说明中
var errServerTooOld = errors.New("Server 版本过旧，不支持按操作签发的源码凭据，请先升级 Server")

const (
	// gitSocketName 是凭据通道的 socket 在它的目录中的文件名（路径要短：sun_path 有上限）。
	gitSocketName = "s"
	// gitSocketDirPattern 是凭据通道的 socket 目录名的模式（在系统临时目录中，os.MkdirTemp 建为 0700）。
	gitSocketDirPattern = "kiln-git-"
	// gitSocketPathMaxLinux / gitSocketPathMaxBSD 是 unix socket 路径（sun_path，含结尾的 NUL）的上限。
	gitSocketPathMaxLinux = 108
	gitSocketPathMaxBSD   = 104
	// maxGitMessage 是通道上一条消息（一行 JSON）的长度上限。
	maxGitMessage = 64 << 10
	// gitIssueTimeout、gitRevokeTimeout 是一次签发（含重试）、一次吊销的时间上限：不随 job 取消（job 结束时仍要关闭操作、
	// 吊销迟到的结果）。
	gitIssueTimeout  = 2 * time.Minute
	gitRevokeTimeout = time.Minute
	// gitCloseTimeout 是 job 收尾时回收通道（关闭操作、吊销、等在途的签发）的总期限：到期后 close 取消仍在进行的签发和吊销、
	// 记下没结束的个数就返回，不等它们（RPC 可能在等 runnerkit.Client 的认证锁，不响应取消），它们之后自行结束，没能吊销的
	// token 到期自然失效。gitCloseParallel 是一个通道同时进行的关闭、吊销 RPC 数（收尾中的、迟到 token 的、连接断开的共用）。
	gitCloseTimeout  = 30 * time.Second
	gitCloseParallel = 8
	// gitRequestTimeout 是读一条请求、写一条回复的期限（连接级的空闲期限；open 的连接之后保持到传输结束）。
	gitRequestTimeout = 30 * time.Second
	// 客户端（传输包装、credential helper、容器中的探测）一侧的期限，只管连上通道、发出请求、读到第一条回复；之后 open
	// 的连接保持到传输结束，不受它们限制。
	// gitDialTimeout 是连接通道的期限：Runner 在本机，监听队列没满时连接立即成功（之后等 accept 的时间算在读回复里）。
	gitDialTimeout = 5 * time.Second
	// gitPingTimeout 是探测写请求、读回复各自的期限：Runner 读到 ping 立即回复，等的只是 accept（退避最长
	// gitAcceptBackoffMax）和调度。
	gitPingTimeout = 5 * time.Second
	// gitOpenReplyTimeout 是传输包装写请求、等 Runner 批准（open 的回复）各自的期限：Runner 读请求、写回复各有
	// gitRequestTimeout，批准本身不联网。
	gitOpenReplyTimeout = 2 * gitRequestTimeout
	// gitGetReplyTimeout 是 credential helper 写请求、等 token（get 的回复）各自的期限：Runner 读请求、写回复各有
	// gitRequestTimeout，中间的签发（含重试）最长 gitIssueTimeout，所以不短于三者之和。
	gitGetReplyTimeout = gitIssueTimeout + 2*gitRequestTimeout
	// ociGitPrepareTimeout 是 oci executor 在容器中为凭据通道做的两步准备（探测 git-socket-check、试建 git exec 镜像）共用
	// 的总期限：探测自己的拨号、写、读（gitDialTimeout + 2 × gitPingTimeout）加上两次 docker exec 的启动和镜像中
	// git --exec-path、建一百来个符号链接的时间。到期按通道不可用处理，不占用整个 job 的时限。
	ociGitPrepareTimeout = time.Minute
	// gitAcceptBackoffMin、gitAcceptBackoffMax 是 Accept 遇到非关闭类错误（例如文件描述符用尽）时的退避区间。
	gitAcceptBackoffMin = 10 * time.Millisecond
	gitAcceptBackoffMax = time.Second
	// gitTokenUser 是 installation token 的用户名（GitHub 的约定）。
	gitTokenUser = "x-access-token"
)

// 通道上的命令。
const (
	gitCmdOpen = "open" // 传输包装：按地址开一个操作，连接保持到传输结束
	gitCmdGet  = "get"  // credential helper：要操作的凭据
	gitCmdPing = "ping" // 连通检查（oci 的 prepare）
)

// gitMsg 是通道上的请求（一行 JSON）。
type gitMsg struct {
	Cmd string `json:"cmd"`
	// open：这次传输的地址（insteadOf 改写之后），传输包装的 pid（只用于日志）。
	URL string `json:"url,omitempty"`
	PID int    `json:"pid,omitempty"`
	// get：传输包装开的操作，以及 git 给 credential helper 的协议、主机和路径。
	Op       string `json:"op,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
}

// gitReply 是通道上的回复（一行 JSON）。
type gitReply struct {
	// open：操作 id；地址不是 job 可以访问的仓库时为空，Denied 是不给凭据的原因（传输照常进行，只是没有 Kiln 的凭据）。
	Op     string `json:"op,omitempty"`
	Denied string `json:"denied,omitempty"`
	// get：凭据。
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// gitTokens 签发、吊销源码 token（job 经 runner.v1 的 IssueGitToken、RevokeGitToken 实现；测试中是替身）。
type gitTokens interface {
	// issueGitToken 为操作 op 签发仓库 repo（owner/name）的只读 token。
	issueGitToken(ctx context.Context, op, repo string) (token string, expires time.Time, err error)
	// revokeGitToken 关闭操作 op 并吊销它的 token（token 为空表示没有收到）。
	revokeGitToken(ctx context.Context, op, token string) error
}

// newGitOpID 返回随机的操作 id。
func newGitOpID() string { return rand.Text() }

// issueRetryable 判断 IssueGitToken 的错误能否用同一个 op 重试：只有 Server 明确返回的 Unavailable（GitHub 确定没有签发，
// 操作不变；connect.IsWireError 区分 Server 发来的错误与客户端自己合成的）。连接断开、网关的 502 / 503 等没有收到 Server
// 回复的情形，Connect 客户端同样合成 Unavailable，但操作的状态不明（Server 可能已经签出 token），与 DeadlineExceeded、
// Canceled 一样不重试：这次 git 访问失败，由调用方按原 op 关闭（gitOpSettled）。其余错误码一律不重试。
func issueRetryable(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnavailable && connect.IsWireError(err)
}

// gitOpSettled 判断签发失败后 Server 上不会有这个操作的打开记录、不需要关闭（③ 的错误码表）。只认 Server 对这次请求的
// 明确答复（connect.IsWireError）：签发前就拒绝的（未声明的仓库 PermissionDenied、参数错误 InvalidArgument），Server
// 已经关闭或不再接受的（操作已关闭、attempt 已结束、租约过期、最终检查不通过 FailedPrecondition，GitHub 结果不明
// Aborted，最终检查超时或内部错误 Internal：Server 返回 Internal 之前已经关闭了登记的操作，或者还没有登记），Server
// 明确回答不认识这个 RPC 的 Unimplemented，attempt 失效或 Runner 被吊销（runnerkit 只按 Server 发来的原因 ErrStale、
// ErrRevoked 转换：原因是精确的字符串，本地合成的错误不会带上它；关闭请求同样会被拒绝）。
//
// 其余错误一律按原 op 关闭：Unavailable 重试用尽、没有收到响应、请求被取消，以及 Connect 客户端在本地合成的任何错误码
// （响应正文无法解码时的 InvalidArgument / Internal、网关的 404 / 502 等按 HTTP 状态合成的码）——这时 Server 可能已经
// 签出了 token。
func gitOpSettled(err error) bool {
	if errors.Is(err, runnerkit.ErrStale) || errors.Is(err, runnerkit.ErrRevoked) {
		return true
	}
	if !connect.IsWireError(err) {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeAborted, connect.CodeInternal,
		connect.CodeUnimplemented:
		return true
	}
	return false
}

// issueGitToken 经 IssueGitToken 签发，只对 Unavailable 用同一个 op 重试（issueRetryable）。旧 Server（Unimplemented）返回
// errServerTooOld。签发到的 token 登记到日志脱敏。
func (j *job) issueGitToken(ctx context.Context, op, repo string) (string, time.Time, error) {
	req := &runnerv1.IssueGitTokenRequest{Ref: j.spec.Ref, OpId: op, Repository: repo}
	resp, err := runnerkit.RetryWhen(ctx, rpcAttempts, issueRetryable, func() (*runnerv1.IssueGitTokenResponse, error) {
		return runnerkit.Call(ctx, j.r.c, j.r.c.RPC().IssueGitToken, req)
	})
	if err != nil {
		return "", time.Time{}, oldServerError(err)
	}
	j.log.AddSecret(resp.Token)
	return resp.Token, time.Unix(resp.ExpiresAt, 0), nil
}

// revokeGitToken 经 RevokeGitToken 关闭操作并吊销。按 op 幂等（关闭是终态，重复关闭、Server 重启后带 token 的关闭都只是
// 再吊销一次），所以没有收到回复时照常重试（runnerkit.Retryable），不会多出 token。
func (j *job) revokeGitToken(ctx context.Context, op, token string) error {
	req := &runnerv1.RevokeGitTokenRequest{Ref: j.spec.Ref, OpId: op, Token: token}
	_, err := runnerkit.Retry(ctx, rpcAttempts, func() (*runnerv1.RevokeGitTokenResponse, error) {
		return runnerkit.Call(ctx, j.r.c, j.r.c.RPC().RevokeGitToken, req)
	})
	return oldServerError(err)
}

// oldServerError 把 Unimplemented 转换为 errServerTooOld，并保留原来的错误。真实的旧 Server 对不认识的 RPC 回复普通的
// HTTP 404，Connect 客户端据此在本地合成 Unimplemented（不是 wire error），所以版本提示不能要求 wire error；网关、代理的
// 404 同样会得到这个提示（原来的错误附在后面，便于区分）。是否需要关闭操作不按这个提示判断：gitOpSettled 只认其中的
// wire error，本地合成的照常按原 op 关闭（旧 Server 上关闭同样失败，只记日志）。
func oldServerError(err error) error {
	if err != nil && connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w（%w）", errServerTooOld, err)
	}
	return err
}

// gitOpLog 把签发、吊销写进 job 日志，两者以操作 id 和 token 指纹配对（设计第 8 节：配对缺失即回收边界）。
type gitOpLog struct {
	log *runnerkit.LogShipper
}

func (l gitOpLog) issued(op, repo, token string, expires time.Time) {
	l.log.Printf("git 凭据：签发只读 token（%s，op %s，指纹 %s，%s 到期）", repo, op, redact.Fingerprint(token), expires.UTC().Format(time.RFC3339))
}

func (l gitOpLog) issueFailed(op, repo string, err error) {
	l.log.Printf("git 凭据：签发 %s 的只读 token 失败（op %s）：%v", repo, op, err)
}

// revoke 请 Server 关闭操作 op、回收它的 token（为空表示没有收到），最多用 gitRevokeTimeout（ctx 先结束时随之结束）。
// 失败只记日志：token 到期自然失效。
//
// RPC 成功只说明 Server 已经关闭了这个操作：Server 对 GitHub 的吊销失败时只记 warn（带同一个指纹和到期时间）、照样返回
// 成功（不重试，设计第 3 节），所以任务日志只写"已请求 Server 关闭"，不写成已吊销；实际的吊销结果以 Server 日志为准。
func (l gitOpLog) revoke(ctx context.Context, tokens gitTokens, op, repo, token string, expires time.Time) {
	ctx, cancel := context.WithTimeout(ctx, gitRevokeTimeout)
	defer cancel()
	if err := tokens.revokeGitToken(ctx, op, token); err != nil {
		l.revokeFailed(op, repo, token, expires, err)
		return
	}
	if token == "" {
		l.log.Printf("git 凭据：已请求 Server 关闭操作（%s，op %s；没有收到 token）", repo, op)
		return
	}
	l.log.Printf("git 凭据：已请求 Server 关闭操作、回收 token（%s，op %s，指纹 %s；GitHub 侧的吊销结果见 Server 日志）", repo, op, redact.Fingerprint(token))
}

// revokeFailed 记下没能请 Server 关闭操作 op 的原因。
func (l gitOpLog) revokeFailed(op, repo, token string, expires time.Time, err error) {
	fp, until := "", "没有收到 token"
	if token != "" {
		fp = "，指纹 " + redact.Fingerprint(token)
		until = expires.UTC().Format(time.RFC3339) + " 到期自然失效"
	}
	l.log.Printf("git 凭据：请求 Server 关闭操作失败（%s，op %s%s；%s）：%v", repo, op, fp, until, err)
}

// gitScope 是 job 可以经凭据通道取得凭据的仓库：任务仓库所在的 <协议>://<主机> 下的任务仓库和 repositories。
//
// 授权按 git 交给传输包装的地址（insteadOf 改写之后）比较：协议、主机（含端口）不区分大小写地相等，路径恰好是 owner/name
// （可带 .git 和首尾的 /，仓库名不区分大小写）。不做规范化：显式的默认端口（github.com:443）、主机名末尾的点、路径中间的
// 多个 / 都不算同一个仓库，这样的地址拿不到 Kiln 的凭据（TestGitScope）。
type gitScope struct {
	scheme, host string
	// repos 是 小写的 owner/name → 声明的写法（GitHub 的仓库名不区分大小写）。
	repos map[string]string
	// task 是任务仓库（没有源码的 job 为空）。
	task string
}

// defaultGitURL 是没有源码的 job 的仓库所在地址（repositories 都在 GitHub 上）。
const defaultGitURL = "https://github.com"

// newGitScope 按 JobSpec 计算 job 可以访问的仓库。
func newGitScope(sp *runnerv1.JobSpec) gitScope {
	s := gitScope{repos: map[string]string{}, task: sp.GetSource().GetRepository()}
	base := defaultGitURL
	if u := sp.GetSource().GetCloneUrl(); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		base = u
	}
	if u, err := url.Parse(base); err == nil {
		s.scheme, s.host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	}
	for _, r := range append([]string{s.task}, sp.Repositories...) {
		if r != "" {
			s.repos[strings.ToLower(r)] = r
		}
	}
	return s
}

// url 是 credential helper 生效的地址（<协议>://<主机>）。
func (s gitScope) url() string { return s.scheme + "://" + s.host }

// approve 返回地址 raw 对应的、job 可以访问的仓库（声明的写法）；不是时返回空和原因（只含 safeGitURL 的地址）。
func (s gitScope) approve(raw string) (repo, reason string) {
	shown := safeGitURL(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Sprintf("地址 %s 无法解析，这次 git 传输不使用 Kiln 的凭据", shown)
	}
	if strings.ToLower(u.Scheme) != s.scheme || strings.ToLower(u.Host) != s.host {
		return "", fmt.Sprintf("地址 %s 不在 %s 下，这次 git 传输不使用 Kiln 的凭据", shown, s.url())
	}
	if r, ok := s.repos[repoOfPath(u.Path)]; ok {
		return r, ""
	}
	return "", fmt.Sprintf("地址 %s 不是任务仓库 %s，也不在 job 的 repositories 中，这次 git 传输不使用 Kiln 的凭据（需要时在 workflow 的 repositories 中声明）", shown, s.task)
}

// matches 判断 credential helper 的请求（git 给出的协议、主机、路径）是否是仓库 repo。
func (s gitScope) matches(protocol, host, path, repo string) bool {
	return strings.EqualFold(protocol, s.scheme) && strings.EqualFold(host, s.host) && repoOfPath(path) == strings.ToLower(repo)
}

// repoOfPath 把地址或 helper 请求中的路径（owner/name、owner/name.git，可能带开头或结尾的 /）转换为小写的 owner/name；
// 不是这种形式时返回空。
func repoOfPath(p string) string {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	owner, name, ok := strings.Cut(p, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return strings.ToLower(p)
}

// unparsableGitURL 是解析不了的地址在诊断中的写法（不回显原文）。
const unparsableGitURL = "（无法解析的地址，不显示）"

// safeGitURL 是诊断中写的地址：只有协议、主机和能认出的仓库（owner/name），不含用户名、密码、query、fragment；路径认不出
// （不是 owner/name，或者有百分号编码）时只写主机；解析不了的地址不回显原文。
func safeGitURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return unparsableGitURL
	}
	repo := ""
	if u.RawPath == "" {
		repo = repoOfPath(u.Path)
	}
	if repo == "" {
		repo = "…"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host + "/" + repo
}

// gitSocketPathMax 返回本平台 unix socket 路径的上限（含结尾的 NUL）。
func gitSocketPathMax() int {
	if runtime.GOOS == "linux" {
		return gitSocketPathMaxLinux
	}
	return gitSocketPathMaxBSD
}

// gitChannel 是一个 job 的本地凭据通道。
type gitChannel struct {
	dir, sock string
	ln        *net.UnixListener
	scope     gitScope
	tokens    gitTokens
	log       gitOpLog
	// ctx 用于签发、吊销：不随 job 取消（job 结束时仍要关闭操作）；cancel 在收尾的期限到了时取消仍在进行的签发和吊销。
	ctx    context.Context
	cancel context.CancelFunc
	// closeTimeout 是收尾的总期限（gitCloseTimeout，测试中缩短）。
	closeTimeout time.Duration
	// stop 在收尾开始时关闭（Accept 的退避等待随之结束）。
	stop chan struct{}
	// revokeSlots 限制同时进行的关闭、吊销（gitCloseParallel）；busy 是还没结束的签发、关闭和吊销（收尾到期时记进日志）。
	revokeSlots chan struct{}
	busy        atomic.Int64

	mu     sync.Mutex
	closed bool
	ops    map[string]*gitOp
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup

	// beforeGet 只给测试用：读到 get 请求之后、处理之前调用。
	beforeGet func()
}

// gitOp 是一次 git 传输的操作。
type gitOp struct {
	id, repo string
	pid      int
	// closed 为 true 后不再交出凭据（连接已关闭或 job 已结束），操作已经从 ops 中摘下。
	closed bool
	// ready 在第一次要凭据时创建（之后 Server 上可能有这个操作），签发结束时关闭。
	ready   chan struct{}
	token   string
	expires time.Time
	err     error
}

// needsClose 判断关闭操作时要不要向 Server 关闭（调用方持有锁）：发出过签发请求、并且没有按约定确定不需要关闭
// （gitOpSettled）。签发还在途时要关闭（只带 op，迟到的 token 由 issue 吊销）。
func (o *gitOp) needsClose() bool {
	return o.ready != nil && (o.err == nil || !gitOpSettled(o.err))
}

// openGitChannel 在 parent（为空时是系统临时目录）中的一个新的 0700 目录中监听凭据通道的 socket（0600）。
func openGitChannel(ctx context.Context, parent string, scope gitScope, tokens gitTokens, log *runnerkit.LogShipper) (*gitChannel, error) {
	dir, err := os.MkdirTemp(parent, gitSocketDirPattern)
	if err != nil {
		return nil, fmt.Errorf("创建 git 凭据通道的目录：%w", err)
	}
	sock := filepath.Join(dir, gitSocketName)
	if n, limit := len(sock), gitSocketPathMax(); n >= limit {
		_ = os.RemoveAll(dir) // 目录是刚建的空目录
		return nil, fmt.Errorf("git 凭据通道的 socket 路径 %s 有 %d 字节，超过 %s 的上限 %d 字节（含结尾的 NUL）：把 Runner 的 TMPDIR 设为较短的目录", sock, n, runtime.GOOS, limit-1)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err == nil {
		err = os.Chmod(sock, 0o600)
	}
	if err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("监听 git 凭据通道 %s：%w", sock, err)
	}
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &gitChannel{
		dir: dir, sock: sock, ln: ln, scope: scope, tokens: tokens, log: gitOpLog{log: log},
		ctx: cctx, cancel: cancel, closeTimeout: gitCloseTimeout, stop: make(chan struct{}), revokeSlots: make(chan struct{}, gitCloseParallel),
		ops: map[string]*gitOp{}, conns: map[net.Conn]struct{}{},
	}
	c.wg.Add(1)
	go c.accept()
	return c, nil
}

func (c *gitChannel) accept() {
	defer c.wg.Done()
	var backoff time.Duration
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			select {
			case <-c.stop:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 非关闭类错误（例如文件描述符用尽）：记下来，退避后再接受，不让传输包装一直等不到回复
			backoff = min(max(2*backoff, gitAcceptBackoffMin), gitAcceptBackoffMax)
			c.log.log.Printf("git 凭据通道：接受连接失败（%v），%s 后重试", err, backoff)
			select {
			case <-c.stop:
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			_ = conn.Close()
			continue
		}
		c.conns[conn] = struct{}{}
		c.wg.Add(1)
		c.mu.Unlock()
		go c.serve(conn)
	}
}

// reply 写一条回复（有写期限）。
func reply(conn net.Conn, r gitReply) error {
	if err := conn.SetWriteDeadline(time.Now().Add(gitRequestTimeout)); err != nil {
		return err
	}
	return writeGitMsg(conn, r)
}

// serve 处理一个连接上的一条请求（读请求有期限）。
func (c *gitChannel) serve(conn net.Conn) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		delete(c.conns, conn)
		c.mu.Unlock()
		_ = conn.Close()
	}()
	if err := conn.SetReadDeadline(time.Now().Add(gitRequestTimeout)); err != nil {
		return
	}
	br := bufio.NewReaderSize(conn, maxGitMessage)
	var m gitMsg
	if err := readGitMsg(br, &m); err != nil {
		if _, ok := errors.AsType[*json.SyntaxError](err); ok {
			_ = reply(conn, gitReply{Error: "无法解析的请求"})
		}
		return
	}
	switch m.Cmd {
	case gitCmdOpen:
		c.open(conn, br, m)
	case gitCmdGet:
		_ = reply(conn, c.credential(m))
	case gitCmdPing:
		_ = reply(conn, gitReply{})
	default:
		_ = reply(conn, gitReply{Error: "不认识的命令"})
	}
}

// open 按地址开一个操作，回复操作 id，然后等连接关闭（传输包装在真实的远程助手退出后关闭，被杀时由内核关闭）：操作随之
// 结束、关闭。地址不是 job 可以访问的仓库时不开操作，只回复原因。
func (c *gitChannel) open(conn net.Conn, br *bufio.Reader, m gitMsg) {
	repo, reason := c.scope.approve(m.URL)
	if repo == "" {
		_ = reply(conn, gitReply{Denied: reason})
		return
	}
	o := &gitOp{id: newGitOpID(), repo: repo, pid: m.PID}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = reply(conn, gitReply{Error: "job 已经结束"})
		return
	}
	c.ops[o.id] = o
	c.mu.Unlock()
	if err := reply(conn, gitReply{Op: o.id}); err == nil && conn.SetReadDeadline(time.Time{}) == nil {
		// 传输包装之后不再发送任何内容：读到 EOF（或出错）即连接关闭
		_, _ = io.Copy(io.Discard, br)
	}
	c.closeOp(o)
}

// credential 回答 credential helper 的请求：只回答仍打开、协议 / 主机 / 路径与批准的仓库一致的操作；第一次要时签发，
// 同一操作之后的请求（一次传输可能有多个 HTTP 请求）返回同一枚。收尾开始后（操作已摘下）一律不签发、不交出。
func (c *gitChannel) credential(m gitMsg) gitReply {
	if c.beforeGet != nil {
		c.beforeGet()
	}
	c.mu.Lock()
	o := c.ops[m.Op]
	if o == nil {
		c.mu.Unlock()
		return gitReply{Error: "这次 git 传输的操作不存在或已经结束，不给凭据"}
	}
	if !c.scope.matches(m.Protocol, m.Host, m.Path, o.repo) {
		c.mu.Unlock()
		return gitReply{Error: fmt.Sprintf("请求的地址与这次传输批准的仓库 %s 不一致，不给凭据", o.repo)}
	}
	first := o.ready == nil
	if first {
		o.ready = make(chan struct{})
	}
	ready := o.ready
	c.mu.Unlock()
	if first {
		c.issue(o)
	}
	<-ready
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case o.closed:
		return gitReply{Error: "这次 git 传输的操作已经结束，不给凭据"}
	case o.err != nil:
		return gitReply{Error: o.err.Error()}
	}
	return gitReply{Username: gitTokenUser, Password: o.token}
}

// issue 为操作签发 token。签发期间操作被关闭（连接断开、job 收尾）时，关闭的一方已经按 op 关闭了操作，迟到的 token 在这里
// 立即吊销、不交出。
func (c *gitChannel) issue(o *gitOp) {
	c.busy.Add(1)
	defer c.busy.Add(-1)
	ctx, cancel := context.WithTimeout(c.ctx, gitIssueTimeout)
	tok, exp, err := c.tokens.issueGitToken(ctx, o.id, o.repo)
	cancel()
	c.mu.Lock()
	late := o.closed
	o.token, o.expires, o.err = tok, exp, err
	close(o.ready)
	c.mu.Unlock()
	switch {
	case err != nil:
		c.log.issueFailed(o.id, o.repo, err)
	case late:
		c.log.issued(o.id, o.repo, tok, exp)
		c.log.log.Printf("git 凭据：op %s 的签发结果在传输结束之后才返回，立即吊销、不交出", o.id)
		c.revoke(o.id, o.repo, tok, exp)
	default:
		c.log.issued(o.id, o.repo, tok, exp)
	}
}

// closeOp 关闭操作（只关闭一次）：按 needsClose 向 Server 关闭并吊销已收到的 token。
func (c *gitChannel) closeOp(o *gitOp) {
	c.mu.Lock()
	if o.closed {
		c.mu.Unlock()
		return
	}
	o.closed = true
	delete(c.ops, o.id)
	need, tok, exp := o.needsClose(), o.token, o.expires
	c.mu.Unlock()
	if need {
		c.revoke(o.id, o.repo, tok, exp)
	}
}

// revoke 向 Server 关闭操作 op 并吊销 token（为空表示没有收到）：占用 revokeSlots 中的一个位置，收尾到期（c.ctx 取消）时
// 不再等位置，只记日志。
func (c *gitChannel) revoke(op, repo, token string, expires time.Time) {
	c.busy.Add(1)
	defer c.busy.Add(-1)
	select {
	case c.revokeSlots <- struct{}{}:
	case <-c.ctx.Done():
		c.log.revokeFailed(op, repo, token, expires, c.ctx.Err())
		return
	}
	defer func() { <-c.revokeSlots }()
	c.log.revoke(c.ctx, c.tokens, op, repo, token, expires)
}

// pendingClose 是收尾时要向 Server 关闭的操作。
type pendingClose struct {
	op, repo, token string
	expires         time.Time
}

// close 在 job 结束时回收通道：
//  1. 在锁内一次停止签发和交付：标记收尾，摘下全部操作（之后到达或已经读到的 get 都找不到操作，不签发）；
//  2. 停止接受新连接，断开全部连接；
//  3. 关闭需要关闭的操作（与迟到 token 的吊销等共用 revokeSlots），等在途的签发返回（迟到的 token 由 issue 吊销）。
//
// 第 3 步最多等 closeTimeout：到期时取消仍在进行的签发和吊销，把没结束的个数记进 job 日志就返回，不再等它们（它们之后自行
// 结束，只用通道自己的状态、已经关闭的连接和 job 的日志——job 结束后写进日志的内容不再上传；没能吊销的 token 到期自然
// 失效）。最后删除 socket 目录。
func (c *gitChannel) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.stop)
	var pending []pendingClose
	for _, o := range c.ops {
		o.closed = true
		if o.needsClose() {
			pending = append(pending, pendingClose{op: o.id, repo: o.repo, token: o.token, expires: o.expires})
		}
	}
	clear(c.ops)
	conns := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
	}
	c.mu.Unlock()
	_ = c.ln.Close() // 关闭失败也不再接受：c.closed 已经设置
	for _, conn := range conns {
		_ = conn.Close() // 连接可能已经由对方关闭
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var revokes sync.WaitGroup
		for _, p := range pending {
			revokes.Go(func() { c.revoke(p.op, p.repo, p.token, p.expires) })
		}
		revokes.Wait()
		c.wg.Wait()
	}()
	deadline := time.NewTimer(c.closeTimeout)
	defer deadline.Stop()
	select {
	case <-done:
	case <-deadline.C:
		c.cancel()
		c.log.log.Printf("git 凭据：收尾到了 %s 的期限，还有 %d 个签发、关闭或吊销没有结束，不再等待（没能吊销的 token 到期自然失效）",
			c.closeTimeout, c.busy.Load())
	}
	c.cancel()
	_ = os.RemoveAll(c.dir) // 系统临时目录中的空目录，删不掉不影响结果
}

// writeGitMsg 写一条消息（一行 JSON）。
func writeGitMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readGitMsg 读一条消息（一行 JSON）。
func readGitMsg(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}
