package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Dashboard 的表单操作（POST，已通过 CSRF 校验）。

func (d *Dashboard) postCancelRun(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	back := runURL(id)
	if err := d.admin.CancelRun(r.Context(), id, se.Login); err != nil {
		d.flashMsg(w, r, back, d.failureFlash(r, err, msgRunCancelFailed, msgRunCancelFailedRef))
		return
	}
	d.flashMsg(w, r, back, message(msgRunCancelled))
}

func (d *Dashboard) postRerun(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	n, err := d.admin.RerunRun(r.Context(), id, store.RerunMode(r.PostFormValue("mode")), se.Login, r.PostFormValue("keep") == "on")
	if err != nil {
		d.flashMsg(w, r, runURL(id), d.failureFlash(r, err, msgRunRerunFailed, msgRunRerunFailedRef))
		return
	}
	d.flashMsg(w, r, fmt.Sprintf("%s?rerun=%d", runURL(id), n), message(msgRunRerunCreated, intArg(argN, int64(n))))
}

func (d *Dashboard) postRerunJob(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	runID, n, err := d.admin.RerunJob(r.Context(), id, se.Login, r.PostFormValue("keep") == "on")
	switch {
	case runID == 0 && connect.CodeOf(err) == connect.CodeNotFound:
		d.notFoundText(w, r, message(msgJobRerunNotFound, intArg(argID, id)))
		return
	case runID == 0 && err != nil:
		// 还没有找到 job 所属的 run 就出错了（例如数据库错误）
		d.adminPageError(w, r, err)
		return
	case err != nil:
		d.flashMsg(w, r, jobURL(id), d.failureFlash(r, err, msgRunRerunFailed, msgRunRerunFailedRef))
		return
	}
	d.flashMsg(w, r, fmt.Sprintf("%s?rerun=%d", runURL(runID), n), message(msgRunRerunCreated, intArg(argN, int64(n))))
}

// formKindAIDev 是注册表单中"AI 开发节点"的取值（AI 执行节点，角色为开发）。
const formKindAIDev = "ai-dev"

// runnerKindRole 把注册表单的 Runner 类型换成 Runner 的类型和角色：空和 ci 是 CI Runner，ai 是 AI 审计，ai-dev 是 AI 开发。
func runnerKindRole(v string) (kind, role string, ok bool) {
	switch v {
	case "", store.RunnerKindCI:
		return store.RunnerKindCI, "", true
	case store.RunnerKindAI:
		return store.RunnerKindAI, store.RunnerRoleReview, true
	case formKindAIDev:
		return store.RunnerKindAI, store.RunnerRoleDevelopment, true
	}
	return "", "", false
}

