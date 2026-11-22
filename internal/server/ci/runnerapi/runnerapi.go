// Package runnerapi 实现 Runner 使用的 RunnerService（PLAN #9.1）：注册与认证（PLAN #9.3）、
// 调度（Acquire，PLAN #14.1）、心跳、日志、CAS 预签名和结果提交。
package runnerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerprogress"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"

	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
)

const (
	// casURLTTL 是下发给 Runner 的 CAS 预签名 URL 的有效期。
	casURLTTL = time.Hour
	// errClassUpload 是结果不完整（上传的对象缺失或 manifest 无效）时 attempt 的错误分类，与 Runner 上报的相同。
	errClassUpload = "infra.upload"
)

// Config 是 RunnerService 需要的配置（由 server.Config 映射）。
type Config struct {
	// LeaseTTL 是 Runner 租约的有效期。
	LeaseTTL time.Duration
	// AcquireLongPoll 是 Acquire 长轮询的最长等待时间。
	AcquireLongPoll time.Duration
	// MasterKey 用于解密下发给 job 的 secret。
	MasterKey []byte
}

// Deps 是 Service 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 没有配置 GitHub App 时为 nil，此时不签发源码 token。
	GitHub *gh.Client
	// GitCred 是按操作签发源码 token 的登记（AI、CI 共用一个实例）；没有配置 GitHub App 时为 nil。
	GitCred *gitcred.Registry
	CAS     cas.Store
	// Keys 签名 CAS URL。
	Keys *auth.Keys
	// Auth 是 Runner 的注册与认证（CI、AI 共用）。
	Auth *runnerauth.Auth
	Logs *logstore.Store
	// Engine 在 Complete 时结束 attempt，并读取上游输出的 manifest。
	Engine *engine.Engine
	// Scheduler 是 CI 调度核心的原子预留入口：Acquire 经它选 job 并领取（PLAN #32）。
	Scheduler *scheduler.Reserver
	// Hub 用于 Acquire 的长轮询，以及状态变化后唤醒各组件。
	Hub *notify.Hub
	Log *slog.Logger
}

// Service 实现 RunnerService（PLAN #9.1）。
type Service struct {
	cfg Config
	db  *store.DB
	gh  *gh.Client
	// gitcred 没有配置 GitHub App 时为 nil。
	gitcred *gitcred.Registry
	cas     cas.Store
	keys    *auth.Keys
	auth    *runnerauth.Auth
	logs    *logstore.Store
	engine  *engine.Engine
	sched   *scheduler.Reserver
	hub     *notify.Hub
	log     *slog.Logger
}

var _ runnerv1connect.RunnerServiceHandler = (*Service)(nil)

// New 返回 RunnerService 的实现。
func New(cfg Config, d Deps) *Service {
	return &Service{
		cfg: cfg, db: d.DB, gh: d.GitHub, gitcred: d.GitCred, cas: d.CAS, keys: d.Keys, auth: d.Auth, logs: d.Logs,
		engine: d.Engine, sched: d.Scheduler, hub: d.Hub, log: d.Log,
	}
}

