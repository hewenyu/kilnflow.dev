package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// harness 是一个独立的数据库、CAS 和日志目录。
// testRetention 是测试使用的保留期：测试数据的时间（"1 day"、"8 days" 等）按 7 天的保留期安排，与默认值无关。
const testRetention = 7 * 24 * time.Hour

type harness struct {
	t    *testing.T
	db   *store.DB
	cas  *cas.FSStore
	logs *logstore.Store
	// logDir 是日志目录（logstore 把日志保存为 <logDir>/<编号>.log）。
	logDir string
	work   string
	c      *Collector
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := store.New(pgtest.Pool(t))
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	logs, err := logstore.New(logDir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, db: db, cas: &cas.FSStore{Root: filepath.Join(dir, "cas")}, logs: logs, logDir: logDir, work: filepath.Join(dir, "publish")}
	h.c = New(Config{Retention: testRetention, PublishWorkDir: h.work}, Deps{DB: db, CAS: h.cas, Logs: logs, Log: slog.New(slog.DiscardHandler)})
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url) VALUES (1, 1, 1, 'alice', 'app', 'https://example.com/alice/app.git')`)
	h.exec(`INSERT INTO runners (id, name, public_key) VALUES (1, 'mini', '\x00')`)
	return h
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.t.Context(), sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

func (h *harness) scan(sql string, dst any, args ...any) {
	h.t.Helper()
	if err := h.db.QueryRow(h.t.Context(), sql, args...).Scan(dst); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// run 写入一个 finishedAgo 之前结束的 run（finishedAgo 为空时 run 还在执行），返回 ID。
func (h *harness) run(finishedAgo string) int64 {
	h.t.Helper()
	state, finished := "succeeded", any(nil)
	if finishedAgo == "" {
		state = "active"
	} else {
		finished = finishedAgo
	}
	var id int64
	h.scan(`INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state, finished_at, created_at)
		VALUES (1, 'ci.yml', 'push', 'refs/heads/main', 'abc', 'webhook', gen_random_uuid()::text, $1,
			now() - $2::interval, now() - coalesce($2::interval, '0') - interval '1 hour') RETURNING id`, &id, state, finished)
	return id
}

// job 写入 run 中的一个 job（kind 为 normal 或 publish），带结果 manifest（可以为空），返回 ID。
func (h *harness) job(runID int64, kind, manifest string) int64 {
	h.t.Helper()
	var id int64
	h.scan(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest, created_at)
		VALUES ($1, $2, gen_random_uuid()::text, 'build', 'build', '{}', 'linux/arm64', 'host', 'succeeded', nullif($3, ''),
			(SELECT created_at FROM runs WHERE id=$2)) RETURNING id`, &id, kind, runID, manifest)
	return id
}

// attempt 写入 job 的一次 attempt 和它的日志，返回 attempt ID。
func (h *harness) attempt(jobID int64) int64 {
	h.t.Helper()
	var id int64
	h.scan(`INSERT INTO attempts (job_id, epoch, runner_id, acquire_request_id) VALUES ($1, (SELECT count(*)+1 FROM attempts WHERE job_id=$1), 1, 'r') RETURNING id`, &id, jobID)
	h.log(id)
	return id
}

