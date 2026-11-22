package web

// formState 是表单提交失败后重新渲染所需的数据：非敏感字段的输入值和各字段的错误。
// 敏感字段（secret 的值、访问令牌）从不放进 Values。
type formState struct {
	Values map[string]string
	// Errors 的键是字段名；formGeneral 是不属于某个字段的错误。
	Errors map[string]string
}

// formGeneral 是 formState.Errors 中不属于某个字段的错误的键。
const formGeneral = "_"

func newForm() *formState {
	return &formState{Values: map[string]string{}, Errors: map[string]string{}}
}

// Val 返回字段的输入值。
func (f *formState) Val(field string) string { return f.Values[field] }

// Err 返回字段的错误。
func (f *formState) Err(field string) string { return f.Errors[field] }

// Failed 表示表单有错误（页面数据中的 Failed，见 json.go）。
func (f *formState) Failed() bool { return len(f.Errors) > 0 }

// fail 记录字段的错误；同一字段只保留第一个。
func (f *formState) fail(field, msg string) {
	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = msg
	}
}
