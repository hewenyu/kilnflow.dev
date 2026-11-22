package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

func TestAutotagCommands(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	at := time.Now().Add(-time.Minute).Unix()
	f.reply("ListAutoTags", &apiv1.ListAutoTagsResponse{Tags: []*apiv1.AutoTag{
		{Sha: "abcdef1234567890", Pr: 7, Tag: "v0.1.42", State: "planned", MergedBy: "kiln-test[bot]", SenderIsBot: true, WorkItemId: 12, Labels: []string{"release:minor"}, UpdatedAt: at},
		{Sha: "1234567890abcdef", Pr: 8, State: "failed", Reason: "没有权限", UpdatedAt: at},
	}})
	got := runApp(t, cfg, "", "autotag", "list", "alice/demo", "-n", "10")
	if got.code != exitOK {
		t.Fatalf("退出码 %d：%s", got.code, got.stderr)
	}
	wantRequest(t, f, "ListAutoTags", &apiv1.ListAutoTagsRequest{Repo: "alice/demo", Limit: 10})
	for _, want := range []string{"#7", "v0.1.42", "planned", "release:minor", "工作项 #12", "kiln-test[bot]（机器人）", "#8", "没有权限"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出 %q 中没有 %q", got.stdout, want)
		}
	}
	f.reply("RetryAutoTag", &apiv1.RetryAutoTagResponse{})
	if got := runApp(t, cfg, "", "autotag", "retry", "alice/demo", "1234567890abcdef"); got.code != exitOK {
		t.Fatalf("retry：%d %s", got.code, got.stderr)
	}
	wantRequest(t, f, "RetryAutoTag", &apiv1.RetryAutoTagRequest{Repo: "alice/demo", Sha: "1234567890abcdef"})
	for _, args := range [][]string{{"autotag"}, {"autotag", "list"}, {"autotag", "retry", "alice/demo"}} {
		if got := runApp(t, cfg, "", args...); got.code != exitUsage {
			t.Errorf("%v：退出码 %d，想要用法错误", args, got.code)
		}
	}
}

