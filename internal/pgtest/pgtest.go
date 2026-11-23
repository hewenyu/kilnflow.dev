// Package pgtest 为需要 Postgres 的单元测试提供已执行迁移的数据库。
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
// 然后按需要调用：
//
//   - URL(t) 得到一个本次独有的新数据库（从已迁移的模板库复制），测试结束时删除。需要独占建库、断连、
//     改表的测试用它。
//   - Pool(t) 得到一个连到"数据库槽位"的连接池：同一个槽位库在一个测试结束后被清空、归还到进程内的空闲池，
//     下一个测试直接复用，不必每次重新复制模板库（复制是迁移之外最大的开销）。每个测试仍然拿到全新的
//     *pgxpool.Pool，上一个测试的连接、事务、session 级 advisory lock、TEMP 表和会话状态随连接关闭一起释放。
//     归还前会校验库结构与模板一致（DDL 改动会让槽位被丢弃）、把表清空并把迁移带入的初始种子行还原。
//
// 两种方式都可以 t.Parallel()。
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
	"strings"
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

	// fileCopyStrategyVersion 是 CREATE DATABASE ... TEMPLATE 支持 STRATEGY 子句的最低版本号
	// （Postgres 15）。15 起默认的 WAL_LOG 要逐页写 WAL，复制模板库比旧的 FILE_COPY 慢，显式用 FILE_COPY。
	fileCopyStrategyVersion = 150000

	// seedSchema 保存迁移后已有初始行的表的快照。它不在 public 中，既不参与结构签名，也不会被清空。
	seedSchema = "pgtest_seed"
	// migrationsTable 记录已执行的迁移，不能清空。
	migrationsTable = "schema_migrations"

	// residualConnWait / residualConnPoll 是归还槽位时等待刚关闭的后端从 pg_stat_activity 消失的时间：
	// 关闭连接是异步的，立刻检查可能看到正在退出的会话。超过这个时间还有连接才丢弃槽位。
	residualConnWait = 2 * time.Second
	residualConnPoll = 20 * time.Millisecond
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
	// serverVersion 是 Postgres 的 server_version_num，决定建库时能否使用 STRATEGY = FILE_COPY。
	serverVersion int

	// admin 是复用的维护库连接：建库、删库很频繁，不必每次都新建 TCP 连接。
	admin adminConn

	// slotMu 保护空闲槽位、全部槽位和模板元数据。复制模板库本身由 admin 的互斥串行化。
	slotMu      sync.Mutex
	freeSlots   []*slot
	allSlots    []*slot
	templateSig string
	userTables  []string
	seedTables  []seedTable
	sequences   []sequenceState
)

// slot 是一个可复用的数据库槽位。resetConn 是这个槽位专属的常驻连接：归还时直接用它校验结构、清空数据、
// 还原种子并检查残留连接，不必每次归还都新建 TCP 连接，也不参与全局 admin 锁的争用。
// 它只在当前持有该槽位的测试归还时使用，不会被并发访问。
type slot struct {
	name      string
	resetConn *pgx.Conn
}

// conn 返回槽位的常驻连接，断开后自动重连。
func (s *slot) conn(ctx context.Context) (*pgx.Conn, error) {
	if s.resetConn != nil && s.resetConn.IsClosed() {
		s.resetConn = nil
	}
	if s.resetConn == nil {
		conn, err := pgx.Connect(ctx, withDatabase(adminURL, s.name))
		if err != nil {
			return nil, err
		}
		s.resetConn = conn
	}
	return s.resetConn, nil
}

// adminConn 是一条常驻的维护库连接，串行执行建库 / 删库。连接断开后下一次调用自动重连。
type adminConn struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

func (a *adminConn) run(ctx context.Context, f func(context.Context, *pgx.Conn) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil && a.conn.IsClosed() {
		a.conn = nil
	}
	if a.conn == nil {
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return err
		}
		a.conn = conn
	}
	err := f(ctx, a.conn)
	if err != nil && a.conn.IsClosed() {
		a.conn = nil
	}
	return err
}

func (a *adminConn) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		_ = a.conn.Close(context.Background())
		a.conn = nil
	}
}

