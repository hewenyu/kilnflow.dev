package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 本文件是各测试共用的数据构造辅助：直接用 SQL 写入最少的必填列，被测的是 store 的读取和更新。

// newDB 返回一个独立的、已执行迁移的数据库。
func newDB(t *testing.T) *DB {
	t.Helper()
	return New(pgtest.Pool(t))
}

// mustExec 执行一条写语句，失败时终止测试。
func mustExec(t *testing.T, q Execer, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.WithoutCancel(t.Context()), sql, args...); err != nil {
		t.Fatalf("执行 %q：%v", sql, err)
	}
}

// insertRepo 写入一个启用中、信息完整的仓库。
func insertRepo(t *testing.T, db *DB, id, instID, ownerID int64, owner, name string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES ($1,$2,$3,$4,$5,true,'main','https://example.com/'||$4||'/'||$5||'.git')`, id, instID, ownerID, owner, name)
}

// pgCode 返回 err 中 PostgreSQL 错误的 SQLSTATE；不是 PostgreSQL 错误时返回空字符串。
func pgCode(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code
	}
	return ""
}
