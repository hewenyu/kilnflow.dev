// Package pgtest 为需要 Postgres 的单元测试提供独立的、已执行迁移的数据库。
//
// 数据库的来源：
//   - 设置了 KILN_TEST_DATABASE_URL 时使用它指向的 Postgres（Kiln CI 中由 services 提供），
//     这个用户需要有建库权限；
//   - 否则用本机的 initdb / pg_ctl 在临时目录中启动一个 Postgres，测试进程结束时停止；
//   - 两者都没有时跳过测试。
//
// 使用方法：包中有用到数据库的测试时，写
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
//
// 然后在测试中调用 Pool(t) 或 URL(t)。每次调用都得到一个新的数据库（从已迁移的模板库复制），测试之间互不影响，
// 可以 t.Parallel()；测试结束后删除。
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/db"
)

// EnvURL 是外部 Postgres 的连接地址（例如 postgres://postgres:pw@db/postgres?sslmode=disable）。
const EnvURL = "KILN_TEST_DATABASE_URL"

const (
	// connectTimeout 是等待外部 Postgres 就绪的时间：CI 中 services 的 postgres 镜像没有 HEALTHCHECK，
	// job 开始时数据库可能还在初始化。
	connectTimeout = 60 * time.Second
	// connectRetry 是等待就绪时的重试间隔。
	connectRetry = 500 * time.Millisecond
	// setupTimeout 是启动本地 Postgres、建模板库等一次性准备的超时。
	setupTimeout = 2 * time.Minute
)

var (
	// mainActive 表示测试由 Main 启动：只有这样才能在进程结束时停止本地 Postgres。
	mainActive bool

	setupOnce sync.Once
	setupErr  error
	skipMsg   string
	// adminURL 是用来建库、删库的连接地址（连到维护库）。
	adminURL string
	// template 是本进程已执行迁移的模板库。
	template string
	// stopLocal 停止本地启动的 Postgres；使用外部 Postgres 时为 nil。
	stopLocal func()

	// createMu 串行化从模板复制数据库：复制期间模板库不能有其他会话。
	createMu sync.Mutex
)

// Main 运行测试，结束后删除模板库、停止本地启动的 Postgres，然后以测试的结果退出。
func Main(m *testing.M) {
	mainActive = true
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Pool 返回一个连到新数据库的连接池，测试结束时关闭并删除数据库。
func Pool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	pool, err := pgxpool.New(tb.Context(), URL(tb))
	if err != nil {
		tb.Fatalf("连接测试数据库：%v", err)
	}
	// 在删库之前关闭（tb.Cleanup 按注册的相反顺序执行）
	tb.Cleanup(pool.Close)
	return pool
}

// URL 返回一个新数据库的连接地址，已执行全部迁移，测试结束时删除。
func URL(tb testing.TB) string {
	tb.Helper()
	if !mainActive {
		tb.Fatal("pgtest：包中需要 func TestMain(m *testing.M) { pgtest.Main(m) }")
	}
	setupOnce.Do(func() { setupErr = setup() })
	if skipMsg != "" {
		tb.Skip(skipMsg)
	}
	if setupErr != nil {
		tb.Fatalf("pgtest：%v", setupErr)
	}
	name := "kiln_t_" + randHex()
	ctx := context.WithoutCancel(tb.Context())
	if err := exec1(ctx, adminURL, func(conn *pgx.Conn) error {
		createMu.Lock()
		defer createMu.Unlock()
		_, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{template}.Sanitize())
		return err
	}); err != nil {
		tb.Fatalf("pgtest：创建数据库：%v", err)
	}
	tb.Cleanup(func() {
		if err := dropDatabase(ctx, name); err != nil {
			tb.Logf("pgtest：删除数据库 %s：%v", name, err)
		}
	})
	return withDatabase(adminURL, name)
}