// Main 运行测试，结束后删除模板库和槽位库、停止本地启动的 Postgres，然后以测试的结果退出。
func Main(m *testing.M) { MainWith(m) }

// MainWith 同 Main，并在退出前依次调用 extra 的清理（例如 gittest.Cleanup 删除测试用的 git 模板目录）。
func MainWith(m *testing.M, extra ...func()) {
	mainActive = true
	code := m.Run()
	cleanup()
	for _, f := range extra {
		f()
	}
	os.Exit(code)
}

// Pool 返回一个连到可复用数据库槽位的连接池。测试结束时先关闭连接池，再清空、校验并归还槽位；
// 结构被测试改动的槽位会被丢弃，不会放回复用池。
func Pool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	ensureSetup(tb)
	ctx := context.WithoutCancel(tb.Context())
	s := acquireSlot(tb)
	pool, err := pgxpool.New(ctx, withDatabase(adminURL, s.name))
	if err != nil {
		discardSlot(ctx, s)
		tb.Fatalf("连接测试数据库：%v", err)
	}
	// tb.Cleanup 按注册的相反顺序执行：先关闭连接池（释放这个测试的连接、事务和会话状态），再归还槽位。
	tb.Cleanup(func() { releaseSlot(ctx, s) })
	tb.Cleanup(pool.Close)
	return pool
}

// URL 返回一个新数据库的连接地址，已执行全部迁移，测试结束时删除。
func URL(tb testing.TB) string {
	tb.Helper()
	ensureSetup(tb)
	name := "kiln_t_" + randHex()
	ctx := context.WithoutCancel(tb.Context())
	if err := createDatabase(ctx, name, template); err != nil {
		tb.Fatalf("pgtest：创建数据库：%v", err)
	}
	tb.Cleanup(func() {
		if err := dropDatabase(ctx, name); err != nil {
			tb.Logf("pgtest：删除数据库 %s：%v", name, err)
		}
	})
	return withDatabase(adminURL, name)
}

// ensureSetup 完成一次性的准备；没有可用的 Postgres 时跳过测试。
func ensureSetup(tb testing.TB) {
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
}

// setup 准备 Postgres、模板库和模板元数据，整个测试进程只执行一次。
func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if u := os.Getenv(EnvURL); u != "" {
		adminURL = u
		if err := waitReady(ctx); err != nil {
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
	if err := admin.run(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		return conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&serverVersion)
	}); err != nil {
		return fmt.Errorf("读取 Postgres 版本：%w", err)
	}

	// 每个测试进程一个模板库：go test 并行执行多个包时共用同一个外部 Postgres。
	template = "kiln_tmpl_" + randHex()
	if err := createDatabase(ctx, template, ""); err != nil {
		return fmt.Errorf("创建模板库：%w", err)
	}
	pool, err := db.Open(ctx, withDatabase(adminURL, template))
	if err != nil {
		return fmt.Errorf("迁移模板库：%w", err)
	}
	pool.Close()

	// 单独连一次模板库计算结构签名、保存种子快照；连接必须在建槽位之前关闭，
	// 否则 CREATE DATABASE ... TEMPLATE 会因模板库还有会话而失败。
	conn, err := pgx.Connect(ctx, withDatabase(adminURL, template))
	if err != nil {
		return fmt.Errorf("连接模板库：%w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	meta, err := buildMetadata(ctx, conn)
	if err != nil {
		return fmt.Errorf("准备模板库：%w", err)
	}
	templateSig, userTables, seedTables, sequences = meta.signature, meta.userTables, meta.seedTables, meta.sequences
	return nil
}

// cleanup 删除全部槽位库和模板库，停止本地 Postgres。
func cleanup() {
	ctx := context.Background()
	slotMu.Lock()
	slots := allSlots
	allSlots = nil
	freeSlots = nil
	slotMu.Unlock()
	for _, s := range slots {
		closeSlotConn(s)
		if err := dropDatabase(ctx, s.name); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest：删除数据库 %s：%v\n", s.name, err)
		}
	}
	if template != "" {
		if err := dropDatabase(ctx, template); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest：删除模板库 %s：%v\n", template, err)
		}
	}
	admin.close()
	if stopLocal != nil {
		stopLocal()
	}
}

