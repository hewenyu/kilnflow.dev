package aidev

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 开发的平台（development.platform，PLAN #26.7）：单个平台或一组平台，不写时是 macOS arm64。

const (
	platLinux = "linux/amd64"
	// devGroupYML 是开发平台为一组（macOS arm64 或 Linux amd64）的配置。
	devGroupYML = devConfigYML + "  platform:\n    any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]\n"
	// devLinuxYML 是开发平台为 Linux amd64 的配置。
	devLinuxYML = devConfigYML + "  platform: {os: linux, arch: amd64}\n"
)

// 选题只在有平台匹配的空闲开发机时规划：开发平台是 Linux 时，只有 macOS 开发机不规划（读过配置之后，同样的情况不再读
// GitHub）；来了一台 Linux 开发机才规划，任务只能在 Linux 上执行。
func TestPlanSelect_platform(t *testing.T) {
	h := newHarness(t, devLinuxYML)
	h.issue(1, "需求", "x")
	res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob")
	if err != nil || res.RequestID != 0 || res.Reason != whyNoFreeSlot {
		t.Fatalf("只有 macOS 开发机：%+v, %v", res, err)
	}
	// 记下了读到的平台：空闲的开发机再来时按它判断，不读 GitHub
	if got := h.queryString(`SELECT array_to_string(dev_platforms, ',') FROM ai_repo_state WHERE repo_id=$1`, testRepoID); got != platLinux {
		t.Fatalf("记下的开发平台 = %q", got)
	}
	before := len(h.fake.Requests())
	if res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || res.Reason != whyNoFreeSlot {
		t.Fatalf("再次规划：%+v, %v", res, err)
	}
	if n := len(h.fake.Requests()) - before; n != 0 {
		t.Fatalf("没有平台匹配的开发机时读了 %d 次 GitHub", n)
	}

	// 同步任务池时清空记下的平台（改了 development.platform 后同步一次即重新读取）
	if _, err := h.svc.SyncPool(h.ctx, testRepoID, "bob"); err != nil {
		t.Fatal(err)
	}
	if h.queryBool(`SELECT dev_platforms IS NOT NULL FROM ai_repo_state WHERE repo_id=$1`, testRepoID) {
		t.Fatal("同步任务池后仍记着开发平台")
	}

	h.addRunnerOn("dev-linux", "host-linux", platLinux, agentcfg.Agents()...)
	task := h.taskOf(h.startSelect())
	if !slices.Equal(task.Platforms, []string{platLinux}) {
		t.Fatalf("选题任务的平台 = %v", task.Platforms)
	}
	if want := "（平台 " + platLinux + "）"; task.Reason == nil || !strings.HasSuffix(*task.Reason, want) {
		t.Errorf("等待原因 = %v，应以 %q 结尾", task.Reason, want)
	}
}

// 不写 development.platform 时与以前相同：任务只在 macOS arm64 上执行。
func TestPlanSelect_defaultPlatform(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "需求", "x")
	if task := h.taskOf(h.startSelect()); !slices.Equal(task.Platforms, []string{testDevPlatform}) {
		t.Fatalf("选题任务的平台 = %v，应为 %s", task.Platforms, testDevPlatform)
	}
}

// 一组平台：选题任务可以在组内任一平台上执行；工具链为组内每个平台各解析一份，lockfile 缺任何一个平台的条目时不规划，
// 原因写明平台。
func TestPlanSelect_platformGroupTools(t *testing.T) {
	h := newHarness(t, strings.Replace(devGroupYML, "version: 1\n", "version: 1\nrequirements: {tools: [go]}\n", 1))
	h.issue(1, "需求", "x")
	macGo, linuxGo := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("c", 64)
	h.commitOn("main", map[string]string{toolchain.LockPath: "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + macGo + "\"\n"}, "只锁 macOS")
	if _, err := h.svc.SyncPool(h.ctx, testRepoID, "bob"); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob")
	if err != nil || res.RequestID != 0 || !strings.Contains(res.Reason, "工具链配置有错误") || !strings.Contains(res.Reason, platLinux) {
		t.Fatalf("lockfile 缺 %s：%+v, %v", platLinux, res, err)
	}

	h.commitOn("main", map[string]string{toolchain.LockPath: "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + macGo + "\"\n" +
		"\"linux/amd64\" = \"" + linuxGo + "\"\n"}, "锁定 Linux")
	if _, err := h.svc.SyncPool(h.ctx, testRepoID, "bob"); err != nil {
		t.Fatal(err)
	}
	task := h.taskOf(h.startSelect())
	if !slices.Equal(task.Platforms, []string{testDevPlatform, platLinux}) {
		t.Fatalf("选题任务的平台 = %v", task.Platforms)
	}
	want := map[string][]aispec.ToolRef{
		testDevPlatform: {{Name: "go", Version: "1.27.3", Platform: testDevPlatform, Digest: macGo}},
		platLinux:       {{Name: "go", Version: "1.27.3", Platform: platLinux, Digest: linuxGo}},
	}
	got := h.specOf(task.ID).PlatformTools
	if len(got) != len(want) {
		t.Fatalf("工具链 = %+v，期望 %+v", got, want)
	}
	for p, w := range want {
		if !slices.Equal(got[p], w) {
			t.Errorf("%s 的工具链 = %+v，期望 %+v", p, got[p], w)
		}
		if tools := h.svc.TaskType().Tools(task, p); !slices.Equal(tools, w) {
			t.Errorf("领取节点为 %s 时的工具链 = %+v", p, tools)
		}
	}
}