// postRunnerToken 生成 Runner 的一次性注册令牌，重定向到一次性展示页（POST → 重定向）：令牌不出现在地址中，
// 刷新展示页不会重新提交表单、生成新令牌。同一个表单（请求 ID）重复提交只生成一个令牌。
func (d *Dashboard) postRunnerToken(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	f := newForm()
	pool, name := strings.TrimSpace(r.PostFormValue("pool")), strings.TrimSpace(r.PostFormValue("name"))
	f.Values["pool"], f.Values["name"] = pool, name
	// Runner 类型：ci（kiln-runner，默认）、ai（kiln-orca 审计）或 ai-dev（kiln-orca 开发，PLAN #26.7、PLAN #33）
	formKind := strings.TrimSpace(r.PostFormValue("kind"))
	f.Values["kind"] = formKind
	kind, role, ok := runnerKindRole(formKind)
	if !ok {
		f.failText("kind", message(msgRunnerNewErrKind))
	}
	if pool != "" && !enrollNamePattern.MatchString(pool) {
		f.failText("pool", message(msgRunnerNewErrPool))
	}
	if name != "" && !enrollNamePattern.MatchString(name) {
		f.failText("name", message(msgRunnerNewErrName))
	}
	id := r.PostFormValue("request_id")
	if !requestIDPattern.MatchString(id) {
		id = newRequestID()
	}
	if !f.Failed() {
		err := d.reveals.ensure(id, se.Login, func() (*enrollReveal, error) {
			// 生成一旦开始就做完，不随请求取消：否则令牌已经写入数据库、请求却被取消，
			// 结果没有保存下来，用同一个表单重试会再生成一个有效的令牌。
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), enrollCreateTimeout)
			defer cancel()
			tok, exp, err := d.admin.CreateEnrollToken(cctx, pool, kind, role, 0, se.Login) // ttl 为 0：默认有效期 1 小时
			if err != nil {
				return nil, err
			}
			// 令牌已经生成：之后不再有会失败的步骤，结果一定保存（展示页按哈希查询令牌的状态）
			v := &enrollReveal{tokenHash: auth.HashEnrollToken(tok), token: tok, pool: pool, kind: kind, role: role, name: name, created: time.Now(), expires: exp}
			if v.pool == "" {
				v.pool = store.DefaultPool
			}
			return v, nil
		})
		if err == nil {
			// 第一次提交生成令牌；重复提交（包括并发的）回到同一个令牌，不再生成
			w.Header().Set("Cache-Control", "no-store")
			d.redirect(w, r, "/runners/enroll/"+id)
			return
		}
		// 表单上方的标题已经说明"没有生成令牌"：这里只给原因（内部错误是错误编号）
		f.failText(formGeneral, d.failureText(r, err))
	}
	p, err := d.newRunnerPage(ctx, f)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.renderRunnerNew(w, r, p, http.StatusUnprocessableEntity)
}

// postRunnerOp 暂停接单（drain）、恢复接单（undrain）、吊销或删除 Runner，回到原来的筛选条件并给出操作结果。
// 已吊销的 Runner 只能删除，不再暂停、恢复或重复吊销；没有吊销的不能删除。
func (d *Dashboard) postRunnerOp(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	name, op := r.PathValue("name"), r.PathValue("op")
	if op != runnerOpDrain && op != runnerOpUndrain && op != runnerOpRevoke && op != runnerOpDelete {
		d.notFoundText(w, r, message(msgRunnerOpUnknown))
		return
	}
	back := runnersURL(r.PostFormValue("state"), r.PostFormValue("pool"))
	v, err := d.runnerTarget(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		d.flashMsg(w, r, back, message(msgRunnerOpNotFound, strArg(argName, name)))
		return
	}
	if v.AI {
		back = aiRunnersPath
	}
	switch {
	case op == runnerOpDelete && !v.Revoked:
		d.flashMsg(w, r, back, message(msgRunnerOpNotRevoked, strArg(argName, name)))
		return
	case op != runnerOpDelete && v.Revoked:
		d.flashMsg(w, r, back, message(msgRunnerOpRevokedAlready, strArg(argName, name)))
		return
	}
	switch op {
	case runnerOpDrain, runnerOpUndrain:
		err = d.admin.SetRunnerDrain(ctx, name, op == runnerOpDrain, se.Login)
	case runnerOpRevoke:
		err = d.admin.RevokeRunner(ctx, name, se.Login)
	case runnerOpDelete:
		err = d.admin.DeleteRunner(ctx, name, se.Login)
	}
	if err != nil {
		d.flashMsg(w, r, back, d.failureFlash(r, err, msgRunnerOpFailed, msgRunnerOpFailedRef, strArg(argName, name)))
		return
	}
	d.flashMsg(w, r, back, runnerOpResult(v, op, d.leaseTTL(), d.longPoll()))
}

