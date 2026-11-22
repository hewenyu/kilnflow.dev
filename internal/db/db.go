// Package db 负责连接 Postgres 和执行迁移（PLAN #20.4：启动时自动迁移，advisory lock 保证只执行一次）。
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/migrations"
)

const (
	migrateLockID = 0x6b696c6e // "kiln"

	// defaultMaxConns 是用户没有配置 pool_max_conns 时的连接池上限。
	// pgxpool 默认取 max(4, CPU 核数)，在小 VPS 上只有 4；而 Server 同时有调度、
	// 租约续期、日志写入、webhook 处理和 Dashboard 查询，4 个连接容易互相等待。
	defaultMaxConns = 16

	// maxConnsParam 是连接串中设置连接池上限的参数名（URL 和 key=value 两种写法通用）。
	maxConnsParam = "pool_max_conns"

	// unlockTimeout 是释放迁移锁的超时；超时说明连接已不可用，直接关闭。
	unlockTimeout = 10 * time.Second
)

// Open 连接数据库并执行迁移。
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("解析数据库地址：%w", err)
	}
	// 用户在连接串中显式设置了上限时尊重用户配置。
	if !strings.Contains(url, maxConnsParam) {
		cfg.MaxConns = defaultMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池：%w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库：%w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// migration 是一个迁移文件。
type migration struct {
	version int
	name    string
}

// loadMigrations 列出内嵌的迁移文件，按版本号排序；版本号重复或格式不对时报错。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("读取迁移文件：%w", err)
	}
	var list []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("迁移文件名 %s 应以数字开头", name)
		}
		if prev, ok := seen[v]; ok {
			return nil, fmt.Errorf("迁移文件 %s 和 %s 的版本号 %d 重复", prev, name, v)
		}
		seen[v] = name
		list = append(list, migration{version: v, name: name})
	}
	slices.SortFunc(list, func(a, b migration) int { return a.version - b.version })
	return list, nil
}

// Migrate 按版本号顺序执行未执行过的迁移。
func Migrate(ctx context.Context, pool *pgxpool.Pool) (err error) {
	list, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取数据库连接：%w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("获取迁移锁：%w", err)
	}
	defer func() {
		// ctx 可能已取消，释放锁用独立的超时；释放失败时关闭底层连接，
		// 避免带着 session 级的 advisory lock 回到连接池。
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, uerr := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, migrateLockID); uerr != nil {
			cerr := conn.Conn().Close(uctx)
			err = errors.Join(err, fmt.Errorf("释放迁移锁：%w", uerr), cerr)
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("创建 schema_migrations：%w", err)
	}
	for _, m := range list {
		if err := apply(ctx, conn.Conn(), m); err != nil {
			return err
		}
	}
	return nil
}

// apply 执行一个尚未执行过的迁移，并在同一事务中记录版本号。
func apply(ctx context.Context, conn *pgx.Conn, m migration) error {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, m.version).Scan(&exists); err != nil {
		return fmt.Errorf("查询迁移 %s 是否已执行：%w", m.name, err)
	}
	if exists {
		return nil
	}
	body, err := migrations.FS.ReadFile(m.name)
	if err != nil {
		return fmt.Errorf("读取迁移 %s：%w", m.name, err)
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("迁移 %s：%w", m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return fmt.Errorf("记录迁移 %s：%w", m.name, err)
		}
		return nil
	})
}
