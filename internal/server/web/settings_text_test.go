package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// settingsErrCase 是设置页一个已知错误的映射：错误（按 admin、store、npm、endpoint 实际返回的方式构造）→ 消息和参数。
type settingsErrCase struct {
	name   string
	err    error
	id     msgID
	params map[string]any
	// zh 是页面上的中文（检查参数确实填进了文案）
	zh string
}

// npmScopeErr 返回 npm.NormalizeScope 对 scope 的错误。
func npmScopeErr(t *testing.T, scope string) error {
	t.Helper()
	_, err := npm.NormalizeScope(scope)
	if err == nil {
		t.Fatalf("scope %q 应不合格", scope)
	}
	return err
}

// registryErr 返回 npm.CheckRegistry 对地址的错误。
func registryErr(t *testing.T, raw string) error {
	t.Helper()
	err := npm.CheckRegistry(raw)
	if err == nil {
		t.Fatalf("地址 %q 应不合格", raw)
	}
	return err
}

func settingsErrCases(t *testing.T) []settingsErrCase {
	t.Helper()
	until := time.Unix(1_800_000_000, 0)
	return []settingsErrCase{
		{"名称为空", admin.ValidName(""), msgSetErrNameEmpty, nil, "名称不能为空"},
		{"名称不合法", admin.ValidName("1-bad"), msgSetErrNameInvalid, map[string]any{argName: "1-bad"}, "名称 1-bad 只能包含字母、数字和 _"},
		{"environment 名称为空", admin.ErrEnvNameRequired, msgSetErrEnvNameRequired, nil, "需要 environment 名称"},
		{"environment 名称两端有空白", &admin.EnvNameSpaceError{Name: " x"}, msgSetErrEnvNameSpace, map[string]any{argName: " x"}, "“ x”不能以空白开头或结尾"},
		{"发布仓库格式", &admin.PublishRepoError{Repo: "bad"}, msgSetErrPublishRepo, map[string]any{argRepo: "bad"}, "owner/name：bad"},
		{"没有 environment", admin.ErrEnvRequired, msgSetErrEnvRequired, nil, "需要指定 environment"},
		{"没有作用域", store.ErrScopeRequired, msgSetErrScopeRequired, nil, "需要指定账号或仓库"},
		{"账号不在 Kiln 中", &store.AccountNotFoundError{Account: "ghost"}, msgSetErrAccountNotFound, map[string]any{argAccount: "ghost"}, "账号 ghost 不在 Kiln 中"},
		{
			"仓库不在 Kiln 中（connect 包装）",
			connect.NewError(connect.CodeInvalidArgument, &store.RepoNotFoundError{Name: "tester/nope"}),
			msgSetErrRepoNotFound,
			map[string]any{argRepo: "tester/nope"},
			"仓库 tester/nope 不在 Kiln 中",
		},
		{"仓库名格式", &store.RepoNameError{Name: "alice"}, msgSetErrRepoName, map[string]any{argRepo: "alice"}, "仓库应写成 owner/name：alice"},
		{"environment 不存在", fmt.Errorf("删除：%w", &store.EnvironmentNotFoundError{Name: "release"}), msgSetErrEnvNotFound, map[string]any{argName: "release"}, "environment release 不存在"},
		{"npm token 为空", admin.ErrNpmTokenEmpty, msgSetErrNpmTokenEmpty, nil, "请填写 npm token"},
		{"npm token 太长", admin.ErrNpmTokenTooLong, msgSetErrNpmTokenLong, nil, "npm token 太长"},
		{"没有 npm 配置", &admin.NpmNotConfiguredError{Account: "tester"}, msgSetErrNpmNotConfigured, map[string]any{argAccount: "tester"}, "账号 tester 没有配置 npm"},
		{"没有文档绑定", &admin.NoDocBindingError{Repo: "tester/demo"}, msgSetErrNoDocBinding, map[string]any{argRepo: "tester/demo"}, "tester/demo 没有文档绑定"},
		{"没有绑定", connect.NewError(connect.CodeFailedPrecondition, admin.ErrNotBound), msgSetErrNotBound, nil, "还没有绑定 GitHub 账号"},
		{"不需要解除", connect.NewError(connect.CodeFailedPrecondition, admin.ErrAlreadyUnbound), msgSetErrAlreadyUnbound, nil, "不需要解除"},
		{
			"GitHub 限流", connect.NewError(connect.CodeResourceExhausted, &admin.RateLimitedError{Until: until}),
			msgSetErrRateLimited,
			map[string]any{argAt: until.Unix()},
			"1800000000 后再试",
		},
		{"scope 没有 @", npmScopeErr(t, "@"), msgSetErrScopeEmpty, map[string]any{argValue: "@"}, "@ 后面不能为空"},
		{"scope 以 _ 开头", npmScopeErr(t, "@_a"), msgSetErrScopeDot, map[string]any{argValue: "@_a"}, "不能以 . 或 _ 开头"},
		{"scope 有大写", npmScopeErr(t, "@Acme"), msgSetErrScopeUpper, map[string]any{argValue: "@Acme"}, "scope @Acme 无效：不能有大写字母"},
		{"scope 有斜线", npmScopeErr(t, "@a/b"), msgSetErrScopeChar, map[string]any{argValue: "@a/b", argChar: "/"}, "不能包含字符“/”"},
		{"scope 不以 @ 开头", &npm.ScopeError{Scope: "acme"}, msgSetErrScopeNoAt, map[string]any{argValue: "acme"}, "scope acme 必须以 @ 开头"},
		{"地址无法解析", registryErr(t, "https://a b/%zz"), msgSetErrURLInvalid, map[string]any{argValue: "https://a b/%zz"}, "无效"},
		{"地址缺少主机名", registryErr(t, "https:///x"), msgSetErrURLNoHost, map[string]any{argValue: "https:///x"}, "缺少主机名"},
		{"地址带查询参数", registryErr(t, "https://npm.example/?x=1"), msgSetErrURLQuery, map[string]any{argValue: "https://npm.example/?x=1"}, "不能带查询参数"},
		{"地址不是 https", registryErr(t, "http://npm.example/"), msgSetErrURLScheme, map[string]any{argValue: "http://npm.example/"}, "必须是 https"},
		// 带用户信息的地址可能含有凭据：消息中没有地址
		{"地址带用户信息", registryErr(t, "https://u:s3cret@npm.example/"), msgSetErrURLUserInfo, nil, "不能包含用户信息"},
		{"地址带用户信息且缺少主机名", registryErr(t, "https://u:s3cret@:443/"), msgSetErrURLUserInfo, nil, "不能包含用户信息"},
		{"地址带用户信息且无法解析", registryErr(t, "https://u:p%ss@npm.example.com/"), msgSetErrURLUserInfo, nil, "不能包含用户信息"},
		{
			"升级 registry 的参数错误（upgradectl 包装）",
			&upgradectl.InvalidError{Msg: "x", Err: registryErr(t, "http://npm.example/")},
			msgSetErrURLScheme,
			map[string]any{argValue: "http://npm.example/"},
			"必须是 https",
		},
	}
}

