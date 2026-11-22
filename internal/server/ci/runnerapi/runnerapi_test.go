package runnerapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestOutcomeFromProto(t *testing.T) {
	for in, want := range map[runnerv1.Outcome]store.Outcome{
		runnerv1.Outcome_OUTCOME_SUCCEEDED:   store.OutcomeSucceeded,
		runnerv1.Outcome_OUTCOME_FAILED:      store.OutcomeFailed,
		runnerv1.Outcome_OUTCOME_INFRA_ERROR: store.OutcomeInfraError,
		runnerv1.Outcome_OUTCOME_CANCELLED:   store.OutcomeCancelled,
		runnerv1.Outcome_OUTCOME_TIMED_OUT:   store.OutcomeTimedOut,
		runnerv1.Outcome(999):                "",
	} {
		if got := outcomeFromProto(in); got != want {
			t.Errorf("outcomeFromProto(%v) = %q，期望 %q", in, got, want)
		}
	}
}

// Runner 上报的空闲 CPU 不是有限值时当作没有上报：不写入 capabilities（否则 store.MustJSON 会 panic），也不参与资源比较。
func TestReportedFreeCPU(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *runnerv1.RunnerState
		cpu  float64
		ok   bool
	}{
		{"没有状态", nil, 0, false},
		{"没有上报资源", &runnerv1.RunnerState{}, 0, false},
		{"有限值", &runnerv1.RunnerState{Free: &runnerv1.Resources{Cpu: 2.5}}, 2.5, true},
		{"NaN", &runnerv1.RunnerState{Free: &runnerv1.Resources{Cpu: math.NaN()}}, 0, false},
		{"正无穷", &runnerv1.RunnerState{Free: &runnerv1.Resources{Cpu: math.Inf(1)}}, 0, false},
		{"负无穷", &runnerv1.RunnerState{Free: &runnerv1.Resources{Cpu: math.Inf(-1)}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpu, ok := reportedFreeCPU(tc.st)
			if cpu != tc.cpu || ok != tc.ok {
				t.Fatalf("reportedFreeCPU = %v, %v，期望 %v, %v", cpu, ok, tc.cpu, tc.ok)
			}
			if tc.st == nil {
				return
			}
			caps := runnerCaps(tc.st)
			if _, has := caps["free_cpu"]; has != tc.ok {
				t.Fatalf("capabilities = %v，期望包含 free_cpu = %v", caps, tc.ok)
			}
			store.MustJSON(caps) // 不应 panic
		})
	}
}

// claimOne 让 Runner 领取一个新的 pending job，返回 job ID 和 attempt。
func (h *harness) claimOne(r *testRunner, opts ...func(*jobRow)) (int64, *runnerv1.AttemptRef) {
	h.t.Helper()
	id := h.pendingJob(opts...)
	jobs := r.mustAcquire(1, 1)
	if jobs[0].JobId != id {
		h.t.Fatalf("应领取 job %d，实际 %d", id, jobs[0].JobId)
	}
	return id, jobs[0].Ref
}

// Heartbeat 续租并记录当前 step；job 被请求取消时返回取消指令；(attempt, epoch) 不是当前值时返回 STALE_EPOCH。
func TestService_Heartbeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	other := h.enrollRunner()
	id, ref := h.claimOne(r)

	// 租约快到期时续租
	h.exec(`UPDATE jobs SET lease_expires_at=now()+interval '1 second' WHERE id=$1`, id)
	h.exec(`UPDATE runners SET last_seen_at=now()-interval '1 hour' WHERE id=$1`, r.id)
	resp, err := r.heartbeat(ref, 2)
	if err != nil {
		t.Fatalf("Heartbeat：%v", err)
	}
	s := h.job(id)
	if resp.CancelRequested || s.Lease == nil || time.Until(*s.Lease) < testLeaseTTL-10*time.Second {
		t.Fatalf("应续租约 %v 且没有取消：cancel=%v lease=%v", testLeaseTTL, resp.CancelRequested, s.Lease)
	}
	if resp.LeaseExpiresAt != s.Lease.Unix() {
		t.Fatalf("返回的租约到期时间 %d 应与数据库一致 %d", resp.LeaseExpiresAt, s.Lease.Unix())
	}
	var step int
	var startedStep bool
	var seen time.Time
	if err := h.db.QueryRow(t.Context(), `SELECT a.current_step, j.started_step, r.last_seen_at FROM attempts a JOIN jobs j ON j.id=a.job_id
		JOIN runners r ON r.id=a.runner_id WHERE a.id=$1`, ref.AttemptId).Scan(&step, &startedStep, &seen); err != nil {
		t.Fatal(err)
	}
	if step != 2 || !startedStep || time.Since(seen) > time.Minute {
		t.Fatalf("应记录当前 step 2、已开始执行 step、Runner 在线：step=%d started=%v seen=%v", step, startedStep, seen)
	}

	// 请求取消后，心跳返回取消指令（仍然续租，等 Runner 确认）
	h.exec(`UPDATE jobs SET cancel_requested=true WHERE id=$1`, id)
	if resp, err := r.heartbeat(ref, 3); err != nil || !resp.CancelRequested {
		t.Fatalf("请求取消后心跳应返回取消指令：%+v %v", resp, err)
	}

	tests := []struct {
		name   string
		runner *testRunner
		ref    *runnerv1.AttemptRef
	}{
		{"epoch 过期", r, &runnerv1.AttemptRef{AttemptId: ref.AttemptId, Epoch: ref.Epoch + 1}},
		{"其他 Runner 的 attempt", other, ref},
		{"attempt 不存在", r, &runnerv1.AttemptRef{AttemptId: ref.AttemptId + 1000, Epoch: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.runner.heartbeat(tc.ref, 0); !rpcerr.IsStaleEpoch(err) {
				t.Fatalf("应返回 STALE_EPOCH：%v", err)
			}
		})
	}
	if _, err := r.heartbeat(nil, 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("缺少 AttemptRef 应返回 InvalidArgument：%v", err)
	}

	// job 已经结束（例如被 sweeper 判为超时）：心跳返回 STALE_EPOCH，Runner 据此放弃
	h.exec(`UPDATE jobs SET state='timed_out' WHERE id=$1`, id)
	if _, err := r.heartbeat(ref, 3); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("job 结束后心跳应返回 STALE_EPOCH：%v", err)
	}
}

