package workflow

import (
	"slices"
	"testing"
)

func TestDispatchInputProblems(t *testing.T) {
	decl := []DispatchInput{
		{Name: "version", Type: InputString, Required: true},
		{Name: "mode", Type: InputChoice, Default: "fast", Options: []string{"fast", "full"}},
		{Name: "dry_run", Type: InputBoolean, Default: "false"},
		{Name: "note", Type: InputString, Required: true, Default: "-"},
	}
	for _, c := range []struct {
		name     string
		decl     []DispatchInput
		inputs   map[string]string
		expected []string
	}{
		{"valid", decl, map[string]string{"version": "1", "mode": "full", "dry_run": "TRUE"}, nil},
		{"defaults satisfy required", decl, map[string]string{"version": "1", "note": ""}, nil},
		{
			"all problems", decl,
			map[string]string{"mode": "slow", "dry_run": "yes", "extra": "1", "another": "2"},
			[]string{"缺少必填的 version", "mode 只能是 fast、full 之一", "dry_run 只能是 true 或 false", "workflow 没有声明 another", "workflow 没有声明 extra"},
		},
		{"empty required", decl, map[string]string{"version": ""}, []string{"缺少必填的 version"}},
		{"no declarations", nil, map[string]string{"a": "1"}, []string{"workflow 没有声明 a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, p := range DispatchInputProblems(c.decl, c.inputs) {
				got = append(got, p.String())
			}
			if !slices.Equal(got, c.expected) {
				t.Errorf("DispatchInputProblems = %q，应为 %q", got, c.expected)
			}
		})
	}
}

// TestDispatchInputProblemCodes：问题带稳定的原因码和原始参数（Dashboard 据此按语言显示，不解析中文说明）。
func TestDispatchInputProblemCodes(t *testing.T) {
	decl := []DispatchInput{
		{Name: "version", Type: InputString, Required: true},
		{Name: "mode", Type: InputChoice, Options: []string{"fast", "full"}},
		{Name: "dry_run", Type: InputBoolean},
	}
	got := DispatchInputProblems(decl, map[string]string{"mode": "slow", "dry_run": "yes", "extra": "1"})
	want := []InputProblem{
		{Code: InputMissing, Name: "version"},
		{Code: InputNotChoice, Name: "mode", Options: []string{"fast", "full"}},
		{Code: InputNotBoolean, Name: "dry_run"},
		{Code: InputUndeclared, Name: "extra"},
	}
	if !slices.EqualFunc(got, want, func(a, b InputProblem) bool {
		return a.Code == b.Code && a.Name == b.Name && slices.Equal(a.Options, b.Options)
	}) {
		t.Errorf("DispatchInputProblems = %+v，应为 %+v", got, want)
	}
}
