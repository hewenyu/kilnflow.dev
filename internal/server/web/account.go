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
	d.render(w, r, "settings_account", "登录与身份", &accountPage{
		Section: sectionAccount, Binding: bind, OAuth: d.oauthEnabled(),
		SignedInAsBound: se.Via == viaGitHub && se.Bound,
		GitHubLogin:     githubLoginURL(accountURL),
	})
}

// postBindStart 开始绑定：跳转到 GitHub 授权页，state 关联当前会话。授权完成后在 finishBind 中绑定。
func (d *Dashboard) postBindStart(w http.ResponseWriter, r *http.Request, se *session) {
	if !d.oauthEnabled() {
		d.flashBack(w, r, accountURL, "没有绑定：这个 Server 没有配置 GitHub 登录（KILN_GITHUB_CLIENT_ID、KILN_GITHUB_CLIENT_SECRET），先在 Server 配置中设置。")
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
		d.flashBack(w, r, accountURL, "没有解除绑定：请勾选确认。")
		return
	}
	if err := d.admin.UnbindAdmin(r.Context(), se.Login); err != nil {
		d.preconditionOrError(w, r, accountURL, "没有解除绑定", err)
		return
	}
	d.flashMsg(w, r, accountURL, message(msgAccountUnbound))
}

// postTokenLogin 开启或关闭 admin token 登录。关闭要求当前会话是用绑定的 GitHub 账号通过 GitHub 登录的。
func (d *Dashboard) postTokenLogin(w http.ResponseWriter, r *http.Request, se *session) {
	enable := r.PostFormValue("enabled") == "1"
	if !enable && (se.Via != viaGitHub || !se.Bound) {
		d.flashBack(w, r, accountURL, "没有关闭：先用绑定的 GitHub 账号通过 GitHub 登录，确认 GitHub 登录可用后再关闭 admin token 登录。")
		return
	}
	if err := d.admin.SetTokenLogin(r.Context(), enable, se.Login); err != nil {
		d.preconditionOrError(w, r, accountURL, "没有修改", err)
		return
	}
	msg := "已关闭 admin token 登录：Dashboard 只能用 GitHub 登录，已用 admin token 登录的会话随即失效。CLI 不受影响。"
	if enable {
		msg = "已开启 admin token 登录。"
	}
	d.flashBack(w, r, accountURL, msg)
}

// member 是成员分区中的一个人。
type member struct {
	// Login 是 GitHub 登录名；名单中写的是数字 ID 时为 ID。
	Login string
	Role  string
	// Bound 表示这是管理员绑定的账号。
	Bound bool
	// Source 说明权限来自哪里（绑定、KILN_ADMINS、KILN_USERS）。
	Source string
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
	add := func(login, role, source string, bound bool) {
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
		add(bind.GitHubLogin, roleAdmin, "绑定的 GitHub 账号", true)
	}
	for _, l := range sortedKeys(d.cfg.Admins) {
		add(l, roleAdmin, "KILN_ADMINS", false)
	}
	for _, l := range sortedKeys(d.cfg.Users) {
		add(l, roleUser, "KILN_USERS", false)
	}
	d.render(w, r, "settings_members", "成员", p)
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
	d.render(w, r, "settings_activity", "活动", p)
}

// preconditionOrError 把操作的前提不满足（FailedPrecondition，例如还没有绑定）显示为回到 to 的提示，其他错误显示错误页。
func (d *Dashboard) preconditionOrError(w http.ResponseWriter, r *http.Request, to, what string, err error) {
	if ce, ok := errors.AsType[*connect.Error](err); ok && ce.Code() == connect.CodeFailedPrecondition {
		d.flashBack(w, r, to, what+"："+ce.Message())
		return
	}
	d.serverError(w, r, err)
}

