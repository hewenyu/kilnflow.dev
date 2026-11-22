package server

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2/airunnerv2connect"
	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 端口的实现：AI 任务在 ai_requests / ai_tasks / ai_attempts 中，节点用 runner.v1 注册、认证（共用的身份），用 airunner.v2
// 领取和执行（PLAN #30.5、#33.6）。

type splitPort struct {
	*portEnv
	// rpc 是共用的注册、认证（runner.v1），ai 是 AI 执行节点的协议（airunner.v2）。
	rpc runnerv1connect.RunnerServiceClient
	ai  airunnerv2connect.AIRunnerServiceClient
}

func newPort(t *testing.T, config string) aiPort {
	t.Helper()
	return newSplitPort(t, config)
}

func newSplitPort(t *testing.T, config string, opts ...func(*Config)) *splitPort {
	t.Helper()
	e := newPortEnv(t, config, opts...)
	return &splitPort{
		portEnv: e,
		rpc:     runnerv1connect.NewRunnerServiceClient(http.DefaultClient, e.url),
		ai:      airunnerv2connect.NewAIRunnerServiceClient(http.DefaultClient, e.url),
	}
}

func (p *splitPort) newRules() bool { return true }

func (p *splitPort) planReview(pr int, head string) aiReq {
	p.t.Helper()
	if _, err := p.s.reviews.PlanEvent(p.ctx, &review.Event{
		Repo: p.repo, Event: gitref.EventPullRequest, Action: "synchronize", Ref: fmt.Sprintf("%s%d/head", gitref.PullPrefix, pr), SHA: head,
		PRNumber: pr, BaseRef: "main", BaseSHA: p.base, HeadRef: prBranch(pr), Sender: portOwner, Source: store.SourceWebhook,
	}); err != nil {
		p.t.Fatalf("规划 PR #%d 的审计：%v", pr, err)
	}
	var id int64
	if err := p.s.db.QueryRow(p.ctx, `SELECT id FROM ai_requests WHERE repo_id=$1 AND type=$2 AND sha=$3 AND rerun_of IS NULL`,
		portRepoID, agentcfg.TaskReview, head).Scan(&id); err != nil {
		p.t.Fatalf("PR #%d 的审计请求：%v", pr, err)
	}
	return aiReq{id: id}
}

func (p *splitPort) rerunReview(req aiReq, delivery string) (aiReq, error) {
	r, err := p.s.reviews.Rerun(p.ctx, review.RerunRequest{
		RequestID: req.id, Mode: review.RerunFailed, Actor: "github:" + portOwner, Source: store.SourceWebhook, DeliveryID: delivery,
	})
	if err != nil {
		return aiReq{}, err
	}
	return aiReq{id: r.ID}, nil
}

// addWorkItem 建一个返修中的工作项。工作项的 claim_attempt_id 引用 ai_attempts：先建一个已结束的选题请求和它的 attempt。
func (p *splitPort) addWorkItem(pr int, _ string) int64 {
	p.t.Helper()
	var reqID, taskID, attemptID, runnerID int64
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `INSERT INTO runners (name, kind, public_key) VALUES ($1, 'ai', '\x00') RETURNING id`,
		fmt.Sprintf("claimer-%d", p.next())).Scan(&runnerID))
	p.exec(`INSERT INTO ai_runners (runner_id, role) VALUES ($1, 'development')`, runnerID)
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state, finished_at)
		SELECT id, $1, $2, $3, 'refs/heads/main', $4, id, 'succeeded', now() FROM n RETURNING id`,
		agentcfg.TaskDevelop, portRepoID, aispec.EventDevelop, p.base).Scan(&reqID))
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes,
		finished_at) VALUES ($1, 'develop', $2, $3, 'codex', '选题', '{}', 'succeeded', 1, now()) RETURNING id`,
		reqID, agentcfg.TaskDevelop, []string{platMac}).Scan(&taskID))
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id) VALUES ($1, 1, $2, 'claim') RETURNING id`,
		taskID, runnerID).Scan(&attemptID))
	var id int64
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, pr_number, head_sha, max_fix_rounds, agent,
		claim_attempt_id, plan) VALUES ($1, 'fixing', 'main', $2, $3, $2, 3, 'codex', $4, '计划') RETURNING id`,
		portRepoID, p.base, pr, attemptID).Scan(&id))
	return id
}