// TestSettingsErrText：admin、store、npm、endpoint、upgradectl 的已知错误按类型（包括包装在 *connect.Error、
// *upgradectl.InvalidError 中的）映射为各自的消息和原始参数；这些消息都可以作为跳转后的提示；CLI 的中文说明不变。
func TestSettingsErrText(t *testing.T) {
	seen := map[msgID]bool{}
	for _, c := range settingsErrCases(t) {
		got := settingsErrText(c.err)
		if got.id != c.id || !maps.Equal(got.params, c.params) {
			t.Errorf("%s：%+v，应为 %s %v", c.name, got, c.id, c.params)
			continue
		}
		if spec, ok := messages[got.id]; !ok || !spec.flash {
			t.Errorf("%s：%s 应登记为 flash（设置页的操作失败后作为提示）", c.name, got.id)
		}
		if zh := textZH(t, got); !strings.Contains(zh, c.zh) {
			t.Errorf("%s：%q 中没有 %q", c.name, zh, c.zh)
		}
		if c.err.Error() == "" {
			t.Errorf("%s：CLI 和日志的说明不应为空", c.name)
		}
		seen[got.id] = true
	}
	// 每条 settingsErr 消息都有对应的错误
	for id := range messages {
		if strings.HasPrefix(string(id), "settingsErr.") && !seen[id] {
			t.Errorf("%s 没有对应的错误（测试缺少用例或消息多余）", id)
		}
	}
	// 不认识的错误、内部错误：没有消息
	for _, err := range []error{errors.New("其他"), connect.NewError(connect.CodeInternal, errors.New("boom"))} {
		if got := settingsErrText(err); !got.IsZero() {
			t.Errorf("%v：%+v，应为空", err, got)
		}
	}
}

