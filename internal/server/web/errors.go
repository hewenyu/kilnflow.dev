package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
)

// 统一的错误页：404、403、400（登录链接无效或过期）、502（GitHub 登录失败）和 500 都使用页面外壳，
// 给出可读的原因和恢复入口（返回运行列表、重新登录、重试），保持正确的 HTTP 状态码；不显示内部错误，
// 内部错误只写日志，页面上给出与日志对应的错误编号。页面脚本调用的接口（日志片段）仍返回纯文本。

// errorPage 是错误页的内容。
type errorPage struct {
	Status int
	// Heading 是标题，Reason 是可读的原因和下一步。
	Heading, Reason string
	// Ref 是错误编号（与服务器日志中的 ref 对应），只有内部错误才有。
	Ref   string
	Links []errorLink
}

// errorLink 是错误页上的一个恢复入口；Primary 为主要操作。
type errorLink struct {
	Label, URL string
	Primary    bool
}

// listLink 是"返回运行列表"。
var listLink = errorLink{Label: "返回运行列表", URL: "/runs"}

// showError 渲染错误页。错误页不缓存。
func (d *Dashboard) showError(w http.ResponseWriter, r *http.Request, p *errorPage) {
	w.Header().Set("Cache-Control", "no-store")
	d.renderStatus(w, r, "error", p.Heading, p, p.Status)
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
		Status: http.StatusInternalServerError, Heading: "服务器出错了", Ref: ref,
		Reason: "处理这个请求时出现了内部错误，详细信息已记录在服务器日志中。可以稍后重试；问题持续出现时，请把下面的错误编号告诉管理员。",
	}
	if r.Method == http.MethodGet {
		p.Links = append(p.Links, errorLink{Label: "重试", URL: r.URL.RequestURI(), Primary: true})
	} else {
		p.Reason = "处理这个操作时出现了内部错误，操作可能没有完成，详细信息已记录在服务器日志中。请回到原来的页面确认结果后再决定是否重试；问题持续出现时，请把下面的错误编号告诉管理员。"
		if back := refererPath(r); back != "" {
			p.Links = append(p.Links, errorLink{Label: "回到原来的页面", URL: back, Primary: true})
		}
	}
	p.Links = append(p.Links, listLink)
	d.showError(w, r, p)
}

// notFound 返回 404 错误页；reason 为空时使用通用的说明。未登录时提供登录入口（登录后回到这个地址）。
func (d *Dashboard) notFound(w http.ResponseWriter, r *http.Request, reason string) {
	if reason == "" {
		reason = "地址可能写错了，或者要找的内容已经不存在。"
	}
	p := &errorPage{Status: http.StatusNotFound, Heading: "找不到这个页面", Reason: reason}
	if d.getSession(r) == nil {
		p.Links = append(p.Links, errorLink{Label: "登录", URL: loginURL(returnTo(r), ""), Primary: true})
	}
	p.Links = append(p.Links, listLink)
	d.showError(w, r, p)
}

// badRequest 返回 400 错误页（地址中的参数无效）。
func (d *Dashboard) badRequest(w http.ResponseWriter, r *http.Request, reason string) {
	d.showError(w, r, &errorPage{Status: http.StatusBadRequest, Heading: "地址中的参数无效", Reason: reason, Links: []errorLink{listLink}})
}

// forbidden 返回 403 错误页：需要管理员权限。GET 请求提供"用管理员身份登录"（登录后回到这个地址）。
func (d *Dashboard) forbidden(w http.ResponseWriter, r *http.Request, se *session) {
	p := &errorPage{
		Status: http.StatusForbidden, Heading: "需要管理员权限",
		Reason: fmt.Sprintf("这个页面或操作只有管理员可以使用。当前登录的 %s 是成员，没有执行任何操作。需要时请联系管理员，或用管理员身份重新登录。", se.Login),
		Links:  []errorLink{{Label: "返回运行列表", URL: "/runs", Primary: true}},
	}
	if next := returnTo(r); next != "" {
		p.Links = append(p.Links, errorLink{Label: "用管理员身份登录", URL: loginURL(next, "")})
	}
	d.showError(w, r, p)
}

// csrfFailed 返回 403 错误页：表单的 CSRF 校验失败（页面打开太久、重新登录过或来自其他网站），操作没有执行。
func (d *Dashboard) csrfFailed(w http.ResponseWriter, r *http.Request) {
	p := &errorPage{
		Status: http.StatusForbidden, Heading: "页面已过期，操作没有执行",
		Reason: "表单的安全校验（CSRF）没有通过，通常是因为页面打开太久、期间重新登录过，或者表单来自其他网站。请回到原来的页面刷新后再操作。",
	}
	if back := refererPath(r); back != "" {
		p.Links = append(p.Links, errorLink{Label: "回到原来的页面", URL: back, Primary: true})
	}
	p.Links = append(p.Links, listLink)
	d.showError(w, r, p)
}

// adminPageError 把 admin.Service 的错误转换为错误页：NotFound 为 404（附服务给出的说明），其他为 500。
// 页面脚本调用的接口用 adminError（纯文本）。
func (d *Dashboard) adminPageError(w http.ResponseWriter, r *http.Request, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeNotFound {
		d.notFound(w, r, ce.Message())
		return
	}
	d.serverError(w, r, err)
}

// adminError 把 admin.Service 的错误转换为页面脚本调用的接口的响应（纯文本）：NotFound 为 404，其他为 500（不显示内部错误）。
// 浏览器直接打开的地址用 adminPageError（错误页）。
func (d *Dashboard) adminError(w http.ResponseWriter, r *http.Request, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeNotFound {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, ce.Message(), http.StatusNotFound)
		return
	}
	ref := newErrorRef()
	d.log.Error("接口出错", "path", r.URL.Path, "ref", ref, "err", err)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "服务器出错了（错误编号 "+ref+"），请稍后重试", http.StatusInternalServerError)
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
	if internalError(err) {
		ref := newErrorRef()
		d.log.Error("操作出错", "method", r.Method, "path", r.URL.Path, "ref", ref, "err", err)
		return "服务器出错了（错误编号 " + ref + "），操作可能没有完成。请确认结果后再决定是否重试；问题持续出现时，请把错误编号告诉管理员。"
	}
	if _, ok := errors.AsType[*connect.Error](err); !ok {
		d.log.Warn("操作没有完成", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	return connectMessage(err)
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

// loginLink 返回"重新登录"入口，登录后回到 next（不安全时省略）。
func loginLink(label, next string, primary bool) errorLink {
	return errorLink{Label: label, URL: loginURL(next, ""), Primary: primary}
}

// githubLoginURL 返回 GitHub 登录的入口地址，登录后回到 next。
func githubLoginURL(next string) string {
	if next == "" || safeNext(next) != next {
		return "/auth/github"
	}
	return "/auth/github?next=" + url.QueryEscape(next)
}
