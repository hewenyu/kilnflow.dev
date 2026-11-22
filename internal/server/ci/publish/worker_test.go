package publish

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// claimRow 是领取相关的 job 字段。
type claimRow struct {
	State        store.JobState
	AttemptCount int
	Leased       bool
	Reason       string
}

func (e *pubEnv) claimRow(id int64) claimRow {
	e.t.Helper()
	var c claimRow
	if err := e.db.QueryRow(e.t.Context(), `SELECT state, attempt_count, lease_expires_at IS NOT NULL, coalesce(reason, '') FROM jobs WHERE id=$1`, id).
		Scan(&c.State, &c.AttemptCount, &c.Leased, &c.Reason); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// 领取：pending 的发布 job 置为 running 并标记"正在由本进程执行"；有标记的不会被再次领取；
// ResetClaims（Server 启动时）只清掉发布 job 的标记，让中断的发布继续执行，不重复计数。普通 job 不受影响。
func TestWorker_claimPublishJob(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	normal := e.upstream(r, "lint", "out", nil, nil)
	e.exec(`UPDATE jobs SET state='running', lease_expires_at=now()+interval '1 minute' WHERE id=$1`, normal)
	id := e.publishJob(r, ghRelease())
	ctx := t.Context()

	j, handled, err := e.w.claimPublishJob(ctx)
	if err != nil || j == nil || j.ID != id || !handled {
		t.Fatalf("应领取发布 job %d：%v %v %v", id, j, handled, err)
	}
	if c := e.claimRow(id); c.State != store.JobRunning || c.AttemptCount != 1 || !c.Leased || c.Reason != "正在发布" {
		t.Fatalf("领取后：%+v", c)
	}
	var outbox int
	if err := e.db.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dedupe_key=$1`, "checkrun:job:"+itoa(id)).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("领取时应写入 Check Run 同步记录：%d %v", outbox, err)
	}

	if j, handled, err := e.w.claimPublishJob(ctx); err != nil || j != nil || handled {
		t.Fatalf("正在执行的发布 job 不应被再次领取：%v %v %v", j, handled, err)
	}

	if err := e.w.ResetClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if c := e.claimRow(normal); !c.Leased {
		t.Fatal("ResetClaims 不应清掉普通 job 的租约")
	}
	j, _, err = e.w.claimPublishJob(ctx)
	if err != nil || j == nil || j.ID != id {
		t.Fatalf("ResetClaims 后应继续领取中断的发布 job：%v %v", j, err)
	}
	if c := e.claimRow(id); c.AttemptCount != 1 || !c.Leased {
		t.Fatalf("继续执行不应重新计数：%+v", c)
	}

	// 标记过期（上一次结束 job 失败）：重新领取，不需要等 Server 重启
	e.exec(`UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, id)
	j, _, err = e.w.claimPublishJob(ctx)
	if err != nil || j == nil || j.ID != id {
		t.Fatalf("标记过期后应重新领取发布 job：%v %v", j, err)
	}
	if c := e.claimRow(id); c.AttemptCount != 1 || !c.Leased {
		t.Fatalf("重新领取不应重新计数，并应重新标记：%+v", c)
	}
}

// 领取顺序：多个 pending 的发布 job 按排队顺序（cistore.QueueOrderSQL）领取：先提交的 run 先，不按 job ID。
func TestWorker_claimPublishOrder(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	late := e.publishJob(e.tagRun("v1.2.0"), ghRelease())
	early := e.publishJob(e.tagRun("v1.3.0"), ghRelease())
	// 先插入的 late 属于后提交的 run
	e.exec(`UPDATE jobs SET created_at=now() - interval '1 hour' WHERE id=$1`, early)
	for _, want := range []int64{early, late} {
		j, _, err := e.w.claimPublishJob(t.Context())
		if err != nil || j == nil || j.ID != want {
			t.Fatalf("应领取先提交的发布 job %d：%v %v", want, j, err)
		}
	}
}

// 取消：还没开始的发布 job 直接取消，不访问 GitHub；开始后、上传前请求取消也会停止，Release 保持 draft。
func TestWorker_publishCancelled(t *testing.T) {
	t.Parallel()
	t.Run("开始前", func(t *testing.T) {
		t.Parallel()
		e := newPubEnv(t)
		r := e.tagRun("v1.2.0")
		e.builds(r)
		id := e.publishJob(r, ghRelease())
		e.exec(`UPDATE jobs SET cancel_requested=true WHERE id=$1`, id)
		e.publish()
		e.wantJob(id, store.JobCancelled, "")
		if n := len(e.fake.Requests()); n != 0 {
			t.Fatalf("不应访问 GitHub，实际 %d 个请求", n)
		}
	})
	t.Run("上传前", func(t *testing.T) {
		t.Parallel()
		e := newPubEnv(t)
		r := e.tagRun("v1.2.0")
		e.builds(r)
		id := e.publishJob(r, ghRelease())
		// 创建 draft 时请求取消（模拟用户在发布进行中点了取消）
		e.faults.set(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/releases") {
				if _, err := e.db.Exec(context.WithoutCancel(req.Context()), `UPDATE jobs SET cancel_requested=true WHERE id=$1`, id); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		e.publish()
		e.wantJob(id, store.JobCancelled, "")
		if rel := e.oneRelease(srcRepo); !rel.Draft || len(rel.Assets) != 0 {
			t.Fatalf("取消后不应上传 asset 或正式发布：draft=%v %v", rel.Draft, assetNames(rel))
		}
		if l := e.log(id); !strings.Contains(l, "已取消") {
			t.Fatalf("日志：\n%s", l)
		}
	})
}

// 发布循环：被 notify.Hub 唤醒后领取并执行发布 job，ctx 结束时退出。
func TestWorker_Run(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.w.Run(ctx)
	}()
	e.hub.All()
	waitFor(t, e.hub, 30*time.Second, "发布 job 结束", func() bool {
		state, _ := e.jobResult(id)
		return state.Terminal()
	})
	e.wantJob(id, store.JobSucceeded, "")
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 结束后 Run 应退出")
	}
}