// TestSettingsFailures：表单上的说明和跳转后的提示。已知错误是对应的消息；内部错误只给错误编号（详细信息只写日志）；
// 其他错误带服务给出的说明。带用户信息的地址（可能含有凭据）既不出现在页面数据中，也不写进日志。
func TestSettingsFailures(t *testing.T) {
	t.Parallel()
	d, _, logs := testDashboard(t, "")
	r := httptest.NewRequest(http.MethodPost, "/settings/npm", nil)
	secretURL := "https://u:s3cret@npm.example/"

	got := d.settingsFormFailure(r, connect.NewError(connect.CodeInvalidArgument, registryErr(t, secretURL)), msgNpmSaveFailed)
	if got.id != msgSetErrURLUserInfo {
		t.Errorf("带用户信息的地址：%+v", got)
	}
	got = d.settingsFormFailure(r, errors.New("别的原因"), msgNpmSaveFailed)
	if textZH(t, got) != "保存 npm 配置失败：别的原因" {
		t.Errorf("其他错误：%q", textZH(t, got))
	}
	pgErr := &pgconn.PgError{Code: "42P01", Message: `relation "npm_accounts" does not exist`}
	got = d.settingsFormFailure(r, fmt.Errorf("保存：%w", pgErr), msgNpmSaveFailed)
	if got.id != msgActionInternal || strings.Contains(textZH(t, got), "npm_accounts") {
		t.Errorf("内部错误应只给错误编号：%+v", got)
	}

	flash := d.settingsFailureFlash(r, &store.AccountNotFoundError{Account: "ghost"}, msgNpmDeleteFailed, msgNpmDeleteFailedRef, strArg(argAccount, "ghost"))
	if flash.id != msgSetErrAccountNotFound {
		t.Errorf("已知错误的提示：%+v", flash)
	}
	// 删除 Docker Hub 连接时 environment 或账号已被并发删除：显示对应的消息，不是笼统的删除失败
	for _, c := range []struct {
		err  error
		want msgID
	}{
		{connect.NewError(connect.CodeInvalidArgument, &store.EnvironmentNotFoundError{Name: "docker-release"}), msgSetErrEnvNotFound},
		{&store.AccountNotFoundError{Account: "ghost"}, msgSetErrAccountNotFound},
	} {
		if got := d.settingsFailureFlash(r, c.err, msgDockerDeleteFailed, msgDockerDeleteFailedRef, strArg(argAccount, "tester")); got.id != c.want {
			t.Errorf("删除 Docker Hub 连接失败（%v）的提示：%+v", c.err, got)
		}
	}
	flash = d.settingsFailureFlash(r, errors.New("别的原因"), msgNpmDeleteFailed, msgNpmDeleteFailedRef, strArg(argAccount, "tester"))
	if textZH(t, flash) != "删除 tester 的 npm 配置失败：别的原因" {
		t.Errorf("其他错误的提示：%q", textZH(t, flash))
	}
	flash = d.settingsFailureFlash(r, fmt.Errorf("删除：%w", pgErr), msgNpmDeleteFailed, msgNpmDeleteFailedRef, strArg(argAccount, "tester"))
	if flash.id != msgNpmDeleteFailedRef || strings.Contains(textZH(t, flash), "npm_accounts") {
		t.Errorf("内部错误的提示：%+v", flash)
	}
	for _, x := range []uiText{got, flash} {
		if _, err := flashValue(fitFlash(x)); err != nil && x.id != msgActionInternal {
			t.Errorf("%s 应能放进地址：%v", x.id, err)
		}
	}
	if !strings.Contains(logs.String(), "npm_accounts") {
		t.Errorf("内部错误的详细信息应写在日志中：%s", logs.String())
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Errorf("日志中不应有地址中的凭据：%s", logs.String())
	}

	// 补对账被 GitHub 限流：给出可以重试的时间（Unix 秒，前端按当前语言显示为时间）
	until := time.Unix(1_800_000_000, 0)
	rl := d.reconcileFailure(r, connect.NewError(connect.CodeResourceExhausted, &admin.RateLimitedError{Until: until}), testRepo)
	if rl.id != msgReposReconcileLimited || rl.params[argAt] != until.Unix() || rl.params[argRepo] != testRepo {
		t.Errorf("限流的提示：%+v", rl)
	}
	if v, err := flashValue(fitFlash(rl)); err != nil || fmt.Sprint(parseFlash(v).params[argAt]) != "1800000000" {
		t.Errorf("限流的提示应能放进地址：%q %v", v, err)
	}

	// 表单中带用户信息的地址不回填（包括缺少主机名、无法解析的）
	for _, raw := range []string{secretURL, "https://u:s3cret@:443/", "https://u:s3cret@", "https://u:p%ss@npm.example.com/"} {
		f := newForm()
		f.Values["registry"] = raw
		dropUserInfo(f, "registry", registryErr(t, raw))
		f.Values["scope"] = "@acme"
		dropUserInfo(f, "scope", registryErr(t, "http://npm.example/"))
		if _, ok := f.Values["registry"]; ok || f.Values["scope"] != "@acme" {
			t.Errorf("%s：只清空带用户信息的地址：%v", raw, f.Values)
		}
	}
}