// setup 准备 Postgres 和模板库，整个测试进程只执行一次。
func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if u := os.Getenv(EnvURL); u != "" {
		adminURL = u
		if err := waitReady(ctx, u); err != nil {
			return fmt.Errorf("连接 %s：%w", EnvURL, err)
		}
	} else {
		u, stop, err := startLocal(ctx)
		if errors.Is(err, exec.ErrNotFound) {
			skipMsg = "没有设置 " + EnvURL + "，本机也没有 initdb，跳过需要 Postgres 的测试"
			return nil
		}
		if err != nil {
			return err
		}
		adminURL, stopLocal = u, stop
	}
	// 每个测试进程一个模板库：go test 并行执行多个包时共用同一个外部 Postgres
	template = "kiln_tmpl_" + randHex()
	if err := exec1(ctx, adminURL, func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{template}.Sanitize())
		return err
	}); err != nil {
		return fmt.Errorf("创建模板库：%w", err)
	}
	pool, err := db.Open(ctx, withDatabase(adminURL, template))
	if err != nil {
		return fmt.Errorf("迁移模板库：%w", err)
	}
	pool.Close()
	return nil
}

// cleanup 删除模板库并停止本地 Postgres。
func cleanup() {
	if template != "" && setupErr == nil {
		if err := dropDatabase(context.Background(), template); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest：删除模板库 %s：%v\n", template, err)
		}
	}
	if stopLocal != nil {
		stopLocal()
	}
}

func dropDatabase(ctx context.Context, name string) error {
	return exec1(ctx, adminURL, func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		return err
	})
}

// exec1 用一个单独的连接执行 f（CREATE / DROP DATABASE 不能在事务中执行，也不需要连接池）。
func exec1(ctx context.Context, u string, f func(conn *pgx.Conn) error) error {
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return f(conn)
}

// waitReady 等待 Postgres 可以连接。
func waitReady(ctx context.Context, u string) error {
	deadline := time.Now().Add(connectTimeout)
	for {
		err := exec1(ctx, u, func(conn *pgx.Conn) error { return conn.Ping(ctx) })
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(connectRetry):
		}
	}
}

// startLocal 在临时目录中初始化并启动一个 Postgres，返回维护库的连接地址和停止函数。
// 本机没有 initdb 时返回 exec.ErrNotFound。
func startLocal(ctx context.Context) (string, func(), error) {
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		return "", nil, exec.ErrNotFound
	}
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	// unix socket 路径有长度限制（macOS 上 104 字节），放在 /tmp 下的短目录中
	dir, err := os.MkdirTemp("/tmp", "kpgt")
	if err != nil {
		return "", nil, err
	}
	removeDir := func() { _ = os.RemoveAll(dir) }
	data := filepath.Join(dir, "data")
	if out, err := exec.CommandContext(ctx, initdb, "-D", data, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--no-locale").CombinedOutput(); err != nil {
		removeDir()
		return "", nil, fmt.Errorf("initdb：%w\n%s", err, out)
	}
	port, err := freePort(ctx)
	if err != nil {
		removeDir()
		return "", nil, err
	}
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off -c full_page_writes=off", port, dir)
	logFile := filepath.Join(dir, "pg.log")
	if out, err := exec.CommandContext(ctx, pgctl, "-D", data, "-o", opts, "-l", logFile, "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(logFile)
		removeDir()
		return "", nil, fmt.Errorf("pg_ctl start：%w\n%s\n%s", err, out, log)
	}
	stop := func() {
		if out, err := exec.CommandContext(context.Background(), pgctl, "-D", data, "-m", "immediate", "stop").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest：pg_ctl stop：%v\n%s\n", err, out)
		}
		removeDir()
	}
	return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port), stop, nil
}

func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("意外的地址类型 %T", l.Addr())
	}
	return addr.Port, nil
}

// withDatabase 把连接地址中的数据库换成 name。
func withDatabase(u, name string) string {
	p, err := url.Parse(u)
	if err != nil {
		// 地址在 setup 中已经成功连接过，不会走到这里
		panic(fmt.Sprintf("pgtest：解析连接地址：%v", err))
	}
	p.Path = "/" + name
	return p.String()
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // Go 1.24 起 crypto/rand.Read 不会返回错误
	return hex.EncodeToString(b)
}
