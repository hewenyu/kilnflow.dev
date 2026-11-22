package outbox

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestList：按状态筛选，按 ID 倒序，最多 limit 条；摘要包含 dedupe_key、类型、次数和最近的错误。
func TestList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`INSERT INTO outbox (dedupe_key, kind, state, attempts, last_error) VALUES
		('a', 'k', 'done', 0, NULL), ('b', 'k', 'dead', 20, 'boom'), ('c', 'k', 'pending', 3, 'later'), ('d', 'k', 'dead', 20, 'again')`)
	tests := []struct {
		name   string
		states []string
		limit  int
		want   []string
	}{
		{"只看 dead", []string{store.OutboxDead}, 10, []string{"d", "b"}},
		{"dead 和 pending", []string{store.OutboxDead, store.OutboxPending}, 10, []string{"d", "c", "b"}},
		{"限制条数", []string{store.OutboxDead, store.OutboxPending, store.OutboxDone}, 2, []string{"d", "c"}},
		{"没有匹配", []string{"other"}, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := List(h.ctx, h.db, tt.states, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, it := range items {
				keys = append(keys, it.DedupeKey)
			}
			if !slices.Equal(keys, tt.want) {
				t.Fatalf("List = %v，期望 %v", keys, tt.want)
			}
		})
	}
	items, err := List(h.ctx, h.db, []string{store.OutboxDead}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if it := items[0]; it.Kind != "k" || it.State != store.OutboxDead || it.Attempts != 20 || it.LastError != "again" || it.UpdatedAt.IsZero() {
		t.Fatalf("摘要 = %+v", it)
	}
}

// TestRetry：手动重试把记录重新排队（pending、清零次数、立即投递），之后由 worker 照常投递；记录不存在时返回 ErrNotFound。
func TestRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	n := h.newItem()
	h.enqueue(n)
	key := testKey(n)
	h.exec(`UPDATE outbox SET state='dead', attempts=20, next_attempt_at=now()+interval '1 day' WHERE dedupe_key=$1`, key)
	id := h.mustRow(key).ID

	var hooked []byte
	hooks := map[string]RetryHook{testKind: func(_ context.Context, _ pgx.Tx, payload []byte) error { hooked = payload; return nil }}
	h.tx(func(tx pgx.Tx) error { return Retry(h.ctx, tx, id, hooks) })
	if len(hooked) == 0 {
		t.Fatal("重试时应调用这类记录注册的 RetryHook")
	}
	if r := h.mustRow(key); r.State != store.OutboxPending || r.Attempts != 0 {
		t.Fatalf("重试后 = %+v，期望 pending、attempts=0", r)
	}
	h.deliver()
	if r := h.mustRow(key); r.State != store.OutboxDone {
		t.Fatalf("重试后投递 = %+v，期望 done", r)
	}
	if c := h.checks(testExternalID(n)); c != 1 {
		t.Fatalf("Check Run 有 %d 个，期望 1 个", c)
	}

	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error { return Retry(h.ctx, tx, 987654, hooks) })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("重试不存在的记录：%v，期望 ErrNotFound", err)
	}
}
