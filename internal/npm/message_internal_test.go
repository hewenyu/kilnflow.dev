package npm

import "testing"

func TestJoinMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   []any
		want string
	}{
		{[]any{"Not found", nil}, "Not found"},
		{[]any{nil, "boom"}, "boom"},
		{[]any{"forbidden", "no access"}, "forbidden：no access"},
		{[]any{"same", "same"}, "same"},
		{[]any{map[string]any{"code": "E1"}, nil}, `{"code":"E1"}`},
	}
	for _, tt := range tests {
		if got := joinMessage(tt.in...); got != tt.want {
			t.Errorf("joinMessage(%v) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}