// runnerOpResult 返回操作成功后的提示；v 是操作前的 Runner，lease 是租约有效期，poll 是一轮领取的最长时间。
func runnerOpResult(v *opTarget, op string, lease, poll time.Duration) uiText {
	n, name := int64(v.Running), strArg(argName, v.Name)
	switch op {
	case runnerOpDrain:
		if n > 0 {
			return message(msgRunnerOpDrainedBusy, name, intArg(argPoll, seconds(poll)), intArg(argCount, n))
		}
		return message(msgRunnerOpDrained, name, intArg(argPoll, seconds(poll)))
	case runnerOpUndrain:
		if !v.Online {
			return message(msgRunnerOpUndrainedOff, name)
		}
		return message(msgRunnerOpUndrained, name)
	case runnerOpDelete:
		return message(msgRunnerOpDeleted, name)
	}
	switch {
	case n == 0:
		return message(msgRunnerOpRevoked, name)
	case v.AI:
		// AI 任务领取后不自动重试（PLAN #33.6）
		return message(msgRunnerOpRevokedBusyAI, name, intArg(argCount, n), intArg(argLease, seconds(lease)))
	}
	return message(msgRunnerOpRevokedBusy, name, intArg(argCount, n), intArg(argLease, seconds(lease)))
}

func (d *Dashboard) postOutboxRetry(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	if err := d.admin.RetryOutbox(r.Context(), id, se.Login); err != nil {
		msg := d.failureFlash(r, err, msgOpsRetryFailed, msgOpsRetryFailedRef, intArg(argID, id))
		if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeNotFound {
			msg = message(msgOpsRetryNotFound, intArg(argID, id))
		}
		d.flashMsg(w, r, "/settings/ops", msg)
		return
	}
	d.flashMsg(w, r, "/settings/ops", message(msgOpsRetried, intArg(argID, id)))
}

// postSync 刷新仓库目录（Settings → 仓库的"刷新仓库"）：只列出 installation 和仓库，不读取分支、tag、PR。
// 结果同时记录在页面上的"最近刷新"中。
func (d *Dashboard) postSync(w http.ResponseWriter, r *http.Request, se *session) {
	back := reposBack(r)
	res, err := d.admin.SyncInstallations(r.Context(), se.Login)
	if err != nil {
		d.flashMsg(w, r, back, d.failureFlash(r, err, msgReposSyncFailed, msgReposSyncFailedRef))
		return
	}
	d.flashMsg(w, r, back, message(msgReposSynced, intArg(argInstallations, int64(res.Installations)), intArg(argRepos, int64(res.Repos))))
}

// postReconcile 对一个仓库补对账：只请求这个仓库的分支、tag 和打开的 PR，补上漏掉的 webhook（PLAN #7.3）。
func (d *Dashboard) postReconcile(w http.ResponseWriter, r *http.Request, se *session) {
	back := reposBack(r)
	repo := r.PostFormValue("repo")
	res, err := d.admin.ReconcileRepo(r.Context(), repo, se.Login)
	if err != nil {
		d.flashMsg(w, r, back, d.reconcileFailure(r, err, repo))
		return
	}
	id := msgReposReconciled
	if res.Baseline {
		id = msgReposReconciledBase
	}
	d.flashMsg(w, r, back, message(id, strArg(argRepo, res.Repo), intArg(argBranches, int64(res.Branches)),
		intArg(argTags, int64(res.Tags)), intArg(argPulls, int64(res.Pulls)), intArg(argAdded, int64(res.Runs))))
}

// reconcileFailure 返回补对账失败后的提示：被 GitHub 限流时给出可以重试的时间（Unix 秒，前端按当前语言显示），
// 仓库不在 Kiln 中等校验错误按 settingsFailureFlash 显示对应的消息，其余带原样的说明或错误编号。
func (d *Dashboard) reconcileFailure(r *http.Request, err error, repo string) uiText {
	if rl, ok := errors.AsType[*admin.RateLimitedError](err); ok {
		return message(msgReposReconcileLimited, strArg(argRepo, repo), intArg(argAt, rl.Until.Unix()))
	}
	return d.settingsFailureFlash(r, err, msgReposReconcileFailed, msgReposReconcileFailedRef, strArg(argRepo, repo))
}

// reposBack 返回仓库页的地址，保留表单中带回的搜索词和页码。
func reposBack(r *http.Request) string {
	page, _ := strconv.Atoi(r.PostFormValue("page"))
	return reposURL(strings.TrimSpace(r.PostFormValue("q")), "", page)
}
