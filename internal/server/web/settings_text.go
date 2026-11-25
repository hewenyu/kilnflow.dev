package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 设置页的文字（多语言，见 docs/I18N.md 4.3）：admin、store、npm、endpoint、upgradectl 给出带原始参数的错误类型或哨兵错误
// （Error() 仍是 CLI 和日志用的中文说明），这里按类型映射为消息，不按错误原文判断；docsync 的检查结果按错误码映射。
// 不认识的错误原样显示服务给出的说明（作为消息的 reason 参数）。

// settingsErrText 返回设置页校验和前提错误的消息；不是已知的错误时为空。
// 错误可以包装在 *connect.Error 或 *upgradectl.InvalidError 中（都支持 Unwrap）。
func settingsErrText(err error) uiText {
	if t := settingsSentinelText(err); !t.IsZero() {
		return t
	}
	if e, ok := errors.AsType[*endpoint.Error](err); ok {
		return endpointErrText(e)
	}
	if e, ok := errors.AsType[*npm.ScopeError](err); ok {
		return npmScopeErrText(e)
	}
	if e, ok := errors.AsType[*admin.RateLimitedError](err); ok {
		return message(msgSetErrRateLimited, intArg(argAt, e.Until.Unix()))
	}
	return settingsNamedErrText(err)
}

// settingsSentinelText 返回哨兵错误（没有参数）的消息。
func settingsSentinelText(err error) uiText {
	for _, s := range []struct {
		err error
		id  msgID
	}{
		{admin.ErrNameEmpty, msgSetErrNameEmpty},
		{admin.ErrEnvNameRequired, msgSetErrEnvNameRequired},
		{admin.ErrEnvRequired, msgSetErrEnvRequired},
		{store.ErrScopeRequired, msgSetErrScopeRequired},
		{admin.ErrNpmTokenEmpty, msgSetErrNpmTokenEmpty},
		{admin.ErrNpmTokenTooLong, msgSetErrNpmTokenLong},
		{admin.ErrNotBound, msgSetErrNotBound},
		{admin.ErrAlreadyUnbound, msgSetErrAlreadyUnbound},
	} {
		if errors.Is(err, s.err) {
			return message(s.id)
		}
	}
	return uiText{}
}

// settingsNamedErrText 返回带对象名（名称、账号、仓库、environment）的错误的消息。
func settingsNamedErrText(err error) uiText {
	if e, ok := errors.AsType[*admin.InvalidNameError](err); ok {
		return message(msgSetErrNameInvalid, strArg(argName, e.Name))
	}
	if e, ok := errors.AsType[*admin.EnvNameSpaceError](err); ok {
		return message(msgSetErrEnvNameSpace, strArg(argName, e.Name))
	}
	if e, ok := errors.AsType[*admin.PublishRepoError](err); ok {
		return message(msgSetErrPublishRepo, strArg(argRepo, e.Repo))
	}
	if e, ok := errors.AsType[*admin.NpmNotConfiguredError](err); ok {
		return message(msgSetErrNpmNotConfigured, strArg(argAccount, e.Account))
	}
	if e, ok := errors.AsType[*admin.NoDocBindingError](err); ok {
		return message(msgSetErrNoDocBinding, strArg(argRepo, e.Repo))
	}
	if e, ok := errors.AsType[*store.AccountNotFoundError](err); ok {
		return message(msgSetErrAccountNotFound, strArg(argAccount, e.Account))
	}
	if e, ok := errors.AsType[*store.RepoNotFoundError](err); ok {
		return message(msgSetErrRepoNotFound, strArg(argRepo, e.Name))
	}
	if e, ok := errors.AsType[*store.RepoNameError](err); ok {
		return message(msgSetErrRepoName, strArg(argRepo, e.Name))
	}
	if e, ok := errors.AsType[*store.EnvironmentNotFoundError](err); ok {
		return message(msgSetErrEnvNotFound, strArg(argName, e.Name))
	}
	return uiText{}
}

// endpointErrText 返回服务地址不合格的消息。地址带用户信息时不回显地址（其中可能有凭据）。
func endpointErrText(e *endpoint.Error) uiText {
	v := strArg(argValue, e.Raw)
	switch e.Problem {
	case endpoint.ProblemInvalid:
		return message(msgSetErrURLInvalid, v)
	case endpoint.ProblemNoHost:
		return message(msgSetErrURLNoHost, v)
	case endpoint.ProblemUserInfo:
		return message(msgSetErrURLUserInfo)
	case endpoint.ProblemQuery:
		return message(msgSetErrURLQuery, v)
	}
	return message(msgSetErrURLScheme, v)
}

// dropUserInfo 在地址带用户信息（可能是凭据）时清空表单中的这个字段，不回填到页面上。
func dropUserInfo(f *formState, field string, err error) {
	if e, ok := errors.AsType[*endpoint.Error](err); ok && e.Problem == endpoint.ProblemUserInfo {
		delete(f.Values, field)
	}
}