// Complete：结果只接受当前 (attempt, epoch)，并且只提交一次（PLAN #6.1）。
func TestService_Complete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id, ref := h.claimOne(r)
	manifest := h.putResult("dist", map[string]string{"bin/app": "binary"}, true)
	ok := &runnerv1.CompleteRequest{Ref: ref, Outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED, ResultManifestDigest: manifest, CpuSeconds: 1.5}

	// 旧的 epoch：拒绝
	stale := &runnerv1.CompleteRequest{Ref: &runnerv1.AttemptRef{AttemptId: ref.AttemptId, Epoch: ref.Epoch - 1}, Outcome: runnerv1.Outcome_OUTCOME_FAILED}
	if err := r.complete(stale); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("旧 epoch 的结果应返回 STALE_EPOCH：%v", err)
	}
	if err := r.complete(ok); err != nil {
		t.Fatalf("Complete：%v", err)
	}
	s := h.job(id)
	if s.State != store.JobSucceeded || s.Result == nil || *s.Result != manifest || s.Lease != nil {
		t.Fatalf("提交成功后 job 应为 succeeded、记录结果、清掉租约：%+v", s)
	}
	var outcome string
	var cpu float64
	if err := h.db.QueryRow(t.Context(), `SELECT outcome, cpu_seconds FROM attempts WHERE id=$1`, ref.AttemptId).Scan(&outcome, &cpu); err != nil {
		t.Fatal(err)
	}
	if outcome != string(store.OutcomeSucceeded) || cpu != 1.5 {
		t.Fatalf("attempt 的结果 %s、CPU %v", outcome, cpu)
	}
	var runState string
	if err := h.db.QueryRow(t.Context(), `SELECT r.state FROM runs r JOIN jobs j ON j.run_id=r.id WHERE j.id=$1`, id).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != string(store.RunSucceeded) {
		t.Fatalf("唯一的 job 成功后 run 应为 succeeded：%s", runState)
	}
	// 重复提交（例如响应丢失后重试）：STALE_EPOCH，结果不变
	if err := r.complete(&runnerv1.CompleteRequest{Ref: ref, Outcome: runnerv1.Outcome_OUTCOME_FAILED}); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("重复提交应返回 STALE_EPOCH：%v", err)
	}
	if s := h.job(id); s.State != store.JobSucceeded {
		t.Fatalf("重复提交不应改变结果：%s", s.State)
	}
}

