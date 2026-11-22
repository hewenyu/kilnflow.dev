package runnerapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Native Runner 的领取（PLAN #14.1、PLAN #32）：长轮询、request_id 快速路径、升级指令、缓存清理在这里；选 job 和领取由调度核心的
// 原子预留入口（scheduler.Reserve）完成，领取即 running，同一个事务中完成领取、epoch+1、签发租约。

// runnerInfo 是领取时用到的 CI Runner 信息（来自 runners 和 ci_runners）。
type runnerInfo struct {
	ID        int64
	Name      string
	Pool      string
	Drain     bool
	Platforms []string
	Executors []string
}

// touchRunner 记录 Runner 在线；st 不为 nil 时同时更新它上报的平台、executor、资源、容量、缓存清单和清理结果。
func (s *Service) touchRunner(ctx context.Context, runnerID int64, st *runnerv1.RunnerState) (*runnerInfo, error) {
	ri := &runnerInfo{ID: runnerID}
	if _, err := s.db.Exec(ctx, `UPDATE runners SET last_seen_at=now(), version=coalesce(nullif($2, ''), version) WHERE id=$1`,
		runnerID, st.GetVersion()); err != nil {
		return ri, err
	}
	if st != nil {
		var capJSON, report []byte
		if c := capacityFromProto(st.GetCapacity()); c != nil {
			capJSON = store.MustJSON(c)
		}
		if st.GetCacheReport() != nil {
			b, err := protojson.Marshal(st.GetCacheReport())
			if err != nil {
				return nil, err
			}
			report = b
		}
		if _, err := s.db.Exec(ctx, `UPDATE ci_runners SET platforms=$2, executors=$3, capabilities=$4, running_jobs=$5,
			capacity=$6, cache_report=coalesce($7, cache_report), cache_reported_at=CASE WHEN $7::jsonb IS NULL THEN cache_reported_at ELSE now() END
			WHERE runner_id=$1`,
			runnerID, orEmpty(st.Platforms), orEmpty(st.Executors), store.MustJSON(runnerCaps(st)), st.RunningJobs, capJSON, report); err != nil {
			return ri, err
		}
	}
	if err := s.db.QueryRow(ctx, `SELECT `+runnerInfoCols+` FROM runners r JOIN ci_runners c ON c.runner_id=r.id WHERE r.id=$1`, runnerID).
		Scan(ri.scanDest()...); err != nil {
		return ri, err
	}
	if st == nil {
		return ri, nil
	}
	return ri, s.recordPurgeResults(ctx, runnerID, st.GetPurgeResults())
}

// runnerInfoCols 是 runnerInfo 读取的列（runners r、ci_runners c），顺序与 scanDest 一致。
const runnerInfoCols = `r.name, c.pool, r.drain, c.platforms, c.executors`

func (ri *runnerInfo) scanDest() []any {
	return []any{&ri.Name, &ri.Pool, &ri.Drain, &ri.Platforms, &ri.Executors}
}

// recordPurgeResults 记录 Runner 执行完的缓存清理请求的结果（只更新属于这台 Runner、还没有完成的）。
func (s *Service) recordPurgeResults(ctx context.Context, runnerID int64, results []*runnerv1.CachePurgeResult) error {
	for _, r := range results {
		b, err := protojson.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(ctx, `UPDATE runner_cache_purges SET state='done', done_at=now(), result=$3
			WHERE id=$1 AND runner_id=$2 AND state='pending'`, r.Id, runnerID, b); err != nil {
			return err
		}
	}
	return nil
}

// pendingPurges 返回这台 Runner 待执行的缓存清理请求。
func (s *Service) pendingPurges(ctx context.Context, runnerID int64) ([]*runnerv1.CachePurge, error) {
	rows, err := s.db.Query(ctx, `SELECT id, coalesce(repo_id, 0), kinds, domain FROM runner_cache_purges
		WHERE runner_id=$1 AND state='pending' ORDER BY id`, runnerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*runnerv1.CachePurge, error) {
		p := &runnerv1.CachePurge{}
		err := row.Scan(&p.Id, &p.RepoId, &p.Kinds, &p.Domain)
		return p, err
	})
}

