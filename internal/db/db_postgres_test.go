package db_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/db"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/migrations"
)

// 需要 Postgres 的测试在外部测试包中：pgtest 依赖本包，内部测试包不能导入它。

func TestMain(m *testing.M) { pgtest.Main(m) }

// migrationVersions 返回内嵌的全部迁移的版本号（按文件名排序即版本顺序）。
func migrationVersions(t *testing.T) []int {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, n := range names {
		var v int
		if _, err := fmt.Sscanf(n, "%d_", &v); err != nil {
			t.Fatalf("迁移文件名 %s：%v", n, err)
		}
		out = append(out, v)
	}
	return out
}

// emptyDatabase 返回一个新数据库的地址，并清空其中已执行的迁移（public schema 重建为空）。
func emptyDatabase(t *testing.T) string {
	t.Helper()
	u := pgtest.URL(t)
	exec(t, u, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	return u
}

// exec 用单独的连接执行 SQL。
func exec(t *testing.T, u, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(t.Context()))
	if _, err := conn.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("执行 %q：%v", sql, err)
	}
}

func open(t *testing.T, u string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(t.Context(), u)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// appliedVersions 返回 schema_migrations 中记录的版本号。
func appliedVersions(t *testing.T, pool *pgxpool.Pool) []int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 在空数据库上 Open 执行全部迁移并逐个记录版本号。
func TestOpen_MigratesEmptyDatabase(t *testing.T) {
	t.Parallel()
	pool := open(t, emptyDatabase(t))
	if got, want := appliedVersions(t, pool), migrationVersions(t); !slices.Equal(got, want) {
		t.Fatalf("schema_migrations = %v，应为 %v", got, want)
	}
	// 最后一个迁移（移除审批）已生效
	var n int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_name='jobs' AND column_name='approved_by'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("jobs.approved_by 应已删除：%d, %v", n, err)
	}
}

// 重复执行 Migrate 不会重新执行已记录的迁移。
func TestMigrate_Idempotent(t *testing.T) {
	t.Parallel()
	pool := open(t, pgtest.URL(t))
	var before time.Time
	if err := pool.QueryRow(t.Context(), `SELECT max(applied_at) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := db.Migrate(t.Context(), pool); err != nil {
			t.Fatalf("第 %d 次重复 Migrate：%v", i+1, err)
		}
	}
	var after time.Time
	if err := pool.QueryRow(t.Context(), `SELECT max(applied_at) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) || !slices.Equal(appliedVersions(t, pool), migrationVersions(t)) {
		t.Fatalf("重复 Migrate 不应重新执行迁移：applied_at %v → %v，版本 %v", before, after, appliedVersions(t, pool))
	}
}

