// Package engine 是 kiln-server 的状态机（PLAN #6.1）：推进 run、就绪判定、action key 与缓存复用、
// attempt 结束、取消、并发组，以及兜底处理租约、超时和卡住的 run 的 sweeper（PLAN #6.2、#6.3）。
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Config 是状态机和 sweeper 需要的配置（由 server.Config 映射）。
type Config struct {
	// InfraRetryMax 是基础设施错误后自动重新排队的最多次数。
	InfraRetryMax int
	// LeaseTTL 是 Runner 租约的有效期。
	LeaseTTL time.Duration
	// CancelGrace 是取消或超时后 Runner 停止 job 的宽限时间。
	CancelGrace time.Duration
	// SweepInterval 是 sweeper 的执行间隔。
	SweepInterval time.Duration
	// NonceRetention 是 Runner 认证随机数的保存时间，必须长于 Runner 认证允许的时钟偏差，才能防止重放。
	NonceRetention time.Duration
}

// defaultSweepInterval 是配置没有给出（或不是正数）时的 sweeper 周期：time.NewTicker 不接受非正数。
const defaultSweepInterval = 5 * time.Second

// Deps 是 Engine 的依赖。
type Deps struct {
	DB *store.DB
	// CAS 用于读取上游 job 的结果 manifest（计算 action key）。
	CAS cas.Store
	// Outbox 在状态变化时写入 Check Run 同步记录。
	Outbox *outbox.Writer
	// Hub 在 sweeper 改变状态后唤醒各组件。
	Hub *notify.Hub
	Log *slog.Logger
	// AttemptEnded 在结束 Runner 执行的 attempt 的事务提交之后调用（经 Engine.AttemptEnded）：签发登记据此吊销这个 attempt
	// 的全部源码 token。为 nil 时不通知。
	AttemptEnded func(ctx context.Context, attemptID int64)
	// ReconcileGitCred 是签发登记的对账（gitcred.Registry.Reconcile，组装层接线）：sweeper 每个周期把查询仍在执行的
	// attempt 的函数交给它，补上漏掉的 AttemptEnded（结束的事务已提交、通知没有发出）。为 nil 时不对账。
	ReconcileGitCred func(ctx context.Context, running func(ctx context.Context, ids []int64) ([]int64, error)) error
}

// Engine 是状态机。除 sweeper 外，所有方法都在调用方的事务中执行；调用方在提交后负责唤醒（notify.Hub.All）。
type Engine struct {
	cfg    Config
	db     *store.DB
	cas    cas.Store
	outbox *outbox.Writer
	hub    *notify.Hub
	log    *slog.Logger
	ended  func(ctx context.Context, attemptID int64)
	// reconcile 见 Deps.ReconcileGitCred。
	reconcile func(ctx context.Context, running func(ctx context.Context, ids []int64) ([]int64, error)) error
	// timeoutsFrom 是 sweeper 开始判定超时的时间（Unix 纳秒），由 ExtendLeasesOnStartup 设置；0 表示立即判定。
	timeoutsFrom atomic.Int64
}

// New 返回 Engine。
func New(cfg Config, d Deps) *Engine {
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}
	return &Engine{cfg: cfg, db: d.DB, cas: d.CAS, outbox: d.Outbox, hub: d.Hub, log: d.Log, ended: d.AttemptEnded, reconcile: d.ReconcileGitCred}
}

