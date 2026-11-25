package web

// formState 是表单提交失败后重新渲染所需的数据：非敏感字段的输入值和各字段的错误。
// 敏感字段（secret 的值、访问令牌）从不放进 Values。
type formState struct {
	Values map[string]string
	// Errors 的键是字段名；formGeneral 是不属于某个字段的错误。错误是消息或原样的文字（见 text.go）。
	Errors map[string]uiText
}

// formGeneral 是 formState.Errors 中不属于某个字段的错误的键。
const formGeneral = "_"

func newForm() *formState {
	return &formState{Values: map[string]string{}, Errors: map[string]uiText{}}
}

// Val 返回字段的输入值。
func (f *formState) Val(field string) string { return f.Values[field] }

// Err 返回字段的错误（原样的文字；消息返回消息键，只用于判断和测试）。
func (f *formState) Err(field string) string {
	t := f.Errors[field]
	if t.id != "" {
		return string(t.id)
	}
	return t.raw
}

// Failed 表示表单有错误（页面数据中的 Failed，见 json.go）。
func (f *formState) Failed() bool { return len(f.Errors) > 0 }

// failText 记录字段的错误（消息或原样的文字）；同一字段只保留第一个。
func (f *formState) failText(field string, t uiText) {
	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = t
	}
}