// Acquire 长轮询领取 job：领取即 running（PLAN #14.1）；同一个 request_id 重试时返回已经分配的那批 job。
func (s *Service) Acquire(ctx context.Context, req *connect.Request[runnerv1.AcquireRequest]) (*connect.Response[runnerv1.AcquireResponse], error) {
	// 只接受 CI Runner：AI 执行节点用 airunner.v2（PLAN #33.6）
	id, err := s.auth.AuthorizeKind(ctx, req.Header(), store.RunnerKindCI)
	if err != nil {
		return nil, err
	}
	resp, err := s.acquire(ctx, id, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// lockAttemptRuns 在锁 attempt 的 job 之前按加锁顺序锁住 job 所在的 run（cistore.LockRuns），返回已锁定的 run ID。
// attempt 不存在时什么都不做，由 attemptJob 返回 STALE_EPOCH。
func lockAttemptRuns(ctx context.Context, tx pgx.Tx, ref *runnerv1.AttemptRef) ([]int64, error) {
	ids, err := store.QueryIDs(ctx, tx, `SELECT job_id FROM attempts WHERE id=$1`, ref.GetAttemptId())
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return cistore.LockRunsOfJob(ctx, tx, ids[0])
}

// attemptJob 校验 (attempt_id, epoch)：必须属于这个 Runner、epoch 是当前值、job 仍在运行。
// 已删除的 Runner 的 attempt 的 runner_id 为 NULL（按 0 读取），不属于任何 Runner。
func (s *Service) attemptJob(ctx context.Context, q store.Querier, runnerID int64, ref *runnerv1.AttemptRef, lock bool) (*cistore.Job, error) {
	if ref == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 AttemptRef"))
	}
	var jobID, epoch, owner int64
	err := q.QueryRow(ctx, `SELECT job_id, epoch, coalesce(runner_id, 0) FROM attempts WHERE id=$1`, ref.AttemptId).Scan(&jobID, &epoch, &owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (owner != runnerID || epoch != ref.Epoch)) {
		return nil, rpcerr.StaleEpoch()
	}
	if err != nil {
		return nil, err
	}
	j, err := cistore.GetJob(ctx, q, jobID, lock)
	if err != nil {
		return nil, err
	}
	if j.Epoch != ref.Epoch || j.State != store.JobRunning {
		return nil, rpcerr.StaleEpoch()
	}
	return j, nil
}

// credentialJob 是下发凭据、secret 之前校验过的 job：attempt 属于调用的 Runner、epoch 是当前值、job 仍在运行，
// environment（如果有）仍存在且 ref 策略允许这个 run 的 ref。
type credentialJob struct {
	job  *cistore.Job
	run  *cistore.Run
	repo *store.Repo
	// env 是 job 的 environment；没有时为 nil。
	env *store.Environment
}

// credentialJob 校验 attempt 并重新检查 environment 的授权（FetchCredentials、FetchSecrets、IssueGitToken 共用）。
func (s *Service) credentialJob(ctx context.Context, runnerID int64, ref *runnerv1.AttemptRef) (*credentialJob, error) {
	j, err := s.attemptJob(ctx, s.db, runnerID, ref, false)
	if err != nil {
		return nil, err
	}
	run, err := cistore.GetRun(ctx, s.db, j.RunID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, s.db, run.RepoID)
	if err != nil {
		return nil, err
	}
	c := &credentialJob{job: j, run: run, repo: repo}
	// 排队或执行期间管理员可能已删除 environment 或收紧 ref 策略。
	// 发放任何凭据前重新校验，不能沿用规划时的授权或回退到其他作用域的同名 secret。
	if name := j.Spec.Environment; name != "" {
		c.env, err = store.LookupEnvironment(ctx, s.db, repo.ID, name)
		if err != nil {
			return nil, err
		}
		if c.env == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("environment %s 不存在，不发放凭据", name))
		}
		if !c.env.RefAllowed(run.Ref) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("environment %s 的 ref 策略不允许 %s，不发放凭据", c.env.Name, run.Ref))
		}
	}
	return c, nil
}

// secrets 返回 job 引用的 secret（environment 级覆盖仓库级，仓库级覆盖账号级）。
func (s *Service) secrets(ctx context.Context, c *credentialJob) (map[string]string, error) {
	return store.SecretsFor(ctx, s.db, s.cfg.MasterKey, c.repo, c.env, c.job.Spec.Secrets)
}

// dependency 查找 job 的 repositories 中的仓库 name：必须与任务仓库 repo 在同一个 installation 下。不在 Kiln 中时只定向读取
// 这一个仓库。暂时的故障（GitHub、数据库）让 Runner 重试（Unavailable），不在授权范围内时拒绝。
func (s *Service) dependency(ctx context.Context, repo *store.Repo, name string) (*store.Repo, error) {
	r, err := repodir.Lookup(ctx, s.db, s.gh, repo.InstallationID, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("查找仓库 %s：%w", name, err))
	}
	if r == nil || r.InstallationID != repo.InstallationID {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("仓库 %s 不在同一个 installation 下", name))
	}
	return r, nil
}

