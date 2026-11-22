package cistore

import (
	"strconv"
	"testing"
	"time"
)

// TestQueueAhead：调度解释按排队顺序（priority DESC、提交时间、queued_at、id）统计排在前面、platform 和 executor 相同的
// pending 普通 job，给出其中排在最前面的 run；不在排队的 job 没有位置。
func TestQueueAhead(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 1, 1, 1, "o", "r")
	base := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	// pending 写入 job：提交时间 base+submitted 秒、queued_at base+queued 秒
	n := 0
	add := func(runID int64, priority, submitted, queued int, mods ...string) int64 {
		t.Helper()
		n++
		id := insertJob(t, db, jobFixture{runID: &runID, key: "job-" + strconv.Itoa(n)})
		mustExec(t, db, `UPDATE jobs SET priority=$2, created_at=$3, queued_at=$4 WHERE id=$1`, id, priority,
			base.Add(time.Duration(submitted)*time.Second), base.Add(time.Duration(queued)*time.Second))
		for _, m := range mods {
			mustExec(t, db, `UPDATE jobs SET `+m+` WHERE id=$1`, id)
		}
		return id
	}
	runs := make([]int64, 5)
	for i := range runs {
		runs[i] = insertRun(t, db, runFixture{repoID: 1})
	}
	// 被统计的：更高 priority 的晚提交（排在最前面）、同 priority 早提交、同提交时间早排队、同提交时间同排队 id 更小
	add(runs[3], 2, 50, 0)
	add(runs[0], 1, 0, 40)
	add(runs[1], 1, 10, 5)
	add(runs[1], 1, 10, 10)
	me := add(runs[1], 1, 10, 10)
	// 不统计的：排在后面的（id 更大、queued_at 更晚、更晚提交、更低 priority），其他平台、executor，发布 job，不在 pending 的
	add(runs[1], 1, 10, 10)
	add(runs[1], 1, 10, 11)
	add(runs[2], 1, 20, 0)
	add(runs[2], 1, 20, 1)
	add(runs[0], 0, 0, 0)
	add(runs[0], 1, 0, 0, `platform='linux/amd64'`)
	add(runs[0], 1, 0, 0, `executor_kind='oci'`)
	add(runs[0], 1, 0, 0, `kind='publish'`)
	add(runs[0], 1, 0, 0, `state='running'`)
	add(runs[0], 1, 0, 0, `state='waiting'`)

	j, err := GetJob(t.Context(), db, me, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := QueueAhead(t.Context(), db, j)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Ahead != 4 || p.FirstRunID != runs[3] {
		t.Fatalf("QueueAhead = %+v，期望前面 4 个、最前面的是 run %d", p, runs[3])
	}

	// 排在最前面：前面没有 job
	head := add(runs[4], 3, 0, 0)
	if j, err = GetJob(t.Context(), db, head, false); err != nil {
		t.Fatal(err)
	}
	if p, err := QueueAhead(t.Context(), db, j); err != nil || p == nil || p.Ahead != 0 || p.FirstRunID != 0 {
		t.Fatalf("排在最前面的 job：%+v %v", p, err)
	}

	// 不在排队的 job（running、发布 job）没有位置
	for _, m := range []string{`state='running'`, `kind='publish'`} {
		id := add(runs[4], 1, 0, 0, m)
		j, err := GetJob(t.Context(), db, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := QueueAhead(t.Context(), db, j); err != nil || p != nil {
			t.Errorf("%s 的 job 不应有排队位置：%+v %v", m, p, err)
		}
	}
}

// TestQueueKeyArgs：QueueKeyArgs 与 QueueKey.Args 的参数个数一致（翻页的 SQL 按它给后面的参数编号）。
func TestQueueKeyArgs(t *testing.T) {
	t.Parallel()
	if n := len(QueueKey{}.Args()); n != QueueKeyArgs {
		t.Fatalf("QueueKey.Args 有 %d 个参数，QueueKeyArgs = %d", n, QueueKeyArgs)
	}
}
