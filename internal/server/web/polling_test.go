package web

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Dashboard 高频轮询（总览每 5 秒刷新）的查询次数与计数正确性测试。

// failQuerier 在收到任何查询时终止测试：用来证明空列表不会白跑一次数据库往返。
type failQuerier struct{ t *testing.T }

func (q failQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Fatal("没有 run 时不应查询数据库")
	return nil, nil
}

func (q failQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Fatal("没有 run 时不应查询数据库")
	return nil
}

// TestCurrentRerunStartsEmpty：空的 run 列表直接返回空结果，不发送 ANY('{}') 查询。
func TestCurrentRerunStartsEmpty(t *testing.T) {
	t.Parallel()
	for _, ids := range [][]int64{nil, {}} {
		got, err := currentRerunStarts(t.Context(), failQuerier{t}, ids)
		if err != nil {
			t.Fatalf("runIDs=%v：%v", ids, err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("runIDs=%v 的结果 = %v，应为空 map", ids, got)
		}
	}
}

// TestOverviewActiveCount：两列都没有达到上限时，导航中的活跃 run 数复用 Running+Queued，
// 与全部活跃 run 一致。统计里的 Active 是过去 24 小时的，不能拿来当活跃 run 数：
// 25 小时前创建、仍在运行的 run 不在统计中，但仍是活跃 run。
func TestOverviewActiveCount(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	old := e.addRun(runOpts{state: store.RunActive})
	e.exec(`UPDATE runs SET created_at = now() - interval '25 hours' WHERE id=$1`, old)
	e.addRun(runOpts{state: store.RunActive})
	e.addRun(runOpts{state: store.RunQueued})

	body := e.get(e.alice, "/").Body.String()
	page := parsePage(t, body)
	p := pageData[overviewPage](t, body)
	if len(p.Running) != 2 || len(p.Queued) != 1 {
		t.Fatalf("运行中 %d 个、排队中 %d 个，应为 2 和 1", len(p.Running), len(p.Queued))
	}
	if page.Active != 3 {
		t.Errorf("活跃 run 数 = %d，应为 3（运行中 2 + 排队中 1）", page.Active)
	}
	if p.Stats.Active != 2 {
		t.Errorf("过去 24 小时的统计 = %d，应为 2（25 小时前创建的 run 不在统计中）", p.Stats.Active)
	}
}

// TestOverviewActiveCountAtLimit：任一列达到上限时不知道上限之外还有多少，照常查询总数。
// 造出超过上限的数据，使"两列列出数量之和"（overviewActiveLimit + 1）与数据库中的真实总数
// （overviewActiveLimit + 2）不同：误用列出的数量、或把上限判断改成恒真都会得到错误结果。
func TestOverviewActiveCountAtLimit(t *testing.T) {
	t.Parallel()
	const overLimit = overviewActiveLimit + 1
	const total = overLimit + 1 // 超过上限的一列 + 另一列的 1 个

	t.Run("运行中超过上限", func(t *testing.T) {
		t.Parallel()
		e := newWebEnv(t)
		for range overLimit {
			e.addRun(runOpts{state: store.RunActive})
		}
		e.addRun(runOpts{state: store.RunQueued})

		body := e.get(e.alice, "/").Body.String()
		page := parsePage(t, body)
		p := pageData[overviewPage](t, body)
		if len(p.Running) != overviewActiveLimit {
			t.Fatalf("运行中列出 %d 个，应为上限 %d", len(p.Running), overviewActiveLimit)
		}
		if len(p.Queued) != 1 {
			t.Fatalf("排队中列出 %d 个，应为 1", len(p.Queued))
		}
		if page.Active != total {
			t.Errorf("活跃 run 数 = %d，应为 %d（列出的 %d + 1 不等于总数）", page.Active, total, overviewActiveLimit)
		}
	})

	t.Run("排队中超过上限", func(t *testing.T) {
		t.Parallel()
		e := newWebEnv(t)
		e.addRun(runOpts{state: store.RunActive})
		for range overLimit {
			e.addRun(runOpts{state: store.RunQueued})
		}

		body := e.get(e.alice, "/").Body.String()
		page := parsePage(t, body)
		p := pageData[overviewPage](t, body)
		if len(p.Running) != 1 {
			t.Fatalf("运行中列出 %d 个，应为 1", len(p.Running))
		}
		if len(p.Queued) != overviewActiveLimit {
			t.Fatalf("排队中列出 %d 个，应为上限 %d", len(p.Queued), overviewActiveLimit)
		}
		if page.Active != total {
			t.Errorf("活跃 run 数 = %d，应为 %d（1 + 列出的 %d 不等于总数）", page.Active, total, overviewActiveLimit)
		}
	})
}