// FetchCredentials 为 attempt 签发源码 token（只读，限定在 job 用到的仓库），并下发 job 引用的 secret。只给旧版 Runner：
// 新版 Runner 用 FetchSecrets 和按操作签发的 IssueGitToken（PLAN #8.4）。
func (s *Service) FetchCredentials(ctx context.Context, req *connect.Request[runnerv1.FetchCredentialsRequest]) (*connect.Response[runnerv1.FetchCredentialsResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	c, err := s.credentialJob(ctx, id, req.Msg.Ref)
	if err != nil {
		return nil, err
	}
	resp := &runnerv1.FetchCredentialsResponse{}
	if s.gh != nil {
		ids := []int64{c.repo.ID}
		for _, name := range c.job.Spec.Repositories {
			r, err := s.dependency(ctx, c.repo, name)
			if err != nil {
				return nil, err
			}
			ids = append(ids, r.ID)
		}
		tok, err := s.gh.CreateToken(ctx, c.repo.InstallationID, ids, map[string]string{"contents": gitReadPermission})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("签发源码 token：%w", err))
		}
		resp.GitToken, resp.GitTokenExpiresAt = tok.Token, tok.ExpiresAt.Unix()
	}
	resp.Secrets, err = s.secrets(ctx, c)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// FetchSecrets 下发 job 引用的 secret（校验与 FetchCredentials 相同），不签发任何 token。
func (s *Service) FetchSecrets(ctx context.Context, req *connect.Request[runnerv1.FetchSecretsRequest]) (*connect.Response[runnerv1.FetchSecretsResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	c, err := s.credentialJob(ctx, id, req.Msg.Ref)
	if err != nil {
		return nil, err
	}
	secrets, err := s.secrets(ctx, c)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&runnerv1.FetchSecretsResponse{Secrets: secrets}), nil
}

// Heartbeat 续租 attempt 的租约并记录当前 step，返回是否已请求取消。
func (s *Service) Heartbeat(ctx context.Context, req *connect.Request[runnerv1.HeartbeatRequest]) (*connect.Response[runnerv1.HeartbeatResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	ref := req.Msg.Ref
	if _, err := s.attemptJob(ctx, s.db, id, ref, false); err != nil {
		return nil, err
	}
	var cancel bool
	var exp time.Time
	err = s.db.QueryRow(ctx, `UPDATE jobs SET lease_expires_at=now()+make_interval(secs => $3), started_step = started_step OR $4
		WHERE current_attempt_id=$1 AND epoch=$2 AND state='running' RETURNING cancel_requested, lease_expires_at`,
		ref.AttemptId, ref.Epoch, s.cfg.LeaseTTL.Seconds(), req.Msg.CurrentStep >= 0).Scan(&cancel, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpcerr.StaleEpoch()
	}
	if err != nil {
		return nil, err
	}
	// 旧版 Runner 不上报进度（nil）：保留原值（NULL），Dashboard 显示"进度暂不可用"
	var prog []byte
	if p := req.Msg.Progress; p != nil {
		if prog, err = json.Marshal(runnerprogress.FromProto(p, req.Msg.CurrentStep, time.Now())); err != nil {
			return nil, err
		}
	}
	if _, err := s.db.Exec(ctx, `UPDATE attempts SET current_step=$2, progress=coalesce($3::jsonb, progress) WHERE id=$1`,
		ref.AttemptId, req.Msg.CurrentStep, prog); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE runners SET last_seen_at=now() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&runnerv1.HeartbeatResponse{CancelRequested: cancel, LeaseExpiresAt: exp.Unix()}), nil
}

