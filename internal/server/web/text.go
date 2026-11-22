package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 页面数据中的文字（多语言，设计见 docs/I18N.md）：
//   - 服务端不按语言拼文案，只给出消息（msgID 加参数），前端按当前语言从词典（web/src/i18n/locales/*.json）取文案，
//     切换语言时立即更新；时间、数量等参数传原始值（Unix 秒、数字），由前端按语言格式化。
//   - 仓库名、登录名、对象名等原始内容作为参数或原样显示的文字（rawText）传递，不当作消息键。
//   - 还没有迁移的页面照常传字符串：uiText 的原样文字编码为 JSON 字符串，与原来的页面数据相同。
// 消息键和参数必须在 messages 中登记：契约测试（text_test.go）检查每条消息在两种语言的词典中都有、参数名和格式一致；
// 跳转后的提示（?flash=）只接受登记为 flash 的消息和参数。

// msgID 是前端词典中的消息键。
type msgID string

// paramKind 是消息参数的类型：文字中按 {name} 插入；数字可以按 {name, number}（数字格式）、{name, time}（Unix 秒）格式化。
type paramKind int

const (
	paramString paramKind = iota + 1
	paramInt
)

// msgSpec 是一条消息的参数和用途。
type msgSpec struct {
	params map[string]paramKind
	// flash 表示可以作为跳转后的提示放进地址（?flash=）。
	flash bool
}

// 服务端给出的消息。新增消息时在 messages 中登记，并在两种语言的词典中加上文案。
const (
	msgLoginTitle         msgID = "login.pageTitle"
	msgLoginNoticeExpired msgID = "login.noticeExpired"
	msgLoginNoticeRevoked msgID = "login.noticeRevoked"
	msgLoginNoticeLogout  msgID = "login.noticeLogout"
	msgLoginErrAdminOff   msgID = "login.errTokenDisabled"
	msgLoginErrAdminEmpty msgID = "login.errTokenEmpty"
	msgLoginErrAdminWrong msgID = "login.errTokenInvalid"

	msgErrLinkRetry          msgID = "error.linkRetry"
	msgErrLinkBack           msgID = "error.linkBack"
	msgErrLinkRuns           msgID = "error.linkRuns"
	msgErrLinkLogin          msgID = "error.linkLogin"
	msgErrLinkLoginAsAdmin   msgID = "error.linkLoginAsAdmin"
	msgErrLinkBackToLogin    msgID = "error.linkBackToLogin"
	msgErrLinkRelogin        msgID = "error.linkRelogin"
	msgErrLinkGitHubRelogin  msgID = "error.linkGitHubRelogin"
	msgErrLinkGitHubRetry    msgID = "error.linkGitHubRetry"
	msgErrLinkAccount        msgID = "error.linkAccount"
	msgErrServerHeading      msgID = "error.serverHeading"
	msgErrServerReasonGet    msgID = "error.serverReasonGet"
	msgErrServerReasonPost   msgID = "error.serverReasonPost"
	msgErrNotFoundHeading    msgID = "error.notFoundHeading"
	msgErrNotFoundReason     msgID = "error.notFoundReason"
	msgErrBadRequestHeading  msgID = "error.badRequestHeading"
	msgErrForbiddenHeading   msgID = "error.forbiddenHeading"
	msgErrForbiddenReason    msgID = "error.forbiddenReason"
	msgErrCSRFHeading        msgID = "error.csrfHeading"
	msgErrCSRFReason         msgID = "error.csrfReason"
	msgErrOAuthOffHeading    msgID = "error.oauthDisabledHeading"
	msgErrOAuthOffReason     msgID = "error.oauthDisabledReason"
	msgErrOAuthBadHeading    msgID = "error.oauthInvalidHeading"
	msgErrOAuthBadReason     msgID = "error.oauthInvalidReason"
	msgErrOAuthBrowserReason msgID = "error.oauthOtherBrowserReason"
	msgErrOAuthExpHeading    msgID = "error.oauthExpiredHeading"
	msgErrOAuthExpReason     msgID = "error.oauthExpiredReason"
	msgErrOAuthDenyHeading   msgID = "error.oauthDeniedHeading"
	msgErrOAuthDenyReason    msgID = "error.oauthDeniedReason"
	msgErrOAuthNoAccHeading  msgID = "error.oauthNoAccessHeading"
	msgErrOAuthNoAccReason   msgID = "error.oauthNoAccessReason"
	msgErrOAuthFailHeading   msgID = "error.oauthFailedHeading"
	msgErrOAuthFailReason    msgID = "error.oauthFailedReason"
	msgErrBindBadHeading     msgID = "error.bindInvalidHeading"
	msgErrBindBadReason      msgID = "error.bindInvalidReason"

	msgAccountBound   msgID = "account.bound"
	msgAccountUnbound msgID = "account.unbound"

	// 操作失败时的通用说明（内部错误只给错误编号，见 failureText）。
	msgActionInternal msgID = "action.internalError"
	// 页面脚本调用的接口：未登录。
	msgAPIUnauthorized msgID = "api.unauthorized"
)

