package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
)

// 统一的错误页：404、403、400（登录链接无效或过期）、502（GitHub 登录失败）和 500 都使用页面外壳，
// 给出可读的原因和恢复入口（返回运行列表、重新登录、重试），保持正确的 HTTP 状态码；不显示内部错误，
// 内部错误只写日志，页面上给出与日志对应的错误编号。页面脚本调用的接口（日志片段）的错误正文是消息的 JSON（apiError）。
// 标题、原因和入口的文字是消息（见 text.go），前端按当前语言显示；调用方给出的原因（例如 admin.Service 的 NotFound 说明）原样显示。

// errorPage 是错误页的内容。
type errorPage struct {
	Status int
	// Heading 是标题，Reason 是可读的原因和下一步。
	Heading, Reason uiText
	// Ref 是错误编号（与服务器日志中的 ref 对应），只有内部错误才有。
	Ref   string
	Links []errorLink
}

// errorLink 是错误页上的一个恢复入口；Primary 为主要操作。
type errorLink struct {
	Label   uiText
	URL     string
	Primary bool
}

// listLink 返回"返回运行列表"。
func listLink() errorLink { return errorLink{Label: message(msgErrLinkRuns), URL: "/runs"} }

// showError 渲染错误页。错误页不缓存。
func (d *Dashboard) showError(w http.ResponseWriter, r *http.Request, p *errorPage) {
	w.Header().Set("Cache-Control", "no-store")
	d.renderPage(w, r, "error", p.Heading, p, p.Status)
}

// newErrorRef 生成错误编号。
func newErrorRef() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// serverError 记录内部错误并返回 500 错误页：页面上只有错误编号，不显示错误内容；GET 请求可以安全重试。
func (d *Dashboard) serverError(w http.ResponseWriter, r *http.Request, err error) {
	ref := newErrorRef()
	d.log.Error("页面出错", "path", r.URL.Path, "ref", ref, "err", err)
	p := &errorPage{
		Status: http.StatusInternalServerError, Heading: message(msgErrServerHeading), Ref: ref,
		Reason: message(msgErrServerReasonGet),
	}
	if r.Method == http.MethodGet {
		p.Links = append(p.Links, errorLink{Label: message(msgErrLinkRetry), URL: r.URL.RequestURI(), Primary: true})
	} else {
		p.Reason = message(msgErrServerReasonPost)
		if back := refererPath(r); back != "" {
			p.Links = append(p.Links, errorLink{Label: message(msgErrLinkBack), URL: back, Primary: true})
		}
	}
	p.Links = append(p.Links, listLink())
	d.showError(w, r, p)
}

// notFound 返回 404 错误页；reason 是原样显示的说明，为空时使用通用的说明。未登录时提供登录入口（登录后回到这个地址）。
func (d *Dashboard) notFound(w http.ResponseWriter, r *http.Request, reason string) {
	d.notFoundText(w, r, rawText(reason))
}

// notFoundText 与 notFound 相同，reason 是消息或原样的文字（零值时使用通用的说明）。
func (d *Dashboard) notFoundText(w http.ResponseWriter, r *http.Request, reason uiText) {
	if reason.IsZero() {
		reason = message(msgErrNotFoundReason)
	}
	p := &errorPage{Status: http.StatusNotFound, Heading: message(msgErrNotFoundHeading), Reason: reason}
	if d.getSession(r) == nil {
		p.Links = append(p.Links, errorLink{Label: message(msgErrLinkLogin), URL: loginURL(returnTo(r), ""), Primary: true})
	}
	p.Links = append(p.Links, listLink())
	d.showError(w, r, p)
}

// badRequest 返回 400 错误页（地址中的参数无效）；reason 原样显示。
func (d *Dashboard) badRequest(w http.ResponseWriter, r *http.Request, reason string) {
	d.badRequestText(w, r, rawText(reason))
}

