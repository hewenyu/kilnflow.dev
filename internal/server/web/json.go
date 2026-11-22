package web

import "encoding/json"

// 页面数据的 JSON 编码：字段照常输出，另外加上模板时代由方法计算的值（前端直接使用，不重复实现规则）。
// 每个类型先转换为没有方法的同构类型（plain），避免 MarshalJSON 递归。

func (f *formState) MarshalJSON() ([]byte, error) {
	type plain formState
	return json.Marshal(struct {
		*plain
		Failed bool
	}{(*plain)(f), f.Failed()})
}

func (p *runPage) MarshalJSON() ([]byte, error) {
	type plain runPage
	return json.Marshal(struct {
		*plain
		Start, End int64
		Ref        string
		History    bool
	}{(*plain)(p), p.Start(), p.End(), p.Ref(), p.History()})
}

func (f *rerunForm) MarshalJSON() ([]byte, error) {
	type plain rerunForm
	return json.Marshal(struct {
		*plain
		Kept int
	}{(*plain)(f), f.Kept()})
}

func (v runView) MarshalJSON() ([]byte, error) {
	type plain runView
	return json.Marshal(struct {
		plain
		Start, End int64
	}{plain(v), v.Start(), v.End()})
}

func (f runFilter) MarshalJSON() ([]byte, error) {
	type plain runFilter
	return json.Marshal(struct {
		plain
		Active bool
	}{plain(f), f.Active()})
}

func (p *runsPage) MarshalJSON() ([]byte, error) {
	type plain runsPage
	return json.Marshal(struct {
		*plain
		Mine bool
	}{(*plain)(p), p.Mine()})
}

func (s overviewStats) MarshalJSON() ([]byte, error) {
	type plain overviewStats
	return json.Marshal(struct {
		plain
		Rate string
	}{plain(s), s.Rate()})
}

// MarshalJSON 输出 job，但不含 Spec（job spec 只在 job 详情页的 jobPage.Spec 中，避免每 5 秒的刷新都传输全部 job 的 spec）。
func (v jobView) MarshalJSON() ([]byte, error) {
	type plain jobView
	return json.Marshal(struct {
		plain
		Spec *struct{} `json:",omitempty"`
	}{plain: plain(v)})
}

func (p *jobPage) MarshalJSON() ([]byte, error) {
	type plain jobPage
	return json.Marshal(struct {
		*plain
		Publish, NoLog bool
		AttemptKey     string
	}{(*plain)(p), p.Publish(), p.NoLog(), p.AttemptKey()})
}

func (t releaseTargetView) MarshalJSON() ([]byte, error) {
	type plain releaseTargetView
	return json.Marshal(struct {
		plain
		URL string
	}{plain(t), t.URL()})
}

func (v *runnerView) MarshalJSON() ([]byte, error) {
	type plain runnerView
	return json.Marshal(struct {
		*plain
		Accepting bool
		ConnClass string
	}{(*plain)(v), v.Accepting(), v.ConnClass()})
}

func (p *runnersPage) MarshalJSON() ([]byte, error) {
	type plain runnersPage
	return json.Marshal(struct {
		*plain
		Filtered   bool
		StateLabel uiText
	}{(*plain)(p), p.Filtered(), p.StateLabel()})
}

func (p *runnerNewPage) MarshalJSON() ([]byte, error) {
	type plain runnerNewPage
	return json.Marshal(struct {
		*plain
		Command string
	}{(*plain)(p), p.Command()})
}

func (v *dockerView) MarshalJSON() ([]byte, error) {
	type plain dockerView
	return json.Marshal(struct {
		*plain
		URL string
	}{(*plain)(v), v.URL()})
}

func (v *npmView) MarshalJSON() ([]byte, error) {
	type plain npmView
	return json.Marshal(struct {
		*plain
		URL string
	}{(*plain)(v), v.URL()})
}

func (p *accountPage) MarshalJSON() ([]byte, error) {
	type plain accountPage
	return json.Marshal(struct {
		*plain
		Step int
	}{(*plain)(p), p.Step()})
}

func (e activityEntry) MarshalJSON() ([]byte, error) {
	type plain activityEntry
	return json.Marshal(struct {
		plain
		Text, Link string
		RunText    uiText
	}{plain(e), e.Text(), e.Link(), e.RunText()})
}

// scopeJSON 是作用域配置页的计算值：URL 是配置页地址，Query 是作用域的查询参数（前端拼接变量和 secret 的新建、编辑页地址）。
type scopeJSON struct {
	URL, Query string
}

func (p *scopePage) scopeJSON() scopeJSON {
	return scopeJSON{URL: p.URL(), Query: scopeQuery(p.Account, p.Repo, p.Env).Encode()}
}

func (p *scopePage) MarshalJSON() ([]byte, error) {
	type plain scopePage
	return json.Marshal(struct {
		*plain
		scopeJSON
	}{(*plain)(p), p.scopeJSON()})
}

// MarshalJSON 输出变量或 secret 的编辑页。itemPage 嵌入了 *scopePage，必须自己实现，否则会用 scopePage 的编码而丢掉自己的字段。
func (p *itemPage) MarshalJSON() ([]byte, error) {
	type plainScope scopePage
	return json.Marshal(struct {
		*plainScope
		scopeJSON
		Kind     itemKind
		Name     string
		Secret   any
		Form     *formState
		IsSecret bool
	}{(*plainScope)(p.scopePage), p.scopeJSON(), p.Kind, p.Name, p.Secret, p.Form, p.IsSecret()})
}
