package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 设置中与人有关的三个分区（只有管理员能访问）：
//
//   - 登录与身份 /settings/account：管理员绑定 GitHub 账号 → 用 GitHub 登录 → 关闭 admin token 登录（PLAN #16.5）。
//   - 成员 /settings/members：谁能登录、是什么角色、最近一次登录；名单来自 Server 配置，这里只读。
//   - 活动 /settings/activity：审计日志，可以按操作者筛选，多人使用时查看谁做了什么。

// 设置分区：登录与身份、成员、活动。
const (
	sectionAccount  = "account"
	sectionMembers  = "members"
	sectionActivity = "activity"
)

// 设置分区的地址。
const (
	accountURL  = "/settings/account"
	membersURL  = "/settings/members"
	activityURL = "/settings/activity"
)

// activityPageSize 是活动分区每页的记录数。
const activityPageSize = 100

// accountPage 是 /settings/account 的数据。
type accountPage struct {
	Section string
	// Binding 是管理员绑定的 GitHub 账号；还没有绑定时为 nil。
	Binding *store.AdminBinding
	// OAuth 表示配置了 GitHub 登录（绑定需要它）。
	OAuth bool
	// SignedInAsBound 表示当前会话是用绑定的 GitHub 账号通过 GitHub 登录的：只有这样才能关闭 token 登录，
	// 证明 GitHub 登录确实可用，避免把自己锁在外面。
	SignedInAsBound bool
	// GitHubLogin 是重新用 GitHub 登录后回到这个页面的地址。
	GitHubLogin string
}

// Step 返回三步中当前要做的一步（1 绑定、2 用 GitHub 登录、3 关闭 token 登录），全部完成时为 4。
func (p *accountPage) Step() int {
	switch {
	case p.Binding == nil:
		return 1
	case !p.SignedInAsBound:
		return 2
	case p.Binding.TokenLogin:
		return 3
	}
	return 4
}

func (d *Dashboard) pageAccount(w http.ResponseWriter, r *http.Request, se *session) {
	bind, err := d.adminBinding(r.Context())
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取登录与身份：%w", err))
		return
	}
	d.renderPage(w, r, "settings_account", message(msgSetTitleAccount), &accountPage{
		Section: sectionAccount, Binding: bind, OAuth: d.oauthEnabled(),
		SignedInAsBound: se.Via == viaGitHub && se.Bound,
		GitHubLogin:     githubLoginURL(accountURL),
	}, http.StatusOK)
}

// postBindStart 开始绑定：跳转到 GitHub 授权页，state 关联当前会话。授权完成后在 finishBind 中绑定。
func (d *Dashboard) postBindStart(w http.ResponseWriter, r *http.Request, se *session) {
	if !d.oauthEnabled() {
		d.flashMsg(w, r, accountURL, message(msgAccountBindNoOAuth))
		return
	}
	d.redirect(w, r, d.authorizeURL(d.oauthState(bindPurposePrefix+d.bindFingerprint(se), accountURL)))
}

// finishBind 完成绑定：只有发起绑定的那个管理员会话可以完成；绑定后当前会话换成这个 GitHub 账号的 GitHub 登录。
func (d *Dashboard) finishBind(w http.ResponseWriter, r *http.Request, fingerprint, code string) {
	se := d.getSession(r)
	if se == nil || !se.Admin() || fingerprint != d.bindFingerprint(se) {
		d.showError(w, r, &errorPage{
			Status: http.StatusBadRequest, Heading: message(msgErrBindBadHeading), Reason: message(msgErrBindBadReason),
			Links: []errorLink{{Label: message(msgErrLinkAccount), URL: accountURL, Primary: true}},
		})
		return
	}
	login, id, ok := d.exchangeOrFail(w, r, code, accountURL)
	if !ok {
		return
	}
	if err := d.admin.BindAdmin(r.Context(), id, login, se.Login); err != nil {
		d.serverError(w, r, err)
		return
	}
	d.setSession(w, session{Login: login, ID: id, Via: viaGitHub})
	d.flashMsg(w, r, accountURL, message(msgAccountBound, strArg(argLogin, login)))
}