// createDatabase 新建一个数据库；fromTemplate 非空时从它复制（并要求它没有其他会话）。
func createDatabase(ctx context.Context, name, fromTemplate string) error {
	sql := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if fromTemplate != "" {
		sql += " TEMPLATE " + pgx.Identifier{fromTemplate}.Sanitize()
		if serverVersion >= fileCopyStrategyVersion {
			sql += " STRATEGY = FILE_COPY"
		}
	}
	return admin.run(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, sql)
		return err
	})
}

func dropDatabase(ctx context.Context, name string) error {
	sql := "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
	return admin.run(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, sql)
		return err
	})
}

// waitReady 等待外部 Postgres 可以连接。
func waitReady(ctx context.Context) error {
	deadline := time.Now().Add(connectTimeout)
	for {
		err := admin.run(ctx, func(ctx context.Context, conn *pgx.Conn) error { return conn.Ping(ctx) })
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

// acquireSlot 借出一个槽位：优先复用空闲池中的库，没有时从模板库复制一个新库。
func acquireSlot(tb testing.TB) *slot {
	tb.Helper()
	slotMu.Lock()
	if n := len(freeSlots); n > 0 {
		s := freeSlots[n-1]
		freeSlots = freeSlots[:n-1]
		slotMu.Unlock()
		return s
	}
	slotMu.Unlock()

	s := &slot{name: "kiln_s_" + randHex()}
	if err := createDatabase(context.WithoutCancel(tb.Context()), s.name, template); err != nil {
		tb.Fatalf("pgtest：创建数据库：%v", err)
	}
	slotMu.Lock()
	allSlots = append(allSlots, s)
	slotMu.Unlock()
	return s
}

// errSlotPolluted 表示槽位的结构已经和模板不一致（测试做过 DDL），不能复用。
var errSlotPolluted = errors.New("pgtest：数据库结构与模板不一致")

// releaseSlot 重置并归还槽位；重置失败时丢弃，绝不污染后续测试。
func releaseSlot(ctx context.Context, s *slot) {
	if err := resetSlot(ctx, s); err != nil {
		if !errors.Is(err, errSlotPolluted) {
			fmt.Fprintf(os.Stderr, "pgtest：重置数据库 %s 失败，丢弃该槽位：%v\n", s.name, err)
		}
		discardSlot(ctx, s)
		return
	}
	slotMu.Lock()
	freeSlots = append(freeSlots, s)
	slotMu.Unlock()
}

// closeSlotConn 关闭槽位专属的常驻连接。
func closeSlotConn(s *slot) {
	if s.resetConn != nil {
		_ = s.resetConn.Close(context.Background())
		s.resetConn = nil
	}
}

// discardSlot 关闭槽位连接、删除槽位库并把它从全部槽位列表中移除。
func discardSlot(ctx context.Context, s *slot) {
	closeSlotConn(s)
	if err := dropDatabase(ctx, s.name); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest：删除数据库 %s：%v\n", s.name, err)
	}
	slotMu.Lock()
	for i, cur := range allSlots {
		if cur == s {
			allSlots = append(allSlots[:i], allSlots[i+1:]...)
			break
		}
	}
	slotMu.Unlock()
}

// resetSlot 校验槽位结构、清空数据并还原种子行，最后确认没有残留连接。
func resetSlot(ctx context.Context, s *slot) error {
	conn, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := resetData(ctx, conn); err != nil {
		return err
	}
	// 连接池已经关闭，刚关闭的后端可能还在退出过程中，等一小会儿再看；仍有别的 goroutine 遗留的连接时不能复用。
	// 用槽位自己的常驻连接查询（pid <> pg_backend_pid() 排除自己），不抢全局 admin 锁。
	deadline := time.Now().Add(residualConnWait)
	for {
		var n int
		if err := conn.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()").Scan(&n); err != nil {
			return fmt.Errorf("检查残留连接：%w", err)
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("仍有 %d 个残留连接", n)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(residualConnPoll):
		}
	}
}