// 总览与 CI 运行（总览、运行列表、启动构建、run 和 job 详情、日志）的消息。
const (
	msgNavOverview msgID = "nav.overview"
	msgNavRuns     msgID = "nav.runs"

	msgSetupReposTitle    msgID = "overview.setupReposTitle"
	msgSetupReposText     msgID = "overview.setupReposText"
	msgSetupReposAction   msgID = "overview.setupReposAction"
	msgSetupRunnerTitle   msgID = "overview.setupRunnerTitle"
	msgSetupRunnerText    msgID = "overview.setupRunnerText"
	msgSetupRunnerAction  msgID = "overview.setupRunnerAction"
	msgSetupBindTitle     msgID = "overview.setupBindTitle"
	msgSetupBindText      msgID = "overview.setupBindText"
	msgSetupBindAction    msgID = "overview.setupBindAction"
	msgSetupTokenTitle    msgID = "overview.setupTokenTitle"
	msgSetupTokenText     msgID = "overview.setupTokenText"
	msgSetupTokenAction   msgID = "overview.setupTokenAction"
	msgRunStateQueued     msgID = "runState.queued"
	msgRunStateActive     msgID = "runState.active"
	msgRunStateSucceeded  msgID = "runState.succeeded"
	msgRunStateFailed     msgID = "runState.failed"
	msgRunStateCancelled  msgID = "runState.cancelled"
	msgRunStateSkipped    msgID = "runState.skipped"
	msgRunNewTitle        msgID = "runNew.pageTitle"
	msgRunNewPickRepo     msgID = "runNew.errPickRepo"
	msgRunNewRepoMissing  msgID = "runNew.errRepoMissing"
	msgRunNewPickWorkflow msgID = "runNew.errPickWorkflow"
	msgRunNewWfMissing    msgID = "runNew.errWorkflowMissing"
	msgRunNewWfNoDispatch msgID = "runNew.errNotDispatchable"
	msgRunNewRefNotFound  msgID = "runNew.errRefNotFound"
	msgRunNewInputs       msgID = "runNew.errInputs"
	msgRunNewDispatched   msgID = "runNew.dispatched"

	msgInputNoEquals      msgID = "runNew.inputNoEquals"
	msgInputNoName        msgID = "runNew.inputNoName"
	msgInputBadName       msgID = "runNew.inputBadName"
	msgInputDuplicate     msgID = "runNew.inputDuplicate"
	msgInputMissing       msgID = "runNew.inputMissing"
	msgInputNotBool       msgID = "runNew.inputNotBoolean"
	msgInputNotChoice     msgID = "runNew.inputNotChoice"
	msgInputUndeclared    msgID = "runNew.inputUndeclared"
	msgRunTitle           msgID = "run.pageTitle"
	msgRunTitleFirst      msgID = "run.pageTitleFirst"
	msgRunTitleRerun      msgID = "run.pageTitleRerun"
	msgRunNotFound        msgID = "run.notFound"
	msgRunActDispatch     msgID = "run.activityDispatch"
	msgRunActCancel       msgID = "run.activityCancel"
	msgRunActRerun        msgID = "run.activityRerun"
	msgRunActRerunFailed  msgID = "run.activityRerunFailed"
	msgRunActRerunAll     msgID = "run.activityRerunAll"
	msgRunActRerunNoCache msgID = "run.activityRerunNoCache"
	msgRunActRerunJob     msgID = "run.activityRerunJob"
	msgRunCancelled       msgID = "run.cancelRequested"
	msgRunCancelFailed    msgID = "run.cancelFailed"
	msgRunCancelFailedRef msgID = "run.cancelFailedRef"
	msgRunRerunCreated    msgID = "run.rerunCreated"
	msgRunRerunFailed     msgID = "run.rerunFailed"
	msgRunRerunFailedRef  msgID = "run.rerunFailedRef"

	msgJobNotFound          msgID = "job.notFound"
	msgJobRerunNotFound     msgID = "job.rerunNotFound"
	msgJobAttemptPublish    msgID = "job.attemptPublish"
	msgJobAttemptInvalid    msgID = "job.attemptInvalid"
	msgJobAttemptMissing    msgID = "job.attemptMissing"
	msgJobOutputGone        msgID = "job.outputGone"
	msgJobLogPurged         msgID = "job.logPurged"
	msgJobLogNoOutput       msgID = "job.logNoOutput"
	msgJobLogPublishPending msgID = "job.logPublishPending"
	msgJobLogReused         msgID = "job.logReused"
	msgJobLogCached         msgID = "job.logCached"
	msgJobLogSkipped        msgID = "job.logSkipped"
	msgJobLogQueued         msgID = "job.logQueued"
	msgJobLogNotRun         msgID = "job.logNotRun"
	msgJobLogNone           msgID = "job.logNone"

	msgLogBadAttempt          msgID = "log.errBadAttempt"
	msgLogBadOffset           msgID = "log.errBadOffset"
	msgLogJobNotFound         msgID = "log.errJobNotFound"
	msgLogJobAttemptNotFound  msgID = "log.errJobAttemptNotFound"
	msgLogTaskNotFound        msgID = "log.errTaskNotFound"
	msgLogTaskAttemptNotFound msgID = "log.errTaskAttemptNotFound"
	msgLogServer              msgID = "log.errServer"
)