// postUnbind 解除绑定，同时恢复 admin token 登录。当前会话是绑定账号且不在 KILN_ADMINS 中时，随即失去管理员权限。
func (d *Dashboard) postUnbind(w http.ResponseWriter, r *http.Request, se *session) {
	if r.PostFormValue("confirm") != "1" {
		d.flashMsg(w, r, accountURL, message(msgAccountUnbindUnconfirmed))
		return
	}
	if err := d.admin.UnbindAdmin(r.Context(), se.Login); err != nil {
		d.preconditionOrError(w, r, accountURL, msgAccountUnbindFailed, err)
		return
	}
	d.flashMsg(w, r, accountURL, message(msgAccountUnbound))
}

// postTokenLogin 开启或关闭 admin token 登录。关闭要求当前会话是用绑定的 GitHub 账号通过 GitHub 登录的。
func (d *Dashboard) postTokenLogin(w http.ResponseWriter, r *http.Request, se *session) {
	enable := r.PostFormValue("enabled") == "1"
	if !enable && (se.Via != viaGitHub || !se.Bound) {
		d.flashMsg(w, r, accountURL, message(msgAccountTokenNeedGitHub))
		return
	}
	if err := d.admin.SetTokenLogin(r.Context(), enable, se.Login); err != nil {
		d.preconditionOrError(w, r, accountURL, msgAccountTokenFailed, err)
		return
	}
	msg := message(msgAccountTokenDisabled)
	if enable {
		msg = message(msgAccountTokenEnabled)
	}
	d.flashMsg(w, r, accountURL, msg)
}

// member 是成员分区中的一个人。
type member struct {
	// Login 是 GitHub 登录名；名单中写的是数字 ID 时为 ID。
	Login string
	Role  string
	// Bound 表示这是管理员绑定的账号。
	Bound bool
	// Source 说明权限来自哪里：绑定（消息），或者名单的环境变量名（KILN_ADMINS、KILN_USERS，原样显示）。
	Source uiText
	// LastLogin 是最近一次登录 Dashboard 的时间（审计日志），从未登录时为零值。
	LastLogin time.Time
	// Me 表示是当前登录的用户。
	Me bool
}

type membersPage struct {
	Section string
	Members []member
	// Admins、Users 是管理员和成员的人数。
	Admins, Users int
	// Bound 表示管理员已经绑定 GitHub 账号。
	Bound bool
}

func (d *Dashboard) pageMembers(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	bind, err := d.adminBinding(ctx)
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取成员：%w", err))
		return
	}
	last, err := lastLogins(ctx, d.db)
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取成员：%w", err))
		return
	}
	p := &membersPage{Section: sectionMembers, Bound: bind != nil}
	seen := map[string]bool{}
	add := func(login, role string, source uiText, bound bool) {
		key := strings.ToLower(login)
		if seen[key] {
			return
		}
		seen[key] = true
		p.Members = append(p.Members, member{
			Login: login, Role: role, Bound: bound, Source: source, LastLogin: last[key],
			Me: strings.EqualFold(login, se.Login),
		})
		if role == roleAdmin {
			p.Admins++
		} else {
			p.Users++
		}
	}
	if bind != nil {
		add(bind.GitHubLogin, roleAdmin, message(msgMemberSourceBound), true)
	}
	for _, l := range sortedKeys(d.cfg.Admins) {
		add(l, roleAdmin, rawText("KILN_ADMINS"), false)
	}
	for _, l := range sortedKeys(d.cfg.Users) {
		add(l, roleUser, rawText("KILN_USERS"), false)
	}
	d.renderPage(w, r, "settings_members", message(msgSetTitleMembers), p, http.StatusOK)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, ok := range m {
		if ok && k != "" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// lastLogins 返回每个登录名（小写）最近一次登录 Dashboard 的时间。
func lastLogins(ctx context.Context, q store.Querier) (map[string]time.Time, error) {
	rows, err := q.Query(ctx, `SELECT lower(actor), max(created_at) FROM audit_log WHERE action='login' GROUP BY lower(actor)`)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	var login string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&login, &at}, func() error {
		out[login] = at
		return nil
	})
	return out, err
}

