package publish

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/release"
)

// diffAssets 按名字比较：缺少、多出、大小或摘要不同都算不一致；没有摘要的只比较大小（记为未校验）。
func TestDiffAssets(t *testing.T) {
	t.Parallel()
	a := release.Asset{Name: "a", Size: 1, SHA256: "aa"}
	b := release.Asset{Name: "b", Size: 2, SHA256: "bb"}
	pa := publishedAsset{Name: "a", Size: 1, SHA256: "aa"}
	pb := publishedAsset{Name: "b", Size: 2, SHA256: "bb"}
	for _, tc := range []struct {
		name       string
		published  []publishedAsset
		assets     []release.Asset
		want       string // String()，为空表示一致
		unverified []string
	}{
		{"一致（顺序不同）", []publishedAsset{pb, pa}, []release.Asset{a, b}, "", nil},
		{"缺少", []publishedAsset{pa}, []release.Asset{a, b}, "缺少：b", nil},
		{"多出", []publishedAsset{pa, pb, {Name: "c"}}, []release.Asset{a, b}, "多出：c", nil},
		{"摘要不同", []publishedAsset{pa, {Name: "b", Size: 2, SHA256: "xx"}}, []release.Asset{a, b}, "不同：b", nil},
		{"大小不同", []publishedAsset{pa, {Name: "b", Size: 3, SHA256: "bb"}}, []release.Asset{a, b}, "不同：b", nil},
		{"重复的名字", []publishedAsset{pa, pa}, []release.Asset{a, b}, "缺少：b；多出：a", nil},
		{"没有摘要，大小相同", []publishedAsset{pa, {Name: "b", Size: 2}}, []release.Asset{a, b}, "", []string{"b"}},
		{"没有摘要，大小不同", []publishedAsset{pa, {Name: "b", Size: 9}}, []release.Asset{a, b}, "不同：b", nil},
	} {
		d := diffAssets(tc.published, tc.assets)
		if got := d.String(); got != tc.want || d.same() != (tc.want == "") || !slices.Equal(d.Unverified, tc.unverified) {
			t.Errorf("%s：差异 %q（same=%v，未校验 %v），期望 %q（未校验 %v）", tc.name, got, d.same(), d.Unverified, tc.want, tc.unverified)
		}
	}
	err := publishedConflict("GitHub Release o/r", "v1.0.0", "不同：b")
	if s := err.Error(); !strings.Contains(s, "GitHub Release o/r 的 v1.0.0 已经以不同的内容发布（不同：b）") || !strings.Contains(s, "请发布新的版本") {
		t.Fatalf("错误信息 = %q", s)
	}
}