// 消息的参数名（同一个参数名在各条消息中的含义相同）。
const (
	argLogin = "login"
	// argCount 是数量：复数形式按它选择。
	argCount = "count"
	// argRef 是错误编号（与服务器日志对应），argReason 是原样显示的失败原因（例如 admin.Service 给出的说明）。
	argRef    = "ref"
	argReason = "reason"
	// argID 是 run、job 的编号；argN 是第几次重跑；argAttempt 是 attempt 的编号。
	argID      = "id"
	argN       = "n"
	argJob     = "job"
	argAttempt = "attempt"
	// argValue 是地址或表单中原样的输入值。
	argValue = "value"
	argRepo  = "repo"
	// argGitRef 是分支、tag 或完整的 ref；argFile 是 workflow 文件名。
	argGitRef = "gitRef"
	argFile   = "file"
	// argLine、argFirst 是 inputs 的行号；argName 是 input 的名称；argText 是原样的一行；argOptions 是可选值（已按语言连接）。
	argLine    = "line"
	argFirst   = "first"
	argName    = "name"
	argText    = "text"
	argOptions = "options"
	argOutput  = "output"
	argPath    = "path"
)

// messages 是服务端给出的全部消息（只读）。
var messages = map[msgID]msgSpec{
	msgLoginTitle: {}, msgLoginNoticeExpired: {}, msgLoginNoticeRevoked: {}, msgLoginNoticeLogout: {},
	msgLoginErrAdminOff: {}, msgLoginErrAdminEmpty: {}, msgLoginErrAdminWrong: {},

	msgErrLinkRetry: {}, msgErrLinkBack: {}, msgErrLinkRuns: {}, msgErrLinkLogin: {}, msgErrLinkLoginAsAdmin: {},
	msgErrLinkBackToLogin: {}, msgErrLinkRelogin: {}, msgErrLinkGitHubRelogin: {}, msgErrLinkGitHubRetry: {}, msgErrLinkAccount: {},
	msgErrServerHeading: {}, msgErrServerReasonGet: {}, msgErrServerReasonPost: {},
	msgErrNotFoundHeading: {}, msgErrNotFoundReason: {}, msgErrBadRequestHeading: {},
	msgErrForbiddenHeading: {}, msgErrForbiddenReason: {params: map[string]paramKind{argLogin: paramString}},
	msgErrCSRFHeading: {}, msgErrCSRFReason: {},
	msgErrOAuthOffHeading: {}, msgErrOAuthOffReason: {}, msgErrOAuthBadHeading: {}, msgErrOAuthBadReason: {}, msgErrOAuthBrowserReason: {},
	msgErrOAuthExpHeading: {}, msgErrOAuthExpReason: {params: map[string]paramKind{argCount: paramInt}},
	msgErrOAuthDenyHeading: {}, msgErrOAuthDenyReason: {},
	msgErrOAuthNoAccHeading: {}, msgErrOAuthNoAccReason: {params: map[string]paramKind{argLogin: paramString}},
	msgErrOAuthFailHeading: {}, msgErrOAuthFailReason: {}, msgErrBindBadHeading: {}, msgErrBindBadReason: {},

	msgAccountBound:   {params: map[string]paramKind{argLogin: paramString}, flash: true},
	msgAccountUnbound: {flash: true},

	msgActionInternal:  {params: map[string]paramKind{argRef: paramString}},
	msgAPIUnauthorized: {},

	msgNavOverview: {}, msgNavRuns: {},
	msgSetupReposTitle: {}, msgSetupReposText: {}, msgSetupReposAction: {},
	msgSetupRunnerTitle: {}, msgSetupRunnerText: {}, msgSetupRunnerAction: {},
	msgSetupBindTitle: {}, msgSetupBindText: {}, msgSetupBindAction: {},
	msgSetupTokenTitle: {}, msgSetupTokenText: {}, msgSetupTokenAction: {},
	msgRunStateQueued: {}, msgRunStateActive: {}, msgRunStateSucceeded: {}, msgRunStateFailed: {}, msgRunStateCancelled: {}, msgRunStateSkipped: {},

	msgRunNewTitle: {}, msgRunNewPickRepo: {}, msgRunNewPickWorkflow: {},
	msgRunNewRepoMissing:  {params: map[string]paramKind{argRepo: paramString}},
	msgRunNewWfMissing:    {params: map[string]paramKind{argGitRef: paramString, argFile: paramString}},
	msgRunNewWfNoDispatch: {params: map[string]paramKind{argFile: paramString}},
	msgRunNewRefNotFound:  {params: map[string]paramKind{argGitRef: paramString}},
	msgRunNewInputs:       {params: map[string]paramKind{argCount: paramInt}},
	msgRunNewDispatched:   {flash: true},
	msgInputNoEquals:      {params: map[string]paramKind{argLine: paramInt, argText: paramString}},
	msgInputNoName:        {params: map[string]paramKind{argLine: paramInt}},
	msgInputBadName:       {params: map[string]paramKind{argLine: paramInt, argName: paramString}},
	msgInputDuplicate:     {params: map[string]paramKind{argLine: paramInt, argName: paramString, argFirst: paramInt}},
	msgInputMissing:       {params: map[string]paramKind{argName: paramString}},
	msgInputNotBool:       {params: map[string]paramKind{argName: paramString}},
	msgInputNotChoice:     {params: map[string]paramKind{argName: paramString, argOptions: paramString}},
	msgInputUndeclared:    {params: map[string]paramKind{argName: paramString}},

	msgRunTitle:           {params: map[string]paramKind{argID: paramInt}},
	msgRunTitleFirst:      {params: map[string]paramKind{argID: paramInt}},
	msgRunTitleRerun:      {params: map[string]paramKind{argID: paramInt, argN: paramInt}},
	msgRunNotFound:        {params: map[string]paramKind{argValue: paramString}},
	msgRunActDispatch:     {},
	msgRunActCancel:       {},
	msgRunActRerun:        {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunFailed:  {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunAll:     {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunNoCache: {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunJob:     {params: map[string]paramKind{argJob: paramInt, argN: paramInt}},
	msgRunCancelled:       {flash: true},
	msgRunCancelFailed:    {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgRunCancelFailedRef: {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgRunRerunCreated:    {params: map[string]paramKind{argN: paramInt}, flash: true},
	msgRunRerunFailed:     {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgRunRerunFailedRef:  {params: map[string]paramKind{argRef: paramString}, flash: true},

	msgJobNotFound:          {params: map[string]paramKind{argValue: paramString}},
	msgJobRerunNotFound:     {params: map[string]paramKind{argID: paramInt}},
	msgJobAttemptPublish:    {},
	msgJobAttemptInvalid:    {params: map[string]paramKind{argValue: paramString}},
	msgJobAttemptMissing:    {params: map[string]paramKind{argAttempt: paramInt}},
	msgJobOutputGone:        {params: map[string]paramKind{argOutput: paramString, argPath: paramString}, flash: true},
	msgJobLogPurged:         {},
	msgJobLogNoOutput:       {},
	msgJobLogPublishPending: {},
	msgJobLogReused:         {params: map[string]paramKind{argJob: paramInt}},
	msgJobLogCached:         {params: map[string]paramKind{argJob: paramInt}},
	msgJobLogSkipped:        {},
	msgJobLogQueued:         {},
	msgJobLogNotRun:         {},
	msgJobLogNone:           {},

	msgLogBadAttempt: {}, msgLogBadOffset: {},
	msgLogJobNotFound:         {params: map[string]paramKind{argID: paramInt}},
	msgLogJobAttemptNotFound:  {params: map[string]paramKind{argID: paramInt, argAttempt: paramInt}},
	msgLogTaskNotFound:        {params: map[string]paramKind{argID: paramInt}},
	msgLogTaskAttemptNotFound: {params: map[string]paramKind{argID: paramInt, argAttempt: paramInt}},
	msgLogServer:              {params: map[string]paramKind{argRef: paramString}},
}

// uiText 是页面数据中的一段文字：消息（id 加参数，前端按当前语言显示），或原样显示的文字（raw）。零值是空串。
// JSON 编码：消息为 {"id": …, "params": {…}}，原样的文字为字符串。
type uiText struct {
	id     msgID
	params map[string]any
	raw    string
}

// msgArg 是消息的一个参数。
type msgArg struct {
	name  string
	value any
}

// strArg、intArg 返回文字、整数参数。
func strArg(name, v string) msgArg       { return msgArg{name, v} }
func intArg(name string, v int64) msgArg { return msgArg{name, v} }

// message 返回消息 id（参数为 args）。
func message(id msgID, args ...msgArg) uiText {
	t := uiText{id: id}
	if len(args) > 0 {
		t.params = make(map[string]any, len(args))
		for _, a := range args {
			t.params[a.name] = a.value
		}
	}
	return t
}

// rawText 返回原样显示的文字（对象名、原始的说明，以及还没有迁移成消息的文案）。
func rawText(s string) uiText { return uiText{raw: s} }

// IsZero 表示没有文字。
func (t uiText) IsZero() bool { return t.id == "" && t.raw == "" }

// textJSON 是消息的 JSON 形式。
type textJSON struct {
	ID     msgID          `json:"id"`
	Params map[string]any `json:"params,omitempty"`
}

func (t uiText) MarshalJSON() ([]byte, error) {
	if t.id == "" {
		return json.Marshal(t.raw)
	}
	return json.Marshal(textJSON{ID: t.id, Params: t.params})
}

// UnmarshalJSON 解码字符串或消息（测试读取页面数据时用；地址中的提示用 parseFlash，另有校验）。
func (t *uiText) UnmarshalJSON(b []byte) error {
	if s := ""; json.Unmarshal(b, &s) == nil {
		*t = rawText(s)
		return nil
	}
	var m textJSON
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = uiText{id: m.ID, params: m.Params}
	return nil
}

// 跳转后的提示（?flash=）的上限：地址可以被任何人构造，超出的不显示。
const (
	// maxFlashBytes 是 flash 参数（消息的 JSON）的最大长度。
	maxFlashBytes = 1024
	// maxFlashArgRunes 是 flash 中一个文字参数的最大字符数。
	maxFlashArgRunes = 200
	// maxLegacyFlashBytes 是还没有迁移的整句提示的最大长度（字节，截断显示）。
	maxLegacyFlashBytes = 600
)

// errFlash 表示地址中的提示不合格（不认识的消息、多余或缺少的参数、类型不对）。
var errFlash = errors.New("提示不合格")

// flashValue 返回放进地址的提示（?flash= 的值）：消息的 JSON。只接受登记为 flash 的消息。
func flashValue(t uiText) (string, error) {
	if _, err := checkFlash(t.id, t.params); err != nil {
		return "", err
	}
	b, err := json.Marshal(textJSON{ID: t.id, Params: t.params})
	if err != nil {
		return "", err
	}
	if len(b) > maxFlashBytes {
		return "", fmt.Errorf("%w：%d 字节，超过 %d", errFlash, len(b), maxFlashBytes)
	}
	return string(b), nil
}

// parseFlash 校验地址中的提示（?flash= 的值）并返回要显示的文字：
//   - 消息（JSON）：只接受登记为 flash 的消息，参数名、类型、长度都要符合，否则不显示（返回零值）；
//   - 其他是还没有迁移的整句提示：原样显示，过长时截断（迁移完成后不再产生）。
//
// 地址可以被任何人构造：提示只是纯文字，前端不把参数当作标签或 HTML。
func parseFlash(v string) uiText {
	if v == "" || len(v) > maxFlashBytes || !utf8.ValidString(v) {
		return uiText{}
	}
	// 消息是 JSON 对象；其他的是旧的整句提示
	if !strings.HasPrefix(v, "{") {
		return rawText(shorten.Text(v, maxLegacyFlashBytes))
	}
	var m textJSON
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || dec.More() {
		return uiText{}
	}
	params, err := checkFlash(m.ID, m.Params)
	if err != nil {
		return uiText{}
	}
	return uiText{id: m.ID, params: params}
}

// checkFlash 检查 flash 中的消息和参数（参数名与登记的完全相同、类型和长度符合），返回规范化的参数。
func checkFlash(id msgID, params map[string]any) (map[string]any, error) {
	spec, ok := messages[id]
	if !ok || !spec.flash {
		return nil, fmt.Errorf("%w：%q 不是可以放进地址的消息", errFlash, id)
	}
	if len(params) != len(spec.params) {
		return nil, fmt.Errorf("%w：%q 的参数个数不对", errFlash, id)
	}
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(params))
	for name, v := range params {
		kind, ok := spec.params[name]
		if !ok {
			return nil, fmt.Errorf("%w：%q 没有参数 %q", errFlash, id, name)
		}
		nv, ok := flashArg(kind, v)
		if !ok {
			return nil, fmt.Errorf("%w：%q 的参数 %q 不合格", errFlash, id, name)
		}
		out[name] = nv
	}
	return out, nil
}

// flashArg 检查一个参数的类型和长度。
func flashArg(kind paramKind, v any) (any, bool) {
	switch kind {
	case paramString:
		s, ok := v.(string)
		return s, ok && utf8.RuneCountInString(s) <= maxFlashArgRunes && !strings.ContainsFunc(s, isControl)
	case paramInt:
		switch n := v.(type) {
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		case int64:
			return n, true
		case int:
			return int64(n), true
		case float64:
			return int64(n), n == math.Trunc(n) && math.Abs(n) < 1<<53
		}
	}
	return nil, false
}

// flashArgText 把原样的文字（对象名、路径、失败原因）整理为能放进 flash 的参数：控制字符换成空格，
// 超过 maxFlashArgRunes 个字符时截断并以省略号结尾，避免整条提示因为参数不合格而不显示。
func flashArgText(s string) string {
	s = strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, shorten.Ellipsis))
	if utf8.RuneCountInString(s) <= maxFlashArgRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxFlashArgRunes-1]) + shorten.Ellipsis
}

// isControl 表示控制字符（换行、制表符等，不应出现在提示的参数中）。
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