// activityEntry 是活动分区中的一条审计日志。
type activityEntry struct {
	ID     int64
	Actor  string
	Action string
	Detail map[string]any
	At     time.Time
}

// activityCols 是 scanActivity 读取的 audit_log 列。
const activityCols = `id, actor, action, detail, created_at`

func scanActivity(row pgx.CollectableRow) (activityEntry, error) {
	var e activityEntry
	err := row.Scan(&e.ID, &e.Actor, &e.Action, &e.Detail, &e.At)
	return e, err
}

type activityPage struct {
	Section string
	// Actor 是按操作者筛选的条件（为空表示全部）。
	Actor   string
	Actors  []string
	Entries []activityEntry
	// OlderURL 是下一页（更早）的地址，没有更早的记录时为空；LatestURL 是回到最新一页的地址（在第一页时为空）。
	OlderURL, LatestURL string
}

func (d *Dashboard) pageActivity(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	q := r.URL.Query()
	p := &activityPage{Section: sectionActivity, Actor: strings.TrimSpace(q.Get("actor"))}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	rows, err := d.db.Query(ctx, `SELECT `+activityCols+` FROM audit_log
		WHERE ($1 = '' OR lower(actor) = lower($1)) AND ($2 = 0 OR id < $2) ORDER BY id DESC LIMIT $3`,
		p.Actor, before, activityPageSize+1)
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取活动：%w", err))
		return
	}
	p.Entries, err = pgx.CollectRows(rows, scanActivity)
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取活动：%w", err))
		return
	}
	actorQ := ""
	if p.Actor != "" {
		actorQ = "actor=" + url.QueryEscape(p.Actor) + "&"
	}
	if len(p.Entries) > activityPageSize {
		p.Entries = p.Entries[:activityPageSize]
		p.OlderURL = activityURL + "?" + actorQ + "before=" + strconv.FormatInt(p.Entries[len(p.Entries)-1].ID, 10)
	}
	if before != 0 {
		p.LatestURL = activityURL + "?" + strings.TrimSuffix(actorQ, "&")
		p.LatestURL = strings.TrimSuffix(p.LatestURL, "?")
	}
	rows, err = d.db.Query(ctx, `SELECT DISTINCT actor FROM audit_log ORDER BY actor`)
	if err != nil {
		d.serverError(w, r, fmt.Errorf("读取活动：%w", err))
		return
	}
	if p.Actors, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		d.serverError(w, r, fmt.Errorf("读取活动：%w", err))
		return
	}
	d.renderPage(w, r, "settings_activity", message(msgSetTitleActivity), p, http.StatusOK)
}

// preconditionOrError 把操作的前提不满足（FailedPrecondition，例如还没有绑定）显示为回到 to 的提示，其他错误显示错误页：
// 已知的前提错误（admin.ErrNotBound、admin.ErrAlreadyUnbound）是对应的消息，其余是 failed（参数是服务给出的说明）。
func (d *Dashboard) preconditionOrError(w http.ResponseWriter, r *http.Request, to string, failed msgID, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeFailedPrecondition {
		msg := settingsErrText(err)
		if msg.IsZero() {
			msg = message(failed, strArg(argReason, flashArgText(ce.Message())))
		}
		d.flashMsg(w, r, to, msg)
		return
	}
	d.serverError(w, r, err)
}

// Link 返回活动相关页面的地址（run、AI 请求、Runner），没有时为空。
func (e activityEntry) Link() string {
	if v, ok := e.Detail["run_id"].(float64); ok && v > 0 {
		return runURL(int64(v))
	}
	if strings.HasPrefix(e.Action, "ai_") {
		// 重跑指向新的请求
		for _, k := range []string{"rerun", "ai_request_id"} {
			if v, ok := e.Detail[k].(float64); ok && v > 0 {
				return aiRequestURL(int64(v))
			}
		}
	}
	if strings.HasPrefix(e.Action, "runner.") && e.Action != "runner.token_create" {
		return "/runners"
	}
	return ""
}
