package pgtest

import (
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

// 每次调用都得到一个独立的、已执行迁移的数据库。
func TestPoolIsolated(t *testing.T) {
	t.Parallel()
	a, b := Pool(t), Pool(t)
	if _, err := a.Exec(t.Context(), `INSERT INTO installations (id, account_id, account_login, account_type) VALUES (1, 1, 'alice', 'User')`); err != nil {
		t.Fatalf("迁移后应有 installations 表：%v", err)
	}
	var n int
	if err := b.QueryRow(t.Context(), `SELECT count(*) FROM installations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("另一个数据库不应看到这条记录，实际 %d 条", n)
	}
	var name string
	if err := a.QueryRow(t.Context(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name == "postgres" {
		t.Fatal("不应连到维护库")
	}
}

func TestParallelCreate(t *testing.T) {
	for range 4 {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			var one int
			if err := Pool(t).QueryRow(t.Context(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
				t.Fatalf("SELECT 1：%v", err)
			}
		})
	}
}