// AttemptEnded 通知 Deps.AttemptEnded：attemptID 已经结束。调用方在 FinishAttempt 所在的事务提交之后调用（与唤醒相同）：
// Runner 的 Complete（完成、取消、Runner 判定的超时）由 runnerapi 调用，租约过期、Server 判定的超时由 sweeper 调用。
// 不经 Runner 执行的 attempt（发布 job、托管池）不签发源码 token，不需要通知。
func (e *Engine) AttemptEnded(ctx context.Context, attemptID int64) {
	if e.ended != nil {
		e.ended(ctx, attemptID)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// AdvanceRun 推进一个 run 当前 rerun 的 waiting job：上游失败的 → skipped，
// 上游全部成功的 → 就绪判定；所有 job 结束后结束 run。
// 按 cistore.LockRuns 的加锁顺序先锁并发组（结束 run 时要释放它）和 run，再锁 job。
func (e *Engine) AdvanceRun(ctx context.Context, tx pgx.Tx, runID int64) error {
	if err := cistore.LockRuns(ctx, tx, []int64{runID}); err != nil {
		return err
	}
	run, err := cistore.GetRun(ctx, tx, runID, true)
	if err != nil {
		return err
	}
	if run.State != store.RunActive {
		return nil
	}
	jobs, err := cistore.RerunJobs(ctx, tx, run.ID, run.CurrentRerun, true)
	if err != nil {
		return err
	}
	byID := map[int64]*cistore.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	for changed := true; changed; {
		changed = false
		for _, j := range jobs {
			if j.State != store.JobWaiting {
				continue
			}
			c, err := e.advanceWaitingJob(ctx, tx, run, j, jobs, byID)
			if err != nil {
				return err
			}
			changed = changed || c
		}
	}
	for _, j := range jobs {
		if !j.State.Terminal() {
			return nil
		}
	}
	return e.finishRun(ctx, tx, run, jobs)
}

// advanceWaitingJob 处理一个 waiting 的 job，返回它的状态是否发生了变化。
func (e *Engine) advanceWaitingJob(ctx context.Context, tx pgx.Tx, run *cistore.Run, j *cistore.Job, jobs []*cistore.Job, byID map[int64]*cistore.Job) (bool, error) {
	if j.Follows != nil {
		f, err := cistore.GetJob(ctx, tx, *j.Follows, false)
		if err != nil {
			return false, err
		}
		switch {
		case f.State == store.JobSucceeded:
			if err := e.succeedFromJob(ctx, tx, j, f, fmt.Sprintf("与 job #%d 的 action key 相同，沿用它的结果", f.ID)); err != nil {
				return false, err
			}
			return true, nil
		case !f.State.Terminal():
			return false, nil
		default:
			// 被跟随的 job 没有成功：不继承失败，重新做就绪判定（PLAN #6.1）
			if _, err := tx.Exec(ctx, `UPDATE jobs SET follows_job_id=NULL WHERE id=$1`, j.ID); err != nil {
				return false, err
			}
			j.Follows = nil
		}
	}
	ready, bad := true, ""
	for _, n := range j.Needs {
		up := byID[n]
		if up == nil {
			continue
		}
		if !up.State.Terminal() {
			ready = false
		} else if up.State != store.JobSucceeded {
			bad = up.DisplayName
		}
	}
	if bad != "" {
		return true, e.SetTerminal(ctx, tx, j, store.JobSkipped, "上游 "+bad+" 没有成功")
	}
	if !ready {
		return false, nil
	}
	old := j.State
	if err := e.readiness(ctx, tx, run, j, jobs); err != nil {
		return false, err
	}
	return j.State != old, nil
}

// readiness 是就绪判定（PLAN #6.1）。j 的上游已经全部成功。
func (e *Engine) readiness(ctx context.Context, tx pgx.Tx, run *cistore.Run, j *cistore.Job, jobs []*cistore.Job) error {
	if j.Kind == workflow.JobKindNormal {
		done, err := e.reuseByActionKey(ctx, tx, run, j, jobs)
		if err != nil || done {
			return err
		}
	}
	return e.MarkPending(ctx, tx, j)
}

// reuseByActionKey 计算 action key，并尝试命中缓存或跟随 action key 相同的 job。
// 返回 true 表示 job 已经处理完（命中缓存或开始跟随），不需要继续就绪判定。
func (e *Engine) reuseByActionKey(ctx context.Context, tx pgx.Tx, run *cistore.Run, j *cistore.Job, jobs []*cistore.Job) (bool, error) {
	if j.ActionKey == nil {
		key, parts, err := e.computeActionKey(ctx, tx, run, j, jobs)
		if err != nil {
			return true, e.SetTerminal(ctx, tx, j, store.JobFailed, "计算 action key 失败："+err.Error())
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET action_key=$2, key_parts=$3 WHERE id=$1`, j.ID, key, store.MustJSON(parts)); err != nil {
			return false, err
		}
		j.ActionKey = &key
	}
	if !j.Spec.Cache || j.Effects || j.ForceExecute {
		return false, nil
	}
	var manifest string
	var producer int64
	err := tx.QueryRow(ctx, `UPDATE action_cache SET last_hit_at=now() WHERE action_key=$1 AND NOT suspect
		RETURNING manifest_digest, producer_job_id`, *j.ActionKey).Scan(&manifest, &producer)
	switch {
	case err == nil:
		var producerRun int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(run_id, 0) FROM jobs WHERE id=$1`, producer).Scan(&producerRun); err != nil {
			return false, err
		}
		reason := fmt.Sprintf("命中缓存（来自 run #%d 的 job #%d）", producerRun, producer)
		return true, e.succeedWith(ctx, tx, j, &manifest, producer, reason)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	var other int64
	err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE action_key=$1 AND state IN ('pending','running') AND id<>$2
		AND NOT force_execute ORDER BY id LIMIT 1`, *j.ActionKey, j.ID).Scan(&other)
	switch {
	case err == nil:
		reason := fmt.Sprintf("等待 action key 相同的 job #%d", other)
		if _, err := tx.Exec(ctx, `UPDATE jobs SET follows_job_id=$2, reason=$3 WHERE id=$1`, j.ID, other, reason); err != nil {
			return false, err
		}
		j.Follows, j.Reason = &other, &reason
		return true, cicheck.EnqueueJob(ctx, e.outbox, tx, j)
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

// MarkPending 把就绪的 job 送入队列。
func (e *Engine) MarkPending(ctx context.Context, tx pgx.Tx, j *cistore.Job) error {
	reason := "等待 Runner 领取"
	if j.Kind == workflow.JobKindPublish {
		reason = "等待 Server 执行发布"
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state=$3, queued_at=now(), reason=$2 WHERE id=$1`, j.ID, reason, store.JobPending); err != nil {
		return err
	}
	j.State, j.Reason = store.JobPending, &reason
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// succeedFromJob 让 j 直接沿用 f 的结果（同 key 合并）。
func (e *Engine) succeedFromJob(ctx context.Context, tx pgx.Tx, j, f *cistore.Job, reason string) error {
	src := f.ID
	if f.CachedFrom != nil {
		src = *f.CachedFrom
	}
	return e.succeedWith(ctx, tx, j, f.ResultManifest, src, reason)
}

// succeedWith 让 j 不执行、直接沿用 job src 产生的结果 manifest 并置为成功（命中缓存或同 key 合并）。
func (e *Engine) succeedWith(ctx context.Context, tx pgx.Tx, j *cistore.Job, manifest *string, src int64, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state=$5, result_manifest=$2, cached_from_job_id=$3, reason=$4,
		finished_at=now(), follows_job_id=NULL WHERE id=$1`, j.ID, manifest, src, reason, store.JobSucceeded); err != nil {
		return err
	}
	j.State, j.ResultManifest, j.CachedFrom, j.Reason, j.Follows = store.JobSucceeded, manifest, &src, &reason, nil
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// SetTerminal 把非 running 的 job 直接置为终态。
func (e *Engine) SetTerminal(ctx context.Context, tx pgx.Tx, j *cistore.Job, state store.JobState, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state=$2, reason=$3, finished_at=now(), lease_expires_at=NULL, follows_job_id=NULL WHERE id=$1`,
		j.ID, state, reason); err != nil {
		return err
	}
	j.State, j.Reason = state, &reason
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// runConclusion 按 PLAN #6.1 计算 run 的结论：有失败即 failure；
// 否则有被取消的 job（fail-fast 除外）即 cancelled；否则 success。
func runConclusion(jobs []*cistore.Job) store.Conclusion {
	c := store.ConclusionSuccess
	for _, j := range jobs {
		if j.State.Failed() {
			return store.ConclusionFailure
		}
		if j.State == store.JobCancelled && deref(j.CancelReason) != string(store.CancelFailFast) {
			c = store.ConclusionCancelled
		}
	}
	return c
}

// finishRun 在 run 的全部 job 结束后计算结论（PLAN #6.1），并释放并发组。
func (e *Engine) finishRun(ctx context.Context, tx pgx.Tx, run *cistore.Run, jobs []*cistore.Job) error {
	c := runConclusion(jobs)
	state := c.RunState()
	if _, err := tx.Exec(ctx, `UPDATE runs SET state=$2, finished_at=now(), updated_at=now() WHERE id=$1`, run.ID, state); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE run_reruns SET state=$3, conclusion=$4, finished_at=now() WHERE run_id=$1 AND rerun=$2`,
		run.ID, run.CurrentRerun, state, c); err != nil {
		return err
	}
	run.State = state
	if err := cicheck.EnqueueRun(ctx, e.outbox, tx, run.ID, run.CurrentRerun); err != nil {
		return err
	}
	if run.Event == workflow.EventPullRequest && run.PRNumber != nil {
		// PR 的验证结束：组装层通知合并队列、提名自动入队（没有失败时）、通知 AI 开发的执行者（PLAN #30.3）
		if err := e.outbox.PRVerified(ctx, tx, run.RepoID, *run.PRNumber, state == store.RunSucceeded || state == store.RunSkipped); err != nil {
			return err
		}
	}
	return e.releaseConcurrency(ctx, tx, run)
}

// releaseConcurrency 在 run 结束后启动并发组中排队的 run（PLAN #6.5）。
func (e *Engine) releaseConcurrency(ctx context.Context, tx pgx.Tx, run *cistore.Run) error {
	if run.ConcurrencyKey == nil {
		return nil
	}
	if err := cistore.LockConcurrencyGroup(ctx, tx, run.RepoID, *run.ConcurrencyKey); err != nil {
		return err
	}
	var next int64
	err := tx.QueryRow(ctx, `SELECT id FROM runs WHERE repo_id=$1 AND concurrency_key=$2 AND state='queued' AND id<>$3
		ORDER BY id LIMIT 1 FOR UPDATE`, run.RepoID, *run.ConcurrencyKey, run.ID).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	active, err := cistore.CountActiveInGroup(ctx, tx, run.RepoID, *run.ConcurrencyKey, 0)
	if err != nil {
		return err
	}
	if active > 0 {
		return nil
	}
	return e.activateQueued(ctx, tx, next)
}

// activateQueued 把排队的 run 置为 active 并推进。
func (e *Engine) activateQueued(ctx context.Context, tx pgx.Tx, id int64) error {
	if _, err := tx.Exec(ctx, `UPDATE runs SET state='active', updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE run_reruns SET state='active' WHERE run_id=$1 AND rerun=(SELECT current_rerun FROM runs WHERE id=$1)`, id); err != nil {
		return err
	}
	return e.AdvanceRun(ctx, tx, id)
}

// computeActionKey 按 PLAN #11.1 计算 action key。
func (e *Engine) computeActionKey(ctx context.Context, tx pgx.Tx, run *cistore.Run, j *cistore.Job, jobs []*cistore.Job) (string, map[string]string, error) {
	repo, err := store.GetRepo(ctx, tx, run.RepoID)
	if err != nil {
		return "", nil, err
	}
	sp := j.Spec
	in := actionkey.Input{
		Salt:         repo.CacheSalt,
		Platform:     sp.Platform,
		Inputs:       deref(j.InputDigest),
		Env:          sp.Env,
		Network:      sp.Network,
		Repositories: sp.Repositories,
		Outputs:      map[string]string{},
		Executor:     executorKeyPart(&sp),
		Services:     servicesKeyPart(sp.Services),
	}
	for _, r := range sp.RuntimeRefs {
		in.Runtimes = append(in.Runtimes, [2]string{r.Name, r.Digest})
	}
	for _, st := range sp.Steps {
		m := map[string]any{"run": st.Run, "shell": st.Shell, "working-directory": st.WorkingDirectory, "env": st.Env}
		if m["env"] == nil {
			m["env"] = map[string]string{}
		}
		if st.ContinueOnError {
			m["continue-on-error"] = true
		}
		in.Steps = append(in.Steps, m)
	}
	for _, o := range sp.Outputs {
		in.Outputs[o.Name] = o.Path
	}
	byKey := map[string]*cistore.Job{}
	for _, x := range jobs {
		byKey[x.JobKey] = x
	}
	for _, f := range sp.Fetch {
		up := byKey[f.JobKey]
		if up == nil || up.ResultManifest == nil {
			return "", nil, fmt.Errorf("上游 %s 没有结果", f.JobKey)
		}
		d, err := e.OutputManifest(ctx, *up.ResultManifest, f.Output)
		if err != nil {
			return "", nil, fmt.Errorf("上游 %s 的输出 %s：%w", f.JobKey, f.Output, err)
		}
		in.Upstream = append(in.Upstream, [2]string{f.JobKey + "." + f.Output, d})
		in.FetchTo = append(in.FetchTo, f.To)
	}
	return actionkey.Compute(in)
}

// executorKeyPart 是 action key 中描述 executor 的部分。
func executorKeyPart(sp *workflow.Spec) map[string]any {
	kind := string(sp.Executor) // 以字符串写入：action key 的规范化只认基础类型
	switch sp.Executor {
	case workflow.ExecutorOCI:
		m := map[string]any{"kind": kind, "image": sp.Container}
		// 只在 buildkit: true 时加入，不改变其他 job 已有的 action key
		if sp.BuildkitImage != "" {
			m["buildkit"] = sp.BuildkitImage
		}
		return m
	case workflow.ExecutorHost:
		host := sp.Host
		if host == "" {
			host = "default"
		}
		m := map[string]any{"kind": kind, "profile": map[string]any{"name": host}}
		// 只在写了 signing 时加入，不改变其他 job 已有的 action key。secret 只以占位符（名称）计入（PLAN #11.3）
		if sp.Signing != nil {
			m["signing"] = signingKeyPart(sp.Signing)
		}
		return m
	}
	return map[string]any{"kind": kind, "image": sp.VM}
}

// signingKeyPart 是 action key 中描述 signing 的部分（只含基础类型）。
func signingKeyPart(s *workflow.Signing) map[string]any {
	certs := []any{}
	for _, c := range s.Certificates {
		certs = append(certs, []any{c.P12, c.Password})
	}
	profiles := []any{}
	for _, p := range s.ProvisioningProfiles {
		profiles = append(profiles, p)
	}
	m := map[string]any{"certificates": certs, "provisioning_profiles": profiles}
	if n := s.Notary; n != nil {
		m["notary"] = []any{n.Profile, n.AppleID, n.TeamID, n.Password, n.Key, n.KeyID, n.Issuer}
	}
	return m
}

// servicesKeyPart 是 action key 中描述 services 的部分（按名称排序）。
func servicesKeyPart(services []workflow.ServiceSpec) []any {
	sorted := slices.Clone(services)
	slices.SortFunc(sorted, func(a, b workflow.ServiceSpec) int { return cmp.Compare(a.Name, b.Name) })
	var out []any
	for _, sv := range sorted {
		env := sv.Env
		if env == nil {
			env = map[string]string{}
		}
		cmd := sv.Command
		if cmd == nil {
			cmd = []string{}
		}
		out = append(out, []any{sv.Name, sv.Image, env, cmd})
	}
	return out
}

// NoSuchOutputError 表示结果 manifest 中没有要找的命名输出。
type NoSuchOutputError struct{ Output string }

func (e *NoSuchOutputError) Error() string { return "没有名为 " + e.Output + " 的输出" }

// OutputManifest 从结果 manifest 中取出某个命名输出的 manifest digest。
// 没有这个输出时返回 *NoSuchOutputError；结果 manifest 已不在 CAS 中时返回的错误包装 cas.ErrNotFound，
// 本身无效（损坏、无法解析）时包装 cas.ErrInvalidObject。
func (e *Engine) OutputManifest(ctx context.Context, result, output string) (string, error) {
	var rm cas.ResultManifest
	if err := cas.ReadJSON(ctx, e.cas, result, &rm); err != nil {
		return "", fmt.Errorf("读取结果 %s：%w", result, err)
	}
	d, ok := rm.Outputs[output]
	if !ok {
		return "", &NoSuchOutputError{Output: output}
	}
	return d, nil
}

// AttemptResult 是一次 attempt 的结束信息（Runner 的 Complete、租约过期或超时）。
type AttemptResult struct {
	Outcome  store.Outcome
	ErrClass string
	ExitCode int
	Message  string
	// Manifest 是结果 manifest 的 digest，只在成功时有意义。
	Manifest string
}

// nextState 按 PLAN #6.1 的 running 行计算 attempt 结束后 job 的状态和原因。
// 返回 store.JobPending 表示基础设施错误后重新排队。
func (e *Engine) nextState(j *cistore.Job, res AttemptResult) (store.JobState, string) {
	switch res.Outcome {
	case store.OutcomeSucceeded:
		return store.JobSucceeded, ""
	case store.OutcomeFailed:
		if res.Message != "" {
			return store.JobFailed, res.Message
		}
		return store.JobFailed, fmt.Sprintf("step 退出码 %d", res.ExitCode)
	case store.OutcomeTimedOut:
		return store.JobTimedOut, "超时"
	case store.OutcomeCancelled:
		return store.JobCancelled, "已取消"
	}
	// infra_error / lost
	switch {
	case j.CancelRequested:
		return store.JobCancelled, "已取消"
	case j.Effects && j.StartedStep:
		return store.JobFailed, "有副作用的 job 已开始执行，不自动重试；请确认外部状态后手动重跑（" + res.ErrClass + "：" + res.Message + "）"
	case j.AttemptCount <= e.cfg.InfraRetryMax:
		return store.JobPending, fmt.Sprintf("第 %d 次尝试遇到基础设施错误（%s：%s），重新排队", j.AttemptCount, res.ErrClass, res.Message)
	}
	return store.JobFailed, fmt.Sprintf("基础设施错误（%s：%s），已重试 %d 次", res.ErrClass, res.Message, e.cfg.InfraRetryMax)
}

// 托管 attempt 的结束规则（PLAN #32.6），排在 nextState 的规则之前。
const (
	// reasonUnknownSubmit 是托管执行的提交结果未知时 job 的原因。
	reasonUnknownSubmit = "托管执行的提交结果未知，等待对账（不会自动重新提交）"
	// reasonManagedEffects 是有副作用的托管 job 已越过预留后遇到基础设施错误时的原因（与已开始 step 的 effects job 相同）。
	reasonManagedEffects = "有副作用的 job 已提交到托管执行，不自动重试；请确认外部状态后手动重跑"
)

// finishState 计算 attempt 结束后 job 的状态和原因：托管 attempt 的两条规则在前，之后是 nextState。
// 返回 store.JobRunning 表示结果未知：attempt 转为 unknown、不结束，job 保持 running（继续占用配额），等待对账（I5）。
func (e *Engine) finishState(j *cistore.Job, exec cistore.AttemptExec, res AttemptResult) (store.JobState, string) {
	infra := res.Outcome == store.OutcomeInfraError
	switch {
	// 提交中或结果未知的托管执行遇到基础设施错误、失联：远端可能已经在执行，不重排，否则会产生第二个远端执行
	case infra && exec.Managed() && (exec.Phase == cistore.PhaseSubmitting || exec.Phase == cistore.PhaseUnknown):
		return store.JobRunning, reasonUnknownSubmit
	case infra && j.Effects && exec.Submitted():
		return store.JobFailed, reasonManagedEffects + "（" + res.ErrClass + "：" + res.Message + "）"
	}
	return e.nextState(j, res)
}

// FinishAttempt 处理一次 attempt 的结束（Complete 或租约过期），PLAN #6.1 的 running 行。
// 调用方应在锁 j 之前用 cistore.LockRunsOfJob 锁住 run（加锁顺序见 cistore.LockRuns），把返回的已锁 run 集合作为
// lockedRuns 传入，并在事务提交后调用 AttemptEnded。
func (e *Engine) FinishAttempt(ctx context.Context, tx pgx.Tx, j *cistore.Job, res AttemptResult, lockedRuns []int64) error {
	exec, err := cistore.GetAttemptExec(ctx, tx, j.CurrentAttemptID)
	if err != nil {
		return err
	}
	state, reason := e.finishState(j, exec, res)
	if state == store.JobRunning {
		return e.markUnknown(ctx, tx, j, reason)
	}
	isPublish := j.Kind == workflow.JobKindPublish
	if j.CurrentAttemptID != nil {
		// 非 publish 路径额外带上 epoch 条件：晚到的、属于旧 epoch 的结果不能结束当前的 attempt（结果只提交一次，I5）。
		sql := `UPDATE attempts SET outcome=$2, error_class=nullif($3,''), exit_code=$4, message=nullif($5,''), finished_at=now()
			WHERE id=$1 AND outcome IS NULL`
		args := []any{*j.CurrentAttemptID, res.Outcome, res.ErrClass, res.ExitCode, res.Message}
		if !isPublish {
			sql += ` AND epoch=$6`
			args = append(args, j.Epoch)
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		if !isPublish && tag.RowsAffected() == 0 {
			return ErrStale
		}
	}
	// 非 publish 路径的 job 更新同样带 epoch 条件并检查 RowsAffected：epoch 已前进时返回 ErrStale，不覆盖新的 attempt。
	if state == store.JobPending {
		// 重排：queued_at 更新，提交时间（created_at）不变，所以仍排在更晚提交的 run 前面（cistore.QueueOrderSQL）
		epochCond, args := "", []any{j.ID, reason, store.JobPending}
		if !isPublish {
			epochCond, args = ` AND epoch=$4`, append(args, j.Epoch)
		}
		tag, err := tx.Exec(ctx, `UPDATE jobs SET state=$3, lease_expires_at=NULL, queued_at=now(), reason=$2, started_step=false WHERE id=$1`+epochCond, args...)
		if err != nil {
			return err
		}
		if !isPublish && tag.RowsAffected() == 0 {
			return ErrStale
		}
		j.State = store.JobPending
		return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
	}
	var rm *string
	if state == store.JobSucceeded && res.Manifest != "" {
		rm = &res.Manifest
	}
	epochCond, args := "", []any{j.ID, state, reason, rm}
	if !isPublish {
		epochCond, args = ` AND epoch=$5`, append(args, j.Epoch)
	}
	tag, err := tx.Exec(ctx, `UPDATE jobs SET state=$2, reason=nullif($3,''), result_manifest=$4, lease_expires_at=NULL, finished_at=now() WHERE id=$1`+epochCond, args...)
	if err != nil {
		return err
	}
	if !isPublish && tag.RowsAffected() == 0 {
		return ErrStale
	}
	j.State, j.ResultManifest = state, rm
	if reason != "" {
		j.Reason = &reason
	}
	if err := writeActionCache(ctx, tx, j); err != nil {
		return err
	}
	if err := cicheck.EnqueueJob(ctx, e.outbox, tx, j); err != nil {
		return err
	}
	if err := e.failFast(ctx, tx, j); err != nil {
		return err
	}
	return e.advanceAfterFinish(ctx, tx, lockedRuns)
}

// writeActionCache 按 PLAN #6.1 的写缓存规则保存成功 job 的结果：
// 强制执行的 job 总是覆盖；否则只覆盖被标记为可疑的条目。
func writeActionCache(ctx context.Context, tx pgx.Tx, j *cistore.Job) error {
	succeeded := j.State == store.JobSucceeded && j.ResultManifest != nil
	cacheable := j.Kind == workflow.JobKindNormal && j.Spec.Cache && !j.Effects
	hasKey := j.ActionKey != nil
	if !succeeded || !cacheable || !hasKey {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO action_cache (action_key, manifest_digest, producer_job_id) VALUES ($1,$2,$3)
		ON CONFLICT (action_key) DO UPDATE SET manifest_digest=EXCLUDED.manifest_digest, producer_job_id=EXCLUDED.producer_job_id,
		created_at=now(), suspect=false WHERE $4 OR action_cache.suspect`, *j.ActionKey, *j.ResultManifest, j.ID, j.ForceExecute)
	return err
}

// failFast 在矩阵变体失败时取消同一矩阵中尚未结束的变体。
func (e *Engine) failFast(ctx context.Context, tx pgx.Tx, j *cistore.Job) error {
	if !j.State.Failed() || !j.Spec.FailFast || len(j.Matrix) == 0 || j.RunID == 0 {
		return nil
	}
	sibs, err := cistore.QueryJobs(ctx, tx, `run_id=$1 AND rerun=$2 AND job_name=$3 AND id<>$4 AND state <> ALL($5) FOR UPDATE`,
		j.RunID, j.Rerun, j.JobName, j.ID, store.TerminalJobStateArgs())
	if err != nil {
		return err
	}
	for _, sib := range sibs {
		if err := e.cancelJob(ctx, tx, sib, store.CancelFailFast); err != nil {
			return err
		}
	}
	return nil
}

// advanceAfterFinish 按升序推进调用方在锁 job 之前已经锁定的 run（cistore.LockRunsOfJob 的返回值）。这里不再重新查询
// follower run：那会在持有 job 行锁之后反向锁住动态新增的 run，违反加锁顺序；新挂上来的 follower run 由 sweepStuckRuns 推进。
func (e *Engine) advanceAfterFinish(ctx context.Context, tx pgx.Tx, lockedRuns []int64) error {
	for _, rid := range lockedRuns {
		if err := e.AdvanceRun(ctx, tx, rid); err != nil {
			return err
		}
	}
	return nil
}

// cancelJob 取消一个 job：未运行的直接 cancelled，运行中的设置 cancel_requested，由 Runner 确认（PLAN #6.1）。
func (e *Engine) cancelJob(ctx context.Context, tx pgx.Tx, j *cistore.Job, reason store.CancelReason) error {
	switch {
	case j.State.Queued():
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state=$4, cancel_reason=$2, reason=$3, finished_at=now(), follows_job_id=NULL WHERE id=$1`,
			j.ID, reason, reason.Text(), store.JobCancelled); err != nil {
			return err
		}
		r := string(reason)
		j.State, j.CancelReason = store.JobCancelled, &r
		return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
	case j.State == store.JobRunning:
		if j.CancelRequested {
			return nil
		}
		exec, err := cistore.GetAttemptExec(ctx, tx, j.CurrentAttemptID)
		if err != nil {
			return err
		}
		if exec.Managed() && exec.Phase == cistore.PhaseReserved {
			// 预留还没有发出任何外部调用：直接撤销并置为 cancelled，不等确认（PLAN #32.6）。
			// 与 Provider 的提交并发、预留已经推进到 submitting 时 cancelReserved 返回 ErrStale：不能当作没发出去，
			// 退回下面的 cancel_requested，由 Provider 确认远端结束。
			switch err := e.cancelReserved(ctx, tx, j, reason); {
			case err == nil:
				return nil
			case !errors.Is(err, ErrStale):
				return err
			}
		}
		// Native 由 Runner 确认（不确认时靠租约过期兜底）；已提交的托管执行由 Provider 确认远端结束
		wait := "（等待 Runner 确认）"
		if exec.Managed() {
			wait = "（等待托管执行确认结束）"
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET cancel_requested=true, cancel_reason=$2, reason=$3 WHERE id=$1`,
			j.ID, reason, reason.Text()+wait); err != nil {
			return err
		}
		r := string(reason)
		j.CancelRequested, j.CancelReason = true, &r
	}
	return nil
}

// CancelRun 取消一个 run 的当前 rerun。
func (e *Engine) CancelRun(ctx context.Context, tx pgx.Tx, runID int64, reason store.CancelReason) error {
	if err := cistore.LockRuns(ctx, tx, []int64{runID}); err != nil {
		return err
	}
	run, err := cistore.GetRun(ctx, tx, runID, true)
	if err != nil {
		return err
	}
	if run.State.Terminal() {
		return nil
	}
	jobs, err := cistore.RerunJobs(ctx, tx, run.ID, run.CurrentRerun, true)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		// 运行中的发布 job 只在阶段 A 可以取消：进入阶段 B（commit_started_at 非空）后不可取消，重启续做时要完成
		// 剩余目标，避免跨目标不一致（PLAN #13.4）。
		if j.Kind == workflow.JobKindPublish && j.State == store.JobRunning {
			started, err := commitStarted(ctx, tx, j.ID)
			if err != nil {
				return err
			}
			if started {
				continue
			}
		}
		if err := e.cancelJob(ctx, tx, j, reason); err != nil {
			return err
		}
	}
	if run.State == store.RunQueued {
		// 排队中的 run：直接结束
		if _, err := tx.Exec(ctx, `UPDATE runs SET state='active' WHERE id=$1`, run.ID); err != nil {
			return err
		}
	}
	return e.AdvanceRun(ctx, tx, run.ID)
}

// commitStarted 判断发布 job 是否已经进入阶段 B（releases.commit_started_at 非空）。没有 releases 行视为未进入。
// 调用方已经持有 jobs 行锁，这里按锁序 runs -> jobs -> releases 再锁 releases 行。
func commitStarted(ctx context.Context, tx pgx.Tx, jobID int64) (bool, error) {
	var started bool
	err := tx.QueryRow(ctx, `SELECT commit_started_at IS NOT NULL FROM releases WHERE job_id=$1 FOR SHARE`, jobID).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return started, err
}
