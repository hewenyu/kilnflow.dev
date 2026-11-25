package pgtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	t.Parallel()
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

// 高并发借还槽位：-race 下不能有数据竞争或死锁，每个测试仍然互相隔离。
func TestParallelSlots(t *testing.T) {
	t.Parallel()
	for i := range 8 {
		t.Run(fmt.Sprintf("worker-%d", i), func(t *testing.T) {
			t.Parallel()
			p := Pool(t)
			ctx := t.Context()
			if _, err := p.Exec(ctx, `INSERT INTO installations (id, account_id, account_login) VALUES ($1, $1, 'alice')`, i+1); err != nil {
				t.Fatalf("写入：%v", err)
			}
			var n int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM installations`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("并行测试之间应互相隔离，实际 %d 行", n)
			}
		})
	}
}

// 连续两次借出的槽位是同一个库，但上一个测试的数据、自增序列和 advisory lock 都被清掉。
func TestSlotReuseResetsState(t *testing.T) {
	resetSlots(t)
	var firstName string
	t.Run("第一次", func(t *testing.T) {
		p := Pool(t)
		firstName = currentDatabase(t, p)
		ctx := t.Context()
		if _, err := p.Exec(ctx, `INSERT INTO installations (id, account_id, account_login) VALUES (100, 100, 'alice')`); err != nil {
			t.Fatal(err)
		}
		// session 级 advisory lock：连接关闭后应自动释放。
		if _, err := p.Exec(ctx, `SELECT pg_advisory_lock(4242)`); err != nil {
			t.Fatal(err)
		}
		// enrollment_tokens.id 是 BIGSERIAL：从 1 开始。
		var id int64
		if err := p.QueryRow(ctx, `INSERT INTO enrollment_tokens (token_hash, expires_at) VALUES ('first', now()) RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != 1 {
			t.Fatalf("自增序列应从头开始，得到 %d", id)
		}
		// 独立序列（CREATE SEQUENCE，不被表拥有）不受 TRUNCATE RESTART IDENTITY 影响，必须单独还原。
		var version int64
		if err := p.QueryRow(ctx, `SELECT nextval('secret_versions')`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Fatalf("独立序列应从头开始，得到 %d", version)
		}
	})
	t.Run("第二次", func(t *testing.T) {
		p := Pool(t)
		ctx := t.Context()
		if got := currentDatabase(t, p); got != firstName {
			t.Fatalf("应复用同一槽位：第一次 %s，第二次 %s", firstName, got)
		}
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM installations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("上一个测试的数据应被清空，实际 %d 行", n)
		}
		var id int64
		if err := p.QueryRow(ctx, `INSERT INTO enrollment_tokens (token_hash, expires_at) VALUES ('second', now()) RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != 1 {
			t.Fatalf("自增序列应被重置，得到 %d", id)
		}
		// pg_locks 是整个集群的视图：共享 Postgres 下别的包的数据库也有 advisory lock，只统计当前库。
		if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("session 级 advisory lock 应随连接关闭释放，实际 %d 个", n)
		}
		var version int64
		if err := p.QueryRow(ctx, `SELECT nextval('secret_versions')`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Fatalf("独立序列应被还原到起点，得到 %d", version)
		}
		// 迁移用 ALTER SEQUENCE ... RESTART WITH 1000 把 runner_upgrade_requests.id 从 1000 开始；
		// 单纯 RESTART IDENTITY 会把序列退回 1，所以归还时必须恢复模板库迁移后的序列位置。
		var runnerID int64
		if err := p.QueryRow(ctx, `INSERT INTO runners (name, public_key) VALUES ('sequence-check', '\x00') RETURNING id`).Scan(&runnerID); err != nil {
			t.Fatal(err)
		}
		var requestID int64
		if err := p.QueryRow(ctx, `INSERT INTO runner_upgrade_requests (runner_id, version, actor) VALUES ($1, '0.2.0', 'admin') RETURNING id`, runnerID).Scan(&requestID); err != nil {
			t.Fatal(err)
		}
		if requestID < 1000 {
			t.Fatalf("迁移用 RESTART WITH 1000 设置的序列位置应被还原，得到 %d", requestID)
		}
	})
}

// 迁移后已有初始行的表在清空后要还原成迁移刚结束时的状态。
// 当前迁移没有这样的表（0023 写入的种子行被 0024 删除），所以测试自己造一个探针表。
func TestSeedRestored(t *testing.T) {
	installSeedProbe(t)
	resetSlots(t)
	var firstName string
	t.Run("改坏种子", func(t *testing.T) {
		p := Pool(t)
		firstName = currentDatabase(t, p)
		ctx := t.Context()
		if _, err := p.Exec(ctx, `DELETE FROM pgtest_seed_probe`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO pgtest_seed_probe (id, note) VALUES (99, 'junk')`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("还原种子", func(t *testing.T) {
		p := Pool(t)
		if got := currentDatabase(t, p); got != firstName {
			t.Fatalf("应复用同一槽位：第一次 %s，第二次 %s", firstName, got)
		}
		rows, err := p.Query(t.Context(), `SELECT id, note FROM pgtest_seed_probe ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id int64
			var note string
			if err := rows.Scan(&id, &note); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%d:%s", id, note))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != "[1:keep]" {
			t.Fatalf("种子行应还原为初始状态，实际 %v", got)
		}
	})
}

// 重命名表或删约束的槽位必须被丢弃，不能把改过的结构放回复用池。
func TestDDLChangeDiscardsSlot(t *testing.T) {
	t.Run("重命名表", func(t *testing.T) {
		resetSlots(t)
		var damaged string
		t.Run("破坏", func(t *testing.T) {
			p := Pool(t)
			damaged = currentDatabase(t, p)
			if _, err := p.Exec(t.Context(), `ALTER TABLE installations RENAME TO installations_gone`); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("新槽位结构完好", func(t *testing.T) {
			p := Pool(t)
			if got := currentDatabase(t, p); got == damaged {
				t.Fatalf("改过 DDL 的槽位应被丢弃，实际复用了 %s", got)
			}
			assertCleanInstallations(t, p)
		})
	})
	t.Run("删除约束", func(t *testing.T) {
		resetSlots(t)
		var damaged string
		t.Run("破坏", func(t *testing.T) {
			p := Pool(t)
			damaged = currentDatabase(t, p)
			if _, err := p.Exec(t.Context(), `ALTER TABLE installations DROP CONSTRAINT installations_pkey`); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("新槽位约束完好", func(t *testing.T) {
			p := Pool(t)
			if got := currentDatabase(t, p); got == damaged {
				t.Fatalf("删过约束的槽位应被丢弃，实际复用了 %s", got)
			}
			assertCleanInstallations(t, p)
			var exists bool
			if err := p.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'installations_pkey')`).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatal("新槽位应保留 installations 的主键约束")
			}
		})
	})
	t.Run("创建函数", func(t *testing.T) {
		resetSlots(t)
		var damaged string
		t.Run("破坏", func(t *testing.T) {
			p := Pool(t)
			damaged = currentDatabase(t, p)
			if _, err := p.Exec(t.Context(), `CREATE FUNCTION pgtest_probe_fn() RETURNS int LANGUAGE sql AS 'SELECT 1'`); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("新槽位没有函数", func(t *testing.T) {
			p := Pool(t)
			if got := currentDatabase(t, p); got == damaged {
				t.Fatalf("建过函数的槽位应被丢弃，实际复用了 %s", got)
			}
			var exists bool
			if err := p.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'pgtest_probe_fn')`).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists {
				t.Fatal("新槽位不应带有测试创建的函数")
			}
		})
	})
	t.Run("修改列默认值", func(t *testing.T) {
		resetSlots(t)
		var damaged string
		t.Run("破坏", func(t *testing.T) {
			p := Pool(t)
			damaged = currentDatabase(t, p)
			if _, err := p.Exec(t.Context(), `ALTER TABLE installations ALTER COLUMN account_type SET DEFAULT 'Organization'`); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("新槽位默认值完好", func(t *testing.T) {
			p := Pool(t)
			if got := currentDatabase(t, p); got == damaged {
				t.Fatalf("改过列默认值的槽位应被丢弃，实际复用了 %s", got)
			}
			// 插入时不指定 account_type：必须拿到迁移定义的默认值，而不是上一个测试设置的 'Organization'。
			var accountType string
			if err := p.QueryRow(t.Context(),
				`INSERT INTO installations (id, account_id, account_login) VALUES (1, 1, 'alice') RETURNING account_type`).Scan(&accountType); err != nil {
				t.Fatal(err)
			}
			if accountType != "User" {
				t.Fatalf("新槽位的 account_type 默认值应为 User，实际 %q", accountType)
			}
		})
	})
	t.Run("禁用触发器", func(t *testing.T) {
		resetSlots(t)
		var damaged string
		t.Run("破坏", func(t *testing.T) {
			p := Pool(t)
			damaged = currentDatabase(t, p)
			if _, err := p.Exec(t.Context(), `ALTER TABLE runs DISABLE TRIGGER ALL`); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("新槽位外键生效", func(t *testing.T) {
			p := Pool(t)
			if got := currentDatabase(t, p); got == damaged {
				t.Fatalf("禁用过触发器的槽位应被丢弃，实际复用了 %s", got)
			}
			// 插入指向不存在仓库的 runs 记录：外键内部触发器必须生效，返回 SQLSTATE 23503。
			_, err := p.Exec(t.Context(),
				`INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state) VALUES (999999, 'ci.yml', 'push', 'refs/heads/main', 'a', 'push', 'probe', 'queued')`)
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgErr.Code != "23503" {
				t.Fatalf("新槽位外键应拒绝非法插入（SQLSTATE 23503），实际 err = %v", err)
			}
		})
	})
}

// leakedConnHangGuard 是等待 releaseSlot 返回的余量：超过 residualConnWait 这么多还没返回，
// 就说明它在等锁而不是按超时丢弃槽位。
const leakedConnHangGuard = 10 * time.Second

// 测试泄漏的外部连接持有表锁时，releaseSlot 不能挂死：先等残留连接归零（最多 residualConnWait），
// 等不到就丢弃槽位，而不是让需要 AccessExclusiveLock 的 TRUNCATE 一直等锁。
// 下一次 Pool 必须拿到结构完好的新槽位。
func TestLeakedLockedConnectionDiscardsSlot(t *testing.T) {
	resetSlots(t)

	var leaked *pgx.Conn
	var leakedName string
	t.Cleanup(func() {
		if leaked != nil {
			_ = leaked.Close(context.Background())
		}
	})
	t.Run("泄漏连接持有表锁", func(t *testing.T) {
		s := acquireSlot(t)
		leakedName = s.name
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, withDatabase(adminURL, s.name))
		if err != nil {
			t.Fatal(err)
		}
		leaked = conn
		if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `LOCK TABLE installations IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		// 旧顺序先执行 TRUNCATE（需要 AccessExclusiveLock），会一直等这个未提交的事务。
		start := time.Now()
		done := make(chan struct{})
		go func() {
			releaseSlot(context.Background(), s)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(residualConnWait + leakedConnHangGuard):
			t.Fatal("releaseSlot 在残留连接持有表锁时没有返回")
		}
		if elapsed := time.Since(start); elapsed < residualConnWait {
			t.Errorf("releaseSlot 只用了 %v 就返回，应等待 residualConnWait（%v）后丢弃", elapsed, residualConnWait)
		}
	})
	t.Run("新槽位完好可用", func(t *testing.T) {
		p := Pool(t)
		if got := currentDatabase(t, p); got == leakedName {
			t.Fatalf("持有表锁的槽位应被丢弃，实际复用了 %s", got)
		}
		assertCleanInstallations(t, p)
	})
}

// resetSlots 清空并删除空闲池中的槽位：需要独占空闲池的测试（复用、种子、DDL）从干净状态开始。
func resetSlots(tb testing.TB) {
	tb.Helper()
	ensureSetup(tb)
	slotMu.Lock()
	slots := freeSlots
	freeSlots = nil
	slotMu.Unlock()
	for _, s := range slots {
		discardSlot(context.Background(), s)
	}
}

func currentDatabase(tb testing.TB, p *pgxpool.Pool) string {
	tb.Helper()
	var name string
	if err := p.QueryRow(tb.Context(), `SELECT current_database()`).Scan(&name); err != nil {
		tb.Fatal(err)
	}
	return name
}

func assertCleanInstallations(tb testing.TB, p *pgxpool.Pool) {
	tb.Helper()
	ctx := tb.Context()
	if _, err := p.Exec(ctx, `INSERT INTO installations (id, account_id, account_login) VALUES (1, 1, 'alice')`); err != nil {
		tb.Fatalf("新槽位结构应完好：%v", err)
	}
	var exists bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'installations_gone')`).Scan(&exists); err != nil {
		tb.Fatal(err)
	}
	if exists {
		tb.Fatal("新槽位不应带有被改名的表")
	}
}

// installSeedProbe 在模板库中加入一张带初始行的表并重新计算元数据，注册测试结束时移除它的清理。
// 只有在顺序执行的测试里调用（并行测试在全部顺序测试结束后才开始）。
func installSeedProbe(tb testing.TB) {
	tb.Helper()
	ensureSetup(tb)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, withDatabase(adminURL, template))
	if err != nil {
		tb.Fatal(err)
	}
	meta, metaErr := addSeedProbe(ctx, conn)
	// 关闭后等模板库的会话归零，否则下一次 CREATE DATABASE ... TEMPLATE 会因还有会话而失败。
	closeErr := closeTemplateConn(ctx, conn)
	if metaErr != nil {
		tb.Fatal(metaErr)
	}
	if closeErr != nil {
		tb.Fatalf("pgtest：等待模板库空闲：%v", closeErr)
	}
	templateSig, userTables, seedTables, sequences = meta.signature, meta.userTables, meta.seedTables, meta.sequences
	tb.Cleanup(func() {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, withDatabase(adminURL, template))
		if err != nil {
			tb.Logf("pgtest：移除种子探针：%v", err)
			return
		}
		if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS public.pgtest_seed_probe`); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			tb.Logf("pgtest：移除种子探针：%v", err)
			return
		}
		meta, err := buildMetadata(ctx, conn)
		closeErr := closeTemplateConn(ctx, conn)
		if err != nil {
			tb.Logf("pgtest：重建模板元数据：%v", err)
			return
		}
		if closeErr != nil {
			tb.Logf("pgtest：等待模板库空闲：%v", closeErr)
			return
		}
		templateSig, userTables, seedTables, sequences = meta.signature, meta.userTables, meta.seedTables, meta.sequences
		resetSlots(tb)
	})
}

// addSeedProbe 在模板库中建立带初始行的探针表，并重新计算模板元数据。
func addSeedProbe(ctx context.Context, conn *pgx.Conn) (metadata, error) {
	if _, err := conn.Exec(ctx, `CREATE TABLE public.pgtest_seed_probe (id bigint PRIMARY KEY, note text NOT NULL)`); err != nil {
		return metadata{}, err
	}
	if _, err := conn.Exec(ctx, `INSERT INTO public.pgtest_seed_probe (id, note) VALUES (1, 'keep')`); err != nil {
		return metadata{}, err
	}
	return buildMetadata(ctx, conn)
}