// 重新排队后再次领取：epoch+1，上一次 attempt 的结果不再被接受。
func TestService_Complete_afterRequeue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id, first := h.claimOne(r)

	// 基础设施错误：重新排队
	if err := r.complete(&runnerv1.CompleteRequest{Ref: first, Outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, ErrorClass: "infra.prepare", Message: "docker 不可用"}); err != nil {
		t.Fatal(err)
	}
	if s := h.job(id); s.State != store.JobPending || !strings.Contains(s.Reason, "重新排队") {
		t.Fatalf("基础设施错误后应重新排队：%+v", s)
	}
	second := r.mustAcquire(1, 1)[0].Ref
	if second.Epoch != first.Epoch+1 || second.AttemptId == first.AttemptId {
		t.Fatalf("再次领取应 epoch+1 并创建新的 attempt：%+v → %+v", first, second)
	}
	if err := r.complete(&runnerv1.CompleteRequest{Ref: first, Outcome: runnerv1.Outcome_OUTCOME_FAILED}); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("上一次 attempt 的结果应返回 STALE_EPOCH：%v", err)
	}
	if err := r.complete(&runnerv1.CompleteRequest{Ref: second, Outcome: runnerv1.Outcome_OUTCOME_FAILED, ExitCode: 2}); err != nil {
		t.Fatal(err)
	}
	if s := h.job(id); s.State != store.JobFailed || s.Reason != "step 退出码 2" || s.AttemptCount != 2 {
		t.Fatalf("当前 attempt 的失败应生效：%+v", s)
	}
}

// 成功的结果必须完整：manifest 无效或引用的文件没有上传时按 infra.upload 处理（重新排队），
// 缺少 outcome 时返回 InvalidArgument。
func TestService_Complete_verifyResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest func(h *harness) string
		reason   string
	}{
		{"digest 无效", func(*harness) string { return "not-a-digest" }, "digest 无效"},
		{"manifest 不存在", func(*harness) string { return cas.Digest([]byte("missing")) }, "读取结果 manifest"},
		{"schema 不对", func(h *harness) string { return h.putBlob([]byte(`{"schema":"other"}`)) }, "schema 不正确"},
		{"输出的文件没有上传", func(h *harness) string { return h.putResult("dist", map[string]string{"app": "data"}, false) }, "没有上传"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.enrollRunner()
			id, ref := h.claimOne(r)
			if err := r.complete(&runnerv1.CompleteRequest{Ref: ref, Outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED, ResultManifestDigest: tc.manifest(h)}); err != nil {
				t.Fatalf("Complete：%v", err)
			}
			s := h.job(id)
			if s.State != store.JobPending || !strings.Contains(s.Reason, "infra.upload") || !strings.Contains(s.Reason, tc.reason) {
				t.Fatalf("结果不完整应按 infra.upload 重新排队，原因包含 %q：%+v", tc.reason, s)
			}
		})
	}
	t.Run("缺少 outcome", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		r := h.enrollRunner()
		_, ref := h.claimOne(r)
		if err := r.complete(&runnerv1.CompleteRequest{Ref: ref}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("缺少 outcome 应返回 InvalidArgument：%v", err)
		}
	})
}

