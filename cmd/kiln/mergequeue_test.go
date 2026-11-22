package main

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

func TestMergeQueueCommands(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	f.reply("AddToMergeQueue", &apiv1.AddToMergeQueueResponse{Added: true})
	got := runApp(t, cfg, "", "merge-queue", "add", "alice/demo", "7")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "AddToMergeQueue", &apiv1.AddToMergeQueueRequest{Repo: "alice/demo", Pr: 7})
	wantLines(t, got.stdout, "已把 alice/demo#7 加入合并队列")

	got = runApp(t, cfg, "", "merge-queue", "remove", "alice/demo", "7")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "RemoveFromMergeQueue", &apiv1.RemoveFromMergeQueueRequest{Repo: "alice/demo", Pr: 7})

	at := time.Now().Add(-3 * time.Minute).Unix()
	f.reply("ListMergeQueue", &apiv1.ListMergeQueueResponse{
		Queued: []*apiv1.MergeQueueEntry{
			{Pr: 7, BaseRef: "main", HeadSha: "abcdef1234567890", Status: "等待验证：1/2 个 run 已结束", EnqueuedBy: "github:alice", EnqueuedAt: at},
			// 每条目标分支各自排队（PLAN #28.5）：排位按同一目标分支计算，不显示目标分支的旧行是 "-"
			{Pr: 8, BaseRef: "dev", HeadSha: "bbbbbb1234567890", Status: "等待验证：1/2 个 run 已结束", EnqueuedBy: "github:bob", EnqueuedAt: at},
			{Pr: 9, BaseRef: "dev", HeadSha: "cccccc1234567890", Status: "目标 dev · 排在第 2 个", EnqueuedBy: "github:bob", EnqueuedAt: at},
			{Pr: 10, HeadSha: "dddddd1234567890", Status: "等待验证：1/2 个 run 已结束", EnqueuedBy: "github:bob", EnqueuedAt: at},
		},
		Recent: []*apiv1.MergeQueueEntry{{Pr: 6, BaseRef: "dev", State: "removed", Reason: "PR 是 draft", FinishedAt: at}},
	})
	got = runApp(t, cfg, "", "merge-queue", "list", "alice/demo")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	for _, want := range []string{"#7", "等待验证：1/2 个 run 已结束", "github:alice", "最近结束", "#6", "已移出", "PR 是 draft"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出 %q 中没有 %q", got.stdout, want)
		}
	}
	// 按目标分支的排位、目标分支列（空值显示 "-"）
	fields := func(pr string) []string {
		for line := range strings.SplitSeq(got.stdout, "\n") {
			if f := strings.Fields(line); len(f) > 2 && f[1] == pr {
				return f
			}
		}
		return nil
	}
	for pr, want := range map[string][2]string{"#7": {"1", "main"}, "#8": {"1", "dev"}, "#9": {"2", "dev"}, "#10": {"1", "-"}} {
		f := fields(pr)
		if len(f) < 3 || f[0] != want[0] || f[2] != want[1] {
			t.Errorf("%s 的行 = %v，想要排位 %s、目标分支 %s", pr, f, want[0], want[1])
		}
	}
	// 最近结束的项也要显示目标分支：只断言 #6 那一行（Queued 区里的 dev 不能掩盖这里的缺失）
	recent := func(pr string) []string {
		for line := range strings.SplitSeq(got.stdout, "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == pr {
				return f
			}
		}
		return nil
	}
	if r := recent("#6"); len(r) < 2 || r[1] != "dev" {
		t.Errorf("最近结束的 #6 应显示目标分支 dev：%v", r)
	}
}

func TestMergeQueueUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"没有子命令", []string{"merge-queue"}},
		{"list 缺少仓库", []string{"merge-queue", "list"}},
		{"add 缺少 PR", []string{"merge-queue", "add", "alice/demo"}},
		{"PR 号无效", []string{"merge-queue", "remove", "alice/demo", "x"}},
		{"PR 号为 0", []string{"merge-queue", "add", "alice/demo", "0"}},
		{"add 的 PR 号超出 int32", []string{"merge-queue", "add", "alice/demo", "4294967297"}},
		{"remove 的 PR 号超出 int32", []string{"merge-queue", "remove", "alice/demo", "2147483648"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			got := runApp(t, cfg, "", tc.args...)
			if got.code != exitUsage || !strings.Contains(got.stderr, "kiln merge-queue") {
				t.Errorf("退出码 %d，stderr %q，期望用法错误", got.code, got.stderr)
			}
			if len(f.requests("AddToMergeQueue"))+len(f.requests("RemoveFromMergeQueue"))+len(f.requests("ListMergeQueue")) != 0 {
				t.Error("不应发出请求")
			}
		})
	}
}
