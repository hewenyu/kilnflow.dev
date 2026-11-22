package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2/airunnerv2connect"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// airunner.v2（PLAN #33.6）：只接受 AI 执行节点（runners.kind = ai，注册和认证仍走 runner.v1）。Server 只支持当前的协议
// 版本（agentcfg.AIProtocol）：上报其他版本的节点只记录状态，不派任务。没有 Herdr、主机指纹和升级指令。
//
// attempt 级的调用（Heartbeat、AppendLog、Complete、OfferDevCandidates、ClaimDevIssues、ValidateReport、GetVerification）在
// 一个事务中同时校验 epoch、任务状态和租约：租约过期即拒绝，不等 sweeper（PLAN #33.6）。

// DevHandler 处理开发任务执行中的候选、领取和报告校验（aidev 实现，组装时设置）。任务的 attempt 已经校验过属于调用的
// 节点、epoch 是当前值、任务在执行中。
type DevHandler interface {
	Offer(ctx context.Context, t *Task, attemptID int64) (*airunnerv2.OfferDevCandidatesResponse, error)
	Claim(ctx context.Context, t *Task, attemptID int64, req *airunnerv2.ClaimDevIssuesRequest) (*airunnerv2.ClaimDevIssuesResponse, error)
}

// Service 实现 AIRunnerService。
type Service struct{ c *Core }

var _ airunnerv2connect.AIRunnerServiceHandler = (*Service)(nil)

// RunnerService 返回 AIRunnerService 的实现。
func (c *Core) RunnerService() *Service { return &Service{c: c} }

func (s *Service) authorize(ctx context.Context, h http.Header) (int64, error) {
	return s.c.auth.AuthorizeKind(ctx, h, store.RunnerKindAI)
}

