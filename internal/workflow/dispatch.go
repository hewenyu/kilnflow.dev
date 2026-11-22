package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// CheckDispatchInputs 按 workflow 声明的 inputs（decl）检查手动触发时填写的值，返回全部问题（可以直接显示给用户）：
// 未声明的名称、缺少必填项、choice 不在选项中、boolean 不是 true / false。规则与 GitHub Actions 一致。
func CheckDispatchInputs(decl []DispatchInput, inputs map[string]string) []string {
	var problems []string
	known := map[string]bool{}
	for _, in := range decl {
		known[in.Name] = true
		v, ok := inputs[in.Name]
		switch {
		case !ok || v == "":
			if in.Required && in.Default == "" {
				problems = append(problems, "缺少必填的 "+in.Name)
			}
		case in.Type == InputBoolean && !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false"):
			problems = append(problems, in.Name+" 只能是 true 或 false")
		case in.Type == InputChoice && len(in.Options) > 0 && !slices.Contains(in.Options, v):
			problems = append(problems, fmt.Sprintf("%s 只能是 %s 之一", in.Name, strings.Join(in.Options, "、")))
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
		problems = append(problems, "workflow 没有声明 "+k)
	}
	return problems
}