// PrepareUpload 只为 CAS 中还没有的对象签发上传地址；PrepareDownload 签发下载地址，对象不存在时 NotFound。
// 签发的地址只能用于对应的操作。
func TestService_PrepareUploadDownload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	_, ref := h.claimOne(r)
	have := h.putBlob([]byte("already there"))
	h.exec(`INSERT INTO blobs (digest, size, last_ref_at) VALUES ($1, 13, now()-interval '1 day')`, have)
	missing := cas.Digest([]byte("new object"))
	// 文件在、blobs 中没有记录（记录已被保留策略的清理删除，PLAN #10.4）：同样要求重新上传
	orphan := h.putBlob([]byte("no record"))

	up, err := h.client.PrepareUpload(t.Context(), withAuth(r.token, &runnerv1.PrepareUploadRequest{
		Ref:   ref,
		Blobs: []*runnerv1.BlobRef{{Digest: have}, {Digest: missing}, {Digest: orphan}},
	}))
	if err != nil {
		t.Fatalf("PrepareUpload：%v", err)
	}
	if len(up.Msg.Uploads) != 2 || up.Msg.Uploads[0].Digest != missing || up.Msg.Uploads[1].Digest != orphan {
		t.Fatalf("只应为缺少的对象和没有记录的对象签发上传地址：%+v", up.Msg.Uploads)
	}
	checkURL(t, h, up.Msg.Uploads[0].Url, missing, auth.CASOpPut)
	var refreshed time.Time
	if err := h.db.QueryRow(t.Context(), `SELECT last_ref_at FROM blobs WHERE digest=$1`, have).Scan(&refreshed); err != nil || time.Since(refreshed) > time.Minute {
		t.Fatalf("已有对象应刷新引用时间：%v %v", refreshed, err)
	}
	if _, err := h.client.PrepareUpload(t.Context(), withAuth(r.token, &runnerv1.PrepareUploadRequest{Ref: ref, Blobs: []*runnerv1.BlobRef{{Digest: "md5:x"}}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("无效的 digest 应返回 InvalidArgument：%v", err)
	}

	down, err := h.client.PrepareDownload(t.Context(), withAuth(r.token, &runnerv1.PrepareDownloadRequest{Ref: ref, Digests: []string{have}}))
	if err != nil {
		t.Fatalf("PrepareDownload：%v", err)
	}
	if len(down.Msg.Downloads) != 1 {
		t.Fatalf("下载地址：%+v", down.Msg.Downloads)
	}
	checkURL(t, h, down.Msg.Downloads[0].Url, have, auth.CASOpGet)
	if _, err := h.client.PrepareDownload(t.Context(), withAuth(r.token, &runnerv1.PrepareDownloadRequest{Ref: ref, Digests: []string{missing}})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("不存在的对象应返回 NotFound：%v", err)
	}
	if _, err := h.client.PrepareDownload(t.Context(), withAuth(r.token, &runnerv1.PrepareDownloadRequest{Ref: ref, Digests: []string{"sha256:../x"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("无效的 digest 应返回 InvalidArgument：%v", err)
	}

	// 不是当前 attempt 的请求：STALE_EPOCH，不签发地址
	stale := &runnerv1.AttemptRef{AttemptId: ref.AttemptId, Epoch: ref.Epoch + 1}
	if _, err := h.client.PrepareUpload(t.Context(), withAuth(r.token, &runnerv1.PrepareUploadRequest{Ref: stale, Blobs: []*runnerv1.BlobRef{{Digest: missing}}})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("PrepareUpload 应返回 STALE_EPOCH：%v", err)
	}
	if _, err := h.client.PrepareDownload(t.Context(), withAuth(r.token, &runnerv1.PrepareDownloadRequest{Ref: stale, Digests: []string{have}})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("PrepareDownload 应返回 STALE_EPOCH：%v", err)
	}
}

// checkURL 检查预签名地址指向 digest，签名对 op 有效、对另一种操作无效，有效期约为 casURLTTL。
func checkURL(t *testing.T, h *harness, raw, digest, op string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/cas/"+digest || q.Get("op") != op {
		t.Fatalf("地址 %s 应指向 /cas/%s?op=%s", raw, digest, op)
	}
	if !h.keys.CheckCASSig(op, digest, q.Get("exp"), q.Get("sig")) {
		t.Fatalf("地址 %s 的签名应对 %s 有效", raw, op)
	}
	otherOp := auth.CASOpGet
	if op == auth.CASOpGet {
		otherOp = auth.CASOpPut
	}
	if h.keys.CheckCASSig(otherOp, digest, q.Get("exp"), q.Get("sig")) {
		t.Fatalf("地址 %s 的签名不应对 %s 有效", raw, otherOp)
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Until(time.Unix(exp, 0)) < casURLTTL-time.Minute || time.Until(time.Unix(exp, 0)) > casURLTTL {
		t.Fatalf("有效期应约为 %v：exp=%s", casURLTTL, q.Get("exp"))
	}
}

// AppendLog 按 offset 去重（重传幂等），job 结束后仍可补传；其他 Runner 不能写。
func TestService_AppendLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	other := h.enrollRunner()
	id, ref := h.claimOne(r)
	appendLog := func(r *testRunner, off int64, data string) (int64, error) {
		resp, err := h.client.AppendLog(t.Context(), withAuth(r.token, &runnerv1.AppendLogRequest{Ref: ref, Offset: off, Data: []byte(data)}))
		if err != nil {
			return 0, err
		}
		return resp.Msg.Committed, nil
	}
	if n, err := appendLog(r, 0, "hello\n"); err != nil || n != 6 {
		t.Fatalf("第一段：%d %v", n, err)
	}
	if n, err := appendLog(r, 0, "hello\n"); err != nil || n != 6 {
		t.Fatalf("重传同一段应幂等：%d %v", n, err)
	}
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, id)
	if n, err := appendLog(r, 6, "bye\n"); err != nil || n != 10 {
		t.Fatalf("job 结束后应允许补传：%d %v", n, err)
	}
	if _, err := appendLog(r, 100, "gap"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("offset 不连续应返回 FailedPrecondition：%v", err)
	}
	if _, err := appendLog(r, -4, "negative"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("负的 offset 应返回 InvalidArgument：%v", err)
	}
	if n, err := appendLog(r, 10, ""); err != nil || n != 10 {
		t.Fatalf("负的 offset 不应写入日志：%d %v", n, err)
	}
	if _, err := appendLog(other, 10, "x"); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("其他 Runner 写日志应返回 STALE_EPOCH：%v", err)
	}
}

// 租约过期、被 Server 判为失联的 attempt 已被取代（job 可能已被其他 Runner 重新领取）：与 Heartbeat、Complete 一样
// 返回 STALE_EPOCH，不再接受日志。
func TestService_AppendLog_lostAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id, ref := h.claimOne(r)
	// 与 sweeper 处理过期租约相同
	if err := h.db.Tx(t.Context(), func(tx pgx.Tx) error {
		j, err := cistore.GetJob(t.Context(), tx, id, true)
		if err != nil {
			return err
		}
		return h.svc.engine.FinishAttempt(t.Context(), tx, j, engine.AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: engine.ErrClassLost}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if s := h.job(id); s.State != store.JobPending {
		t.Fatalf("租约过期后 job 应重新排队：%+v", s)
	}
	_, err := h.client.AppendLog(t.Context(), withAuth(r.token, &runnerv1.AppendLogRequest{Ref: ref, Offset: 0, Data: []byte("late\n")}))
	if !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("已判为失联的 attempt 写日志应返回 STALE_EPOCH：%v", err)
	}
}

// FetchCredentials 下发 job 引用的 secret：environment 级覆盖仓库级，仓库级覆盖账号级；没有 GitHub App 时不签发源码 token。
func TestService_FetchCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	repo := h.addRepo()
	var envID int64
	if err := h.db.QueryRow(t.Context(), `INSERT INTO environments (owner_id, repo_id, name) VALUES (9001, $1, 'prod') RETURNING id`, repo).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	h.setSecret(store.ScopeOwner, 9001, "A", "owner-a")
	h.setSecret(store.ScopeOwner, 9001, "B", "owner-b")
	h.setSecret(store.ScopeRepo, repo, "B", "repo-b")
	h.setSecret(store.ScopeRepo, repo, "C", "repo-c")
	h.setSecret(store.ScopeEnvironment, envID, "C", "env-c")
	h.setSecret(store.ScopeRepo, repo, "UNUSED", "x")
	id := h.addJob(h.addRun(repo), func(j *jobRow) {
		j.spec.Environment, j.spec.Secrets = "prod", []string{"A", "B", "C"}
	})
	ref := r.mustAcquire(1, 1)[0].Ref
	resp, err := h.client.FetchCredentials(t.Context(), withAuth(r.token, &runnerv1.FetchCredentialsRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("FetchCredentials：%v", err)
	}
	want := map[string]string{"A": "owner-a", "B": "repo-b", "C": "env-c"}
	if !maps.Equal(resp.Msg.Secrets, want) || resp.Msg.GitToken != "" {
		t.Fatalf("secret = %v，期望 %v；没有 GitHub App 时不应有源码 token：%q", resp.Msg.Secrets, want, resp.Msg.GitToken)
	}
	h.exec(`UPDATE jobs SET state='cancelled' WHERE id=$1`, id)
	if _, err := h.client.FetchCredentials(t.Context(), withAuth(r.token, &runnerv1.FetchCredentialsRequest{Ref: ref})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("job 结束后不应再下发 secret：%v", err)
	}
}

// environment 的授权在发放凭据时重新检查：领取后收紧 ref 策略或删除环境，不能继续下发 secret 或签发源码 token。
func TestService_FetchCredentials_rechecksEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		change  string
		wantErr string
	}{
		{"允许的 ref", "", ""},
		{"ref 策略已收紧", `UPDATE environments SET ref_policy='["refs/tags/v*"]' WHERE id=$1`, "ref 策略不允许"},
		{"environment 已删除", `DELETE FROM environments WHERE id=$1`, "不存在"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := fakegithub.New("tester")
			t.Cleanup(fake.Close)
			h := newHarness(t, func(o *harnessOpts) {
				o.github = &gh.Client{AppID: fake.AppID, Key: testRSAKey(t), APIURL: fake.URL, UploadURL: fake.URL}
			})
			r := h.enrollRunner()
			repo := h.addGitHubRepo(fake)
			var envID int64
			if err := h.db.QueryRow(t.Context(), `INSERT INTO environments (owner_id, repo_id, name, ref_policy)
				VALUES (9001, $1, 'prod', '["refs/heads/main"]') RETURNING id`, repo).Scan(&envID); err != nil {
				t.Fatal(err)
			}
			h.setSecret(store.ScopeRepo, repo, "TOKEN", "repo-token")
			h.setSecret(store.ScopeEnvironment, envID, "TOKEN", "env-token")
			h.addJob(h.addRun(repo), func(j *jobRow) {
				j.spec.Environment, j.spec.Secrets = "prod", []string{"TOKEN"}
			})
			ref := r.mustAcquire(1, 1)[0].Ref
			if tc.change != "" {
				h.exec(tc.change, envID)
			}
			resp, err := h.client.FetchCredentials(t.Context(), withAuth(r.token, &runnerv1.FetchCredentialsRequest{Ref: ref}))
			if tc.wantErr == "" {
				if err != nil || resp.Msg.Secrets["TOKEN"] != "env-token" || resp.Msg.GitToken == "" {
					t.Fatalf("授权仍有效时应下发环境 secret 和源码 token：%+v %v", resp, err)
				}
				return
			}
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.wantErr) || resp != nil {
				t.Fatalf("授权变化后应拒绝下发凭据：%+v %v", resp, err)
			}
			if requests := fake.Requests(); len(requests) != 0 {
				t.Fatalf("授权无效时不应请求 GitHub 签发 token：%v", requests)
			}
		})
	}
}

