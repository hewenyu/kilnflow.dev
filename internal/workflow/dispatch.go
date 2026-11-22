package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// InputProblemCode 是手动触发时一个 input 的问题类型（稳定的原因码，Dashboard 据此按语言显示）。
type InputProblemCode string

const (
	// InputMissing：缺少必填的 input（没有默认值）。
	InputMissing InputProblemCode = "missing"
	// InputNotBoolean：boolean 的值不是 true / false。
	InputNotBoolean InputProblemCode = "not_boolean"
	// InputNotChoice：choice 的值不在 Options 中。
	InputNotChoice InputProblemCode = "not_choice"
	// InputUndeclared：workflow 没有声明这个 input。
	InputUndeclared InputProblemCode = "undeclared"
)

// InputProblem 是手动触发时一个 input 的问题：Name 是 input 的名称，Options 是 choice 的可选值（只有 InputNotChoice 有）。
type InputProblem struct {
	Code    InputProblemCode
	Name    string
	Options []string
}

// String 返回问题的中文说明（CLI 和日志使用；Dashboard 按 Code 显示）。
func (p InputProblem) String() string {
	switch p.Code {
	case InputMissing:
		return "缺少必填的 " + p.Name
	case InputNotBoolean:
		return p.Name + " 只能是 true 或 false"
	case InputNotChoice:
		return fmt.Sprintf("%s 只能是 %s 之一", p.Name, strings.Join(p.Options, "、"))
	case InputUndeclared:
		return "workflow 没有声明 " + p.Name
	}
	return string(p.Code) + "：" + p.Name
}

// DispatchInputProblems 按 workflow 声明的 inputs（decl）检查手动触发时填写的值，返回全部问题：
// 缺少必填项、boolean 不是 true / false、choice 不在选项中（按声明的顺序），以及未声明的名称（按名称排序）。规则与 GitHub Actions 一致。
func DispatchInputProblems(decl []DispatchInput, inputs map[string]string) []InputProblem {
	var problems []InputProblem
	known := map[string]bool{}
	for _, in := range decl {
		known[in.Name] = true
		v, ok := inputs[in.Name]
		switch {
		case !ok || v == "":
			if in.Required && in.Default == "" {
				problems = append(problems, InputProblem{Code: InputMissing, Name: in.Name})
			}
		case in.Type == InputBoolean && !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false"):
			problems = append(problems, InputProblem{Code: InputNotBoolean, Name: in.Name})
		case in.Type == InputChoice && len(in.Options) > 0 && !slices.Contains(in.Options, v):
			problems = append(problems, InputProblem{Code: InputNotChoice, Name: in.Name, Options: in.Options})
		}
	}
	var unknown []string
	for k := range inputs {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	for _, k := range unknown {
		problems = append(problems, InputProblem{Code: InputUndeclared, Name: k})
	}
	return problems
}