// AppendLog 追加 attempt 的日志（按 offset 去重，重传是幂等的）。
func (s *Service) AppendLog(ctx context.Context, req *connect.Request[runnerv1.AppendLogRequest]) (*connect.Response[runnerv1.AppendLogResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	ref := req.Msg.Ref
	if req.Msg.Offset < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("无效的 offset %d", req.Msg.Offset))
	}
	// 日志要求 attempt 属于这个 Runner，并且没有被 Server 判为失联（失联的 attempt 已被取代，job 可能已被重新领取；
	// 与 Heartbeat、Complete 一致返回 STALE_EPOCH）。正常结束的 attempt 仍允许补传最后一段。
	var owner int64
	var errClass string
	err = s.db.QueryRow(ctx, `SELECT coalesce(runner_id, 0), coalesce(error_class, '') FROM attempts WHERE id=$1 AND epoch=$2`,
		ref.GetAttemptId(), ref.GetEpoch()).Scan(&owner, &errClass)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (owner != id || errClass == engine.ErrClassLost)) {
		return nil, rpcerr.StaleEpoch()
	}
	if err != nil {
		return nil, err
	}
	n, err := s.logs.Append(ref.AttemptId, req.Msg.Offset, req.Msg.Data)
	if errors.Is(err, logstore.ErrBadOffset) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		// 存储故障（磁盘满、权限等）：作为内部错误记录日志，Runner 稍后重传
		return nil, fmt.Errorf("写入日志：%w", err)
	}
	return connect.NewResponse(&runnerv1.AppendLogResponse{Committed: n}), nil
}

// PrepareUpload 返回 CAS 中还没有的对象的上传地址（预签名 URL）；已有的对象刷新引用时间（last_ref_at）。
func (s *Service) PrepareUpload(ctx context.Context, req *connect.Request[runnerv1.PrepareUploadRequest]) (*connect.Response[runnerv1.PrepareUploadResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.attemptJob(ctx, s.db, id, req.Msg.Ref, false); err != nil {
		return nil, err
	}
	resp := &runnerv1.PrepareUploadResponse{}
	for _, b := range req.Msg.Blobs {
		if !cas.ValidDigest(b.Digest) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("无效的 digest %q", b.Digest))
		}
		has, err := s.cas.Has(ctx, b.Digest)
		if err != nil {
			return nil, err
		}
		if has {
			// 只有刷新到 blobs 记录才算已存在：保留策略的清理在行锁下删除对象（PLAN #10.4），
			// 记录已被删除（或者文件没有记录）时让 Runner 重新上传
			tag, err := s.db.Exec(ctx, `UPDATE blobs SET last_ref_at=now() WHERE digest=$1`, b.Digest)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() > 0 {
				continue
			}
		}
		resp.Uploads = append(resp.Uploads, &runnerv1.UploadTarget{Digest: b.Digest, Url: s.keys.CASURL(auth.CASOpPut, b.Digest, casURLTTL)})
	}
	return connect.NewResponse(resp), nil
}

// PrepareDownload 返回 CAS 对象的下载地址（预签名 URL）。
func (s *Service) PrepareDownload(ctx context.Context, req *connect.Request[runnerv1.PrepareDownloadRequest]) (*connect.Response[runnerv1.PrepareDownloadResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.attemptJob(ctx, s.db, id, req.Msg.Ref, false); err != nil {
		return nil, err
	}
	resp := &runnerv1.PrepareDownloadResponse{}
	for _, d := range req.Msg.Digests {
		if !cas.ValidDigest(d) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("无效的 digest %q", d))
		}
		has, err := s.cas.Has(ctx, d)
		if err != nil {
			return nil, err
		}
		if !has {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("对象 %s 不存在", d))
		}
		resp.Downloads = append(resp.Downloads, &runnerv1.UploadTarget{Digest: d, Url: s.keys.CASURL(auth.CASOpGet, d, casURLTTL)})
	}
	return connect.NewResponse(resp), nil
}

