package mergequeue

import "testing"

// OwnsMerge（PLAN #29.4）：事件接入据此判断一次合并是不是由合并队列负责——正在合并的 PR 由队列负责；已经
// 合并的只有合并 commit 与记录一致、并且记录不是自动出队的才由队列负责；排队但不在合并、或者合并 commit 不同都不算。

func TestOwnsMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i, tt := range []struct {
		name             string
		state, action    string
		mergeSHA, reason string
		want             bool
	}{
		{"正在合并", "queued", "merging", "", "", true},
		{"已经合并", "merged", "none", sha, "", true},
		{"合并后自动出队", "merged", "none", sha, "验证超时，自动出队", false},
		{"别的 commit", "merged", "none", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", false},
		{"排队但没在合并", "queued", "none", "", "", false},
	} {
		pr := i + 1
		h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, action, merge_sha, reason) VALUES ($1,$2,$3,$4,$5,$6)`,
			repoID, pr, tt.state, tt.action, tt.mergeSHA, tt.reason)
		got, err := OwnsMerge(h.ctx, h.db, repoID, pr, sha)
		if err != nil {
			t.Fatalf("%s：%v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s：OwnsMerge = %v，想要 %v", tt.name, got, tt.want)
		}
	}
	// 别的仓库的记录不算
	h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, action) VALUES ($1,$2,'queued','merging')`, repoID+1, 99)
	if got, err := OwnsMerge(h.ctx, h.db, repoID, 99, sha); err != nil || got {
		t.Fatalf("别的仓库的合并 = %v, %v，想要 false", got, err)
	}
}
