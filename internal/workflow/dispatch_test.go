package workflow

import (
	"slices"
	"testing"
)

func TestCheckDispatchInputs(t *testing.T) {
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
			if got := CheckDispatchInputs(c.decl, c.inputs); !slices.Equal(got, c.expected) {
				t.Errorf("CheckDispatchInputs = %q，应为 %q", got, c.expected)
			}
		})
	}
}
