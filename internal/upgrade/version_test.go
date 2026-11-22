package upgrade

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"0.1.41": true, "1.0.0-rc.1": true, "0.0.0-stage": true, "v0.1.41": false, "0.1": false, "01.0.0": false,
		"../0.1.0": false, "0.1.0/../x": false, "0.1.0-a..b": false, "0.1.0-": false, "0.1.0\t": false, "": false, "0.1.0+meta": false,
		// 预发布的每一段都可以含连字符（semver 允许，独立审查发现）
		"1.3.0-rc-1": true, "1.3.0-alpha-beta.2": true, "1.3.0--": true,
		// 长度上限（启动器只认这么长的目录名）
		"1.1.0-alpha." + strings.Repeat("a", MaxVersionLen-12): true, "1.1.0-alpha." + strings.Repeat("a", MaxVersionLen-11): false,
	} {
		if Valid(v) != want {
			t.Errorf("Valid(%q) = %v", v, !want)
		}
	}
	if Normalize("v0.1.41") != "0.1.41" || Normalize("dev-abc") != "" {
		t.Error("Normalize")
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		latest, cur string
		want        bool
	}{
		{"0.1.42", "v0.1.41", true},
		{"0.1.41", "v0.1.41", false},
		{"0.1.40", "v0.1.41", false},        // 不降级
		{"0.1.41", "v0.1.41-rc.1", true},    // 预发布低于正式版本
		{"0.1.41", "dev-abc", true},         // 开发构建：明确点击时可以升级
		{"", "v0.1.41", false},              // 没有 latest
		{"not-a-version", "dev-abc", false}, // latest 无效
	} {
		if got := Newer(tc.latest, tc.cur); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.latest, tc.cur, got)
		}
	}
}

func TestPackageName(t *testing.T) {
	t.Parallel()
	if got := PackageName(ProgramCI, "darwin", "arm64"); got != "@yuebanlaosiji/kilnflow-runner-darwin-arm64" {
		t.Errorf("PackageName = %q", got)
	}
	if PackageFile(ProgramCI) != "package/kiln-runner" {
		t.Error("PackageFile")
	}
	if Registry("") != DefaultRegistry || Registry(" ") != DefaultRegistry || Registry("https://r.example/") != "https://r.example/" {
		t.Error("Registry")
	}
}