// runnerCaps 返回写入 ci_runners.capabilities 的内容：主机指纹、上报的空闲资源。
func runnerCaps(st *runnerv1.RunnerState) map[string]any {
	caps := map[string]any{"host_fingerprint": st.GetHostFingerprint()}
	if st.GetFree() != nil {
		if cpu, ok := reportedFreeCPU(st); ok {
			caps["free_cpu"] = cpu
		}
		caps["free_memory"] = st.GetFree().GetMemoryBytes()
	}
	return caps
}

// reportedFreeCPU 返回 Runner 上报的空闲 CPU。没有上报，或者不是有限值（NaN、±Inf）时 ok 为 false：
// 非有限值不能编码为 JSON（store.MustJSON 会 panic），也不能用来比较资源，当作没有上报。
func reportedFreeCPU(st *runnerv1.RunnerState) (cpu float64, ok bool) {
	if st.GetFree() == nil {
		return 0, false
	}
	cpu = st.GetFree().GetCpu()
	if math.IsNaN(cpu) || math.IsInf(cpu, 0) {
		return 0, false
	}
	return cpu, true
}

// finitePositive 判断 Runner 上报的数值是否可以写入数据库：NaN、±Inf 和 0 / 负数都不写
// （NaN 与任何数比较都为假、Inf 能写进 double precision 却让统计失真）。
func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 0) }