func (h *harness) log(id int64) {
	h.t.Helper()
	if _, err := h.logs.Append(id, 0, []byte("hello\n")); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) hasLog(id int64) bool {
	h.t.Helper()
	n, err := h.logs.Size(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return n > 0
}

// blob 写入一个 CAS 对象及其 blobs 记录（last_ref_at 为 refAgo 之前），返回 digest。
func (h *harness) blob(data []byte, refAgo string) string {
	h.t.Helper()
	d := cas.Digest(data)
	if _, err := h.cas.Put(h.t.Context(), d, bytes.NewReader(data)); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO blobs (digest, size, last_ref_at) VALUES ($1, $2, now() - $3::interval)`, d, len(data), refAgo)
	return d
}

// result 写入一个结果：一个文件、它的输出 manifest 和结果 manifest，都在 refAgo 之前引用过。
// 返回结果 manifest 和文件的 digest。
func (h *harness) result(content, refAgo string) (string, string) {
	h.t.Helper()
	file := h.blob([]byte(content), refAgo)
	om, err := json.Marshal(cas.OutputManifest{Schema: cas.OutputSchema, Files: []cas.FileEntry{{Path: "bin", Digest: file, Size: int64(len(content))}}})
	if err != nil {
		h.t.Fatal(err)
	}
	omd := h.blob(om, refAgo)
	rm, err := json.Marshal(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{"bin": omd}})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.blob(rm, refAgo), file
}

func (h *harness) hasBlob(d string) bool {
	h.t.Helper()
	ok, err := h.cas.Has(h.t.Context(), d)
	if err != nil {
		h.t.Fatal(err)
	}
	var rows int
	h.scan(`SELECT count(*) FROM blobs WHERE digest=$1`, &rows, d)
	if ok != (rows == 1) {
		h.t.Fatalf("对象 %s：文件存在 = %v，记录数 = %d，应当一致", d, ok, rows)
	}
	return ok
}

func (h *harness) collect() Result {
	h.t.Helper()
	res, err := h.c.Collect(h.t.Context())
	if err != nil {
		h.t.Fatalf("Collect：%v", err)
	}
	return res
}

// 结束超过保留期的 run：删除其中各次 attempt 和发布 job 的日志，记录 purged_at；
// 还在保留期内、还没有结束的 run 不动；已经清理过的 run 不重复清理。
func TestCollectPurgesRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.run("8 days")
	build := h.job(old, "normal", "")
	a1, a2 := h.attempt(build), h.attempt(build)
	pub := h.job(old, "publish", "")
	h.log(logstore.PublishLogID(pub))

	recent := h.run("6 days")
	ra := h.attempt(h.job(recent, "normal", ""))
	active := h.run("")
	aa := h.attempt(h.job(active, "normal", ""))

	res := h.collect()
	if res.Runs != 1 || res.Logs != 3 {
		t.Errorf("清理结果：%+v，应清理 1 个 run 的 3 份日志", res)
	}
	for _, id := range []int64{a1, a2, logstore.PublishLogID(pub)} {
		if h.hasLog(id) {
			t.Errorf("日志 %d 应已删除", id)
		}
	}
	for _, id := range []int64{ra, aa} {
		if !h.hasLog(id) {
			t.Errorf("日志 %d 不应删除", id)
		}
	}
	run, err := cistore.GetRun(t.Context(), h.db, old, false)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Purged() || run.PurgedAt == nil {
		t.Errorf("run 应记为已清理：%+v", run.PurgedAt)
	}
	j, err := cistore.GetJob(t.Context(), h.db, build, false)
	if err != nil {
		t.Fatal(err)
	}
	if !run.JobPurged(j) {
		t.Error("run 中的 job 应记为已清理")
	}
	for _, id := range []int64{recent, active} {
		r, err := cistore.GetRun(t.Context(), h.db, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.PurgedAt != nil {
			t.Errorf("run %d 不应清理", id)
		}
	}
	if res := h.collect(); res.Runs != 0 {
		t.Errorf("第二次清理不应再处理同一个 run：%+v", res)
	}

	// 重跑后再次结束超过保留期：再清理一次，新的 job 才记为已清理
	h.exec(`UPDATE runs SET purged_at=now() - interval '3 days', finished_at=now() - interval '1 day' WHERE id=$1`, old)
	run, err = cistore.GetRun(t.Context(), h.db, old, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Purged() {
		t.Error("重跑结束后最新一次运行还没有清理")
	}
	if res := h.collect(); res.Runs != 0 {
		t.Errorf("重跑结束还不到保留期，不应清理：%+v", res)
	}
	h.exec(`UPDATE runs SET purged_at=now() - interval '10 days', finished_at=now() - interval '8 days' WHERE id=$1`, old)
	if res := h.collect(); res.Runs != 1 {
		t.Errorf("重跑结束超过保留期，应再清理一次：%+v", res)
	}
}

// 一个 run 的日志删不掉时只跳过这个 run（下一轮再试）：结束得更晚的 run 照常清理，其他步骤照常执行，
// 失败的 run 不记为已清理；返回的错误指出是哪个 run。
func TestCollectSkipsFailedRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stuck := h.run("9 days")
	sa := h.attempt(h.job(stuck, "normal", ""))
	// 把日志换成非空目录：os.Remove 删除失败
	p := filepath.Join(h.logDir, fmt.Sprintf("%d.log", sa))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	later := h.run("8 days")
	la := h.attempt(h.job(later, "normal", ""))
	h.exec(`INSERT INTO events (delivery_id, event_type, payload, processed_at) VALUES ('old', 'push', '{}', now() - interval '8 days')`)

	res, err := h.c.Collect(t.Context())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("run %d", stuck)) {
		t.Errorf("应返回 run %d 清理失败的错误：%v", stuck, err)
	}
	if res.Runs != 1 || res.Events != 1 {
		t.Errorf("清理结果：%+v，应清理 1 个 run 和 1 个事件", res)
	}
	if h.hasLog(la) {
		t.Error("结束得更晚的 run 的日志应已删除")
	}
	for id, want := range map[int64]bool{stuck: false, later: true} {
		r, err := cistore.GetRun(t.Context(), h.db, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Purged() != want {
			t.Errorf("run %d 已清理 = %v，应为 %v", id, r.Purged(), want)
		}
	}
}

// purgeRun 锁住 run 后发现不再需要清理（例如期间开始了重跑）时什么也不做，不计入清理的 run 数。
func TestPurgeRunSkipsIneligible(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.run("1 day")
	a := h.attempt(h.job(id, "normal", ""))
	err := h.db.Tx(t.Context(), func(tx pgx.Tx) error {
		purged, n, err := h.c.purgeRun(t.Context(), tx, id, time.Now().Add(-testRetention))
		if purged || n != 0 {
			t.Errorf("保留期内的 run 不应清理：purged=%v，删除 %d 份日志", purged, n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !h.hasLog(a) {
		t.Error("日志不应删除")
	}
}

// 产物的标记-清除：还没有清理的 run 和有效的缓存条目引用的对象保留；不可达且超过保留期没有引用的对象删除；
// 不可达但最近引用过的（例如正在上传）保留；没有记录的旧文件登记后删除，上传中断的临时文件删除。
func TestCollectSweepsBlobs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// 已清理的 run 的结果：不可达
	purgedResult, purgedFile := h.result("purged", "9 days")
	h.job(h.run("8 days"), "normal", purgedResult)
	// 保留期内的 run 的结果：可达（对象本身很久没有引用过，也不能删）
	liveResult, liveFile := h.result("live", "9 days")
	h.job(h.run("1 day"), "normal", liveResult)
	// 缓存条目引用的结果：最近命中的保留，过期的删除
	cachedResult, cachedFile := h.result("cached", "9 days")
	h.exec(`INSERT INTO action_cache (action_key, manifest_digest, producer_job_id, created_at, last_hit_at) VALUES ('k1', $1, 1, now() - interval '20 days', now() - interval '1 day')`, cachedResult)
	expiredResult, expiredFile := h.result("expired", "9 days")
	h.exec(`INSERT INTO action_cache (action_key, manifest_digest, producer_job_id, created_at) VALUES ('k2', $1, 1, now() - interval '8 days')`, expiredResult)
	// 不可达但最近引用过：保留
	fresh := h.blob([]byte("uploading"), "1 hour")
	// 没有记录的旧文件和上传临时文件
	orphan := cas.Digest([]byte("orphan"))
	if _, err := h.cas.Put(t.Context(), orphan, bytes.NewReader([]byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	orphanPath, err := h.cas.Path(orphan)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(orphanPath), ".upload-123")
	if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-9 * 24 * time.Hour)
	for _, p := range []string{orphanPath, tmp} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	res := h.collect()
	for _, d := range []string{liveResult, liveFile, cachedResult, cachedFile, fresh} {
		if !h.hasBlob(d) {
			t.Errorf("对象 %s 应保留", d)
		}
	}
	for _, d := range []string{purgedResult, purgedFile, expiredResult, expiredFile, orphan} {
		if h.hasBlob(d) {
			t.Errorf("对象 %s 应删除", d)
		}
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("上传临时文件应删除：%v", err)
	}
	// 两个结果各 3 个对象、孤立文件和临时文件
	if res.CacheEntries != 1 || res.Blobs != 8 || res.BlobBytes == 0 {
		t.Errorf("清理结果：%+v", res)
	}
	var keys []string
	rows, err := h.db.Query(t.Context(), `SELECT action_key FROM action_cache ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if len(keys) != 1 || keys[0] != "k1" {
		t.Errorf("缓存条目 = %v，只应保留 k1", keys)
	}
}

// 损坏的 manifest（内容与 digest 不一致）不会让清理停摆：标记时跳过它，其他不可达的旧对象照常删除。
// 之前它让每一轮都返回错误、不清除任何对象，直到引用它的 run 也过了保留期。
func TestCollectSkipsCorruptManifest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	corruptResult, _ := h.result("corrupt-result", "9 days")
	h.job(h.run("1 day"), "normal", corruptResult)
	brokenOutput, _ := h.result("corrupt-output", "9 days")
	h.job(h.run("1 day"), "normal", brokenOutput)
	liveResult, liveFile := h.result("live", "9 days")
	h.job(h.run("1 day"), "normal", liveResult)
	stale := h.blob([]byte("stale"), "9 days")

	overwrite := func(d string) {
		t.Helper()
		p, err := h.cas.Path(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	overwrite(corruptResult)
	var rm cas.ResultManifest
	if err := cas.ReadJSON(t.Context(), h.cas, brokenOutput, &rm); err != nil {
		t.Fatal(err)
	}
	overwrite(rm.Outputs["bin"])

	h.collect()
	if h.hasBlob(stale) {
		t.Error("不可达的旧对象应删除")
	}
	for _, d := range []string{corruptResult, brokenOutput, liveResult, liveFile} {
		if !h.hasBlob(d) {
			t.Errorf("对象 %s 应保留", d)
		}
	}
}

// 删除时在行锁下复查 last_ref_at：标记之后被重新引用（PrepareUpload 刷新了 last_ref_at）的对象不删除。
func TestDeleteBlobRechecksLastRef(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	d := h.blob([]byte("x"), "9 days")
	h.exec(`UPDATE blobs SET last_ref_at=now() WHERE digest=$1`, d)
	var res Result
	if err := h.c.deleteBlob(t.Context(), d, time.Now().Add(-testRetention), &res); err != nil {
		t.Fatal(err)
	}
	if !h.hasBlob(d) || res.Blobs != 0 {
		t.Errorf("重新引用过的对象不应删除：%+v", res)
	}
}

// 已处理超过保留期的 webhook 事件、已投递或放弃的 outbox 记录、过期的注册令牌和发布 job 残留的工作目录被删除；
// 未处理的事件、待投递的 outbox 记录和保留期内的不动。
func TestCollectDeletesRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`INSERT INTO events (delivery_id, event_type, payload, processed_at) VALUES
		('old', 'push', '{}', now() - interval '8 days'), ('new', 'push', '{}', now() - interval '1 day'), ('pending', 'push', '{}', NULL)`)
	h.exec(`INSERT INTO outbox (kind, state, updated_at) VALUES
		('check', 'done', now() - interval '8 days'), ('check', 'dead', now() - interval '8 days'),
		('check', 'pending', now() - interval '8 days'), ('check', 'done', now())`)
	h.exec(`INSERT INTO enrollment_tokens (token_hash, expires_at) VALUES ('a', now() - interval '8 days'), ('b', now())`)
	oldDir, newDir := filepath.Join(h.work, "1"), filepath.Join(h.work, "2")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatal(err)
	}

	res := h.collect()
	if res.Events != 1 || res.Outbox != 2 || res.EnrollTokens != 1 || res.WorkDirs != 1 {
		t.Errorf("清理结果：%+v", res)
	}
	var n int
	h.scan(`SELECT count(*) FROM events`, &n)
	if n != 2 {
		t.Errorf("events 剩 %d 行，应剩 2 行", n)
	}
	h.scan(`SELECT count(*) FROM outbox`, &n)
	if n != 2 {
		t.Errorf("outbox 剩 %d 行，应剩 2 行", n)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("残留的工作目录应删除：%v", err)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("保留期内的工作目录不应删除：%v", err)
	}
}

