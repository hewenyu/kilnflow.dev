package main

import (
	"strings"
	"testing"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// 「待决策清单」的 CLI：命令名是 `kiln doc decision-doc …` / `kiln doc decision-sync …`，
// token 只从标准输入读取，参数错误在发请求前就报出来。

const cliDocID = "55555555-5555-4555-8555-555555555555"

func TestDecisionDocCommands(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	f.reply("SetDecisionDoc", &apiv1.SetDecisionDocResponse{
		DecisionDoc: &apiv1.DecisionDoc{
			Repo: "alice/demo", Provider: "outline", BaseUrl: "https://outline.example.com", DocId: cliDocID,
			CollectionId: "col", Generation: 3, HasToken: true, PendingCount: 2, UnknownCount: 1,
		},
		CancelledCount: 4, UnknownCount: 5,
	})
	got := runApp(t, cfg, "  secret-token\n", "doc", "decision-doc", "set", "alice/demo",
		"--doc-id", cliDocID, "--base-url", "https://outline.example.com")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "SetDecisionDoc", &apiv1.SetDecisionDocRequest{
		Repo: "alice/demo", BaseUrl: "https://outline.example.com", DocId: cliDocID, Token: "secret-token",
	})
	if strings.Contains(got.stdout, "secret-token") {
		t.Errorf("输出泄露了 token：%q", got.stdout)
	}
	for _, want := range []string{cliDocID, "4 条待处理条目已取消", "5 条结果不明"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出 %q 中没有 %q", got.stdout, want)
		}
	}

	f.reply("ListDecisionDocs", &apiv1.ListDecisionDocsResponse{DecisionDocs: []*apiv1.DecisionDoc{{
		Repo: "alice/demo", DocId: cliDocID, BaseUrl: "https://outline.example.com", PendingCount: 2, UnknownCount: 1, HasToken: true,
	}}})
	got = runApp(t, cfg, "", "doc", "decision-doc", "list")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "alice/demo") || !strings.Contains(got.stdout, cliDocID) {
		t.Errorf("输出 = %q", got.stdout)
	}

	f.reply("ClearDecisionDoc", &apiv1.ClearDecisionDocResponse{CancelledCount: 1, UnknownCount: 2})
	got = runApp(t, cfg, "", "doc", "decision-doc", "clear", "alice/demo")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "ClearDecisionDoc", &apiv1.ClearDecisionDocRequest{Repo: "alice/demo"})
}

func TestDecisionSyncCommands(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	f.reply("ListDecisionSyncs", &apiv1.ListDecisionSyncsResponse{Syncs: []*apiv1.DecisionSync{{
		Id: 9, Repo: "alice/demo", State: "unknown", EntryKind: "decision", Category: "policy",
		Marker: "kiln-dec:42:wi7:t2:0", WorkItemId: 7, TaskId: 2, EntryIdx: 0, LastErrorCode: "update_outcome_unknown",
	}}})
	got := runApp(t, cfg, "", "doc", "decision-sync", "list", "--repo", "alice/demo", "--state", "unknown", "--limit", "10")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "ListDecisionSyncs", &apiv1.ListDecisionSyncsRequest{Repo: "alice/demo", State: "unknown", Limit: 10})
	for _, want := range []string{"#9", "alice/demo", "unknown", "decision/policy", "kiln-dec:42:wi7:t2:0"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出 %q 中没有 %q", got.stdout, want)
		}
	}

	// 位置参数不能被静默忽略（否则会列出全部仓库）。
	got = runApp(t, cfg, "", "doc", "decision-sync", "list", "alice/demo")
	if got.code != exitUsage {
		t.Fatalf("多余位置参数应报用法错误，退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "不接受位置参数") || !strings.Contains(got.stderr, "alice/demo") {
		t.Errorf("stderr = %q", got.stderr)
	}

	f.reply("ReconcileDecisionSync", &apiv1.ReconcileDecisionSyncResponse{
		Sync: &apiv1.DecisionSync{Id: 9, Repo: "alice/demo", State: "delivered"}, Found: true,
	})
	got = runApp(t, cfg, "", "doc", "decision-sync", "reconcile", "9")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "ReconcileDecisionSync", &apiv1.ReconcileDecisionSyncRequest{Id: 9})
	if !strings.Contains(got.stdout, "delivered") {
		t.Errorf("输出 = %q", got.stdout)
	}

	f.reply("ResolveDecisionSync", &apiv1.ResolveDecisionSyncResponse{
		Sync: &apiv1.DecisionSync{Id: 9, Repo: "alice/demo", State: "cancelled", ResolutionNote: "确认不需要"},
	})
	got = runApp(t, cfg, "没有原因\n", "doc", "decision-sync", "resolve", "9", "--state", "cancelled", "--note", "确认不需要")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "ResolveDecisionSync", &apiv1.ResolveDecisionSyncRequest{Id: 9, State: "cancelled", Note: "确认不需要"})
}

func TestDecisionCommandsUsage(t *testing.T) {
	cfg := isolate(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"doc", "decision-doc", "set", "alice/demo"}, "用法：kiln doc decision-doc set REPO --doc-id UUID"},
		{[]string{"doc", "decision-doc", "clear"}, "用法：kiln doc decision-doc clear REPO"},
		{[]string{"doc", "decision-sync", "reconcile"}, "用法：kiln doc decision-sync reconcile ID"},
		{[]string{"doc", "decision-sync", "resolve", "9"}, "用法：kiln doc decision-sync resolve ID --state delivered|cancelled --note REASON"},
		{[]string{"doc", "decision-sync", "bogus"}, "未知的子命令 doc decision-sync bogus"},
	} {
		got := runApp(t, cfg, "", tc.args...)
		if got.code != exitUsage || !strings.Contains(got.stderr, tc.want) {
			t.Errorf("%v：退出码 = %d，stderr = %q，期望包含 %q", tc.args, got.code, got.stderr, tc.want)
		}
	}
}