// 配置了 GitHub App 时签发源码 token：job 的 repositories 必须在同一个 installation 下，否则拒绝（不签发）。
func TestService_FetchCredentials_gitToken(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	h := newHarness(t, func(o *harnessOpts) {
		o.github = &gh.Client{AppID: fake.AppID, Key: testRSAKey(t), APIURL: fake.URL, UploadURL: fake.URL}
	})
	r := h.enrollRunner()
	app := h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "app" })
	h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "lib" })
	h.addGitHubRepo(fake, func(o *repoOpts) { o.name, o.installation = "foreign", 778 })
	tests := []struct {
		name    string
		repos   []string
		wantErr string // 空表示应签发 token
	}{
		{"只有本仓库", nil, ""},
		{"同一个 installation 的仓库", []string{"tester/lib"}, ""},
		{"其他 installation 的仓库", []string{"tester/foreign"}, "不在同一个 installation 下"},
		{"App 访问不到的仓库", []string{"tester/missing"}, "不在同一个 installation 下"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.addJob(h.addRun(app), func(j *jobRow) { j.spec.Repositories = tc.repos })
			ref := r.mustAcquire(1, 1)[0].Ref
			resp, err := h.client.FetchCredentials(t.Context(), withAuth(r.token, &runnerv1.FetchCredentialsRequest{Ref: ref}))
			if tc.wantErr == "" {
				if err != nil || !strings.HasPrefix(resp.Msg.GitToken, "ghs_") || resp.Msg.GitTokenExpiresAt == 0 {
					t.Fatalf("应签发源码 token：%+v %v", resp, err)
				}
				return
			}
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v，期望 FailedPrecondition 且包含 %q", err, tc.wantErr)
			}
		})
	}
}

