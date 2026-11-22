package autotag

import (
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestBumpFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		labels []string
		bump   Bump
		skip   bool
	}{
		{nil, BumpPatch, false},
		{[]string{"bug"}, BumpPatch, false},
		{[]string{LabelMinor}, BumpMinor, false},
		{[]string{LabelMinor, LabelMajor}, BumpMajor, false},
		{[]string{LabelMajor, LabelSkip}, BumpPatch, true},
		{[]string{"Release:Major"}, BumpPatch, false}, // label 区分大小写（与 GitHub 的 label 名一致）
	} {
		b, skip := BumpFor(tc.labels)
		if b != tc.bump || skip != tc.skip {
			t.Errorf("BumpFor(%v) = %s, %v，想要 %s, %v", tc.labels, b, skip, tc.bump, tc.skip)
		}
	}
}

func TestNext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prefix string
		tags   []string
		bump   Bump
		want   string
	}{
		{"没有 tag", "v", nil, BumpMajor, "v0.1.0"}, // 第一个版本是 initial，不再递增
		{"patch", "v", []string{"v1.2.3"}, BumpPatch, "v1.2.4"},
		{"minor 清零 patch", "v", []string{"v1.2.3"}, BumpMinor, "v1.3.0"},
		{"major 清零", "v", []string{"v1.2.3"}, BumpMajor, "v2.0.0"},
		{"按数值比较", "v", []string{"v0.1.9", "v0.1.10", "v0.1.2"}, BumpPatch, "v0.1.11"},
		{"预发布不参与", "v", []string{"v1.0.0", "v2.0.0-rc.1"}, BumpPatch, "v1.0.1"},
		{"其他格式不参与", "v", []string{"v1.0.0", "v1.1", "v1.2.0.1", "v01.5.0", "nightly", "v9.9.9+meta"}, BumpPatch, "v1.0.1"},
		{"其他前缀不参与", "v", []string{"v1.0.0", "cli/v5.0.0", "5.0.0"}, BumpPatch, "v1.0.1"},
		{"带目录的前缀", "cli/v", []string{"v9.0.0", "cli/v1.0.0"}, BumpMinor, "cli/v1.1.0"},
		{"没有前缀", "", []string{"v9.0.0", "1.0.0"}, BumpPatch, "1.0.1"},
		{"超过 9 位的数字不参与", "v", []string{"v1.0.0", "v9223372036854775808.0.0", "v1000000000.0.0"}, BumpPatch, "v1.0.1"},
	} {
		if got := Next(tc.prefix, "0.1.0", tc.tags, tc.bump); got != tc.want {
			t.Errorf("%s：Next = %q，想要 %q", tc.name, got, tc.want)
		}
	}
}

// TestNextRealTags：用 Kiln 仓库真实的 tag（2026-11-09 的快照，52 个，含 v0.0.0-docker-test.* 和 docker-test-*）对拍：
// 下一个版本是 v0.1.42。
func TestNextRealTags(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/kiln-tags-2026-11-09.txt")
	if err != nil {
		t.Fatal(err)
	}
	tags := strings.Fields(string(data))
	if len(tags) != 52 {
		t.Fatalf("快照应有 52 个 tag：%d", len(tags))
	}
	if got := Latest("v", tags); got != "v0.1.41" {
		t.Fatalf("Latest = %q，想要 v0.1.41", got)
	}
	if got := Next("v", "0.1.0", tags, BumpPatch); got != "v0.1.42" {
		t.Fatalf("Next = %q，想要 v0.1.42", got)
	}
}

// TestNextProperties：随机的 tag 集合（版本 tag 和各种干扰项）上，Next 的结果总是合法的版本 tag、不在已有的 tag 中、
// 比所有已有的版本 tag 都大；递增的位以下清零。
func TestNextProperties(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	noise := []string{"nightly", "v1.0", "v1.0.0-rc.1", "v01.0.0", "x1.0.0", "v1.0.0.0", "v-1.0.0", ""}
	for range 2000 {
		var tags []string
		var maxV version
		have := false
		for range r.IntN(12) {
			if r.IntN(3) == 0 {
				tags = append(tags, noise[r.IntN(len(noise))])
				continue
			}
			v := version{r.IntN(4), r.IntN(12), r.IntN(30)}
			tags = append(tags, "v"+v.String())
			if !have || maxV.less(v) {
				maxV, have = v, true
			}
		}
		b := Bump(r.IntN(3))
		next := Next("v", "0.1.0", tags, b)
		nv, ok := parseVersion("v", next)
		if !ok {
			t.Fatalf("%v：Next = %q 不是版本 tag", tags, next)
		}
		for _, tg := range tags {
			if tg == next {
				t.Fatalf("%v：Next = %q 已经存在", tags, next)
			}
		}
		if !have {
			if next != "v0.1.0" {
				t.Fatalf("没有版本 tag 时应为 initial：%q", next)
			}
			continue
		}
		if !maxV.less(nv) {
			t.Fatalf("%v：Next = %q 不大于 %s", tags, next, maxV)
		}
		if want := maxV.bump(b); nv != want {
			t.Fatalf("%v %s：Next = %q，想要 v%s", tags, b, next, want)
		}
	}
}

func TestIsVersionTag(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"v1.2.3": true, "v0.0.0": true, "v1.2.3-rc": false, "1.2.3": false, "v1.2": false, "v" + strconv.Itoa(1) + ".0.0": true} {
		if got := IsVersionTag("v", name); got != want {
			t.Errorf("IsVersionTag(v, %q) = %v", name, got)
		}
	}
}
