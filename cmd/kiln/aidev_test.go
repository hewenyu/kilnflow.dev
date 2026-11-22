package main

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

func TestAIDevCommands(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	ok := func(args ...string) string {
		t.Helper()
		got := runApp(t, cfg, "", args...)
		if got.code != exitOK {
			t.Fatalf("%v：退出码 = %d，stderr = %q", args, got.code, got.stderr)
		}
		return got.stdout
	}
	f.reply("SyncAIDev", &apiv1.SyncAIDevResponse{Listed: 7, Complete: true, Changed: 2, Candidates: 5})
	wantLines(t, ok("ai", "dev", "sync", "alice/demo"), "已同步任务池：GitHub 上有 7 个 kiln:ready 的 Issue，变化 2 个，可选 5 个（有空闲的开发机时开始选题）")
	wantRequest(t, f, "SyncAIDev", &apiv1.SyncAIDevRequest{Repo: "alice/demo"})
	f.reply("SyncAIDev", &apiv1.SyncAIDevResponse{Listed: 1000})
	if out := ok("ai", "dev", "sync", "alice/demo"); !strings.Contains(out, "没有读完") {
		t.Errorf("没有读完时的输出 %q 中没有提示", out)
	}

	ok("ai", "dev", "pause", "alice/demo")
	wantRequest(t, f, "PauseAIDev", &apiv1.PauseAIDevRequest{Repo: "alice/demo", Paused: true})
	ok("ai", "dev", "resume", "alice/demo")
	if got := f.requests("PauseAIDev"); len(got) != 2 || !proto.Equal(got[1], &apiv1.PauseAIDevRequest{Repo: "alice/demo"}) {
		t.Fatalf("resume 的请求 = %v", got)
	}
	ok("ai", "dev", "stop", "#7")
	wantRequest(t, f, "StopWorkItem", &apiv1.StopWorkItemRequest{Id: 7})
	ok("ai", "dev", "retry", "7")
	wantRequest(t, f, "RetryWorkItem", &apiv1.RetryWorkItemRequest{Id: 7})

	f.reply("ListWorkItems", &apiv1.ListWorkItemsResponse{Items: []*apiv1.WorkItem{{
		Id: 7, Repo: "alice/demo", State: "blocked", Issues: []int32{3, 4}, PrNumber: 9, HeadSha: "abcdef1234567890",
		FixRounds: 5, RoundFixes: 2, MaxFixRounds: 2, Hold: true, Reason: "返修 2 轮后仍未通过", UpdatedAt: time.Now().Add(-time.Minute).Unix(),
	}}, Occupancy: &apiv1.RepoOccupancy{WorkItem: 7, Reason: "工作项 #7 进行中（implement / blocked）；同一个仓库按工作项串行"}})
	out := ok("ai", "dev", "list", "alice/demo", "-n", "5")
	wantRequest(t, f, "ListWorkItems", &apiv1.ListWorkItemsRequest{Repo: "alice/demo", Limit: 5})
	for _, want := range []string{"仓库占用：工作项 #7 进行中", "#7", "blocked", "#3 #4", "#9 abcdef1", "2/2", "kiln:hold", "返修 2 轮后仍未通过"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出 %q 中没有 %q", out, want)
		}
	}
	if strings.Contains(out, "设计") || strings.Contains(out, "第 1 轮") {
		t.Errorf("一个 PR 完成的工作项不显示阶段：%q", out)
	}

	// 先设计的工作项显示阶段，部分完成之后显示轮次
	f.reply("ListWorkItems", &apiv1.ListWorkItemsResponse{Items: []*apiv1.WorkItem{
		{Id: 8, Repo: "alice/demo", State: "pr_open", Phase: "design", DesignFirst: true, ImplRound: 1, UpdatedAt: time.Now().Unix()},
		{Id: 9, Repo: "alice/demo", State: "developing", Phase: "implement", DesignFirst: true, DesignPr: 4, ImplRound: 2, UpdatedAt: time.Now().Unix()},
		{Id: 10, Repo: "alice/demo", State: "fixing", RoundFixes: 1, RoundConflicts: 2, MaxFixRounds: 2, UpdatedAt: time.Now().Unix()},
	}})
	out = ok("ai", "dev", "list", "alice/demo")
	for _, want := range []string{"设计 pr_open", "实现 developing（第 2 轮）", "1/2（冲突 2/2）"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出 %q 中没有 %q", out, want)
		}
	}
	if strings.Contains(out, "仓库占用") {
		t.Errorf("没有返回占用时不显示：%q", out)
	}
	// 没有被占用
	f.reply("ListWorkItems", &apiv1.ListWorkItemsResponse{Occupancy: &apiv1.RepoOccupancy{}})
	out = ok("ai", "dev", "list", "alice/demo")
	for _, want := range []string{"仓库占用：没有", "没有工作项"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出 %q 中没有 %q", out, want)
		}
	}
}

func TestAIDevUsage(t *testing.T) {
	for _, args := range [][]string{
		{"ai", "dev"},
		{"ai", "dev", "sync"},
		{"ai", "dev", "start", "a/b"},
		{"ai", "dev", "stop"},
		{"ai", "dev", "stop", "x"},
		{"ai", "dev", "retry", "0"},
		{"ai", "dev", "list", "a/b", "c/d"},
		{"ai", "dev", "nope", "a/b"},
	} {
		cfg, f, _ := loggedIn(t)
		got := runApp(t, cfg, "", args...)
		if got.code != exitUsage || !strings.Contains(got.stderr, "kiln ai dev") {
			t.Errorf("%v：退出码 %d，stderr %q，期望用法错误", args, got.code, got.stderr)
		}
		if len(f.requests("StopWorkItem"))+len(f.requests("RetryWorkItem"))+len(f.requests("SyncAIDev"))+len(f.requests("ListWorkItems")) != 0 {
			t.Errorf("%v：不应发出请求", args)
		}
	}
}
