package docsync

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// TestSameDocRefs：任务 spec 的文档引用与快照逐篇一致才算同一份依据；空 ID、重复 ID、任一字段不同都不算。
// 审计任务校验与迁移前家族依据固化共用这个判断，[A, A] 不能因为按 ID 查到同一篇而被当成 [A] 或 [A, B]。
func TestSameDocRefs(t *testing.T) {
	t.Parallel()
	a := Snapshot{DocID: "a", Title: "甲", Revision: 1, SHA256: "aa", URL: "u-a"}
	b := Snapshot{DocID: "b", Title: "乙", Revision: 2, SHA256: "bb", URL: "u-b"}
	ref := func(s Snapshot) aispec.DocRef {
		return aispec.DocRef{ID: s.DocID, Title: s.Title, Revision: s.Revision, SHA256: s.SHA256, URL: s.URL}
	}
	cases := []struct {
		name  string
		refs  []aispec.DocRef
		snaps []Snapshot
		want  bool
	}{
		{"一致", []aispec.DocRef{ref(a), ref(b)}, []Snapshot{a, b}, true},
		{"顺序无关", []aispec.DocRef{ref(b), ref(a)}, []Snapshot{a, b}, true},
		{"都为空", nil, nil, true},
		{"数量不同", []aispec.DocRef{ref(a)}, []Snapshot{a, b}, false},
		{"内容不同", []aispec.DocRef{{ID: "a", Title: "甲", Revision: 9, SHA256: "aa", URL: "u-a"}}, []Snapshot{a}, false},
		{"缺失", []aispec.DocRef{ref(b)}, []Snapshot{a}, false},
		{"引用重复", []aispec.DocRef{ref(a), ref(a)}, []Snapshot{a, b}, false},
		{"快照重复", []aispec.DocRef{ref(a), ref(a)}, []Snapshot{a, a}, false},
		{"引用空 ID", []aispec.DocRef{{Title: "甲"}}, []Snapshot{{Title: "甲"}}, false},
		{"快照空 ID", []aispec.DocRef{{}}, []Snapshot{{}}, false},
		{"重复的畸形引用不能匹配两篇不同文档", []aispec.DocRef{ref(a), ref(a)}, []Snapshot{a, b}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := SameDocRefs(c.refs, c.snaps); got != c.want {
				t.Fatalf("SameDocRefs = %v，应为 %v", got, c.want)
			}
		})
	}
}