func TestRunnerUpgradeCommands(t *testing.T) {
	request := &apiv1.RunnerUpgradeRequest{Id: 1000, Version: "0.1.42", State: "pending"}
	for _, tc := range []struct {
		args   []string
		method string
		reply  proto.Message
		want   proto.Message
		out    string
	}{
		{
			[]string{"runner", "upgrade", "mini"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request, Created: true}, &apiv1.RequestRunnerUpgradeRequest{Runner: "mini"},
			"mini 将升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "mini"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request}, &apiv1.RequestRunnerUpgradeRequest{Runner: "mini"},
			"已经在升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "start", "mini"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request, Created: true}, &apiv1.RequestRunnerUpgradeRequest{Runner: "mini"},
			"mini 将升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "start", "mini", "0.1.42"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request, Created: true}, &apiv1.RequestRunnerUpgradeRequest{Runner: "mini", Version: "0.1.42"},
			"mini 将升级到 0.1.42",
		},
		// 与子命令同名的 Runner 用 start 升级
		{
			[]string{"runner", "upgrade", "start", "check"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request, Created: true}, &apiv1.RequestRunnerUpgradeRequest{Runner: "check"},
			"check 将升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "start", "start"},
			"RequestRunnerUpgrade",
			&apiv1.RequestRunnerUpgradeResponse{Request: request, Created: true}, &apiv1.RequestRunnerUpgradeRequest{Runner: "start"},
			"start 将升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "cancel", "mini"},
			"CancelRunnerUpgrade",
			&apiv1.CancelRunnerUpgradeResponse{Request: request}, &apiv1.CancelRunnerUpgradeRequest{Runner: "mini"},
			"已取消 mini 升级到 0.1.42",
		},
		{
			[]string{"runner", "upgrade", "check"},
			"CheckRunnerUpgrade",
			&apiv1.CheckRunnerUpgradeResponse{Count: 3}, &apiv1.CheckRunnerUpgradeRequest{},
			"已请求 3 台",
		},
		{
			[]string{"runner", "upgrade", "check", "mini"},
			"CheckRunnerUpgrade",
			&apiv1.CheckRunnerUpgradeResponse{Count: 1}, &apiv1.CheckRunnerUpgradeRequest{Runner: "mini"},
			"已请求 1 台",
		},
		{
			[]string{"runner", "upgrade", "registry", "https://npm.example/"},
			"SetRunnerUpgradeRegistry",
			&apiv1.SetRunnerUpgradeRegistryResponse{}, &apiv1.SetRunnerUpgradeRegistryRequest{Registry: "https://npm.example/"},
			"https://npm.example/",
		},
		{
			[]string{"runner", "upgrade", "registry", "default"},
			"SetRunnerUpgradeRegistry",
			&apiv1.SetRunnerUpgradeRegistryResponse{}, &apiv1.SetRunnerUpgradeRegistryRequest{},
			"官方 registry",
		},
	} {
		cfg, f, _ := loggedIn(t)
		f.reply(tc.method, tc.reply)
		got := runApp(t, cfg, "", tc.args...)
		if got.code != exitOK {
			t.Fatalf("%v：退出码 %d：%s", tc.args, got.code, got.stderr)
		}
		wantRequest(t, f, tc.method, tc.want)
		if !strings.Contains(got.stdout, tc.out) {
			t.Errorf("%v：输出 %q 中没有 %q", tc.args, got.stdout, tc.out)
		}
	}
	cfg, f, _ := loggedIn(t)
	f.reply("GetRunnerUpgrade", &apiv1.GetRunnerUpgradeResponse{DefaultRegistry: "https://registry.npmjs.org/"})
	checked := time.Now().Add(-5 * time.Minute).Unix()
	f.reply("ListRunners", &apiv1.ListRunnersResponse{Runners: []*apiv1.Runner{
		{Name: "mini", Kind: "ci", Version: "v0.1.41", Upgrade: &apiv1.RunnerUpgradeInfo{
			State: "rolled_back", Latest: "0.1.42", CheckedAt: checked, CanUpgrade: true,
			Request: &apiv1.RunnerUpgradeRequest{Version: "0.1.42", State: "failed", Error: "启动 3 次没有确认"},
		}},
		{Name: "busy", Kind: "ci", Version: "v0.1.41", Upgrade: &apiv1.RunnerUpgradeInfo{
			State: "draining", Latest: "0.1.42", Reason: "正在升级到 0.1.42", Request: &apiv1.RunnerUpgradeRequest{Version: "0.1.42", State: "pending"},
		}},
		{Name: "old", Kind: "ci", Version: "v0.1.30", Upgrade: &apiv1.RunnerUpgradeInfo{Reason: "这台 Runner 的版本不支持按台升级"}},
	}})
	got := runApp(t, cfg, "", "runner", "upgrade")
	if got.code != exitOK {
		t.Fatalf("退出码 %d：%s", got.code, got.stderr)
	}
	// 只列出 CI Runner：AI 执行节点的连接器用 npm 升级（PLAN #33.3）
	if slices.Contains(lines(got.stdout), "reviewer AI v0.1.40 0.1.42 - 可以升级：kiln runner upgrade start reviewer 0.1.42") {
		t.Errorf("输出 %q 中不应有 AI 执行节点", got.stdout)
	}
	for _, want := range []string{
		"官方（https://registry.npmjs.org/）", "0.1.42（5m 前）", "失败 0.1.42：启动 3 次没有确认",
		"kiln runner upgrade start mini 0.1.42", "进行中 0.1.42（Runner：draining）", "不支持按台升级",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("输出 %q 中没有 %q", got.stdout, want)
		}
	}
	for _, args := range [][]string{
		{"runner", "upgrade", "mini", "extra"},
		{"runner", "upgrade", "start"},
		{"runner", "upgrade", "start", "mini", "0.1.42", "extra"},
		{"runner", "upgrade", "cancel", "registry", "extra"},
		{"runner", "upgrade", "cancel"},
		{"runner", "upgrade", "registry"},
		{"runner", "upgrade", "check", "a", "b"},
	} {
		if got := runApp(t, cfg, "", args...); got.code != exitUsage {
			t.Errorf("%v：退出码 %d，想要用法错误", args, got.code)
		}
	}
}