// 查找 job 的 repositories 时 GitHub 暂时不可用：返回可重试的 Unavailable，而不是 FailedPrecondition（Runner 不重试）。
func TestService_FetchCredentials_lookupUnavailable(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // 连接被拒绝
	h := newHarness(t, func(o *harnessOpts) {
		o.github = &gh.Client{AppID: 1, Key: testRSAKey(t), APIURL: down.URL, UploadURL: down.URL}
	})
	r := h.enrollRunner()
	h.addJob(h.addRun(h.addRepo()), func(j *jobRow) { j.spec.Repositories = []string{"tester/not-in-kiln"} })
	ref := r.mustAcquire(1, 1)[0].Ref
	_, err := h.client.FetchCredentials(t.Context(), withAuth(r.token, &runnerv1.FetchCredentialsRequest{Ref: ref}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "tester/not-in-kiln") {
		t.Fatalf("错误 = %v，期望 Unavailable 且包含仓库名", err)
	}
}

// fetch 的上游输出写进 JobSpec：Runner 按 manifest digest 下载上游产物。
func TestService_Acquire_fetchSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	run := h.addRun(h.addRepo())
	result := h.putResult("dist", map[string]string{"app": "binary"}, true)
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest)
		VALUES ('normal', $1, 'build', 'build', 'build', '{}', 'linux/arm64', 'oci', 'succeeded', $2)`, run, result)
	h.addJob(run, func(j *jobRow) {
		j.spec.Fetch = []workflow.FetchRef{{JobKey: "build", Output: "dist", To: "in/"}}
	})
	spec := r.mustAcquire(1, 1)[0]
	var rm cas.ResultManifest
	if err := cas.ReadJSON(t.Context(), h.cas, result, &rm); err != nil {
		t.Fatal(err)
	}
	if len(spec.Fetch) != 1 || spec.Fetch[0].From != "build.dist" || spec.Fetch[0].To != "in/" || spec.Fetch[0].ManifestDigest != rm.Outputs["dist"] {
		t.Fatalf("Fetch = %+v，期望来自 build.dist、manifest %s", spec.Fetch, rm.Outputs["dist"])
	}
}

// 无法生成 JobSpec 的 job（上游缺少声明的输出）置为失败，不回滚整次领取：同一次领取中的其他 job 照常领到，
// 坏 job 也不会一直排在候选最前面、让所有 Runner 都领不到 job。
func TestService_Acquire_badSpecFailsJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	run := h.addRun(h.addRepo())
	result := h.putResult("dist", map[string]string{"app": "binary"}, true)
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest)
		VALUES ('normal', $1, 'build', 'build', 'build', '{}', 'linux/arm64', 'oci', 'succeeded', $2)`, run, result)
	bad := h.addJob(run, func(j *jobRow) {
		j.priority = 2 // 排在候选最前面
		j.spec.Fetch = []workflow.FetchRef{{JobKey: "build", Output: "missing", To: "in/"}}
	})
	good := h.addJob(run)
	specs := r.mustAcquire(2, 1)
	if specs[0].JobId != good || specs[0].Ref.GetAttemptId() == 0 || specs[0].Ref.GetEpoch() != 1 {
		t.Fatalf("领到 job %d（attempt %v），期望 job %d 且带 attempt", specs[0].JobId, specs[0].Ref, good)
	}
	if s := h.job(bad); s.State != store.JobFailed || !strings.Contains(s.Reason, "没有名为 missing 的输出") || s.Attempt != nil {
		t.Fatalf("坏 job = %+v，期望 failed、原因说明缺少输出、没有 attempt", s)
	}
}