func (p *splitPort) planFix(wi int64, round int) aiReq {
	p.t.Helper()
	branch := fmt.Sprintf("kiln/dev-%d", wi)
	spec := &aispec.DevelopSpec{
		Mode: aispec.DevFix, Agent: agentcfg.AgentCodex, WorkItemID: wi, Branch: branch, BaseRef: "main", Round: round,
	}
	planned, err := p.s.ai.Plan(p.ctx, &ai.NewRequest{
		Type: agentcfg.TaskDevelop, RepoID: portRepoID, Event: aispec.EventDevelop, Ref: gitref.BranchPrefix + branch, SHA: p.base,
		Trigger: aispec.DevFix, Source: store.SourceAIDev, Sender: "kiln",
		DedupeKey: fmt.Sprintf("%s:%d:fix:%d:%d", aispec.EventDevelop, portRepoID, wi, round), GroupKey: fmt.Sprintf("fix:work-item:%d", wi),
		Tasks: []ai.NewTask{{
			Key: "develop", Platforms: []string{platMac}, Agent: agentcfg.AgentCodex, DisplayName: "返修",
			Spec: spec, TimeoutMinutes: int(agentcfg.MaxDevTimeout.Minutes()),
		}},
		Detail: func(ctx context.Context, tx pgx.Tx, id int64) error {
			_, err := tx.Exec(ctx, `INSERT INTO ai_dev_requests (request_id, mode, work_item_id, round) VALUES ($1,$2,$3,$4)`, id, aispec.DevFix, wi, round)
			return err
		},
	})
	if err != nil {
		p.t.Fatalf("规划返修：%v", err)
	}
	tasks := p.tasks(aiReq{id: planned.Request.ID})
	p.exec(`UPDATE dev_work_items SET task_id=$2 WHERE id=$1`, wi, tasks[0])
	return aiReq{id: planned.Request.ID}
}

func (p *splitPort) runner(o runnerSpec) *portRunner {
	p.t.Helper()
	if o.role == "" {
		o.role = store.RunnerRoleReview
	}
	if o.agents == nil {
		o.agents = []string{agentcfg.AgentCodex}
	}
	tok, _, err := auth.CreateEnrollToken(p.ctx, p.s.db, "", store.RunnerKindAI, o.role, time.Hour)
	if err != nil {
		p.t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	r := &portRunner{name: fmt.Sprintf("ai-%d", p.next()), role: o.role, agents: o.agents, host: o.host}
	if r.host == "" {
		r.host = "host-" + r.name
	}
	resp, err := p.rpc.Enroll(p.ctx, connect.NewRequest(&runnerv1.EnrollRequest{
		Token: tok, Name: r.name, PublicKey: pub,
		State: &runnerv1.RunnerState{Version: "test"},
	}))
	if err != nil {
		p.t.Fatalf("Enroll：%v", err)
	}
	r.id = resp.Msg.RunnerId
	r.token = authenticate(p.t, p.rpc, r.id, priv)
	return r
}

func (p *splitPort) acquire(r *portRunner, requestID string) (*portTask, error) {
	resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: p.state(r), RequestId: requestID}))
	if err != nil {
		return nil, err
	}
	at := resp.Msg.GetTask()
	if at == nil {
		return nil, nil
	}
	t := &portTask{id: at.TaskId, attempt: at.Ref.AttemptId, epoch: at.Ref.Epoch, agent: at.GetAgent()}
	if rv := at.GetReview(); rv != nil {
		t.prNumber = int(rv.PrNumber)
	}
	return t, nil
}

