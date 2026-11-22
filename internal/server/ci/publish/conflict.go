package publish

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/release"
)

// 已发布的版本不可变（PLAN #13.4）：同一版本已经以完全相同的内容发布时幂等成功，内容不同时发布失败，
// 不覆盖、也不静默按成功处理。各发布目标（GitHub Release、npm、镜像、HTTP 上传）都用 publishedConflict 报告。

// publishedConflictError 是同一版本已经以不同内容发布的错误。
type publishedConflictError struct {
	// target 是发布目标（例如 "GitHub Release tester/app"；调用方已经在错误前加了目标名时为空），version 是版本（tag）。
	target, version string
	// detail 说明哪里不同。
	detail string
}

func (e *publishedConflictError) Error() string {
	what := e.version
	if e.target != "" {
		what = e.target + " 的 " + e.version
	}
	return fmt.Sprintf("%s 已经以不同的内容发布（%s）；已发布的版本不能覆盖，请发布新的版本", what, e.detail)
}

// publishedConflict 返回 publishedConflictError。
func publishedConflict(target, version, detail string) error {
	return &publishedConflictError{target: target, version: version, detail: detail}
}

// publishedAsset 是发布目标上已有的一个文件。SHA256 为空表示目标没有提供摘要（只能比较大小）。
type publishedAsset struct {
	Name   string
	Size   int64
	SHA256 string
}

// assetDiff 是已发布的文件与本次要发布的文件的差异。
type assetDiff struct {
	// Missing 是本次有、已发布的没有的；Extra 是已发布的有、本次没有的；Changed 是同名但大小或摘要不同的。
	Missing, Extra, Changed []string
	// Unverified 是目标没有提供摘要、只比较了大小的文件。
	Unverified []string
}

// diffAssets 按名字比较已发布的文件与本次要发布的文件。已发布的清单中有重复的名字时，重复项算作多出。
func diffAssets(published []publishedAsset, assets []release.Asset) assetDiff {
	var d assetDiff
	want := map[string]release.Asset{}
	for _, a := range assets {
		want[a.Name] = a
	}
	seen := map[string]bool{}
	for _, p := range published {
		a, ok := want[p.Name]
		switch {
		case !ok || seen[p.Name]:
			d.Extra = append(d.Extra, p.Name)
		case a.Size != p.Size || (p.SHA256 != "" && p.SHA256 != a.SHA256):
			d.Changed = append(d.Changed, p.Name)
		case p.SHA256 == "":
			d.Unverified = append(d.Unverified, p.Name)
		}
		seen[p.Name] = true
	}
	for _, a := range assets {
		if !seen[a.Name] {
			d.Missing = append(d.Missing, a.Name)
		}
	}
	for _, s := range []*[]string{&d.Missing, &d.Extra, &d.Changed, &d.Unverified} {
		slices.Sort(*s)
	}
	return d
}

// same 判断内容是否一致（只比较了大小的文件不算不一致）。
func (d assetDiff) same() bool { return len(d.Missing)+len(d.Extra)+len(d.Changed) == 0 }

// String 是给用户看的差异说明。
func (d assetDiff) String() string {
	var parts []string
	for _, p := range []struct {
		label string
		names []string
	}{{"缺少", d.Missing}, {"多出", d.Extra}, {"不同", d.Changed}} {
		if len(p.names) > 0 {
			parts = append(parts, p.label+"："+strings.Join(p.names, "、"))
		}
	}
	return strings.Join(parts, "；")
}
