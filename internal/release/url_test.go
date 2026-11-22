package release

import "testing"

func TestSafeWebURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://example.com/a.zip", true},
		{"http://example.com/a.zip", true},
		{"https://example.com:8443/a?sig=x", true},
		{"https://example.com/a.zip#frag", true},
		{"javascript:void(0)", false},
		{"data:text/html;base64,PHNjcmlwdD4=", false},
		{"mailto:someone@example.com", false},
		{"file:///outside/file.txt", false},
		{"//example.com/a.zip", false},
		{"/a.zip", false},
		{"", false},
		{"https://user:pass@example.com/a.zip", false},
		{"https:///a.zip", false},
		{`https://example.com\@evil.com/a.zip`, false},
		{"https://example.com/a\x00.zip", false},
		{"https://example.com/a\n.zip", false},
	}
	for _, c := range cases {
		if got := SafeWebURL(c.raw); got != c.want {
			t.Errorf("SafeWebURL(%q) = %v，期望 %v", c.raw, got, c.want)
		}
	}
}