// resetData 在槽位的常驻连接上完成结构校验、清空和种子还原。
func resetData(ctx context.Context, conn *pgx.Conn) error {
	var sig string
	if err := conn.QueryRow(ctx, schemaSignatureSQL).Scan(&sig); err != nil {
		return fmt.Errorf("计算 schema 签名：%w", err)
	}
	if sig != templateSig {
		return errSlotPolluted
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if len(userTables) > 0 {
		names := make([]string, len(userTables))
		for i, t := range userTables {
			names[i] = qualified("public", t)
		}
		if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
			return fmt.Errorf("清空表：%w", err)
		}
	}
	for _, st := range seedTables {
		insert := "INSERT INTO " + qualified("public", st.name)
		if st.generatedAlways {
			insert += " OVERRIDING SYSTEM VALUE"
		}
		insert += " SELECT * FROM " + qualified(seedSchema, st.name)
		if _, err := tx.Exec(ctx, insert); err != nil {
			return fmt.Errorf("还原 %s 的种子数据：%w", st.name, err)
		}
	}
	// sequences 里只包含 TRUNCATE ... RESTART IDENTITY 处理不了的序列：迁移用 RESTART WITH 改过位置的
	// （例如 runner_upgrade_requests 从 1000 开始），以及不受 TRUNCATE 影响的独立序列。默认位置已由
	// RESTART IDENTITY 处理，避免每次归还都执行几十次 setval。
	for _, seq := range sequences {
		if _, err := tx.Exec(ctx, "SELECT setval($1::regclass, $2, $3)", qualified("public", seq.name), seq.lastValue, seq.isCalled); err != nil {
			return fmt.Errorf("还原序列 %s：%w", seq.name, err)
		}
	}
	return tx.Commit(ctx)
}

// metadata 是模板库的结构签名、要清空的表、要还原的种子表和序列位置。
type metadata struct {
	signature  string
	userTables []string
	seedTables []seedTable
	sequences  []sequenceState
}

// seedTable 是迁移后已有初始行、需要在每次归还时还原的表。
type seedTable struct {
	name            string
	generatedAlways bool
}

// sequenceState 是模板库迁移结束时 public 中一个序列的位置。
type sequenceState struct {
	name      string
	lastValue int64
	isCalled  bool
}

// buildMetadata 计算 public 的结构签名，记录要清空的表，并把有初始行的表快照到 seedSchema。
// 每次都重建快照 schema，模板库和测试用的探针表都走同一条路径。
func buildMetadata(ctx context.Context, conn *pgx.Conn) (metadata, error) {
	var m metadata
	if err := conn.QueryRow(ctx, schemaSignatureSQL).Scan(&m.signature); err != nil {
		return m, fmt.Errorf("计算 schema 签名：%w", err)
	}

	rows, err := conn.Query(ctx, userTablesSQL, migrationsTable)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return m, err
		}
		m.userTables = append(m.userTables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return m, err
	}
	rows.Close()

	seqRows, err := conn.Query(ctx, userSequencesSQL)
	if err != nil {
		return m, err
	}
	var seqNames []string
	for seqRows.Next() {
		var name string
		if err := seqRows.Scan(&name); err != nil {
			seqRows.Close()
			return m, err
		}
		seqNames = append(seqNames, name)
	}
	if err := seqRows.Err(); err != nil {
		seqRows.Close()
		return m, err
	}
	seqRows.Close()
	for _, name := range seqNames {
		st := sequenceState{name: name}
		get := "SELECT last_value, is_called FROM " + qualified("public", name)
		if err := conn.QueryRow(ctx, get).Scan(&st.lastValue, &st.isCalled); err != nil {
			return m, fmt.Errorf("读取序列 %s：%w", name, err)
		}
		// 有归属的序列（serial / identity）会被 TRUNCATE ... RESTART IDENTITY 重置到起始值；位置是默认的
		// （last_value = 1 且还没 nextval 过）就不必再 setval。独立序列（CREATE SEQUENCE）不受 TRUNCATE 影响，
		// 即使模板里是默认位置也要记录下来，否则测试推进过它就回不到初始状态。
		owned, err := sequenceOwned(ctx, conn, name)
		if err != nil {
			return m, err
		}
		if owned && st.lastValue == 1 && !st.isCalled {
			continue
		}
		m.sequences = append(m.sequences, st)
	}

	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{seedSchema}.Sanitize()+" CASCADE"); err != nil {
		return m, err
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{seedSchema}.Sanitize()); err != nil {
		return m, err
	}
	for _, table := range m.userTables {
		var n int64
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+qualified("public", table)).Scan(&n); err != nil {
			return m, fmt.Errorf("统计 %s 的行数：%w", table, err)
		}
		if n == 0 {
			continue
		}
		st := seedTable{name: table}
		if err := conn.QueryRow(ctx, generatedAlwaysSQL, table).Scan(&st.generatedAlways); err != nil {
			return m, fmt.Errorf("检查 %s 的 identity 列：%w", table, err)
		}
		snapshot := "CREATE TABLE " + qualified(seedSchema, table) + " AS TABLE " + qualified("public", table)
		if _, err := conn.Exec(ctx, snapshot); err != nil {
			return m, fmt.Errorf("保存 %s 的种子数据：%w", table, err)
		}
		m.seedTables = append(m.seedTables, st)
	}
	return m, nil
}