// badRequestText 与 badRequest 相同，reason 是消息或原样的文字。
func (d *Dashboard) badRequestText(w http.ResponseWriter, r *http.Request, reason uiText) {
	d.showError(w, r, &errorPage{
		Status: http.StatusBadRequest, Heading: message(msgErrBadRequestHeading), Reason: reason, Links: []errorLink{listLink()},
	})
}

// forbidden 返回 403 错误页：需要管理员权限。GET 请求提供"用管理员身份登录"（登录后回到这个地址）。
func (d *Dashboard) forbidden(w http.ResponseWriter, r *http.Request, se *session) {
	p := &errorPage{
		Status: http.StatusForbidden, Heading: message(msgErrForbiddenHeading),
		Reason: message(msgErrForbiddenReason, strArg(argLogin, se.Login)),
		Links:  []errorLink{{Label: message(msgErrLinkRuns), URL: "/runs", Primary: true}},
	}
	if next := returnTo(r); next != "" {
		p.Links = append(p.Links, errorLink{Label: message(msgErrLinkLoginAsAdmin), URL: loginURL(next, "")})
	}
	d.showError(w, r, p)
}

// csrfFailed 返回 403 错误页：表单的 CSRF 校验失败（页面打开太久、重新登录过或来自其他网站），操作没有执行。
func (d *Dashboard) csrfFailed(w http.ResponseWriter, r *http.Request) {
	p := &errorPage{
		Status: http.StatusForbidden, Heading: message(msgErrCSRFHeading), Reason: message(msgErrCSRFReason),
	}
	if back := refererPath(r); back != "" {
		p.Links = append(p.Links, errorLink{Label: message(msgErrLinkBack), URL: back, Primary: true})
	}
	p.Links = append(p.Links, listLink())
	d.showError(w, r, p)
}

// adminPageError 把 admin.Service 的错误转换为错误页：NotFound 为 404（附服务给出的说明），其他为 500。
// 页面脚本调用的接口用 logError（消息的 JSON）。
func (d *Dashboard) adminPageError(w http.ResponseWriter, r *http.Request, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeNotFound {
		d.notFound(w, r, ce.Message())
		return
	}
	d.serverError(w, r, err)
}

// logSubject 是日志接口读取的对象：CI 的 job 或 AI 任务（找不到时的说明不同）。
type logSubject int

const (
	logOfJob logSubject = iota + 1
	logOfAITask
)

// notFound 返回找不到对象（attempt 为 0）或指定的 attempt 时的消息。
func (s logSubject) notFound(id, attempt int64) uiText {
	switch {
	case s == logOfAITask && attempt != 0:
		return message(msgLogTaskAttemptNotFound, intArg(argID, id), intArg(argAttempt, attempt))
	case s == logOfAITask:
		return message(msgLogTaskNotFound, intArg(argID, id))
	case attempt != 0:
		return message(msgLogJobAttemptNotFound, intArg(argID, id), intArg(argAttempt, attempt))
	}
	return message(msgLogJobNotFound, intArg(argID, id))
}

// apiError 写出页面脚本调用的接口（日志片段）的错误：状态码 status，正文是消息的 JSON（与页面数据中的消息相同，前端按当前语言显示）。
func apiError(w http.ResponseWriter, status int, t uiText) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(t)
}

// logError 把读取日志时 admin.Service 的错误转换为接口的响应：NotFound 为 404（id 是 job 或 AI 任务，attempt 为 0 表示当前尝试），
// 其他为 500，只给错误编号（不显示内部错误）。不按错误原文判断：找不到的是对象本身还是指定的 attempt，由请求的参数决定。
// 浏览器直接打开的地址用 adminPageError（错误页）。
func (d *Dashboard) logError(w http.ResponseWriter, r *http.Request, subj logSubject, id, attempt int64, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeNotFound {
		apiError(w, http.StatusNotFound, subj.notFound(id, attempt))
		return
	}
	ref := newErrorRef()
	d.log.Error("接口出错", "path", r.URL.Path, "ref", ref, "err", err)
	apiError(w, http.StatusInternalServerError, message(msgLogServer, strArg(argRef, ref)))
}

