package decisioninbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ReconcileSync 的并发回归：锁外读到的状态在排队等锁期间可能被后台处理或人工结案改掉。
// 这些用例用真实的 Postgres 会话级 advisory lock，并通过 pg_stat_activity 确认调用方已经阻塞在等锁上，
// 从而确定性地复现“锁外读到 attempted / unknown，等锁期间被改掉”的时序。

// stateWaitTimeout 是等待 ReconcileSync 阻塞在 advisory lock 上的上限。
const stateWaitTimeout = 10 * time.Second

// reconcileOutcome 汇总后台 goroutine 的调用结果。
type reconcileOutcome struct {
	res *ReconcileResult
	err error
}

// lockRepo 在一条专用连接上持有仓库的 decision-inbox advisory lock，返回该连接与幂等的释放函数。
// 连接本身不在事务里，可以在持锁期间执行其他语句（例如模拟并发改状态）。
func lockRepo(t *testing.T, db *store.DB, repoID int64) (*pgxpool.Conn, func()) {
	t.Helper()
	ctx := t.Context()
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire：%v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey(repoID)); err != nil {
		conn.Release()
		t.Fatalf("获取 advisory lock：%v", err)
	}
	released := false
	return conn, func() {
		if released {
			return
		}
		released = true
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey(repoID)); err != nil {
			t.Errorf("释放 advisory lock：%v", err)
		}
		conn.Release()
	}
}