// 多个 Server 同时启动：advisory lock 保证迁移只执行一次，全部成功。
func TestMigrate_ConcurrentServers(t *testing.T) {
	t.Parallel()
	u := emptyDatabase(t)
	const servers = 4
	pools := make([]*pgxpool.Pool, servers)
	for i := range pools {
		p, err := pgxpool.New(t.Context(), u)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		pools[i] = p
	}
	var wg sync.WaitGroup
	errs := make([]error, servers)
	for i, p := range pools {
		wg.Go(func() { errs[i] = db.Migrate(t.Context(), p) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个 Migrate：%v", i, err)
		}
	}
	if got, want := appliedVersions(t, pools[0]), migrationVersions(t); !slices.Equal(got, want) {
		t.Fatalf("schema_migrations = %v，应为 %v", got, want)
	}
}

// 迁移锁被其他会话持有时 Migrate 等待；等到 ctx 结束时返回获取锁的错误。锁释放后可以继续。
func TestMigrate_WaitsForLock(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	pool := open(t, u)
	holder, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(context.WithoutCancel(t.Context()))
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, db.MigrateLockID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := db.Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "获取迁移锁") {
		t.Fatalf("锁被占用时应返回获取迁移锁的错误：%v", err)
	}
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_unlock($1)`, db.MigrateLockID); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("锁释放后 Migrate：%v", err)
	}
}

// Migrate 结束后（成功或失败）都释放迁移锁，连接回到连接池时不带着锁。
func TestMigrate_ReleasesLock(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	pool := open(t, u)
	// 让版本 1 看起来没有执行过：再次执行会因为表已存在而失败
	exec(t, u, `DELETE FROM schema_migrations WHERE version=1`)
	err := db.Migrate(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "0001_init.sql") {
		t.Fatalf("应返回迁移 0001 的错误：%v", err)
	}
	// 失败的迁移整体回滚，没有记录版本号
	if got := appliedVersions(t, pool); slices.Contains(got, 1) {
		t.Fatalf("失败的迁移不应记录：%v", got)
	}
	// 其他会话能立即拿到锁
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(t.Context()))
	var ok bool
	if err := conn.QueryRow(t.Context(), `SELECT pg_try_advisory_lock($1)`, db.MigrateLockID).Scan(&ok); err != nil || !ok {
		t.Fatalf("Migrate 失败后迁移锁应已释放：%v, %v", ok, err)
	}
}

func TestOpen_MaxConns(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	withParam := func(k, v string) string {
		c := *p
		q := c.Query()
		q.Set(k, v)
		c.RawQuery = q.Encode()
		return c.String()
	}
	pass, _ := p.User.Password()
	keyValue := fmt.Sprintf("host=%s user=%s password='%s' dbname=%s sslmode=disable pool_max_conns=2",
		p.Hostname(), p.User.Username(), pass, strings.TrimPrefix(p.Path, "/"))
	// 地址中没有端口时（例如 CI 中的 postgres://…@postgres/…）不写 port，用默认的 5432；写成空的 port= 无法解析
	if port := p.Port(); port != "" {
		keyValue += " port=" + port
	}
	for _, c := range []struct {
		name string
		url  string
		want int32
	}{
		{"没有配置时用默认上限", u, db.DefaultMaxConns},
		{"URL 中显式配置", withParam("pool_max_conns", "3"), 3},
		{"显式配置大于默认值", withParam("pool_max_conns", "40"), 40},
		{"key=value 写法", keyValue, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := open(t, c.url).Config().MaxConns; got != c.want {
				t.Fatalf("MaxConns = %d，应为 %d", got, c.want)
			}
		})
	}
}

func TestOpen_Errors(t *testing.T) {
	t.Parallel()
	u, err := url.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	missing := *u
	missing.Path = "/kiln_no_such_database"
	for _, c := range []struct {
		name, url, want string
	}{
		{"地址无法解析", "postgres://%zz", "解析数据库地址"},
		{"连接池参数无效", u.String() + "&pool_max_conns=abc", "解析数据库地址"},
		{"连不上", "postgres://postgres@127.0.0.1:1/x?sslmode=disable&connect_timeout=2", "连接数据库"},
		{"数据库不存在", missing.String(), "连接数据库"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pool, err := db.Open(t.Context(), c.url)
			if err == nil {
				pool.Close()
				t.Fatal("应返回错误")
			}
			if pool != nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Open = %v, %v；错误应包含 %q", pool, err, c.want)
			}
		})
	}
}

// 0016：已有的 pull_request run 的 dedupe_key 补上目标分支（与 planner.dedupeKey 的新格式一致），其他 key 不变；可以重复执行。
func TestMigrate_PRDedupeKeyBaseRef(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	exec(t, u, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`)
	runs := []struct{ workflow, event, ref, key, ctx, want string }{
		{
			"ci.yml", "pull_request", "refs/pull/5/head", "42:ci.yml:pull_request:refs/pull/5/head:aaaa", `{"base_ref":"main"}`,
			"42:ci.yml:pull_request:refs/pull/5/head:aaaa:main",
		},
		{
			"lint.yml", "pull_request", "refs/pull/5/head", "42:lint.yml:pull_request:refs/pull/5/head:aaaa:main", `{"base_ref":"main"}`,
			"42:lint.yml:pull_request:refs/pull/5/head:aaaa:main",
		},
		{
			"ai-review", "pull_request", "refs/pull/5/head", "42:ai-review:pull_request:refs/pull/5/head:aaaa:main", `{"base_ref":"main"}`,
			"42:ai-review:pull_request:refs/pull/5/head:aaaa:main",
		},
		{"ci.yml", "push", "refs/heads/main", "42:ci.yml:push:refs/heads/main:aaaa", `{}`, "42:ci.yml:push:refs/heads/main:aaaa"},
	}
	for _, r := range runs {
		exec(t, u, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, github_ctx, state) VALUES (42, $1, $2, $3, 'aaaa', 'webhook', $4, $5, 'active')`,
			r.workflow, r.event, r.ref, r.key, r.ctx)
	}
	pool := open(t, u)
	for range 2 {
		exec(t, u, `DELETE FROM schema_migrations WHERE version=16`)
		if err := db.Migrate(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(t.Context(), `SELECT dedupe_key FROM runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range runs {
		if got[i] != r.want {
			t.Errorf("run %d：dedupe_key = %q，应为 %q", i, got[i], r.want)
		}
	}
}