// connectMessage 返回错误的可读说明：管理操作返回的 *connect.Error 去掉状态码前缀（例如 failed_precondition: ）。
func connectMessage(err error) string {
	if ce, ok := errors.AsType[*connect.Error](err); ok {
		return ce.Message()
	}
	return err.Error()
}

// failure 返回表单操作失败时显示在提示或表单上的说明（管理操作的错误，例如 admin.Service 返回的）：
//   - 内部错误（见 internalError：数据库、网络、磁盘、超时，或 Internal、Unknown 的 *connect.Error）与 500 错误页相同：
//     页面上只有通用的说明和错误编号，详细信息只写日志（原来数据库错误的原文会显示在页面上，日志中没有记录）；
//   - 其余是给用户看的说明（校验失败、前提不满足、不存在等），显示原文（去掉 connect 的状态码前缀）；
//     不是 *connect.Error 的（admin.Service 的校验错误多是普通的 error）也写一条日志，便于排查。
func (d *Dashboard) failure(r *http.Request, err error) string {
	if ref := d.failureRef(r, err); ref != "" {
		return "服务器出错了（错误编号 " + ref + "），操作可能没有完成。请确认结果后再决定是否重试；问题持续出现时，请把错误编号告诉管理员。"
	}
	return connectMessage(err)
}

// failureText 与 failure 相同，返回页面数据中的文字：内部错误是带错误编号的消息（按当前语言显示），其余是原样的说明。
func (d *Dashboard) failureText(r *http.Request, err error) uiText {
	if ref := d.failureRef(r, err); ref != "" {
		return message(msgActionInternal, strArg(argRef, ref))
	}
	return rawText(connectMessage(err))
}

// failureFlash 返回操作失败后跳转时的提示：内部错误用 withRef（参数是错误编号），其余用 withReason（参数是原样的说明，过长时截断）。
func (d *Dashboard) failureFlash(r *http.Request, err error, withReason, withRef msgID) uiText {
	if ref := d.failureRef(r, err); ref != "" {
		return message(withRef, strArg(argRef, ref))
	}
	return message(withReason, strArg(argReason, flashArgText(connectMessage(err))))
}

// failureRef 记录操作失败的日志：内部错误（见 internalError）生成并返回错误编号，详细信息只写日志；
// 其余返回空（不是 *connect.Error 的也写一条日志，便于排查）。
func (d *Dashboard) failureRef(r *http.Request, err error) string {
	if internalError(err) {
		ref := newErrorRef()
		d.log.Error("操作出错", "method", r.Method, "path", r.URL.Path, "ref", ref, "err", err)
		return ref
	}
	if _, ok := errors.AsType[*connect.Error](err); !ok {
		d.log.Warn("操作没有完成", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	return ""
}

// internalError 判断错误是否是不能显示给用户的内部错误：Internal、Unknown、DataLoss 的 *connect.Error，
// 或者 admin.IsInternal 认定的内部错误（数据库、网络、文件系统、超时和取消），以及行不存在（pgx.ErrNoRows）。
// 这些错误的原文可能含有 SQL、表名、数据库或内网地址、文件路径。
func internalError(err error) bool {
	if ce, ok := errors.AsType[*connect.Error](err); ok {
		switch ce.Code() {
		case connect.CodeInternal, connect.CodeUnknown, connect.CodeDataLoss:
			return true
		}
	}
	return admin.IsInternal(err) || errors.Is(err, pgx.ErrNoRows)
}

// loginLink 返回登录入口（文字为 label），登录后回到 next（不安全时省略）。
func loginLink(label msgID, next string, primary bool) errorLink {
	return errorLink{Label: message(label), URL: loginURL(next, ""), Primary: primary}
}

// githubLoginURL 返回 GitHub 登录的入口地址，登录后回到 next。
func githubLoginURL(next string) string {
	if next == "" || safeNext(next) != next {
		return "/auth/github"
	}
	return "/auth/github?next=" + url.QueryEscape(next)
}
