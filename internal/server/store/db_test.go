package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// raiseSQL 让 PostgreSQL 返回指定 SQLSTATE 的错误（模拟死锁、序列化失败等）。
func raiseSQL(code string) string {
	return `DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '` + code + `'; END $$`
}

func TestDB_Tx(t *testing.T) {
	t.Parallel()
	errApp := errors.New("业务错误")
	cases := []struct {
		name string
		// failures 是前几次尝试返回的错误（之后的尝试成功）。failCode 非空时用 PostgreSQL 错误，否则用 errApp。
		failCode string
		failures int
		// wantAttempts 是 f 被调用的次数；wantCommitted 表示最后一次写入是否提交。
		wantAttempts  int
		wantCommitted bool
		wantCode      string
		wantAppErr    bool
	}{
		{name: "一次成功", wantAttempts: 1, wantCommitted: true},
		{name: "序列化失败后重试成功", failCode: pgSerializationFailure, failures: 2, wantAttempts: 3, wantCommitted: true},
		{name: "死锁后重试成功", failCode: pgDeadlockDetected, failures: 1, wantAttempts: 2, wantCommitted: true},
		{name: "一直序列化失败时最多尝试 txMaxAttempts 次", failCode: pgSerializationFailure, failures: 100, wantAttempts: txMaxAttempts, wantCode: pgSerializationFailure},
		{name: "唯一约束冲突不重试", failCode: "23505", failures: 100, wantAttempts: 1, wantCode: "23505"},
		{name: "非数据库错误不重试", failures: 100, wantAttempts: 1, wantAppErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := newDB(t)
			attempts := 0
			err := db.Tx(t.Context(), func(tx pgx.Tx) error {
				attempts++
				// 每次尝试都先写入一行：失败的尝试必须整体回滚
				if _, err := tx.Exec(t.Context(), `INSERT INTO kv (key, value) VALUES ('k', $1)`, strconv.Itoa(attempts)); err != nil {
					return err
				}
				if attempts > c.failures {
					return nil
				}
				if c.failCode == "" {
					return errApp
				}
				_, err := tx.Exec(t.Context(), raiseSQL(c.failCode))
				return err
			})
			if attempts != c.wantAttempts {
				t.Errorf("f 被调用 %d 次，应为 %d 次", attempts, c.wantAttempts)
			}
			if got := pgCode(err); got != c.wantCode {
				t.Errorf("错误的 SQLSTATE 为 %q，应为 %q（err=%v）", got, c.wantCode, err)
			}
			if errors.Is(err, errApp) != c.wantAppErr {
				t.Errorf("errors.Is(err, errApp) = %v，应为 %v（err=%v）", !c.wantAppErr, c.wantAppErr, err)
			}
			var n int
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM kv`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := 0
			if c.wantCommitted {
				want = 1
			}
			if n != want {
				t.Errorf("kv 中有 %d 行，应为 %d 行（失败的尝试应回滚）", n, want)
			}
		})
	}
}

// 等待重试时 ctx 被取消：立即返回，错误同时包含数据库错误和 ctx 的错误。
func TestDB_TxCancelledWhileWaitingForRetry(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		attempts++
		_, err := tx.Exec(ctx, raiseSQL(pgDeadlockDetected))
		cancel()
		return err
	})
	if attempts != 1 {
		t.Errorf("ctx 取消后不应再重试，f 被调用了 %d 次", attempts)
	}
	if !errors.Is(err, context.Canceled) || pgCode(err) != pgDeadlockDetected {
		t.Errorf("错误应同时包含死锁和 context.Canceled：%v", err)
	}
}

// 两个事务以相反的顺序锁两行，形成真实的死锁：PostgreSQL 中止其中一个，Tx 重试后两者都成功。
func TestDB_TxRetriesRealDeadlock(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	mustExec(t, db, `INSERT INTO kv (key, value) VALUES ('a','0'), ('b','0')`)
	// 两个事务各自锁住第一行之后才去锁第二行，保证一定形成环
	locked := make(chan struct{}, 2)
	proceed := make(chan struct{})
	run := func(first, second string) <-chan error {
		done := make(chan error, 1)
		go func() {
			attempt := 0
			done <- db.Tx(t.Context(), func(tx pgx.Tx) error {
				attempt++
				if _, err := tx.Exec(t.Context(), `UPDATE kv SET value=value::int+1 WHERE key=$1`, first); err != nil {
					return err
				}
				if attempt == 1 {
					locked <- struct{}{}
					<-proceed
				}
				_, err := tx.Exec(t.Context(), `UPDATE kv SET value=value::int+1 WHERE key=$1`, second)
				return err
			})
		}()
		return done
	}
	d1, d2 := run("a", "b"), run("b", "a")
	<-locked
	<-locked
	close(proceed)
	for _, d := range []<-chan error{d1, d2} {
		if err := <-d; err != nil {
			t.Fatalf("死锁后应重试成功：%v", err)
		}
	}
	rows, err := db.Query(t.Context(), `SELECT value FROM kv ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(values, []string{"2", "2"}) {
		t.Fatalf("两个事务都应恰好提交一次，结果为 %v", values)
	}
}

func TestQueryIDs(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	cases := []struct {
		name    string
		sql     string
		args    []any
		want    []int64
		wantErr bool
	}{
		{name: "多行", sql: `SELECT x FROM generate_series(3,1,-1) x`, want: []int64{3, 2, 1}},
		{name: "带参数", sql: `SELECT x FROM generate_series(1,$1::int) x`, args: []any{2}, want: []int64{1, 2}},
		{name: "没有结果", sql: `SELECT 1::bigint WHERE false`, want: []int64{}},
		{name: "SQL 错误", sql: `SELECT FROM no_such_table`, wantErr: true},
		{name: "列类型不是整数", sql: `SELECT 'x'`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := QueryIDs(t.Context(), db, c.sql, c.args...)
			if (err != nil) != c.wantErr {
				t.Fatalf("QueryIDs 的错误为 %v，wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && !slices.Equal(got, c.want) {
				t.Fatalf("QueryIDs = %v，应为 %v", got, c.want)
			}
		})
	}
}

// 数据库出错时各查询返回错误，不会误判为"不存在"（nil、ErrRepoNotFound、InstallationUnknown 等）。
func TestQueriesReportDatabaseErrors(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	repo := &Repo{ID: 1, OwnerID: 1}
	scope := &ScopeRef{Kind: ScopeRepo, ID: 1}
	cases := map[string]func() error{
		"RecordConfigChange": func() error {
			tx, err := db.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
			return RecordConfigChange(ctx, tx, 1, "a", "op", nil)
		},
		"FindRepo": func() error {
			_, err := FindRepo(ctx, db, "a/b")
			if errors.Is(err, ErrRepoNotFound) {
				t.Error("FindRepo：数据库错误不应判为仓库不存在")
			}
			return err
		},
		"GetInstallationState": func() error {
			_, err := GetInstallationState(ctx, db, 1)
			return err
		},
		"MarkInstallationDeleted": func() error { return MarkInstallationDeleted(ctx, db, InstallationMeta{ID: 1}) },
		"DeactivateInstallation":  func() error { return DeactivateInstallation(ctx, db, 1) },
		"RepoStates": func() error {
			_, err := RepoStates(ctx, db, []int64{1})
			return err
		},
		"LoadSyncStatus": func() error {
			_, err := LoadSyncStatus(ctx, db)
			return err
		},
		"ListRunners": func() error {
			_, err := ListRunners(ctx, db, RunnerFilter{})
			return err
		},
		"LookupEnvironment": func() error {
			_, err := LookupEnvironment(ctx, db, 1, "release")
			return err
		},
		"VariablesFor": func() error {
			_, err := VariablesFor(ctx, db, repo, nil)
			return err
		},
		"ListVariables": func() error {
			_, err := ListVariables(ctx, db, scope)
			return err
		},
		"ListSecretMeta": func() error {
			_, err := ListSecretMeta(ctx, db, scope)
			return err
		},
		"SecretsFor": func() error {
			_, err := SecretsFor(ctx, db, nil, repo, nil, []string{"A"})
			return err
		},
		"ResolveScope 按账号": func() error {
			_, err := ResolveScope(ctx, db, "alice", "", "")
			if err != nil && strings.Contains(err.Error(), "不在 Kiln 中") {
				t.Error("ResolveScope：数据库错误不应判为账号不存在")
			}
			return err
		},
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s 应返回 ctx 的错误：%v", name, err)
		}
	}
}

func TestMustJSON(t *testing.T) {
	t.Parallel()
	if got := string(MustJSON(map[string]any{"b": 1, "a": "x"})); got != `{"a":"x","b":1}` {
		t.Errorf("MustJSON = %s", got)
	}
	if got := string(MustJSON(nil)); got != "null" {
		t.Errorf("MustJSON(nil) = %s", got)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("无法编码的值应 panic")
		}
		if err, ok := r.(error); !ok || !strings.Contains(err.Error(), "chan") {
			t.Fatalf("panic 的值应是编码错误：%v", r)
		}
	}()
	MustJSON(make(chan int))
}
