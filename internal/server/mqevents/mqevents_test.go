package mqevents

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const repoID = 7

// outboxKinds 返回 outbox 中各 dedupe_key 的类型。
func outboxKinds(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(t.Context(), `SELECT dedupe_key, kind FROM outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, kind string
		if err := rows.Scan(&k, &kind); err != nil {
			t.Fatal(err)
		}
		out[k] = kind
	}
	return out
}

func candidates(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM merge_auto_candidates WHERE repo_id=$1`, repoID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNominateAutoMerge：PR 不在队列中时记下候选并通知合并队列；已在队列中、没有配置 GitHub App 时什么也不写。
func TestNominateAutoMerge(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	w := outbox.NewWriter(true)

	if err := NominateAutoMerge(t.Context(), outbox.NewWriter(false), db, repoID, 1); err != nil {
		t.Fatal(err)
	}
	if candidates(t, db) != 0 || len(outboxKinds(t, db)) != 0 {
		t.Fatal("没有配置 GitHub App 时不应写入")
	}

	if _, err := db.Exec(t.Context(), `INSERT INTO merge_queue (repo_id, pr_number) VALUES ($1, 2)`, repoID); err != nil {
		t.Fatal(err)
	}
	if err := NominateAutoMerge(t.Context(), w, db, repoID, 2); err != nil {
		t.Fatal(err)
	}
	if candidates(t, db) != 0 || len(outboxKinds(t, db)) != 0 {
		t.Fatal("已在队列中的 PR 不应提名")
	}

	if err := NominateAutoMerge(t.Context(), w, db, repoID, 1); err != nil {
		t.Fatal(err)
	}
	if candidates(t, db) != 1 {
		t.Fatal("应记下候选")
	}
	if k := outboxKinds(t, db)[mergeQueueKey(repoID)]; k != KindMergeQueue {
		t.Fatalf("应通知合并队列，outbox = %v", outboxKinds(t, db))
	}
}

// TestKickMergeQueue：只在仓库（或指定的 PR）有排队中的 PR 时写入通知。
func TestKickMergeQueue(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	w := outbox.NewWriter(true)
	kick := func(pr int) bool {
		t.Helper()
		if _, err := db.Exec(t.Context(), `DELETE FROM outbox`); err != nil {
			t.Fatal(err)
		}
		if err := KickMergeQueue(t.Context(), w, db, repoID, pr); err != nil {
			t.Fatal(err)
		}
		return outboxKinds(t, db)[mergeQueueKey(repoID)] == KindMergeQueue
	}
	if kick(0) {
		t.Fatal("队列为空时不应通知")
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO merge_queue (repo_id, pr_number) VALUES ($1, 3), ($1, 4)`, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE merge_queue SET state='merged' WHERE pr_number=4`); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		pr   int
		want bool
	}{{0, true}, {3, true}, {4, false}, {5, false}}
	for _, c := range cases {
		if got := kick(c.pr); got != c.want {
			t.Errorf("KickMergeQueue(pr=%d) 通知 = %v，期望 %v", c.pr, got, c.want)
		}
	}
}

// TestEnqueue：合并队列、自动打 tag 的通知按仓库合并为一条。
func TestEnqueue(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	w := outbox.NewWriter(true)
	for range 2 {
		if err := EnqueueMergeQueue(t.Context(), w, db, repoID); err != nil {
			t.Fatal(err)
		}
		if err := EnqueueAutoTag(t.Context(), w, db, repoID); err != nil {
			t.Fatal(err)
		}
	}
	got := outboxKinds(t, db)
	if len(got) != 2 || got[mergeQueueKey(repoID)] != KindMergeQueue || got["autotag:repo:7"] != KindAutoTag {
		t.Fatalf("outbox = %v", got)
	}
}
