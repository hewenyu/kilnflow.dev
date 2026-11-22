package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

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
		f.fail("kind", "Runner 类型只能是 CI Runner、AI 审计节点 或 AI 开发节点")
	}
	if pool != "" && !enrollNamePattern.MatchString(pool) {
		f.fail("pool", "pool 只能包含字母、数字、点、下划线和连字符，以字母或数字开头，最长 64 个字符")
	}
	if name != "" && !enrollNamePattern.MatchString(name) {
		f.fail("name", "名称只能包含字母、数字、点、下划线和连字符，以字母或数字开头，最长 64 个字符")
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
		f.fail(formGeneral, "生成注册令牌失败："+d.failure(r, err))
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
		d.notFound(w, r, "没有这个 Runner 操作，没有执行任何操作。")
		return
	}
	back := runnersURL(r.PostFormValue("state"), r.PostFormValue("pool"))
	v, err := d.runnerTarget(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		d.flashBack(w, r, back, fmt.Sprintf("没有名为 %s 的 Runner，没有执行任何操作。", name))
		return
	}
	if v.AI {
		back = aiRunnersPath
	}
	switch {
	case op == runnerOpDelete && !v.Revoked:
		d.flashBack(w, r, back, name+" 没有吊销，没有执行任何操作。先吊销再删除。")
		return
	case op != runnerOpDelete && v.Revoked:
		d.flashBack(w, r, back, name+" 已经被吊销，没有执行任何操作。要继续使用这台机器，请重新注册。")
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
		d.flashBack(w, r, back, fmt.Sprintf("操作 %s 失败：%s", name, d.failure(r, err)))
		return
	}
	d.flashBack(w, r, back, runnerOpResult(v, op, d.leaseTTL(), d.longPoll()))
}

// runnerOpResult 返回操作成功后的说明；v 是操作前的 Runner，lease 是租约有效期，poll 是一轮领取的最长时间。
func runnerOpResult(v *opTarget, op string, lease, poll time.Duration) string {
	n := v.Running
	switch op {
	case runnerOpDrain:
		msg := fmt.Sprintf("已暂停 %s 接收新任务（它正在进行的一轮领取结束后生效，最长约 %s）。", v.Name, humanDuration(poll))
		if n > 0 {
			return msg + fmt.Sprintf("正在执行的 %d 个任务会继续执行到结束；需要时点“恢复接单”。", n)
		}
		return msg + "它现在没有正在执行的任务；需要时点“恢复接单”。"
	case runnerOpUndrain:
		if !v.Online {
			return v.Name + " 已恢复接单，但它目前离线，重新连接后才会领取任务。"
		}
		return v.Name + " 已恢复接单，有空闲时会领取新任务。"
	}
	if op == runnerOpDelete {
		return fmt.Sprintf("已删除 %s：不再显示在列表中，名称可以重新注册；它执行过的尝试仍显示这个名称。", v.Name)
	}
	msg := fmt.Sprintf("已吊销 %s，它之后的请求都会被拒绝。", v.Name)
	if n > 0 {
		if v.AI {
			// AI 任务领取后不自动重试（PLAN #33.6）
			return msg + fmt.Sprintf("它正在执行的 %d 个 AI 任务会被中断，约 %s后判为 Runner 失联并结束。要恢复这台机器，请为它重新注册。", n, humanDuration(lease))
		}
		msg += fmt.Sprintf("它正在执行的 %d 个任务会被中断，约 %s后判为 Runner 失联，按基础设施错误处理（多数会重新排队）。", n, humanDuration(lease))
	}
	return msg + "要恢复这台机器，请为它重新注册。"
}

func (d *Dashboard) postOutboxRetry(w http.ResponseWriter, r *http.Request, se *session) {
	if err := d.admin.RetryOutbox(r.Context(), pathID(r), se.Login); err != nil {
		d.flashBack(w, r, "/settings/ops", d.failure(r, err))
		return
	}
	d.flashBack(w, r, "/settings/ops", "已重新排队")
}

// postSync 刷新仓库目录（Settings → 仓库的"刷新仓库"）：只列出 installation 和仓库，不读取分支、tag、PR。
// 结果同时记录在页面上的"最近刷新"中。
func (d *Dashboard) postSync(w http.ResponseWriter, r *http.Request, se *session) {
	back := reposBack(r)
	res, err := d.admin.SyncInstallations(r.Context(), se.Login)
	if err != nil {
		d.flashBack(w, r, back, "刷新仓库失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, back, fmt.Sprintf("已刷新仓库：%d 个 installation，%d 个启用中的仓库", res.Installations, res.Repos))
}

// postReconcile 对一个仓库补对账：只请求这个仓库的分支、tag 和打开的 PR，补上漏掉的 webhook（PLAN #7.3）。
func (d *Dashboard) postReconcile(w http.ResponseWriter, r *http.Request, se *session) {
	back := reposBack(r)
	repo := r.PostFormValue("repo")
	res, err := d.admin.ReconcileRepo(r.Context(), repo, se.Login)
	if err != nil {
		d.flashBack(w, r, back, repo+" 补对账失败："+d.failure(r, err))
		return
	}
	msg := fmt.Sprintf("%s 补对账完成：%d 个分支、%d 个 tag、%d 个打开的 PR，补上 %d 个 run", res.Repo, res.Branches, res.Tags, res.Pulls, res.Runs)
	if res.Baseline {
		msg += "。这是第一次对账，之前没有记录过的 ref 只记录为基线，不构建"
	}
	d.flashBack(w, r, back, msg)
}

// reposBack 返回仓库页的地址，保留表单中带回的搜索词和页码。
func reposBack(r *http.Request) string {
	page, _ := strconv.Atoi(r.PostFormValue("page"))
	return reposURL(strings.TrimSpace(r.PostFormValue("q")), "", page)
}