// 返修、设计合并后的实现与选题一样按默认分支上的 development.platform 规划（任一平台的开发机都能接着做：每次执行都从
// 远端的正式分支新建工作区，不依赖上一轮所在的机器）。
func TestPlatformFixAndImplement(t *testing.T) {
	group := []string{testDevPlatform, platLinux}
	t.Run("返修", func(t *testing.T) {
		h := newHarness(t, devGroupYML)
		h.issue(1, "加功能", "要做 a")
		w, pr := h.developed(1)
		h.failingRun(pr, w.HeadSHA)
		h.mustProcess()
		if task := h.fixTask(h.wantState(w.ID, StateFixing)); !slices.Equal(task.Platforms, group) {
			t.Fatalf("返修任务的平台 = %v，应为 %v", task.Platforms, group)
		}
	})
	t.Run("实现", func(t *testing.T) {
		h := newHarness(t, devGroupYML)
		h.issue(1, "加功能", "要做 a")
		w, res, designPR := h.designPROpen()
		w, _ = h.mergeDesign(w, res, designPR)
		h.mustProcess()
		w = h.workItem(w.ID)
		if w.TaskID == nil {
			t.Fatalf("没有规划实现：%s", jsonString(w))
		}
		if task := h.task(*w.TaskID); !slices.Equal(task.Platforms, group) {
			t.Fatalf("实现任务的平台 = %v，应为 %v", task.Platforms, group)
		}
	})
}

// 排队中的别的仓库的开发任务只占用它能执行的平台上的槽位（AI 审计：只能在 macOS 上执行的排队任务曾让平台组一直不规划，
// Linux 开发机空着）。
func TestPlanSelect_queuedElsewhere(t *testing.T) {
	// macOS 的开发机暂停接单或离线，只剩一台空闲的 Linux 开发机，另一个仓库有只能在 macOS 上执行的排队任务：
	// 平台组的仓库照样规划，任务由 Linux 开发机执行
	for name, offMac := range map[string]string{
		"暂停接单": `UPDATE runners SET drain=true WHERE id=$1`,
		"离线":   `UPDATE runners SET last_seen_at = now() - interval '1 hour' WHERE id=$1`,
	} {
		t.Run("macOS 专属的排队任务不占 Linux 开发机/"+name, func(t *testing.T) {
			h := newHarness(t, devGroupYML)
			h.issue(1, "需求", "x")
			h.queueElsewhere(devConfigAgent, testDevPlatform)
			h.exec(offMac, h.runnerID)
			linux := h.addRunnerOn("dev-linux", "host-linux", platLinux, agentcfg.Agents()...)
			task := h.taskOf(h.startSelect())
			if !slices.Contains(task.Platforms, platLinux) {
				t.Fatalf("选题任务的平台 = %v", task.Platforms)
			}
			h.acquireBy(task.ID, linux)
		})
	}

	// 反过来：没有 Linux 开发机时，只能在 Linux 上执行的排队任务不占 macOS 开发机的槽位
	t.Run("Linux 专属的排队任务不占 macOS 开发机", func(t *testing.T) {
		h := newHarness(t, devGroupYML)
		h.issue(1, "需求", "x")
		h.queueElsewhere(devConfigAgent, platLinux)
		h.acquireBy(h.taskOf(h.startSelect()).ID, h.runnerID)
	})

	// 一组平台的排队任务：能放到仓库的平台之外时不占用，只能用仓库的平台上的槽位时扣减
	t.Run("一组平台的排队任务", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "需求", "x")
		h.queueElsewhere(devConfigAgent, testDevPlatform, platLinux)
		if res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || res.RequestID != 0 || res.Reason != whyNoFreeSlot {
			t.Fatalf("只有一台 macOS 开发机、排队任务只能用它：%+v, %v", res, err)
		}
		h.addRunnerOn("dev-linux", "host-linux", platLinux, agentcfg.Agents()...)
		if task := h.taskOf(h.startSelect()); !slices.Equal(task.Platforms, []string{testDevPlatform}) {
			t.Fatalf("选题任务的平台 = %v", task.Platforms)
		}
	})
}