// state 返回节点 r 在 Acquire 中上报的状态（Orca Server、Docker 就绪，r 提供的 Agent 都可用，默认一次执行一个任务）。
func (p *splitPort) state(r *portRunner) *airunnerv2.AIRunnerState {
	st := &airunnerv2.AIRunnerState{
		Version: "test", Protocol: agentcfg.AIProtocol, Platform: cmp.Or(r.platform, platMac), HostId: r.host,
		Running: int32(r.running), Capacity: int32(max(r.capacity, 1)),
		Orca: &airunnerv2.OrcaState{Ready: true, Version: "1.4.245", RuntimeId: "runtime-test"}, Docker: &airunnerv2.Readiness{Ready: true},
	}
	for _, a := range r.agents {
		st.Agents = append(st.Agents, &airunnerv2.AgentStatus{
			Name: a, Installed: true, Version: "1.0", State: agentcfg.AgentAvailable, CheckedAt: time.Now().Unix(),
		})
	}
	return st
}

func ref(t *portTask) *airunnerv2.TaskRef {
	return &airunnerv2.TaskRef{AttemptId: t.attempt, Epoch: t.epoch}
}

func (p *splitPort) heartbeat(r *portRunner, t *portTask) (bool, error) {
	resp, err := p.ai.Heartbeat(p.ctx, withToken(r.token, &airunnerv2.HeartbeatRequest{Ref: ref(t), Stage: "running", Text: "执行中"}))
	if err != nil {
		return false, err
	}
	return resp.Msg.CancelRequested, nil
}

func (p *splitPort) appendLog(r *portRunner, t *portTask, offset int64, data string) error {
	_, err := p.ai.AppendLog(p.ctx, withToken(r.token, &airunnerv2.AppendLogRequest{Ref: ref(t), Offset: offset, Data: []byte(data)}))
	return err
}

// outcomes 把端口的结果映射到 airunner.v2。
var outcomes = map[string]airunnerv2.Outcome{
	string(store.OutcomeSucceeded):  airunnerv2.Outcome_OUTCOME_SUCCEEDED,
	string(store.OutcomeFailed):     airunnerv2.Outcome_OUTCOME_FAILED,
	string(store.OutcomeInfraError): airunnerv2.Outcome_OUTCOME_INFRA_ERROR,
	string(store.OutcomeCancelled):  airunnerv2.Outcome_OUTCOME_CANCELLED,
	string(store.OutcomeTimedOut):   airunnerv2.Outcome_OUTCOME_TIMED_OUT,
}

// portDispatchID 是测试节点随 Complete 提交的 Orca dispatch（最终的执行关联，心跳不上报它）。
const portDispatchID = "dispatch-1"

func (p *splitPort) complete(r *portRunner, t *portTask, res portResult) (bool, error) {
	req := &airunnerv2.CompleteRequest{
		Ref: ref(t), Outcome: outcomes[res.outcome], ErrorClass: res.errClass, Message: res.message, AgentVersion: "1.0",
		Binding: &airunnerv2.OrcaBinding{RuntimeId: "runtime-test", StartRequestId: "start-" + strconv.FormatInt(t.attempt, 10), DispatchId: portDispatchID},
	}
	if res.report != "" {
		req.Result = &airunnerv2.CompleteRequest_ReviewReport{ReviewReport: []byte(res.report)}
	}
	if res.delivery != "" {
		req.Result = &airunnerv2.CompleteRequest_Delivery{Delivery: &airunnerv2.DevDelivery{Report: []byte(res.delivery)}}
	}
	resp, err := p.ai.Complete(p.ctx, withToken(r.token, req))
	if err != nil {
		return false, err
	}
	return resp.Msg.GetReplayed(), nil
}

func (p *splitPort) expireLease(t *portTask) {
	p.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, t.id)
	p.sweep()
}

func (p *splitPort) age(t *portTask, d time.Duration) {
	p.exec(`UPDATE ai_tasks SET started_at=started_at - make_interval(secs => $2), lease_expires_at=now() + interval '1 hour' WHERE id=$1`,
		t.id, d.Seconds())
}