// aiText 返回 AI 请求的活动的说明（PLAN #30.9）；不是 AI 请求的活动时返回 false。
func (e activityEntry) aiText() (string, bool) {
	str := func(k string) string { return fmt.Sprint(e.Detail[k]) }
	num := func(k string) string {
		v, _ := e.Detail[k].(float64)
		return strconv.FormatInt(int64(v), 10)
	}
	switch e.Action {
	case "ai_review.trigger":
		return fmt.Sprintf("手动启动了 AI 审计请求 #%s（%s %s）", num("ai_request_id"), str("repo"), str("ref")), true
	case "ai_request.cancel":
		return "取消了 AI 请求 #" + num("ai_request_id"), true
	case "ai_request.rerun":
		return fmt.Sprintf("重跑了 AI 审计请求 #%s，新的请求 #%s", num("ai_request_id"), num("rerun")), true
	}
	return "", false
}

// Text 返回这条活动的中文说明（不含操作者），Link 返回相关页面的地址（没有时为空）。
func (e activityEntry) Text() string {
	if t, ok := e.aiText(); ok {
		return t
	}
	return e.text()
}

// text 返回 AI 请求之外的活动的说明。
func (e activityEntry) text() string {
	str := func(k string) string {
		if v, ok := e.Detail[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	num := func(k string) string {
		if v, ok := e.Detail[k].(float64); ok {
			return strconv.FormatInt(int64(v), 10)
		}
		return str(k)
	}
	switch e.Action {
	case "login":
		if str("via") == viaToken {
			return "用 admin token 登录了 Dashboard"
		}
		return "用 GitHub 登录了 Dashboard"
	case "run.dispatch":
		return "手动启动了 run #" + num("run_id")
	case "run.cancel":
		return "取消了 run #" + num("run_id")
	case "run.rerun":
		if j := num("job_id"); j != "" && j != "0" {
			return fmt.Sprintf("重跑了 run #%s 中的 job #%s（%s）", num("run_id"), j, rerunLabel(atoiOr0(num("rerun"))))
		}
		return fmt.Sprintf("重跑了 run #%s（%s）", num("run_id"), rerunLabel(atoiOr0(num("rerun"))))
	case "runner.drain":
		return "暂停了 Runner " + str("name") + " 接单"
	case "runner.undrain":
		return "恢复了 Runner " + str("name") + " 接单"
	case "runner.revoke":
		return "吊销了 Runner " + str("name")
	case "runner.delete":
		return "删除了已吊销的 Runner " + str("name")
	case "runner.token_create":
		return "生成了 pool " + str("pool") + " 的 Runner 注册令牌"
	case "runner.enroll":
		return "用注册令牌注册到 pool " + str("pool")
	case "repos.sync":
		return "刷新了仓库列表"
	case "repo.reconcile":
		return "对 " + str("repo") + " 补对账"
	case "cache.bump_salt":
		return "让 " + str("repo") + " 的缓存全部失效"
	case "outbox.retry":
		return "重试了 outbox #" + num("id")
	case "admin.bind":
		if p := str("previous"); p != "" {
			return "把管理员绑定的 GitHub 账号从 " + p + " 换成了 " + str("github_login")
		}
		return "把管理员绑定到 GitHub 账号 " + str("github_login")
	case "admin.unbind":
		return "解除了管理员绑定的 GitHub 账号 " + str("github_login")
	case "admin.token_login.disable":
		return "关闭了 Dashboard 的 admin token 登录"
	case "admin.token_login.enable":
		return "开启了 Dashboard 的 admin token 登录"
	}
	if op, ok := strings.CutPrefix(e.Action, "config."); ok {
		what := map[string]string{ //nolint:gosec // 审计操作的中文说明，不是凭据
			"variable.set": "设置了变量", "variable.delete": "删除了变量", "secret.set": "设置了 secret", "secret.delete": "删除了 secret",
			"environment.upsert": "保存了 environment", "environment.delete": "删除了 environment",
		}[op]
		if what == "" {
			what = "修改了配置 " + op
		}
		name := str("name")
		if name == "" {
			name = str("environment")
		}
		if s := str("scope"); s != "" {
			return strings.TrimSpace(what + " " + name + "（" + s + "）")
		}
		return strings.TrimSpace(what + " " + name)
	}
	return e.Action
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

func atoiOr0(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
