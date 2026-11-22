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
)

// 消息的参数名（同一个参数名在各条消息中的含义相同）。
const (
	argLogin = "login"
	// argCount 是数量：复数形式按它选择。
	argCount = "count"
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

// isControl 表示控制字符（换行、制表符等，不应出现在提示的参数中）。
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