// Acquire 长轮询领取一个 AI 任务：领取即 running；同一个 request_id 重试时返回同一个任务。
func (s *Service) Acquire(ctx context.Context, req *connect.Request[airunnerv2.AcquireRequest]) (*connect.Response[airunnerv2.AcquireResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	resp, err := s.c.acquire(ctx, id, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *Core) acquire(ctx context.Context, runnerID int64, req *airunnerv2.AcquireRequest) (*airunnerv2.AcquireResponse, error) {
	st := req.GetState()
	r, protocol, drain, err := c.touchRunner(ctx, runnerID, st)
	if err != nil {
		return nil, err
	}
	resp := &airunnerv2.AcquireResponse{Directives: &airunnerv2.AIDirectives{Drain: drain}}
	if task, err := c.replay(ctx, c.db, runnerID, req.GetRequestId()); err != nil || task != nil {
		resp.Task = task
		return resp, err
	}
	// 不派任务时立即返回，不长轮询、不当作空闲的节点：已暂停、协议不是当前版本、节点这一轮不接任务（standby）、
	// 节点上报的执行中任务已经到并发上限（它有活干，等它下一次领取）。这里只是不进入长轮询的预检，是否还有空闲槽位
	// 由 tryAcquire 按 RunnerFreeSlotsSQL 判断
	if drain || protocol != agentcfg.AIProtocol || req.GetStandby() || int(st.GetRunning()) >= r.Capacity {
		return resp, nil
	}
	wait := c.cfg.AcquireLongPoll
	if w := time.Duration(req.GetWaitSeconds()) * time.Second; w > 0 && w < wait {
		wait = w
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var (
		ch   <-chan struct{}
		cur  *cursor
		idle bool // 已经通知过这个节点空闲（每次长轮询一次）
	)
	for {
		if cur == nil {
			ch = c.hub.JobsChanged()
		}
		res, err := c.tryAcquire(ctx, r, req.GetRequestId(), cur)
		switch {
		case errors.Is(err, errRevoked):
			return nil, rpcerr.RunnerRevoked()
		case errors.Is(err, errDrained):
			resp.Directives.Drain = true
			return resp, nil
		case err != nil:
			return nil, err
		}
		if res.task != nil {
			resp.Task = res.task
			if !res.replayed {
				c.hub.All()
			}
			return resp, nil
		}
		if cur = res.next; cur != nil || res.again {
			if ctx.Err() != nil {
				return resp, nil
			}
			continue
		}
		if !idle && c.idle != nil {
			idle = true
			if err := c.idle(ctx, r.Role); err != nil {
				c.log.Warn("AI 执行节点空闲的通知", "runner", runnerID, "err", err)
			}
		}
		woke := false
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-ch:
			woke = true
		}
		if !woke {
			return resp, nil
		}
	}
}

// attemptTask 校验 (attempt_id, epoch)：必须属于这个节点、epoch 是当前值、任务仍在执行、租约没有过期。lock 为真时按加锁
// 顺序锁住请求和任务（并发组 → 请求 → 任务），返回请求。租约按 leaseHolds 判断：没有加锁（lock 为假）时只是入口的预检，
// 写入前的最终判断由各写入在自己的事务中持锁之后做（lockLiveAttempt、requireLease、leaseLiveSQL）。
func (c *Core) attemptTask(ctx context.Context, q store.Querier, runnerID int64, ref *airunnerv2.TaskRef, lock bool) (*Request, *Task, error) {
	if ref == nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 TaskRef"))
	}
	a, err := attemptOf(ctx, q, runnerID, ref)
	if err != nil {
		return nil, nil, err
	}
	var r *Request
	var t *Task
	if tx, ok := q.(pgx.Tx); ok && lock {
		r, t, err = lockTaskForFinish(ctx, tx, a.TaskID)
	} else {
		t, err = GetTask(ctx, q, a.TaskID, false)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := requireLease(ctx, q, a.TaskID, a.ID); err != nil {
		return nil, nil, err
	}
	return r, t, nil
}

// Heartbeat 续租 attempt 的租约、记录进度和 Orca 中的执行关联，返回是否已请求取消。
//
// 续约在一个事务中先锁住任务行，再由续约的写入自己按 clock_timestamp() 判断租约（leaseLiveSQL）：预检通过之后、等锁期间到期的
// 租约不会被续活，0 行即返回 STALE_EPOCH（PLAN #33.6）。
func (s *Service) Heartbeat(ctx context.Context, req *connect.Request[airunnerv2.HeartbeatRequest]) (*connect.Response[airunnerv2.HeartbeatResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	c, m := s.c, req.Msg
	if m.GetRef() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 TaskRef"))
	}
	progress, err := json.Marshal(AttemptProgress{Stage: shorten.Text(m.GetStage(), maxProgressText), Text: shorten.Text(m.GetText(), maxProgressText)})
	if err != nil {
		return nil, err
	}
	var binding []byte
	if b := m.GetBinding(); b != nil {
		if binding, err = json.Marshal(bindingFromProto(b)); err != nil {
			return nil, err
		}
	}
	var cancel bool
	var exp time.Time
	err = c.db.Tx(ctx, func(tx pgx.Tx) error {
		a, err := attemptOf(ctx, tx, id, m.GetRef())
		if err != nil {
			return err
		}
		// 先拿到任务行锁：之后的续约语句在持锁状态下判断租约，不会把等锁期间过期的租约续活
		if _, err := lockTaskRow(ctx, tx, a.TaskID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE ai_tasks SET lease_expires_at=clock_timestamp()+make_interval(secs => $3)
			WHERE id=$1 AND `+leaseLiveSQL+` RETURNING cancel_requested, lease_expires_at`,
			a.TaskID, a.ID, c.cfg.LeaseTTL.Seconds()).Scan(&cancel, &exp)
		if errors.Is(err, pgx.ErrNoRows) {
			return rpcerr.StaleEpoch()
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_attempts SET progress=$2, orca=coalesce($3, orca) WHERE id=$1`, a.ID, progress, binding); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE runners SET last_seen_at=now() WHERE id=$1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&airunnerv2.HeartbeatResponse{CancelRequested: cancel, LeaseExpiresAt: exp.Unix()}), nil
}

// maxProgressText 是保存的进度说明的长度上限（字节，与连接器的上限一致）。
const maxProgressText = 200

// bindingFromProto 把上报的 Orca 关联转换为保存的形式。
func bindingFromProto(b *airunnerv2.OrcaBinding) *OrcaBinding {
	if b == nil {
		return nil
	}
	return &OrcaBinding{
		RuntimeID: b.GetRuntimeId(), StartRequestID: b.GetStartRequestId(), RunID: b.GetRunId(), TaskID: b.GetTaskId(),
		DispatchID: b.GetDispatchId(), WorktreePath: b.GetWorktreePath(), WorktreeName: b.GetWorktreeName(),
	}
}

// AppendLog 追加 attempt 的日志（按 offset 去重，重传是幂等的）。attempt 必须属于这个节点、是当前的 epoch，并且租约没有过期
// （PLAN #33.6：失联、租约过期而 sweeper 还没处理的 attempt 不再接受日志）。正常结束的路径先在租约有效时传完剩余日志、再
// Complete，不受影响；任务已经有结果之后的补传一律拒绝——这次执行已经结束，日志只是过程数据，不需要在结束后再补。
func (s *Service) AppendLog(ctx context.Context, req *connect.Request[airunnerv2.AppendLogRequest]) (*connect.Response[airunnerv2.AppendLogResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	c, ref := s.c, req.Msg.GetRef()
	if req.Msg.GetOffset() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("无效的 offset %d", req.Msg.GetOffset()))
	}
	var n int64
	err = c.db.Tx(ctx, func(tx pgx.Tx) error {
		a, err := attemptOf(ctx, tx, id, ref)
		if err != nil {
			return err
		}
		if a.ErrorClass != nil && *a.ErrorClass == ErrClassLost {
			return rpcerr.StaleEpoch()
		}
		// 锁住任务行之后按租约规则复查（leaseLiveSQL）：过期即拒绝，不等 sweeper
		if _, err := lockLiveAttempt(ctx, tx, a.TaskID, a.ID); err != nil {
			return err
		}
		if n, err = c.logs.Append(a.ID, req.Msg.GetOffset(), req.Msg.GetData()); err != nil {
			return fmt.Errorf("写入日志：%w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE ai_attempts SET log_size=greatest(log_size, $2) WHERE id=$1`, a.ID, n)
		return err
	})
	if errors.Is(err, logstore.ErrBadOffset) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&airunnerv2.AppendLogResponse{Committed: n}), nil
}

// Complete 提交 attempt 的结果（PLAN #33.6）：同一个 attempt 只接受一次，用相同的内容重试时返回同一个回执（replayed），
// 内容不同时返回 ALREADY_EXISTS；epoch 不是当前值、任务已经结束或租约过期时返回 STALE_EPOCH。
func (s *Service) Complete(ctx context.Context, req *connect.Request[airunnerv2.CompleteRequest]) (*connect.Response[airunnerv2.CompleteResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetRef() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 TaskRef"))
	}
	res := Result{
		Outcome: outcomeFromProto(m.GetOutcome()), ErrClass: m.GetErrorClass(), Message: m.GetMessage(),
		AgentVersion: m.GetAgentVersion(), AgentModel: m.GetAgentModel(), ReviewReport: m.GetReviewReport(), Delivery: m.GetDelivery(),
		Digest: resultDigest(m), Orca: bindingFromProto(m.GetBinding()),
	}
	if res.Outcome == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 outcome"))
	}
	c := s.c
	var replayed bool
	err = c.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		replayed, err = c.completeAttempt(ctx, tx, id, m.GetRef(), res)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.attemptEnded(ctx, m.GetRef().GetAttemptId())
	if !replayed {
		c.hub.All()
	}
	return connect.NewResponse(&airunnerv2.CompleteResponse{Replayed: replayed}), nil
}

// completeAttempt 在一个事务中提交结果。调用方已经确认这是当前 epoch 的一次领取。
func (c *Core) completeAttempt(ctx context.Context, tx pgx.Tx, runnerID int64, ref *airunnerv2.TaskRef, res Result) (bool, error) {
	a, err := attemptOf(ctx, tx, runnerID, ref)
	if err != nil {
		return false, err
	}
	if done, replayed, err := alreadyCompleted(a, res); done {
		return replayed, err
	}
	// 加锁顺序：并发组 → 请求 → 任务；锁住之后复查这个 attempt 还是不是当前的（与并发的 Complete、sweeper 竞争）
	r, t, err := lockTaskForFinish(ctx, tx, a.TaskID)
	if err != nil {
		return false, err
	}
	if a, err = GetAttempt(ctx, tx, ref.GetAttemptId()); err != nil {
		return false, err
	}
	if done, replayed, err := alreadyCompleted(a, res); done {
		return replayed, err
	}
	if err := requireLease(ctx, tx, a.TaskID, a.ID); err != nil {
		return false, err
	}
	return false, c.finishTask(ctx, tx, r, t, res)
}

// attemptOf 读 attempt 并校验它属于这个节点、epoch 是这次领取的：不是时返回 STALE_EPOCH。
func attemptOf(ctx context.Context, q store.Querier, runnerID int64, ref *airunnerv2.TaskRef) (*Attempt, error) {
	var taskID, epoch, owner int64
	err := q.QueryRow(ctx, `SELECT task_id, epoch, coalesce(runner_id, 0) FROM ai_attempts WHERE id=$1`, ref.GetAttemptId()).
		Scan(&taskID, &epoch, &owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (owner != runnerID || epoch != ref.GetEpoch())) {
		return nil, rpcerr.StaleEpoch()
	}
	if err != nil {
		return nil, err
	}
	return GetAttempt(ctx, q, ref.GetAttemptId())
}

// alreadyCompleted 判断 attempt 是否已经有结果：相同摘要的重试返回 replayed，不同摘要返回 ALREADY_EXISTS。
func alreadyCompleted(a *Attempt, res Result) (done, replayed bool, err error) {
	switch {
	case a.Outcome == nil:
		return false, false, nil
	case deref(a.ResultDigest) == res.Digest:
		return true, true, nil
	}
	return true, false, rpcerr.AlreadyExists()
}

// resultDigest 是 Complete 的内容摘要：outcome、错误分类、说明、结果正文和执行关联。同一 attempt 用相同内容重试时
// Server 不再处理（PLAN #33.6）。
func resultDigest(m *airunnerv2.CompleteRequest) string {
	h := sha256.New()
	for _, part := range []string{
		m.GetOutcome().String(), m.GetErrorClass(), m.GetMessage(), m.GetAgentVersion(), m.GetAgentModel(),
	} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	var body []byte
	switch res := m.GetResult().(type) {
	case *airunnerv2.CompleteRequest_ReviewReport:
		body = res.ReviewReport
	case *airunnerv2.CompleteRequest_Delivery:
		fmt.Fprintf(h, "delivery:%s:%s:%t\n", res.Delivery.GetBranch(), res.Delivery.GetSha(), res.Delivery.GetCheckpoint())
		body = res.Delivery.GetReport()
	}
	fmt.Fprintf(h, "body:%d\n", len(body))
	h.Write(body)
	if b := m.GetBinding(); b != nil {
		fmt.Fprintf(h, "orca:%s:%s:%s:%s:%s\n", b.GetRuntimeId(), b.GetStartRequestId(), b.GetRunId(), b.GetTaskId(), b.GetDispatchId())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// outcomeFromProto 把节点上报的结果转换为 ai_attempts.outcome 的取值；未知值返回空字符串。
func outcomeFromProto(o airunnerv2.Outcome) store.Outcome {
	switch o {
	case airunnerv2.Outcome_OUTCOME_SUCCEEDED:
		return store.OutcomeSucceeded
	case airunnerv2.Outcome_OUTCOME_FAILED:
		return store.OutcomeFailed
	case airunnerv2.Outcome_OUTCOME_INFRA_ERROR:
		return store.OutcomeInfraError
	case airunnerv2.Outcome_OUTCOME_CANCELLED:
		return store.OutcomeCancelled
	case airunnerv2.Outcome_OUTCOME_TIMED_OUT:
		return store.OutcomeTimedOut
	}
	return ""
}

// devTask 校验开发任务的 attempt 并返回任务。
func (s *Service) devTask(ctx context.Context, h http.Header, ref *airunnerv2.TaskRef) (*Task, error) {
	id, err := s.authorize(ctx, h)
	if err != nil {
		return nil, err
	}
	if s.c.dev == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("没有启用 AI 开发（Server 没有配置）"))
	}
	_, t, err := s.c.attemptTask(ctx, s.c.db, id, ref, false)
	return t, err
}

// OfferDevCandidates 给出开发任务的候选（PLAN #26.10 第 2 步）。
func (s *Service) OfferDevCandidates(ctx context.Context, req *connect.Request[airunnerv2.OfferDevCandidatesRequest]) (*connect.Response[airunnerv2.OfferDevCandidatesResponse], error) {
	t, err := s.devTask(ctx, req.Header(), req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	resp, err := s.c.dev.Offer(ctx, t, req.Msg.GetRef().GetAttemptId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ClaimDevIssues 原子领取选中的 Issue（PLAN #26.11）：只有当前 epoch 的执行能领取，同一个 attempt 重复请求返回同一结果。
// 响应中的 message 是给 Agent 的下一步说明（Server 生成，PLAN #33.5）。
func (s *Service) ClaimDevIssues(ctx context.Context, req *connect.Request[airunnerv2.ClaimDevIssuesRequest]) (*connect.Response[airunnerv2.ClaimDevIssuesResponse], error) {
	t, err := s.devTask(ctx, req.Header(), req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	resp, err := s.c.dev.Claim(ctx, t, req.Msg.GetRef().GetAttemptId(), req.Msg)
	if err != nil {
		return nil, err
	}
	s.c.hub.All()
	return connect.NewResponse(resp), nil
}

// ValidateReport 校验 Agent 写出的报告（审计、选题、交付），不改变任何状态（PLAN #33.5）：规则只在 Server 中定义一次，
// 连接器据此当场告诉 Agent 哪里不对。
func (s *Service) ValidateReport(ctx context.Context, req *connect.Request[airunnerv2.ValidateReportRequest]) (*connect.Response[airunnerv2.ValidateReportResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	kind, err := reportKindFromProto(req.Msg.GetKind())
	if err != nil {
		return nil, err
	}
	c := s.c
	_, t, err := c.attemptTask(ctx, c.db, id, req.Msg.GetRef(), false)
	if err != nil {
		return nil, err
	}
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return nil, err
	}
	problems, err := typ.ValidateReport(ctx, t, req.Msg.GetRef().GetAttemptId(), kind, req.Msg.GetReport())
	if errors.Is(err, ErrUnknownReportKind) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&airunnerv2.ValidateReportResponse{Valid: len(problems) == 0, Problems: problems}), nil
}

// reportKindFromProto 把上报的报告种类转换为 ReportKind；没有指定时返回错误。
func reportKindFromProto(k airunnerv2.ReportKind) (ReportKind, error) {
	switch k {
	case airunnerv2.ReportKind_REPORT_KIND_REVIEW:
		return ReportReview, nil
	case airunnerv2.ReportKind_REPORT_KIND_SELECTION:
		return ReportSelection, nil
	case airunnerv2.ReportKind_REPORT_KIND_DELIVERY:
		return ReportDelivery, nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("未知的报告种类 %v", k))
}

// LockCurrentAttempt 在事务 tx 中按加锁顺序（并发组 → 请求 → 任务）锁住任务，持锁之后复查 attemptID 仍是它执行中的当前
// attempt、租约没有过期（租约规则见 leaseLiveSQL）；不是时返回 STALE_EPOCH。DevHandler 在自己的事务中写入前调用
// （I4：只有当前 epoch 的执行能领取）。
func LockCurrentAttempt(ctx context.Context, tx pgx.Tx, taskID, attemptID int64) (*Task, error) {
	_, t, err := lockTaskForFinish(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if err := requireLease(ctx, tx, taskID, attemptID); err != nil {
		return nil, err
	}
	return t, nil
}