// sequenceOwned 判断序列是否被表的列拥有（serial / identity）。
func sequenceOwned(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var owned bool
	if err := conn.QueryRow(ctx, sequenceOwnedSQL, qualified("public", name)).Scan(&owned); err != nil {
		return false, fmt.Errorf("检查序列 %s 的归属：%w", name, err)
	}
	return owned, nil
}

// schemaSignatureSQL 对 public 中全部普通表 / 分区表、列与类型、约束、索引、序列、函数和触发器生成一个确定性签名。
// 任一结构被测试改动（重命名表、删约束、建函数等）都会得到不同的签名，从而丢弃该槽位。
const schemaSignatureSQL = `
SELECT md5(coalesce(string_agg(sig, E'\n' ORDER BY sig), ''))
FROM (
  SELECT format('table:%s', c.relname) AS sig
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
  UNION ALL
  SELECT format('column:%s.%s:%s:%s', c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull)
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT format('constraint:%s:%s:%s:%s', c.relname, con.conname, con.contype, pg_get_constraintdef(con.oid))
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public'
  UNION ALL
  SELECT format('index:%s:%s', tc.relname, pg_get_indexdef(i.indexrelid))
  FROM pg_index i
  JOIN pg_class tc ON tc.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = tc.relnamespace
  WHERE n.nspname = 'public'
  UNION ALL
  SELECT format('sequence:%s', c.relname)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND c.relkind = 'S'
  UNION ALL
  SELECT format('function:%s:%s', p.proname, pg_get_functiondef(p.oid))
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p')
  UNION ALL
  SELECT format('trigger:%s:%s:%s', c.relname, t.tgname, pg_get_triggerdef(t.oid))
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'public' AND NOT t.tgisinternal
) s`

// userTablesSQL 列出要清空的表：public 中的普通表和分区表，但保留迁移记录。
const userTablesSQL = `
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname <> $1
ORDER BY c.relname`

// userSequencesSQL 列出 public 中的序列：归还时要恢复到模板库迁移刚结束时的位置。
const userSequencesSQL = `
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind = 'S'
ORDER BY c.relname`

// sequenceOwnedSQL 判断序列是否被某个表的列拥有（serial / identity）。只有这类序列会被 TRUNCATE ... RESTART IDENTITY 重置。
const sequenceOwnedSQL = `
SELECT EXISTS (
  SELECT 1 FROM pg_depend d
  WHERE d.classid = 'pg_class'::regclass
    AND d.objid = $1::regclass
    AND d.refclassid = 'pg_class'::regclass
    AND d.deptype IN ('a', 'i')
)`

// generatedAlwaysSQL 判断表是否有 GENERATED ALWAYS AS IDENTITY 列：还原种子行时要用 OVERRIDING SYSTEM VALUE。
const generatedAlwaysSQL = `
SELECT coalesce(bool_or(a.attidentity = 'a'), false)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname = $1 AND a.attnum > 0 AND NOT a.attisdropped`

// qualified 返回带 schema 的安全标识符。
func qualified(schema, name string) string {
	return pgx.Identifier{schema, name}.Sanitize()
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