// npmScopeErrText 返回 npm scope 不合格的消息。
func npmScopeErrText(e *npm.ScopeError) uiText {
	v := strArg(argValue, e.Scope)
	if e.Part == nil {
		return message(msgSetErrScopeNoAt, v)
	}
	switch e.Part.Problem {
	case npm.PartEmpty:
		return message(msgSetErrScopeEmpty, v)
	case npm.PartLeadingDot:
		return message(msgSetErrScopeDot, v)
	case npm.PartUppercase:
		return message(msgSetErrScopeUpper, v)
	}
	return message(msgSetErrScopeChar, v, strArg(argChar, string(e.Part.Char)))
}

// settingsFormFailure 返回设置页表单保存失败时显示在表单上的说明：内部错误是带错误编号的消息（详细信息只写日志），
// 已知的校验和前提错误是对应的消息，其余是 failed（参数是服务给出的说明）。
func (d *Dashboard) settingsFormFailure(r *http.Request, err error, failed msgID) uiText {
	if !internalError(err) {
		if t := settingsErrText(err); !t.IsZero() {
			return t
		}
	}
	if ref := d.failureRef(r, err); ref != "" {
		return message(msgActionInternal, strArg(argRef, ref))
	}
	return message(failed, strArg(argReason, connectMessage(err)))
}

// settingsFailureFlash 返回设置页操作失败后跳转时的提示：已知的校验和前提错误是对应的消息（都登记为 flash），
// 其余同 failureFlash（内部错误用 withRef，其他用 withReason）。
func (d *Dashboard) settingsFailureFlash(r *http.Request, err error, withReason, withRef msgID, args ...msgArg) uiText {
	if !internalError(err) {
		if t := settingsErrText(err); !t.IsZero() {
			return t
		}
	}
	return d.failureFlash(r, err, withReason, withRef, args...)
}

// docCheckMsgs 是文档读取验证的错误码对应的消息。
var docCheckMsgs = map[string]msgID{
	docsync.DocCheckAuthFailed:         msgDocCheckAuthFailed,
	docsync.DocCheckForbidden:          msgDocCheckForbidden,
	docsync.DocCheckNotFound:           msgDocCheckNotFound,
	docsync.DocCheckCollectionNotFound: msgDocCheckCollectionNotFound,
	docsync.DocCheckSampleNotFound:     msgDocCheckSampleNotFound,
	docsync.DocCheckRateLimited:        msgDocCheckRateLimited,
	docsync.DocCheckUpstream:           msgDocCheckUpstream,
	docsync.DocCheckInvalidResponse:    msgDocCheckInvalidResponse,
	docsync.DocCheckInvalidBaseURL:     msgDocCheckInvalidBaseURL,
	docsync.DocCheckNetworkError:       msgDocCheckNetworkError,
	docsync.DocCheckTimeout:            msgDocCheckTimeout,
	docsync.DocCheckInvalidDocument:    msgDocCheckInvalidDocument,
	docsync.DocCheckSampleInvalid:      msgDocCheckSampleInvalid,
}

// docCheckText 返回文档读取验证失败的消息；code 为空时为空，不认识的错误码（kiln-server 与页面版本不一致）原样给出错误码。
func docCheckText(code string) uiText {
	if code == "" {
		return uiText{}
	}
	if id, ok := docCheckMsgs[code]; ok {
		return message(id)
	}
	return message(msgDocCheckUnknown, strArg(argCode, code))
}

// ---- 活动（审计日志） ----

// Text 返回这条活动的说明（不含操作者）：已知的操作是消息，参数是审计记录中的原始值；不认识的操作原样显示操作名。
func (e activityEntry) Text() uiText {
	if t := e.aiText(); !t.IsZero() {
		return t
	}
	if t := e.text(); !t.IsZero() {
		return t
	}
	if op, ok := strings.CutPrefix(e.Action, "config."); ok {
		return e.configText(op)
	}
	return rawText(e.Action)
}