// acquire 处理一次长轮询的 Acquire。
func (s *Service) acquire(ctx context.Context, runnerID int64, req *runnerv1.AcquireRequest) (*runnerv1.AcquireResponse, error) {
	ri, err := s.touchRunner(ctx, runnerID, req.State)
	if err != nil {
		return nil, err
	}
	purges, err := s.pendingPurges(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	resp := &runnerv1.AcquireResponse{Directives: &runnerv1.RunnerDirectives{Drain: ri.Drain, CachePurges: purges}}
	// 自升级（PLAN #27.13）：记录上报的状态（必要时结束升级请求），下发进行中的请求、registry 和检查序号
	if err := s.upgradeDirectives(ctx, ri, req, resp.Directives); err != nil {
		return nil, err
	}
	// 幂等领取：同一个 request_id 返回已经分配的那批任务（PLAN #6.1）。这里是快速路径：
	// 与之重叠的同一 request_id 的长轮询在领取事务中持有 Runner 行锁后还会再查一次
	if req.RequestId != "" {
		specs, err := s.replayAcquire(ctx, s.db, runnerID, req.RequestId)
		if err != nil {
			return nil, err
		}
		if len(specs) > 0 {
			resp.Jobs = specs
			return resp, nil
		}
	}
	// 有待执行的清理请求时 Runner 不会执行新任务（等空闲后先清理），这一轮不领取
	if ri.Drain || req.MaxJobs <= 0 || len(purges) > 0 {
		return resp, nil
	}
	wait := s.cfg.AcquireLongPoll
	if req.WaitSeconds > 0 && time.Duration(req.WaitSeconds)*time.Second < wait {
		wait = time.Duration(req.WaitSeconds) * time.Second
	}
	// 整个长轮询共用一个定时器；不用 context.WithDeadline，否则到期时会中途取消 tryAcquire 的事务
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var (
		ch      <-chan struct{}
		cursor  *scheduler.Cursor
		retryAt time.Time
		waited  bool // 已经等待过（之后返回前要重新决定升级指令）
	)
	for {
		if cursor == nil {
			// 每一轮从队首开始考察；分页考察期间的变化由这一轮开始时取得的通知覆盖
			ch, retryAt = s.hub.JobsChanged(), time.Time{}
		}
		res, err := s.tryAcquire(ctx, ri, req, cursor)
		switch {
		case errors.Is(err, scheduler.ErrRevoked):
			return nil, rpcerr.RunnerRevoked()
		case errors.Is(err, scheduler.ErrDrained):
			// 长轮询期间被暂停接单：立即返回最新的指令
			resp.Directives.Drain = true
			return resp, nil
		case err != nil:
			return nil, err
		}
		if len(res.Claims) > 0 {
			return s.claimed(ctx, ri, resp, &res, waited)
		}
		if res.Failed > 0 {
			// 这次预留把 job 本身有问题的候选置为失败：它们已经离开队列，等着的其他 Runner（包括自己）应当立即重新考察，
			// 而不是等到这一次长轮询到期。从队首重新开始（游标指向的位置是旧的一页之后，失败之后那里不再是接着考察的地方）。
			s.hub.All()
			cursor = nil
			if ctx.Err() != nil {
				return resp, nil
			}
			continue
		}
		if !res.RetryAt.IsZero() && (retryAt.IsZero() || res.RetryAt.Before(retryAt)) {
			retryAt = res.RetryAt
		}
		// 这一页的候选都领不了：立即考察下一页。每页一个事务，锁住的候选不超过一页。
		// 长轮询到期也要考察完：否则队列长到一次长轮询扫不完时，每次都从队首重新开始，后面的 job 仍然领不到
		if cursor = res.Next; cursor != nil {
			if ctx.Err() != nil {
				return resp, nil
			}
			continue
		}
		waited = true
		done, err := s.afterWait(ctx, ri, resp, waitJobs(ctx, ch, retryAt, timer))
		if err != nil {
			return nil, err
		}
		if done {
			return resp, nil
		}
	}
}

// claimed 返回领到的任务；等待过（waited）时按当前的升级请求重新决定升级指令。
func (s *Service) claimed(ctx context.Context, ri *runnerInfo, resp *runnerv1.AcquireResponse,
	res *scheduler.Result[*runnerv1.JobSpec], waited bool,
) (*runnerv1.AcquireResponse, error) {
	resp.Jobs = specsOf(res.Claims)
	if !res.Replayed {
		s.hub.All()
	}
	if waited {
		if _, err := s.refreshUpgrade(ctx, ri, resp.Directives); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// afterWait 在长轮询的一次等待之后（woke 为假表示长轮询到期或 ctx 结束）决定是否立即返回。
func (s *Service) afterWait(ctx context.Context, ri *runnerInfo, resp *runnerv1.AcquireResponse, woke bool) (bool, error) {
	// 等待期间管理员创建、取消了升级请求或请求检查（会通知长轮询）：重新决定指令，变了就立即返回；到期返回前
	// 也重新决定，不下发长轮询开始时算出的旧指令
	changed, err := s.refreshUpgrade(ctx, ri, resp.Directives)
	if err != nil || !woke || changed {
		return true, err
	}
	// 等待期间发起的缓存清理请求：立即返回，让 Runner 尽快收到
	purges, err := s.pendingPurges(ctx, ri.ID)
	if err != nil {
		return true, err
	}
	if len(purges) > 0 {
		resp.Directives.CachePurges = purges
		return true, nil
	}
	return false, nil
}

// refreshUpgrade 按当前的升级请求、registry 和检查序号重新决定要下发的内容写到 d，返回是否与之前不同。不再记录上报的
// 状态（这次 Acquire 开始时已经记录）。ctx 已经结束（Runner 断开）时不查询，返回原样。
func (s *Service) refreshUpgrade(ctx context.Context, ri *runnerInfo, d *runnerv1.RunnerDirectives) (bool, error) {
	select {
	case <-ctx.Done():
		return false, nil
	default:
	}
	up, err := upgradectl.Refresh(ctx, s.db, ri.ID)
	if err != nil {
		return false, err
	}
	return up.Fill(d), nil
}

// upgradeDirectives 记录 Runner 上报的升级状态，把升级指令、registry 和检查序号填到 d（PLAN #27.13）。
func (s *Service) upgradeDirectives(ctx context.Context, ri *runnerInfo, req *runnerv1.AcquireRequest, d *runnerv1.RunnerDirectives) error {
	up, err := upgradectl.Acquire(ctx, s.db, upgradectl.AcquireRunner{
		ID: ri.ID, Name: ri.Name, Kind: store.RunnerKindCI, Version: req.GetState().GetVersion(), Status: req.GetState().GetUpgrade(),
	})
	if err != nil {
		return err
	}
	up.Fill(d)
	return nil
}

// waitJobs 等待队列变化（ch）或到 retryAt（为了让更合适的 Runner 领取而跳过的 job 到期后再试，期间那台 Runner
// 领走了就不会再看到它；零值表示没有）。长轮询到期（timer）或 ctx 结束时返回 false。
func waitJobs(ctx context.Context, ch <-chan struct{}, retryAt time.Time, timer *time.Timer) bool {
	retry := time.NewTimer(time.Hour)
	defer retry.Stop()
	if !retryAt.IsZero() {
		retry.Reset(max(time.Until(retryAt), 10*time.Millisecond))
	}
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	case <-ch:
	case <-retry.C:
	}
	return true
}

// replayAcquire 返回 Runner 用 requestID 已经领取、还在执行的那批任务（没有时为空）。
func (s *Service) replayAcquire(ctx context.Context, q store.Querier, runnerID int64, requestID string) ([]*runnerv1.JobSpec, error) {
	list, err := scheduler.Replayed(ctx, q, runnerID, requestID)
	if err != nil {
		return nil, err
	}
	out := make([]*runnerv1.JobSpec, 0, len(list))
	for _, c := range list {
		spec, err := s.buildJobSpec(ctx, q, c.Job, c.AttemptID, c.Epoch)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// tryAcquire 考察从 after 开始的一页候选（after 为 nil 时从队首开始），为 Runner 选出并领取 job（scheduler.Reserve）。
// 同一个 request_id 已经领取过时返回那批任务（Result.Replayed）。
func (s *Service) tryAcquire(ctx context.Context, ri *runnerInfo, req *runnerv1.AcquireRequest, after *scheduler.Cursor) (scheduler.Result[*runnerv1.JobSpec], error) {
	return scheduler.Reserve(ctx, s.sched, scheduler.Trigger{Native: nativeTrigger(ri, req)}, after, s.prepareSpec)
}

// prepareSpec 是 Native 领取的 Prepare 回调：生成下发给 Runner 的 JobSpec。job 本身的问题（*specError）转换为
// scheduler.PrepareError，只让这个 job 失败。
func (s *Service) prepareSpec(ctx context.Context, q store.Querier, j *cistore.Job, attemptID, epoch int64) (*runnerv1.JobSpec, error) {
	spec, err := s.buildJobSpec(ctx, q, j, attemptID, epoch)
	if bad, ok := errors.AsType[*specError](err); ok {
		return nil, &scheduler.PrepareError{Err: fmt.Errorf("无法生成下发给 Runner 的 job：%w", bad)}
	}
	return spec, err
}

// specsOf 返回领到的 JobSpec，并写上领取得到的 attempt 和 epoch。
func specsOf(claims []scheduler.Claim[*runnerv1.JobSpec]) []*runnerv1.JobSpec {
	out := make([]*runnerv1.JobSpec, 0, len(claims))
	for _, c := range claims {
		c.Prepared.Ref = &runnerv1.AttemptRef{AttemptId: c.AttemptID, Epoch: c.Epoch}
		out = append(out, c.Prepared)
	}
	return out
}

// defaultInputs 返回等价于默认 inputs 的模式（整个仓库，不含 workflow 目录 .kiln/workflows/）。
// 每次返回独立的切片：它会放进下发给 Runner 的 JobSpec，不能与其他 JobSpec 共用。
func defaultInputs() []string { return []string{"**", "!" + workflow.Dir + "/**"} }

// specError 表示 job 本身的问题导致无法生成 JobSpec（重试也不会好），与数据库等暂时性错误区分。
type specError struct{ err error }

func (e *specError) Error() string { return e.err.Error() }
func (e *specError) Unwrap() error { return e.err }

// buildJobSpec 生成下发给 Runner 的 JobSpec（不含 token 和 secret 的值）。
// job 本身的问题返回 *specError。
func (s *Service) buildJobSpec(ctx context.Context, q store.Querier, j *cistore.Job, attemptID, epoch int64) (*runnerv1.JobSpec, error) {
	run, err := cistore.GetRun(ctx, q, j.RunID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, q, run.RepoID)
	if err != nil {
		return nil, err
	}
	sp := j.Spec
	inputs := sp.Inputs
	if sp.InputsDefault {
		inputs = defaultInputs()
	}
	out := &runnerv1.JobSpec{
		Ref:             &runnerv1.AttemptRef{AttemptId: attemptID, Epoch: epoch},
		LeaseTtlSeconds: clampInt32(int64(s.cfg.LeaseTTL / time.Second)),
		Source: &runnerv1.Source{
			RepoId:      repo.ID,
			Repository:  repo.FullName(),
			CloneUrl:    repo.CloneURL,
			Sha:         run.SHA,
			Inputs:      inputs,
			InputDigest: deref(j.InputDigest),
		},
		Executor:         &runnerv1.ExecutorSpec{Kind: string(sp.Executor), Image: sp.Container, Platform: sp.Platform, BuildkitImage: sp.BuildkitImage},
		Env:              sp.Env,
		TimeoutSeconds:   clampInt32(sp.TimeoutMinutes * 60),
		Resources:        &runnerv1.Resources{Cpu: sp.CPU, MemoryBytes: sp.MemoryBytes},
		Effects:          sp.Effects,
		NeedsCredentials: true,
		KeepOnFailure:    j.KeepOnFailure,
		Network:          sp.Network,
		JobId:            j.ID,
		RunId:            j.RunID,
		JobKey:           j.JobKey,
		DisplayName:      j.DisplayName,
		Workflow:         run.Workflow,
		SecretNames:      sp.Secrets,
		Repositories:     sp.Repositories,
		Signing:          signingSpec(sp.Signing),
	}

	for _, r := range sp.RuntimeRefs {
		out.Runtimes = append(out.Runtimes, &runnerv1.RuntimeRef{Name: r.Name, Version: r.Version, Platform: r.Platform, Digest: r.Digest})
	}
	for _, st := range sp.Steps {
		out.Steps = append(out.Steps, &runnerv1.Step{Name: st.Name, Run: st.Run, Shell: st.Shell, WorkingDirectory: st.WorkingDirectory, Env: st.Env, ContinueOnError: st.ContinueOnError})
	}
	for _, o := range sp.Outputs {
		out.Outputs = append(out.Outputs, &runnerv1.OutputSpec{Name: o.Name, Path: o.Path})
	}
	for _, sv := range sp.Services {
		out.Services = append(out.Services, &runnerv1.Service{Name: sv.Name, Image: sv.Image, Env: sv.Env, Command: sv.Command})
	}
	for _, tc := range sp.Toolcache {
		out.Toolcache = append(out.Toolcache, &runnerv1.ToolCache{Name: tc.Name, Env: tc.Env})
	}
	for _, f := range sp.Fetch {
		var rm *string
		err := q.QueryRow(ctx, `SELECT result_manifest FROM jobs WHERE run_id=$1 AND rerun=$2 AND job_key=$3`, j.RunID, j.Rerun, f.JobKey).Scan(&rm)
		if errors.Is(err, pgx.ErrNoRows) {
			// 上游 job 不在这次运行中：与没有结果一样是 job 本身的问题，不能让整次领取回滚
			return nil, &specError{fmt.Errorf("fetch 的上游 %s 不存在", f.JobKey)}
		}
		if err != nil {
			return nil, fmt.Errorf("fetch 的上游 %s：%w", f.JobKey, err)
		}
		if rm == nil {
			return nil, &specError{fmt.Errorf("fetch 的上游 %s 没有结果", f.JobKey)}
		}
		d, err := s.engine.OutputManifest(ctx, *rm, f.Output)
		if _, ok := errors.AsType[*engine.NoSuchOutputError](err); ok || errors.Is(err, cas.ErrNotFound) || errors.Is(err, cas.ErrInvalidObject) {
			return nil, &specError{fmt.Errorf("fetch 的上游 %s：%w", f.JobKey, err)}
		}
		if err != nil {
			return nil, err
		}
		out.Fetch = append(out.Fetch, &runnerv1.FetchSpec{From: f.JobKey + "." + f.Output, To: f.To, ManifestDigest: d})
	}
	if out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = workflow.DefaultTimeoutMinutes * 60
	}
	return out, nil
}

// clampInt32 把 int 转换为 int32，超出范围时取边界值（用于填写 protobuf 的计数字段）。
func clampInt32[T int | int64](n T) int32 {
	switch {
	case int64(n) > math.MaxInt32:
		return math.MaxInt32
	case int64(n) < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// signingSpec 把 job 的 signing 转换为下发给 Runner 的形式（secret 仍是占位符）。
func signingSpec(s *workflow.Signing) *runnerv1.Signing {
	if s == nil {
		return nil
	}
	out := &runnerv1.Signing{ProvisioningProfiles: s.ProvisioningProfiles}
	for _, c := range s.Certificates {
		out.Certificates = append(out.Certificates, &runnerv1.SigningCertificate{P12: c.P12, Password: c.Password})
	}
	if n := s.Notary; n != nil {
		out.Notary = &runnerv1.Notary{
			Profile: n.Profile, AppleId: n.AppleID, TeamId: n.TeamID, Password: n.Password,
			Key: n.Key, KeyId: n.KeyID, Issuer: n.Issuer,
		}
	}
	return out
}