// fetch 的上游不在这次运行中、上游的结果 manifest 已损坏时同样只让这个 job 失败：
// 之前这两种情况返回内部错误、回滚整次领取，排在最前面的坏 job 让所有兼容的 Runner 都领不到 job。
func TestService_Acquire_badFetchUpstreamFailsJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// upstream 在 run 中准备上游 job，返回 fetch 的 job_key
		upstream func(h *harness, run int64) string
		reason   string
	}{
		{
			name:     "上游不存在",
			upstream: func(*harness, int64) string { return "gone" },
			reason:   "fetch 的上游 gone 不存在",
		},
		{
			name: "上游结果损坏",
			upstream: func(h *harness, run int64) string {
				result := h.putResult("dist", map[string]string{"app": "binary"}, true)
				p, err := h.cas.Path(result)
				if err != nil {
					h.t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
					h.t.Fatal(err)
				}
				h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest)
					VALUES ('normal', $1, 'build', 'build', 'build', '{}', 'linux/arm64', 'oci', 'succeeded', $2)`, run, result)
				return "build"
			},
			reason: "内容损坏",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.enrollRunner()
			run := h.addRun(h.addRepo())
			key := tc.upstream(h, run)
			bad := h.addJob(run, func(j *jobRow) {
				j.priority = 2 // 排在候选最前面
				j.spec.Fetch = []workflow.FetchRef{{JobKey: key, Output: "dist", To: "in/"}}
			})
			good := h.addJob(run)
			if specs := r.mustAcquire(2, 1); specs[0].JobId != good {
				t.Fatalf("领到 job %d，期望 %d", specs[0].JobId, good)
			}
			if s := h.job(bad); s.State != store.JobFailed || !strings.Contains(s.Reason, tc.reason) {
				t.Fatalf("坏 job = %+v，期望 failed、原因包含 %q", s, tc.reason)
			}
		})
	}
}

// 日志存储故障（这里让日志文件的位置是一个目录）是 Server 的内部错误：返回可重试的 CodeInternal 并记录日志，
// 不能和 offset 不连续一样当作 FailedPrecondition（不记录、Runner 也不重试）。
func TestService_AppendLog_storageError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	_, ref := h.claimOne(r)
	if err := os.Mkdir(filepath.Join(h.logDir, fmt.Sprintf("%d.log", ref.AttemptId)), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.AppendLog(t.Context(), withAuth(r.token, &runnerv1.AppendLogRequest{Ref: ref, Offset: 0, Data: []byte("hello\n")}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("日志存储故障应返回 CodeInternal：%v", err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "Runner API 内部错误") || !strings.Contains(logs, "AppendLog") {
		t.Fatalf("应记录内部错误日志：%s", logs)
	}
}

// 数据库等内部错误经 ErrorInterceptor 变成 CodeInternal 并记录日志；返回给 Runner 的仍是可重试的错误。
func TestService_internalErrorThroughInterceptor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	_, ref := h.claimOne(r)
	have := h.putBlob([]byte("blob"))
	h.exec(`ALTER TABLE blobs RENAME TO blobs_gone`) // 让刷新引用时间的 UPDATE 失败
	_, err := h.client.PrepareUpload(t.Context(), withAuth(r.token, &runnerv1.PrepareUploadRequest{Ref: ref, Blobs: []*runnerv1.BlobRef{{Digest: have}}}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("数据库错误应返回 CodeInternal：%v", err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "Runner API 内部错误") || !strings.Contains(logs, "PrepareUpload") {
		t.Fatalf("应记录内部错误日志：%s", logs)
	}
}

// 注册和认证时的内部错误（数据库等）不能当作拒绝返回：经 ErrorInterceptor 变成可重试的 CodeInternal 并记录日志，
// 而不是 PermissionDenied（Runner 不重试）或 Unauthenticated。
func TestService_enrollAuthenticateInternalError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	tok, _, err := auth.CreateEnrollToken(t.Context(), h.db, "", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`ALTER TABLE enrollment_tokens RENAME TO enrollment_tokens_gone`)
	h.exec(`ALTER TABLE auth_nonces RENAME TO auth_nonces_gone`)
	_, err = h.client.Enroll(t.Context(), connect.NewRequest(&runnerv1.EnrollRequest{Token: tok, Name: "db-down", PublicKey: pub}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Enroll 遇到数据库错误应返回 CodeInternal：%v", err)
	}
	_, err = h.client.Authenticate(t.Context(), connect.NewRequest(r.authRequest(time.Now(), randNonce())))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Authenticate 遇到数据库错误应返回 CodeInternal：%v", err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "Enroll") || !strings.Contains(logs, "Authenticate") {
		t.Fatalf("应记录内部错误日志：%s", logs)
	}
}

// Heartbeat 保存 Runner 上报的进度（换算成时间）；旧版 Runner 不上报时不保存，没有上报进度的心跳不覆盖。
func TestService_HeartbeatProgress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	_, ref := h.claimOne(r)
	if _, err := r.heartbeat(ref, 2); err != nil {
		t.Fatalf("Heartbeat：%v", err)
	}
	// 旧版 Runner 不上报进度：不保存
	if got := h.progress(ref.AttemptId); got != nil {
		t.Fatalf("没有上报进度时应为空：%+v", got)
	}
	// 上报的进度换算成时间后保存；之后没有上报进度的心跳不覆盖
	_, err := r.h.client.Heartbeat(t.Context(), withAuth(r.token, &runnerv1.HeartbeatRequest{Ref: ref, CurrentStep: 2, Progress: &runnerv1.Progress{
		Phase: runnerv1.Phase_PHASE_UPLOAD, Detail: "上传产物到 Kiln Server", PhaseElapsedMs: 90_000, IdleMs: 2_000,
		FilesTotal: 1, BytesTotal: 1000, BytesSent: 400, FilesSkipped: 2, Retries: 1, LastError: "HTTP 502",
	}}))
	if err != nil {
		t.Fatalf("Heartbeat：%v", err)
	}
	if _, err := r.heartbeat(ref, 2); err != nil {
		t.Fatalf("Heartbeat：%v", err)
	}
	p := h.progress(ref.AttemptId)
	now := time.Now().Unix()
	if p == nil || p.Phase != store.PhaseUpload || p.Detail != "上传产物到 Kiln Server" || p.Step != 2 || p.FilesTotal != 1 ||
		p.BytesTotal != 1000 || p.BytesSent != 400 || p.FilesSkipped != 2 || p.Retries != 1 || p.LastError != "HTTP 502" {
		t.Fatalf("保存的进度 = %+v", p)
	}
	if d := now - p.PhaseStartedAt; d < 89 || d > 100 {
		t.Errorf("阶段开始时间应为约 90 秒前：%d", d)
	}
	if d := now - p.MovedAt; d < 1 || d > 10 {
		t.Errorf("最近进展时间应为约 2 秒前：%d", d)
	}
}

// 只领一个 job、队首的坏 job（job 本身的问题）在准备时被置为失败时，这次领取立即从队首重新考察并领到下一个 job，
// 不等长轮询到期被唤醒：失败的 job 已经离开队列，队列此刻已经变了。
func TestService_Acquire_badSpecReexaminesQueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	run := h.addRun(h.addRepo())
	result := h.putResult("dist", map[string]string{"app": "binary"}, true)
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest)
		VALUES ('normal', $1, 'build', 'build', 'build', '{}', 'linux/arm64', 'oci', 'succeeded', $2)`, run, result)
	bad := h.addJob(run, func(j *jobRow) {
		j.priority = 2 // 排在候选最前面，容量只够它一个
		j.spec.Fetch = []workflow.FetchRef{{JobKey: "build", Output: "missing", To: "in/"}}
	})
	good := h.addJob(run)
	if specs := r.mustAcquire(1, 1); specs[0].JobId != good {
		t.Fatalf("领到 job %d，期望重新考察后领到 %d", specs[0].JobId, good)
	}
	if s := h.job(bad); s.State != store.JobFailed {
		t.Fatalf("坏 job = %+v，期望 failed", s)
	}
}

// TestFinitePositive：Runner 上报的数值只有有限的正数才写入数据库（NaN、±Inf、0 和负数都不写）。
func TestFinitePositive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    float64
		want bool
	}{
		{1.5, true},
		{0.001, true},
		{0, false},
		{-1, false},
		{math.NaN(), false},
		{math.Inf(1), false},
		{math.Inf(-1), false},
	}
	for _, c := range cases {
		if got := finitePositive(c.v); got != c.want {
			t.Errorf("finitePositive(%v) = %v，期望 %v", c.v, got, c.want)
		}
	}
}