// TestDocCheckText：文档读取验证的每个错误码都有自己的消息（可以作为跳转后的提示），中文与 docsync 给 CLI 的说明一致；
// 不认识的错误码原样给出错误码，没有错误码时为空。
func TestDocCheckText(t *testing.T) {
	seen := map[msgID]bool{}
	for _, code := range docsync.DocCheckErrorCodes() {
		got := docCheckText(code)
		if spec, ok := messages[got.id]; !ok || !spec.flash || got.id == msgDocCheckUnknown || seen[got.id] {
			t.Errorf("%s：%+v 应是自己的、登记为 flash 的消息", code, got)
			continue
		}
		seen[got.id] = true
		if want := "验证失败：" + docsync.DocCheckErrorText(code); textZH(t, got) != want {
			t.Errorf("%s：%q，应为 %q", code, textZH(t, got), want)
		}
	}
	if len(docCheckMsgs) != len(docsync.DocCheckErrorCodes()) {
		t.Errorf("docCheckMsgs 有 %d 个错误码，docsync 有 %d 个", len(docCheckMsgs), len(docsync.DocCheckErrorCodes()))
	}
	if got := docCheckText("new_code"); got.id != msgDocCheckUnknown || got.params[argCode] != "new_code" {
		t.Errorf("不认识的错误码：%+v", got)
	}
	if got := docCheckText(""); !got.IsZero() {
		t.Errorf("没有错误码：%+v", got)
	}
	// 验证结果的提示
	n := 3
	for _, c := range []struct {
		res  admin.DocVerifyResult
		want string
	}{
		{admin.DocVerifyResult{Status: docsync.DocCheckStatusOK, DocCount: &n, SampleDocID: "d1"}, "验证通过：文档集可访问，共 3 篇文档，抽查了 d1 的正文。"},
		{admin.DocVerifyResult{Status: docsync.DocCheckStatusEmpty}, "验证通过：文档集可访问，但当前没有文档（冻结时会记录一个空集合）。"},
		{admin.DocVerifyResult{Status: docsync.DocCheckStatusFailed, ErrorCode: docsync.DocCheckTimeout}, "验证失败：验证超时：文档系统在时限内没有返回结果"},
		{admin.DocVerifyResult{Status: docsync.DocCheckStatusFailed}, "验证失败（原因 ）"},
	} {
		got := docVerifyMessage(&c.res)
		if !strings.HasPrefix(textZH(t, got), c.want) {
			t.Errorf("%+v：%q，应以 %q 开头", c.res, textZH(t, got), c.want)
		}
		if _, err := flashValue(fitFlash(got)); err != nil {
			t.Errorf("%+v：提示不能放进地址：%v", c.res, err)
		}
	}
}

