package web

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 多语言的消息契约（text.go、docs/I18N.md）：服务端给出的消息必须在前端的两种语言的词典中都有、参数一致；
// 地址中的提示（?flash=）只接受登记的消息和合格的参数。

// localesDir 是前端词典所在的目录（相对本包）。
const localesDir = "../../../web/src/i18n/locales"

// testLocales 是前端支持的语言（web/src/i18n/locale-id.ts）。
var testLocales = []string{"zh-CN", "en"}

// loadDict 读取语言 locale 的词典：值是文案，或复数的各个形式（{"one": …, "other": …}）。
func loadDict(t *testing.T, locale string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(localesDir, locale+".json"))
	if err != nil {
		t.Fatalf("读取词典 %s：%v", locale, err)
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("解析词典 %s：%v", locale, err)
	}
	return d
}

// entryForms 返回词典中一条文案的全部写法（复数文案的每个形式）。
func entryForms(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var forms map[string]string
	if err := json.Unmarshal(raw, &forms); err != nil || forms["other"] == "" {
		t.Fatalf("词典的值应为文案或带 other 的复数形式：%s", raw)
	}
	return slices.Collect(maps.Values(forms))
}

// placeholderRe 与前端 messages.ts 的占位符相同：{name}、{name, number}、{name, time}、{name, duration}。
var placeholderRe = regexp.MustCompile(`\{(\w+)(?:,\s*(number|time|duration))?\}`)

// entryParams 返回文案中的参数名和格式（复数的各个形式合在一起）。
func entryParams(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range entryForms(t, raw) {
		for _, m := range placeholderRe.FindAllStringSubmatch(f, -1) {
			out[m[1]] = m[2]
		}
	}
	return out
}

// textZH 按中文词典显示 uiText（与前端的规则相同：复数取 other，数字原样），测试据此检查页面上的中文。
func textZH(t *testing.T, x uiText) string {
	t.Helper()
	if x.id == "" {
		return x.raw
	}
	raw, ok := loadDict(t, "zh-CN")[string(x.id)]
	if !ok {
		t.Fatalf("中文词典中没有 %q", x.id)
	}
	forms := entryForms(t, raw)
	s := forms[0]
	return placeholderRe.ReplaceAllStringFunc(s, func(p string) string {
		m := placeholderRe.FindStringSubmatch(p)
		name := m[1]
		v, ok := x.params[name]
		if !ok {
			t.Errorf("%q 缺少参数 %q", x.id, name)
			return "…"
		}
		if m[2] == "duration" {
			// 时长（秒）：与前端相同，整分钟显示为分钟，其余显示为秒
			return humanDuration(time.Duration(toInt64(t, v)) * time.Second)
		}
		if f, ok := v.(float64); ok {
			// 从页面数据解码的数字是 float64：按整数显示，与前端相同
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return fmt.Sprint(v)
	})
}

// toInt64 返回整数参数的值（直接构造的是 int64，从页面数据解码的是 float64）。
func toInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i
		}
	}
	t.Errorf("参数 %v 不是整数", v)
	return 0
}

// textsZH 按中文词典显示一组 uiText。
func textsZH(t *testing.T, xs []uiText) []string {
	t.Helper()
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, textZH(t, x))
	}
	return out
}

// flashZH 按中文词典显示地址中的提示（经 parseFlash 校验，与页面上显示的相同）。
func flashZH(t *testing.T, v string) string {
	t.Helper()
	return textZH(t, parseFlash(v))
}

// wantPageZH 检查页面 body 的名称、导航和中文标题。
func wantPageZH(t *testing.T, body, page, nav, title string) {
	t.Helper()
	if pg := parsePage(t, body); pg.Page != page || pg.Nav != nav || textZH(t, pg.Title) != title {
		t.Fatalf("页面 = %q，导航 = %q，标题 = %q；应为 %q、%q、%q", pg.Page, pg.Nav, textZH(t, pg.Title), page, nav, title)
	}
}

// pageTextZH 返回页面上显示的主要文字（中文）：错误页的标题和原因，登录页的说明和表单错误。
func pageTextZH(t *testing.T, body string) string {
	t.Helper()
	switch pg := parsePage(t, body); pg.Page {
	case "error":
		p := pageData[errorPage](t, body)
		return textZH(t, p.Heading) + "\n" + textZH(t, p.Reason)
	case "login":
		p := pageData[loginData](t, body)
		out := []string{textZH(t, p.Notice)}
		for _, e := range p.Form.Errors {
			out = append(out, textZH(t, e))
		}
		return strings.Join(out, "\n")
	default:
		t.Fatalf("不是错误页或登录页：%s", pg.Page)
		return ""
	}
}

// TestMessagesInDictionaries：服务端的每条消息在两种语言的词典中都有，参数名与登记的相同；
// 按数字、时间格式化的参数必须是整数。
func TestMessagesInDictionaries(t *testing.T) {
	for _, locale := range testLocales {
		dict := loadDict(t, locale)
		for id, spec := range messages {
			raw, ok := dict[string(id)]
			if !ok {
				t.Errorf("%s 词典中没有服务端的消息 %q", locale, id)
				continue
			}
			got := entryParams(t, raw)
			if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(spec.params))) {
				t.Errorf("%s 词典中 %q 的参数 %v，服务端登记的是 %v", locale, id, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(spec.params)))
			}
			for name, format := range got {
				if format != "" && spec.params[name] != paramInt {
					t.Errorf("%s 词典中 %q 的参数 %q 按 %s 格式化，服务端应登记为整数", locale, id, name, format)
				}
			}
		}
	}
}

