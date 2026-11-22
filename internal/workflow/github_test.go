package workflow

import (
	"maps"
	"slices"
	"testing"
)

func TestPullRef(t *testing.T) {
	if got := PullRef(42); got != "refs/pull/42/head" {
		t.Errorf("PullRef(42) = %q", got)
	}
}

func TestRefInfo(t *testing.T) {
	cases := []struct {
		ref, wantType, wantName string
	}{
		{"refs/heads/main", RefTypeBranch, "main"},
		{"refs/heads/feat/x", RefTypeBranch, "feat/x"},
		{"refs/tags/v1.2.3", RefTypeTag, "v1.2.3"},
		{"refs/pull/7/head", RefTypeBranch, "7/head"}, // 与 Actions 一致
		{"main", RefTypeBranch, "main"},
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			typ, name := RefInfo(c.ref)
			if typ != c.wantType || name != c.wantName {
				t.Errorf("RefInfo(%q) = %q, %q，期望 %q, %q", c.ref, typ, name, c.wantType, c.wantName)
			}
		})
	}
}

// Map 的键与 JSON 字段名相同，并且所有字段总是存在。
func TestGitHubContextMap(t *testing.T) {
	g := &GitHubContext{
		EventName: "push", SHA: "s", Ref: "refs/heads/main", RefName: "main", RefType: "branch",
		Repository: "o/r", RepositoryOwner: "o", Actor: "a",
	}
	m := g.Map()
	want := []string{"actor", "base_ref", "event_name", "head_ref", "ref", "ref_name", "ref_type", "repository", "repository_owner", "sha"}
	if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
		t.Errorf("键 = %v，期望 %v", got, want)
	}
	if m["event_name"] != "push" || m["sha"] != "s" || m["repository_owner"] != "o" || m["actor"] != "a" || m["head_ref"] != "" {
		t.Errorf("Map() = %v", m)
	}

	// 可以直接用作 PlanContext.GitHub，引用空字段不报错
	pc := PlanContext{GitHub: m}
	p := mustExpand(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, steps: [{ run: \"${{ github.head_ref }}-${{ github.actor }}\" }] }\n", pc)
	if got := planJob(t, p, "a").Spec.Steps[0].Run; got != "-a" {
		t.Errorf("steps.run = %q", got)
	}
}