// 0017：pr_plans 不从升级前的数据回填（ref_heads 和已有的 run 证明不了规划完成：规划中途失败时已经有部分 run，
// 基线也记录 ref_heads），合并队列不会把只规划了一部分的 head 当成可以合并。
func TestMigrate_PRPlansNoBackfill(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	exec(t, u, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`)
	exec(t, u, `INSERT INTO ref_heads (repo_id, ref, sha, base_ref) VALUES (42, 'refs/pull/5/head', 'aaaa', 'main')`)
	exec(t, u, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, github_ctx, state, pr_number)
		VALUES (42, 'ci.yml', 'pull_request', 'refs/pull/5/head', 'aaaa', 'webhook', 'k', '{"base_ref":"main"}', 'succeeded', 5)`)
	exec(t, u, `DROP TABLE pr_plans; DELETE FROM schema_migrations WHERE version=17`)
	pool := open(t, u)
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pr_plans`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pr_plans 有 %d 行（%v），升级时不应回填", n, err)
	}
}

// 0021：旧版本已经请求过关闭、结果不明的 issue（issues_marks 中有、issues_closed 中没有）记到 issues_uncertain，
// 新版本不再留言、不再关闭它（独立审查发现：以前直接删掉 issues_marks，用户重新打开的 issue 会被再次关闭）。
func TestMigrate_IssuesMarksToUncertain(t *testing.T) {
	t.Parallel()
	u := pgtest.URL(t)
	exec(t, u, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`)
	// 回到 0021 之前的表结构
	exec(t, u, `ALTER TABLE merge_queue DROP COLUMN comment_state, DROP COLUMN issues, DROP COLUMN issues_uncertain,
		ADD COLUMN issues_marks jsonb NOT NULL DEFAULT '{}'; DELETE FROM schema_migrations WHERE version=21`)
	rows := []struct {
		pr     int
		state  string
		closed string
		marks  string
		want   []int32
	}{
		{1, "pending", "{4}", `{"3": 10, "4": 11, "12": 12}`, []int32{3, 12}}, // #4 已经关闭过，不算结果不明
		{2, "pending", "{}", `{}`, []int32{}},
		{3, "done", "{}", `{"5": 1}`, []int32{}}, // 已经结束的不再处理
	}
	for _, r := range rows {
		exec(t, u, `INSERT INTO merge_queue (repo_id, pr_number, head_sha, enqueued_by, state, issues_state, issues_closed, issues_marks)
			VALUES (42, $1, 'aaaa', 'alice', 'merged', $2, $3::int[], $4::jsonb)`, r.pr, r.state, r.closed, r.marks)
	}
	pool := open(t, u)
	for _, r := range rows {
		var got []int32
		var issues *string
		if err := pool.QueryRow(t.Context(), `SELECT issues_uncertain, issues::text FROM merge_queue WHERE pr_number=$1`, r.pr).Scan(&got, &issues); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, r.want) || issues != nil {
			t.Errorf("PR #%d：issues_uncertain = %v、issues = %v，想要 %v、NULL", r.pr, got, issues, r.want)
		}
	}
}