// TestMsgIDsRegistered：包中声明的每个 msgID 常量都登记在 messages 中（没有登记的消息不经过契约检查）。
func TestMsgIDsRegistered(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "msgID" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok {
						t.Errorf("%s：msgID 常量应为字符串字面量", fset.Position(v.Pos()))
						continue
					}
					s, _ := strconv.Unquote(lit.Value)
					n++
					if _, ok := messages[msgID(s)]; !ok {
						t.Errorf("%s：消息 %q 没有登记在 messages 中", fset.Position(v.Pos()), s)
					}
				}
			}
		}
	}
	if n != len(messages) {
		t.Errorf("声明了 %d 个 msgID 常量，messages 中登记了 %d 条", n, len(messages))
	}
}

func TestUITextJSON(t *testing.T) {
	for _, c := range []struct {
		text uiText
		want string
	}{
		{uiText{}, `""`},
		{rawText("仓库 tester/demo"), `"仓库 tester/demo"`},
		{message(msgLoginNoticeLogout), `{"id":"login.noticeLogout"}`},
		{message(msgErrForbiddenReason, strArg(argLogin, "bob")), `{"id":"error.forbiddenReason","params":{"login":"bob"}}`},
		{message(msgErrOAuthExpReason, intArg(argCount, 10)), `{"id":"error.oauthExpiredReason","params":{"count":10}}`},
	} {
		b, err := json.Marshal(c.text)
		if err != nil || string(b) != c.want {
			t.Errorf("编码 %+v = %s（%v），应为 %s", c.text, b, err, c.want)
		}
		var back uiText
		if err := json.Unmarshal(b, &back); err != nil || back.id != c.text.id || back.raw != c.text.raw {
			t.Errorf("解码 %s = %+v（%v）", b, back, err)
		}
	}
}

func TestParseFlash(t *testing.T) {
	bound := func(login string) string {
		v, err := flashValue(message(msgAccountBound, strArg(argLogin, login)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := flashZH(t, bound("octocat")); got != "已绑定 GitHub 账号 octocat，现在以 octocat 的身份登录。" {
		t.Errorf("绑定的提示 = %q", got)
	}
	if got := parseFlash(`{"id":"account.unbound"}`); got.id != msgAccountUnbound || got.params != nil {
		t.Errorf("没有参数的提示 = %+v", got)
	}
	// 不合格的消息不显示：不认识、不是 flash、参数多了少了或类型不对、过长、带控制字符、JSON 后面还有内容
	for _, v := range []string{
		`{"id":"nope.nope"}`,
		`{"id":"error.forbiddenReason","params":{"login":"bob"}}`,
		`{"id":"account.unbound","params":{"login":"x"}}`,
		`{"id":"account.bound"}`,
		`{"id":"account.bound","params":{"login":7}}`,
		`{"id":"account.bound","params":{"who":"x"}}`,
		`{"id":"account.bound","params":{"login":"` + strings.Repeat("长", maxFlashArgRunes+1) + `"}}`,
		`{"id":"account.bound","params":{"login":"a\nb"}}`,
		`{"id":"account.unbound"}{"id":"account.unbound"}`,
		`{"id":`,
		`{` + strings.Repeat(" ", maxFlashBytes) + `}`,
		"\xff\xfe",
	} {
		if got := parseFlash(v); !got.IsZero() {
			t.Errorf("parseFlash(%q) = %+v，应不显示", v, got)
		}
	}
	// 还没有迁移的整句提示原样显示，过长的截断
	if got := parseFlash("已请求取消"); got.raw != "已请求取消" || got.id != "" {
		t.Errorf("旧的提示 = %+v", got)
	}
	if got := parseFlash(strings.Repeat("长", 300)); got.id != "" || len(got.raw) > maxLegacyFlashBytes+len(shorten.Ellipsis) || got.raw == "" {
		t.Errorf("过长的旧提示应截断：%d 字节", len(got.raw))
	}
	// 只有登记为 flash 的消息可以放进地址
	if _, err := flashValue(message(msgErrForbiddenReason, strArg(argLogin, "bob"))); err == nil {
		t.Error("不是 flash 的消息不应能放进地址")
	}
	if _, err := flashValue(message(msgAccountBound, strArg(argLogin, strings.Repeat("x", maxFlashArgRunes+1)))); err == nil {
		t.Error("过长的参数不应能放进地址")
	}
}

// TestFlashInPage：页面数据中的提示是校验后的消息；不合格的提示不出现在页面数据中。
func TestFlashInPage(t *testing.T) {
	_, mux, _ := testDashboard(t, "")
	for _, c := range []struct {
		flash string
		want  string
	}{
		{`{"id":"account.unbound"}`, `{"id":"account.unbound"}`},
		{`{"id":"account.bound","params":{"login":"octocat"}}`, `{"id":"account.bound","params":{"login":"octocat"}}`},
		{`{"id":"error.csrfReason"}`, `""`},
		{"已保存", `"已保存"`},
	} {
		for _, browser := range []bool{false, true} {
			w := serve(mux, testReq{target: "/no/such/page?flash=" + url.QueryEscape(c.flash), browser: browser})
			b, _ := json.Marshal(parsePage(t, w.Body.String()).Flash)
			if got := string(b); got != c.want {
				t.Errorf("flash=%s（浏览器 %v）：页面数据中的 Flash = %s，应为 %s", c.flash, browser, got, c.want)
			}
		}
	}
}
