package store

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestListRunners(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	mustExec(t, db, `INSERT INTO runners (id, name, kind, public_key, version, state, drain, last_seen_at) VALUES
		(1, 'mac-mini', 'ci', '\x00', 'v0.1.0', 'active', false, '2026-10-01T00:00:00Z'),
		(2, 'builder', 'ci', '\x00', 'v0.2.0', 'revoked', true, NULL),
		(3, 'zeta', 'ai', '\x00', '', 'active', true, NULL)`)

	t.Run("字段完整读出", func(t *testing.T) {
		t.Parallel()
		got, err := ListRunners(t.Context(), db, RunnerFilter{ID: 1})
		if err != nil {
			t.Fatal(err)
		}
		seen := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		want := &Runner{ID: 1, Name: "mac-mini", Kind: RunnerKindCI, Version: "v0.1.0", State: RunnerActive, LastSeen: &seen}
		if len(got) != 1 {
			t.Fatalf("ListRunners = %d 台，应为 1 台", len(got))
		}
		if !got[0].LastSeen.Equal(seen) {
			t.Errorf("LastSeen = %v，应为 %v", got[0].LastSeen, seen)
		}
		got[0].LastSeen = &seen
		if !reflect.DeepEqual(got[0], want) {
			t.Errorf("ListRunners = %+v，应为 %+v", got[0], want)
		}
		if got[0].Revoked() {
			t.Error("active 的 Runner 不应判为已吊销")
		}
	})

	t.Run("没有联系过、已吊销", func(t *testing.T) {
		t.Parallel()
		got, err := ListRunners(t.Context(), db, RunnerFilter{Name: "builder"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("ListRunners = %d 台，应为 1 台", len(got))
		}
		r := got[0]
		if r.LastSeen != nil || !r.Drain || !r.Revoked() {
			t.Errorf("LastSeen=%v Drain=%v Revoked=%v", r.LastSeen, r.Drain, r.Revoked())
		}
	})

	cases := []struct {
		name   string
		filter RunnerFilter
		want   []string
	}{
		{"零值列出全部并按名称排序", RunnerFilter{}, []string{"builder", "mac-mini", "zeta"}},
		{"按 ID", RunnerFilter{ID: 3}, []string{"zeta"}},
		{"按名称", RunnerFilter{Name: "mac-mini"}, []string{"mac-mini"}},
		{"按类型", RunnerFilter{Kind: RunnerKindAI}, []string{"zeta"}},
		{"ID 和名称同时满足", RunnerFilter{ID: 2, Name: "builder"}, []string{"builder"}},
		{"ID 和名称不一致", RunnerFilter{ID: 1, Name: "builder"}, nil},
		{"ID 和类型不一致", RunnerFilter{ID: 1, Kind: RunnerKindAI}, nil},
		{"不存在的 ID", RunnerFilter{ID: 99}, nil},
		{"名称区分大小写", RunnerFilter{Name: "Builder"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ListRunners(t.Context(), db, c.filter)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, r := range got {
				names = append(names, r.Name)
			}
			if !slices.Equal(names, c.want) {
				t.Fatalf("ListRunners(%+v) = %v，应为 %v", c.filter, names, c.want)
			}
		})
	}
}