// 删除了内容时写一条审计日志，什么也没删除时不写。
func TestRunOnceAudits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.attempt(h.job(h.run("8 days"), "normal", ""))
	h.c.runOnce(t.Context())
	h.c.runOnce(t.Context())
	var n int
	var detail []byte
	h.scan(`SELECT count(*) FROM audit_log WHERE action='retention.collect' AND actor='kiln'`, &n)
	if n != 1 {
		t.Fatalf("审计日志 %d 条，应为 1 条（第二次没有清理内容）", n)
	}
	h.scan(`SELECT detail::text FROM audit_log WHERE action='retention.collect'`, &detail)
	var res Result
	if err := json.Unmarshal(detail, &res); err != nil || res.Runs != 1 || res.Logs != 1 {
		t.Errorf("审计日志的内容：%s（%v）", detail, err)
	}
}

// fakePurger 是一个领域的清理，记录调用时的 cutoff。
type fakePurger struct {
	cutoff time.Time
	n      map[string]int64
	err    error
}

func (f *fakePurger) Purge(_ context.Context, cutoff time.Time) (map[string]int64, error) {
	f.cutoff = cutoff
	return f.n, f.err
}

// TestCollectPurgers：其他领域的清理每轮按同一个 cutoff 调用，结果累加到 Domains；一个出错不影响其他的，错误一并返回。
func TestCollectPurgers(t *testing.T) {
	t.Parallel()
	a := &fakePurger{n: map[string]int64{"ai_requests": 2}}
	b := &fakePurger{n: map[string]int64{"ai_requests": 1, "ai_logs": 3}, err: errors.New("boom")}
	c := New(Config{}, Deps{Purgers: []Purger{a, b}})
	var res Result
	before := time.Now().Add(-DefaultRetention)
	err := c.runPurgers(t.Context(), before, &res)
	if err == nil {
		t.Fatal("领域清理的错误应返回")
	}
	if res.Domains["ai_requests"] != 3 || res.Domains["ai_logs"] != 3 {
		t.Fatalf("Domains = %v", res.Domains)
	}
	if !a.cutoff.Equal(before) || !b.cutoff.Equal(before) {
		t.Fatalf("cutoff = %v / %v，期望 %v", a.cutoff, b.cutoff, before)
	}
	if res.empty() {
		t.Fatal("领域删除了内容时不应算作空")
	}
}
