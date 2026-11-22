package store

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
)

// TestWithAdvisoryLockSmallPool：持锁者自己不再借连接（用锁内事务在专用连接上执行），
// 所以连接池很小时也不会与等待同一把锁的请求互相等待。
func TestWithAdvisoryLockSmallPool(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := New(pool)

	const key = "kiln:doc-diff:1"
	// holderInTx、waiterAcquired 都是可观察的同步点，不依赖「借连接要多久」。
	holderInTx := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	// 无论测试在哪一步返回（断言失败、超时、panic），都释放持锁者并在关闭连接池之前等它退出。
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
	})

	var wg sync.WaitGroup
	holderErr := make(chan error, 1)
	wg.Go(func() {
		holderErr <- db.WithAdvisoryLock(t.Context(), key, func(ctx context.Context, conn *pgxpool.Conn) error {
			// 持锁期间在同一个连接上开事务：不应再向池里借连接。
			if err := TxOn(ctx, conn, func(tx pgx.Tx) error {
				var one int
				return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
			}); err != nil {
				return err
			}
			close(holderInTx)
			<-release
			return TxOn(ctx, conn, func(tx pgx.Tx) error {
				var one int
				return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
			})
		})
	})
	// 持锁者退出后归还连接：先于 t.Cleanup(pool.Close) 注册，所以先执行（Cleanup 后进先出）。
	t.Cleanup(func() { wg.Wait() })

	select {
	case <-holderInTx:
	case <-time.After(10 * time.Second):
		t.Fatal("持锁者没能在自己的连接上开事务（可能又在借连接）")
	}

	// 第二个请求借走第二条连接并等待同一把锁。用「AcquiredConns 变成 2」作为等待者已经拿到连接的证据，
	// 轮询到为止（有截止时间），不依赖固定的 sleep。
	waiterDone := make(chan error, 1)
	wg.Go(func() {
		waiterDone <- db.WithAdvisoryLock(t.Context(), key, func(context.Context, *pgxpool.Conn) error { return nil })
	})
	deadline := time.Now().Add(10 * time.Second)
	for pool.Stat().AcquiredConns() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("等待者没有借到第二条连接（AcquiredConns=%d），池可能被持锁者耗尽", pool.Stat().AcquiredConns())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 池已满（2/2）而持锁者仍持有自己的连接：再在自己的连接上开一次事务，证明它没有因为池满而卡住。
	releaseOnce.Do(func() { close(release) })

	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("等待者：%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等待锁的请求没有完成")
	}
	if err := <-holderErr; err != nil {
		t.Fatalf("持锁者的事务失败（连接池被自己耗尽）：%v", err)
	}
}

// TestWithAdvisoryLockUnlockFailureDestroysConnection：解锁失败时不能把可能仍持有会话锁的连接放回池中。
// 让解锁语句在服务端确定失败（所在角色没有 pg_advisory_unlock 的执行权限）：连接本身还是好的，
// 只有"关闭物理连接"能阻止它带着会话锁回到池里。
func TestWithAdvisoryLockUnlockFailureDestroysConnection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	u := pgtest.URL(t)
	admin, err := pgxpool.New(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	var super bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if !super {
		t.Skipf("解锁失败需要切换到一个受限角色（吊销 pg_advisory_unlock 的执行权限），当前用户不是超级用户")
	}
	// 角色是集群级的，用库名（每个测试独立）拼出唯一名字；解锁权限只在这个库上吊销，不影响其它测试。
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	role := "kiln_no_unlock_" + strings.TrimPrefix(filepath.Base(parsed.Path), "kiln_t_")
	if _, err := admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	// 先注册、后建连接池：Cleanup 后进先出，连接池会先关闭。
	t.Cleanup(func() {
		if _, err := admin.Exec(context.WithoutCancel(ctx), `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Errorf("删除角色：%v", err)
		}
	})
	if _, err := admin.Exec(ctx, `REVOKE EXECUTE ON FUNCTION pg_advisory_unlock(bigint) FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(u)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := New(pool)

	const key = "kiln:test-unlock-failure"
	if err := db.WithAdvisoryLock(ctx, key, func(ctx context.Context, conn *pgxpool.Conn) error {
		var locked bool
		// 受限角色仍然能加锁：失败的只有解锁。
		return conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&locked)
	}); err != nil {
		t.Fatalf("WithAdvisoryLock：%v", err)
	}

	// 销毁在池里是异步的：等它落定，再断言池中没有留下任何连接。
	deadline := time.Now().Add(10 * time.Second)
	for pool.Stat().TotalConns() > 0 || pool.Stat().AcquiredConns() > 0 {
		if time.Now().After(deadline) {
			stat := pool.Stat()
			t.Fatalf("解锁失败的连接没有被销毁：total=%d idle=%d", stat.TotalConns(), stat.IdleConns())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 会话锁随被关闭的连接释放，其它会话能立刻拿到同一把锁。
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		t.Fatalf("解锁失败的连接可能仍持有会话锁：%v", err)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
		t.Fatal(err)
	}
}

// TestWithAdvisoryLockReusesConnection：解锁正常时连接回到池中复用，并且确实不再持锁（用另一条独立连接验证，
// 避免同一会话里可重入的 advisory lock 假通过）。
func TestWithAdvisoryLockReusesConnection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := New(pool)

	const key = "kiln:test-unlock-reuse"
	var pid uint32
	if err := db.WithAdvisoryLock(ctx, key, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}); err != nil {
		t.Fatal(err)
	}
	// 第一条借到的应该是解锁后放回池中的那条连接：后端 PID 相同说明连接被复用、没有因为解锁失败被销毁。
	reused, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Release()
	var got uint32
	if err := reused.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != pid {
		t.Errorf("复用的连接 pid = %d，期望 %d", got, pid)
	}
	// 换一条独立连接验证会话锁真的释放了：同一会话重入会永远成功，证明不了什么。
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	var locked bool
	if err := other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("解锁后另一个会话拿不到锁：解锁没有生效")
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
		t.Fatal(err)
	}
}

// TestTryAdvisoryLock：拿到时互斥（另一条连接拿不到），释放后另一条连接能拿到；没拿到时不返回释放函数。
func TestTryAdvisoryLock(t *testing.T) {
	t.Parallel()
	db := New(pgtest.Pool(t))
	const key = "kiln:test-try-lock"
	ctx := t.Context()
	a, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	unlock, ok, err := TryAdvisoryLock(ctx, a, key)
	if err != nil || !ok || unlock == nil {
		t.Fatalf("第一次应拿到锁：ok=%v err=%v", ok, err)
	}
	b, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	if unlock2, ok, err := TryAdvisoryLock(ctx, b, key); err != nil || ok || unlock2 != nil {
		t.Fatalf("锁被持有时不应拿到：ok=%v 有释放函数=%v err=%v", ok, unlock2 != nil, err)
	}
	if err := unlock(ctx); err != nil {
		t.Fatalf("释放：%v", err)
	}
	unlock2, ok, err := TryAdvisoryLock(ctx, b, key)
	if err != nil || !ok {
		t.Fatalf("释放后应拿到锁：ok=%v err=%v", ok, err)
	}
	if err := unlock2(ctx); err != nil {
		t.Fatalf("第二次释放：%v", err)
	}
}