func (p *splitPort) sweep()   { p.s.ai.SweepOnce(p.ctx) }
func (p *splitPort) restart() { mustNil(p.t, p.s.ai.ExtendLeasesOnStartup(p.ctx)) }

// breakSpec 把任务的执行输入改成无法解析的内容。
func (p *splitPort) breakSpec(taskID int64) {
	p.exec(`UPDATE ai_tasks SET spec='"broken"' WHERE id=$1`, taskID)
}

func (p *splitPort) cancel(req aiReq) {
	mustNil(p.t, p.s.ai.Cancel(p.ctx, req.id, ai.CancelByUser, "test"))
}

func (p *splitPort) tasks(req aiReq) []int64 {
	p.t.Helper()
	ids, err := store.QueryIDs(p.ctx, p.s.db, `SELECT id FROM ai_tasks WHERE request_id=$1 ORDER BY id`, req.id)
	mustNil(p.t, err)
	return ids
}

func (p *splitPort) task(id int64) taskView {
	p.t.Helper()
	var v taskView
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT t.state, coalesce(t.reason, ''), coalesce(t.last_skip_reason, ''), t.epoch, t.attempt_count,
		coalesce(a.error_class, '') FROM ai_tasks t LEFT JOIN ai_attempts a ON a.id=t.current_attempt_id WHERE t.id=$1`, id).
		Scan(&v.state, &v.reason, &v.skipReason, &v.epoch, &v.attempts, &v.errClass))
	return v
}

func (p *splitPort) reqState(req aiReq) string {
	p.t.Helper()
	var st string
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT state FROM ai_requests WHERE id=$1`, req.id).Scan(&st))
	return st
}

// settle 投递 PR 验证结束的事件（outbox.KindPRVerified，由组装层转给合并队列和 AI 开发），不投递其他记录：断言要看到它们
// 写下的通知。
func (p *splitPort) settle() {
	p.t.Helper()
	rows, err := p.s.db.Query(p.ctx, `SELECT id, payload FROM outbox WHERE kind=$1 AND state='pending' ORDER BY id`, outbox.KindPRVerified)
	mustNil(p.t, err)
	type item struct {
		ID      int64
		Payload []byte
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	mustNil(p.t, err)
	for _, it := range items {
		mustNil(p.t, p.s.handlePRVerified(p.ctx, it.Payload))
		p.exec(`UPDATE outbox SET state='done' WHERE id=$1`, it.ID)
	}
}

func (p *splitPort) reqSHA(req aiReq) string {
	p.t.Helper()
	var sha string
	mustNil(p.t, p.s.db.QueryRow(p.ctx, `SELECT sha FROM ai_requests WHERE id=$1`, req.id).Scan(&sha))
	return sha
}

func (p *splitPort) processAIDev() { mustNil(p.t, p.s.aiDev.Process(p.ctx, portRepoID)) }

// ---------- 共用 ----------

func authenticate(t *testing.T, rpc runnerv1connect.RunnerServiceClient, id int64, priv ed25519.PrivateKey) string {
	t.Helper()
	ts, nonce := time.Now().Unix(), rand.Text()
	sig := ed25519.Sign(priv, fmt.Appendf(nil, "kiln-runner-auth:%d:%d:%s", id, ts, nonce))
	resp, err := rpc.Authenticate(t.Context(), connect.NewRequest(&runnerv1.AuthenticateRequest{RunnerId: id, Timestamp: ts, Nonce: nonce, Signature: sig}))
	if err != nil {
		t.Fatalf("Authenticate：%v", err)
	}
	return resp.Msg.AccessToken
}

func withToken[T any](tok string, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+tok)
	return req
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (p *splitPort) env() *portEnv { return p.portEnv }

// prReviewKind 是发布 PR review 的 outbox 记录的类型。
func prReviewKind(aiPort) string { return review.KindPRReview }