// TestActivityText：活动的说明是消息，参数是审计记录中的原始值；不认识的操作原样显示操作名。
func TestActivityText(t *testing.T) {
	entry := func(action string, kv ...any) activityEntry {
		d := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				t.Fatalf("Detail 的键应是字符串：%v", kv[i])
			}
			d[k] = kv[i+1]
		}
		return activityEntry{Action: action, Detail: d}
	}
	for _, c := range []struct {
		e    activityEntry
		want string
	}{
		{entry("login", "via", viaToken), "用 admin token 登录了 Dashboard"},
		{entry("login", "via", "github"), "用 GitHub 登录了 Dashboard"},
		{entry("run.dispatch", "run_id", float64(7)), "手动启动了 run #7"},
		{entry("run.cancel", "run_id", float64(7)), "取消了 run #7"},
		{entry("run.rerun", "run_id", float64(7), "rerun", float64(0)), "重跑了 run #7（首次运行）"},
		{entry("run.rerun", "run_id", float64(7), "rerun", float64(2)), "重跑了 run #7（第 2 次重跑）"},
		{entry("run.rerun", "run_id", float64(7), "job_id", float64(9), "rerun", float64(1)), "重跑了 run #7 中的 job #9（第 1 次重跑）"},
		{entry("run.rerun", "run_id", float64(7), "job_id", float64(9)), "重跑了 run #7 中的 job #9（首次运行）"},
		{entry("runner.drain", "name", "mac-1"), "暂停了 Runner mac-1 接单"},
		{entry("runner.undrain", "name", "mac-1"), "恢复了 Runner mac-1 接单"},
		{entry("runner.revoke", "name", "mac-1"), "吊销了 Runner mac-1"},
		{entry("runner.delete", "name", "mac-1"), "删除了已吊销的 Runner mac-1"},
		{entry("runner.token_create", "pool", "default"), "生成了 pool default 的 Runner 注册令牌"},
		{entry("runner.enroll", "pool", "default"), "用注册令牌注册到 pool default"},
		{entry("repos.sync"), "刷新了仓库列表"},
		{entry("repo.reconcile", "repo", testRepo), "对 tester/demo 补对账"},
		{entry("cache.bump_salt", "repo", testRepo), "让 tester/demo 的缓存全部失效"},
		{entry("outbox.retry", "id", float64(3)), "重试了 outbox #3"},
		{entry("admin.bind", "github_login", "octocat"), "把管理员绑定到 GitHub 账号 octocat"},
		{entry("admin.bind", "github_login", "octocat", "previous", "alice"), "把管理员绑定的 GitHub 账号从 alice 换成了 octocat"},
		{entry("admin.unbind", "github_login", "octocat"), "解除了管理员绑定的 GitHub 账号 octocat"},
		{entry("admin.token_login.disable"), "关闭了 Dashboard 的 admin token 登录"},
		{entry("admin.token_login.enable"), "开启了 Dashboard 的 admin token 登录"},
		{entry("config.variable.set", "scope", "tester", "name", "FOO", "value", "bar"), "设置了变量 FOO（tester）"},
		{entry("config.variable.delete", "scope", "tester", "name", "FOO"), "删除了变量 FOO（tester）"},
		{entry("config.secret.set", "scope", testRepo, "name", "TOKEN"), "设置了 secret TOKEN（tester/demo）"},
		{entry("config.secret.delete", "scope", testRepo, "name", "TOKEN"), "删除了 secret TOKEN（tester/demo）"},
		{entry("config.environment.upsert", "scope", "tester", "name", "release"), "保存了 environment release（tester）"},
		{entry("config.environment.delete", "scope", "tester/release"), "删除了 environment tester/release"},
		{entry("config.npm.set", "account", "tester", "scope", "@acme", "registry", npm.DefaultRegistry), "保存了 tester 的 npm 配置（@acme）"},
		{entry("config.npm.delete", "account", "tester"), "删除了 tester 的 npm 配置"},
		{entry("config.doc.bind", "repo", testRepo), "绑定了 tester/demo 的外部文档"},
		{entry("config.doc.unbind", "repo", testRepo), "解除了 tester/demo 的外部文档绑定"},
		{entry("config.doc.decision_set", "repo", testRepo), "配置了 tester/demo 的待决策清单"},
		{entry("config.doc.decision_clear", "repo", testRepo), "清除了 tester/demo 的待决策清单"},
		{entry("config.something.new"), "修改了配置 something.new"},
		{entry("ai_request.cancel", "ai_request_id", float64(5)), "取消了 AI 请求 #5"},
	} {
		got := c.e.Text()
		if got.id == "" {
			t.Errorf("%s：应是消息：%+v", c.e.Action, got)
		}
		if zh := textZH(t, got); zh != c.want {
			t.Errorf("%s：%q，应为 %q", c.e.Action, zh, c.want)
		}
	}
	// 不认识的操作原样显示操作名；变量的值不出现在说明中
	if got := entry("future.action", "x", "y").Text(); got.id != "" || got.raw != "future.action" {
		t.Errorf("不认识的操作：%+v", got)
	}
	b, err := json.Marshal(entry("config.variable.set", "scope", "tester", "name", "FOO", "value", "s3cret-ish").Text())
	if err != nil || strings.Contains(string(b), "s3cret-ish") {
		t.Errorf("说明中不应有变量的值：%s %v", b, err)
	}
}