// detailStr 返回审计记录中的文字参数（没有时为空）。
func (e activityEntry) detailStr(k string) string {
	if v, ok := e.Detail[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// detailInt 返回审计记录中的数字参数（JSON 中的数字解析为 float64；没有时为 0）。
func (e activityEntry) detailInt(k string) int64 {
	v, _ := e.Detail[k].(float64)
	return int64(v)
}

// aiText 返回 AI 请求的活动的说明；不是时为空。
func (e activityEntry) aiText() uiText {
	switch e.Action {
	case "ai_review.trigger":
		return message(msgActAIReview, intArg(argID, e.detailInt("ai_request_id")),
			strArg(argRepo, e.detailStr("repo")), strArg(argGitRef, e.detailStr("ref")))
	case "ai_request.cancel":
		return message(msgActAICancel, intArg(argID, e.detailInt("ai_request_id")))
	case "ai_request.rerun":
		return message(msgActAIRerun, intArg(argID, e.detailInt("ai_request_id")), intArg(argRequest, e.detailInt("rerun")))
	}
	return uiText{}
}

// activityPlain 是只有一个文字参数（或没有参数）的活动：操作 → 消息和参数名（参数名为空表示没有参数）、审计记录中的键。
var activityPlain = map[string]struct {
	id       msgID
	arg, key string
}{
	"runner.drain":              {msgActRunnerDrain, argName, "name"},
	"runner.undrain":            {msgActRunnerUndrain, argName, "name"},
	"runner.revoke":             {msgActRunnerRevoke, argName, "name"},
	"runner.delete":             {msgActRunnerDelete, argName, "name"},
	"runner.token_create":       {msgActRunnerToken, argPool, "pool"},
	"runner.enroll":             {msgActRunnerEnroll, argPool, "pool"},
	"repos.sync":                {msgActReposSync, "", ""},
	"repo.reconcile":            {msgActRepoReconcile, argRepo, "repo"},
	"cache.bump_salt":           {msgActCacheBump, argRepo, "repo"},
	"admin.unbind":              {msgActAdminUnbind, argLogin, "github_login"},
	"admin.token_login.disable": {msgActTokenDisable, "", ""},
	"admin.token_login.enable":  {msgActTokenEnable, "", ""},
}

// text 返回 AI 请求和配置修改之外的活动的说明；不认识的操作为空。
func (e activityEntry) text() uiText {
	if p, ok := activityPlain[e.Action]; ok {
		if p.arg == "" {
			return message(p.id)
		}
		return message(p.id, strArg(p.arg, e.detailStr(p.key)))
	}
	run := intArg(argRun, e.detailInt("run_id"))
	switch e.Action {
	case "login":
		if e.detailStr("via") == viaToken {
			return message(msgActLoginToken)
		}
		return message(msgActLoginGitHub)
	case "run.dispatch":
		return message(msgActRunDispatch, run)
	case "run.cancel":
		return message(msgActRunCancel, run)
	case "run.rerun":
		return e.rerunText(run)
	case "outbox.retry":
		return message(msgActOutboxRetry, intArg(argID, e.detailInt("id")))
	case "admin.bind":
		login := strArg(argLogin, e.detailStr("github_login"))
		if p := e.detailStr("previous"); p != "" {
			return message(msgActAdminRebind, strArg(argPrevious, p), login)
		}
		return message(msgActAdminBind, login)
	}
	return uiText{}
}

// rerunText 返回重跑的说明：重跑整个 run 或其中一个 job，第几次重跑（0 是首次运行）。
func (e activityEntry) rerunText(run msgArg) uiText {
	n := e.detailInt("rerun")
	if job := e.detailInt("job_id"); job != 0 {
		if n == 0 {
			return message(msgActRunRerunJobFirst, run, intArg(argJob, job))
		}
		return message(msgActRunRerunJob, run, intArg(argJob, job), intArg(argN, n))
	}
	if n == 0 {
		return message(msgActRunRerunFirst, run)
	}
	return message(msgActRunRerun, run, intArg(argN, n))
}

// activityConfig 是有名称和作用域的配置修改（store.RecordConfigChange 记下的 name、scope）。
var activityConfig = map[string]msgID{
	"variable.set": msgActVarSet, "variable.delete": msgActVarDelete,
	"secret.set": msgActSecretSet, "secret.delete": msgActSecretDelete,
	"environment.upsert": msgActEnvUpsert,
}

// configText 返回配置修改（config.<op>）的说明；参数（名称、作用域、仓库、账号）是审计记录中的原始值。
func (e activityEntry) configText(op string) uiText {
	if id, ok := activityConfig[op]; ok {
		return message(id, strArg(argName, e.detailStr("name")), strArg(argScope, e.detailStr("scope")))
	}
	repo := strArg(argRepo, e.detailStr("repo"))
	switch op {
	case "environment.delete":
		return message(msgActEnvDelete, strArg(argScope, e.detailStr("scope")))
	case "npm.set":
		return message(msgActNpmSet, strArg(argAccount, e.detailStr("account")), strArg(argScope, e.detailStr("scope")))
	case "npm.delete":
		return message(msgActNpmDelete, strArg(argAccount, e.detailStr("account")))
	case "doc.bind":
		return message(msgActDocBind, repo)
	case "doc.unbind":
		return message(msgActDocUnbind, repo)
	case "doc.decision_set":
		return message(msgActDecisionSet, repo)
	case "doc.decision_clear":
		return message(msgActDecisionClear, repo)
	}
	return message(msgActConfigOther, strArg(argOp, op))
}