// waitForAdvisoryWaiter 等到该测试数据库里出现一个等待 advisory lock 的会话。
// 每个测试用独立的数据库（pgtest），current_database() 只包含本用例的连接。
func waitForAdvisoryWaiter(t *testing.T, db *store.DB) {
	t.Helper()
	deadline := time.Now().Add(stateWaitTimeout)
	for {
		var waiting int
		if err := db.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`).Scan(&waiting); err != nil {
			t.Fatalf("查询 pg_stat_activity：%v", err)
		}
		if waiting >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ReconcileSync 没有阻塞在 advisory lock 上")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flipSyncState 模拟另一条路径在等锁期间把条目从 from 改成 to（终态，next_retry_at 置空）。
func flipSyncState(t *testing.T, conn *pgxpool.Conn, id int64, from, to string) {
	t.Helper()
	tag, err := conn.Exec(t.Context(), `UPDATE dev_decision_syncs SET state=$3, next_retry_at=NULL, updated_at=now()
		WHERE id=$1 AND state=$2`, id, from, to)
	if err != nil {
		t.Fatalf("并发改状态 %s -> %s：%v", from, to, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("并发改状态 %s -> %s 影响 %d 行，期望 1", from, to, tag.RowsAffected())
	}
}

// markerDoc 返回一篇包含指定 marker 的文档。
func markerDoc(marker string) docprovider.Document {
	doc := newDoc()
	doc.Content += "`" + marker + "`\n"
	return doc
}

// 用例 A：锁外读到 attempted，等锁期间后台处理把它转成 unknown；释放锁后必须用锁内重读到的状态推进，
// 找到 marker -> delivered，返回 Found=true 且状态确实写入数据库。
func TestReconcileSyncConcurrentAttemptedToUnknownFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	w := &fakeWriter{doc: markerDoc(marker)}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{State: "attempted", TargetGen: cfg.Generation})

	holder, release := lockRepo(t, db, testRepoID)
	defer release()

	out := make(chan reconcileOutcome, 1)
	go func() {
		res, err := svc.ReconcileSync(t.Context(), id, "admin")
		out <- reconcileOutcome{res, err}
	}()
	waitForAdvisoryWaiter(t, db)
	flipSyncState(t, holder, id, "attempted", "unknown")
	release()

	got := <-out
	if got.err != nil {
		t.Fatalf("ReconcileSync：%v", got.err)
	}
	if !got.res.Found || got.res.Sync == nil || got.res.Sync.State != "delivered" {
		t.Fatalf("结果 = Found=%v Sync=%+v，期望 Found=true 且 delivered", got.res.Found, got.res.Sync)
	}
	if v := mustSync(t, db, id); v.State != "delivered" {
		t.Fatalf("数据库状态 = %s，期望 delivered", v.State)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("复查绝不能写文档，实际 %d 次", sets)
	}
}

// 用例 B：等锁期间条目被并发改成终态 cancelled（attempted -> cancelled 与 unknown -> cancelled 两条路径）。
// 释放锁后必须返回明确的“状态已变化”错误，绝不返回 Found=true，也不访问文档，数据库保持 cancelled。
func TestReconcileSyncConcurrentTerminalChangeRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		from string
	}{
		{"attempted_to_cancelled", "attempted"},
		{"unknown_to_cancelled", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t)
			insertFixture(t, db)
			cfg := putConfig(t, db, testDocID)
			// 即便文档里有 marker，也不能因为状态已变而误报成功。
			marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
			w := &fakeWriter{doc: markerDoc(marker)}
			svc := newService(t, db, w)
			id := insertSync(t, db, syncSeed{State: tc.from, TargetGen: cfg.Generation})

			holder, release := lockRepo(t, db, testRepoID)
			defer release()

			out := make(chan reconcileOutcome, 1)
			go func() {
				res, err := svc.ReconcileSync(t.Context(), id, "admin")
				out <- reconcileOutcome{res, err}
			}()
			waitForAdvisoryWaiter(t, db)
			flipSyncState(t, holder, id, tc.from, "cancelled")
			release()

			got := <-out
			if got.err == nil {
				t.Fatalf("状态已变时应返回错误，实际 res = %+v", got.res)
			}
			if !strings.Contains(got.err.Error(), "状态已经变化") {
				t.Errorf("错误 = %v，期望包含“状态已经变化”", got.err)
			}
			if v := mustSync(t, db, id); v.State != "cancelled" {
				t.Fatalf("数据库状态 = %s，期望保持 cancelled", v.State)
			}
			if gets, _ := w.calls(); gets != 0 {
				t.Errorf("状态已变时不应访问文档，实际读取 %d 次", gets)
			}
		})
	}
}

// 用例 C：锁外读到 attempted，等锁期间被转成 unknown；没有找到 marker 时不报错，
// 保持 unknown，返回 Found=false 且状态为 unknown。
func TestReconcileSyncConcurrentAttemptedToUnknownNotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{State: "attempted", TargetGen: cfg.Generation})

	holder, release := lockRepo(t, db, testRepoID)
	defer release()

	out := make(chan reconcileOutcome, 1)
	go func() {
		res, err := svc.ReconcileSync(t.Context(), id, "admin")
		out <- reconcileOutcome{res, err}
	}()
	waitForAdvisoryWaiter(t, db)
	flipSyncState(t, holder, id, "attempted", "unknown")
	release()

	got := <-out
	if got.err != nil {
		t.Fatalf("ReconcileSync：%v", got.err)
	}
	if got.res.Found {
		t.Error("没有 marker 时不应 Found")
	}
	if got.res.Sync == nil || got.res.Sync.State != "unknown" {
		t.Fatalf("返回状态 = %+v，期望 unknown", got.res.Sync)
	}
	if v := mustSync(t, db, id); v.State != "unknown" {
		t.Fatalf("数据库状态 = %s，期望 unknown", v.State)
	}
}

// 条件更新必须检查影响行数：拿到锁并重读状态之后、写库之前，条目又被另一条路径（配置轮转走的是另一把锁）
// 改成 cancelled，条件更新影响 0 行。这时必须报错，不能返回 Found=true。
func TestReconcileSyncStateChangedDuringVerifyIsRejected(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	w := &fakeWriter{doc: markerDoc(marker)}
	id := insertSync(t, db, syncSeed{State: "unknown", TargetGen: cfg.Generation})
	// 只读查证期间把条目改成 cancelled（已拿到锁、已重读过状态，但在写库之前）。
	w.get = func(f *fakeWriter) (docprovider.Document, error) {
		if _, err := db.Exec(t.Context(), `UPDATE dev_decision_syncs SET state='cancelled', next_retry_at=NULL,
			updated_at=now() WHERE id=$1 AND state='unknown'`, id); err != nil {
			t.Errorf("并发改状态：%v", err)
		}
		return f.doc, nil
	}
	svc := newService(t, db, w)

	res, err := svc.ReconcileSync(t.Context(), id, "admin")
	if !errors.Is(err, errSyncStateChanged) {
		t.Fatalf("err = %v，期望 errSyncStateChanged", err)
	}
	if res != nil {
		t.Fatalf("出错时不应返回结果：%+v", res)
	}
	if v := mustSync(t, db, id); v.State != "cancelled" {
		t.Fatalf("数据库状态 = %s，期望保持 cancelled", v.State)
	}
}