// Complete 提交 attempt 的结果；(attempt_id, epoch) 不是当前值时返回 STALE_EPOCH，保证结果只提交一次。
func (s *Service) Complete(ctx context.Context, req *connect.Request[runnerv1.CompleteRequest]) (*connect.Response[runnerv1.CompleteResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	res := engine.AttemptResult{
		Outcome: outcomeFromProto(m.Outcome), ErrClass: m.ErrorClass, ExitCode: int(m.ExitCode),
		Message: m.Message, Manifest: m.ResultManifestDigest,
	}
	if res.Outcome == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 outcome"))
	}
	// 先确认 attempt 属于这台 Runner、epoch 是当前值、job 仍在运行：结果校验要读 CAS 中的 manifest，
	// 不属于自己的、已经被取代的 attempt 不该去读存储，也不该因为存储故障被当成本次的结果问题
	if _, err := s.attemptJob(ctx, s.db, id, m.Ref, false); err != nil {
		return nil, err
	}
	if res.Outcome == store.OutcomeSucceeded {
		if err := s.verifyResult(ctx, m.ResultManifestDigest); err != nil {
			// 结果不完整：按 infra.upload 处理
			res.Outcome, res.ErrClass, res.Message = store.OutcomeInfraError, errClassUpload, err.Error()
		}
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		runs, err := lockAttemptRuns(ctx, tx, m.Ref)
		if err != nil {
			return err
		}
		j, err := s.attemptJob(ctx, tx, id, m.Ref, true)
		if err != nil {
			return err
		}
		if finitePositive(m.CpuSeconds) {
			if _, err := tx.Exec(ctx, `UPDATE attempts SET cpu_seconds=$2 WHERE id=$1`, m.Ref.AttemptId, m.CpuSeconds); err != nil {
				return err
			}
		}
		return s.engine.FinishAttempt(ctx, tx, j, res, runs)
	})
	if err != nil {
		return nil, err
	}
	s.engine.AttemptEnded(ctx, m.Ref.AttemptId)
	s.hub.All()
	return connect.NewResponse(&runnerv1.CompleteResponse{}), nil
}

// verifyResult 检查结果 manifest 引用的对象都已存在（PLAN #10.2）。
func (s *Service) verifyResult(ctx context.Context, digest string) error {
	if !cas.ValidDigest(digest) {
		return fmt.Errorf("结果 manifest 的 digest 无效：%q", digest)
	}
	var rm cas.ResultManifest
	if err := cas.ReadJSON(ctx, s.cas, digest, &rm); err != nil {
		return fmt.Errorf("读取结果 manifest：%w", err)
	}
	if rm.Schema != cas.ResultSchema {
		return fmt.Errorf("结果 manifest 的 schema 不正确：%s", rm.Schema)
	}
	for name, od := range rm.Outputs {
		var om cas.OutputManifest
		if err := cas.ReadJSON(ctx, s.cas, od, &om); err != nil {
			return fmt.Errorf("输出 %s 的 manifest：%w", name, err)
		}
		for _, f := range om.Files {
			if f.Symlink != "" {
				continue
			}
			ok, err := s.cas.Has(ctx, f.Digest)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("输出 %s 的文件 %s（%s）没有上传", name, f.Path, f.Digest)
			}
		}
	}
	return nil
}

// outcomeFromProto 把 Runner 上报的结果转换为 attempts.outcome 的取值；未知值返回空字符串。
func outcomeFromProto(o runnerv1.Outcome) store.Outcome {
	switch o {
	case runnerv1.Outcome_OUTCOME_SUCCEEDED:
		return store.OutcomeSucceeded
	case runnerv1.Outcome_OUTCOME_FAILED:
		return store.OutcomeFailed
	case runnerv1.Outcome_OUTCOME_INFRA_ERROR:
		return store.OutcomeInfraError
	case runnerv1.Outcome_OUTCOME_CANCELLED:
		return store.OutcomeCancelled
	case runnerv1.Outcome_OUTCOME_TIMED_OUT:
		return store.OutcomeTimedOut
	}
	return ""
}
