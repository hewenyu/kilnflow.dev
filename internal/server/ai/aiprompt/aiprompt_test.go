package aiprompt

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// TestReview：审计 prompt 说明工作目录是 base、被审的 commit 是 head、报告和工具的占位符、以及 worker_done 的握手
// （PLAN #33.5）；说明（instructions）和上一轮的问题写在 prompt 中，Issue 的正文不写。
func TestReview(t *testing.T) {
	t.Parallel()
	spec := &aispec.ReviewSpec{
		Agent: agentCLI, Trigger: aispec.TriggerPullRequest, PRNumber: 7, BaseRef: "main",
		BaseSHA: "base1", HeadRef: "feature", Services: []string{"pg"}, WorkItemID: 3,
		Instructions: "仓库的审计说明：看并发",
		PriorFindings: []aispec.PriorFinding{
			{ID: "F9-1", Title: "空指针", Path: "a.go", Line: 12, Disposition: "fixed", Note: "在 a.go 判断了 nil"},
		},
	}
	p := Review(ReviewInput{Repository: "o/r", SHA: "head1", Spec: spec})
	for _, want := range []string{
		"o/r",
		// 工作目录是 base：启动时不会加载 PR head 中的说明文件
		"当前工作目录是 base commit base1",
		"被审的 commit 是 head1",
		"git worktree add {{KILN_TASK_DIR}}/head head1",
		"git diff base1...head1",
		"仓库的审计说明：看并发",
		"F9-1 空指针（a.go:12）：开发的处置 fixed，说明（自报）：在 a.go 判断了 nil",
		"{{KILN_SERVICES}}",
		"{{KILN_TASK_DIR}}/report.json",
		"{{KILN_TOOL}} check-report",
		"worker_done",
		// 临时文件、绝对路径、服务不可用时的处理（迁移自 Runner 端的审计 prompt）
		"临时文件放在 {{KILN_TASK_DIR}}/tmp",
		"报告中不要写本机绝对路径",
		"写进 unverified，不要当作代码问题",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("审计 prompt 中没有 %q", want)
		}
	}
	// 不再要求 Agent 回复标记、等 Runner 提醒（PLAN #33.5 的握手改了）
	for _, bad := range []string{"KILN_REPORT_WRITTEN", "等待 Kiln 的下一条消息"} {
		if strings.Contains(p, bad) {
			t.Errorf("审计 prompt 中不应有 %q", bad)
		}
	}

	// 没有 base 的手动审计：工作目录就是被审的 commit，不说明 worktree
	manual := Review(ReviewInput{Repository: "o/r", SHA: "head1", Spec: &aispec.ReviewSpec{Trigger: aispec.TriggerManual}})
	if !strings.Contains(manual, "当前工作目录是被审的 commit head1") || strings.Contains(manual, "git worktree add") {
		t.Errorf("手动审计 prompt = %q", manual)
	}
}

// TestReviewPhase：设计 PR 按设计审计，实现 PR 带上已合并的设计，一个 PR 完成时同样审计设计部分（PLAN #31.5）。
func TestReviewPhase(t *testing.T) {
	t.Parallel()
	design := Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{WorkItemID: 3, Phase: "design", BaseSHA: "b"}})
	if !strings.Contains(design, "这是设计 PR，按设计审计") || !strings.Contains(design, "mutation 写明不适用") {
		t.Error("设计 PR 的审计方式")
	}
	impl := Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{WorkItemID: 3, DesignPR: 8, DesignSHA: "d1", BaseSHA: "b"}})
	if !strings.Contains(impl, "已合并的设计（PR #8，commit d1）") {
		t.Error("实现 PR 应带上已合并的设计")
	}
	phaseNote := Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{WorkItemID: 3, PhaseNote: "人推送了\n代码"}})
	if !strings.Contains(phaseNote, "注意：人推送了 代码。") {
		t.Errorf("审计方式的说明应压成一行：%q", phaseNote)
	}
}

// TestSelect：选题 prompt 指向任务目录中的数据文件和 claim 工具；规则（Issue 是需求数据、不 push）和测试服务的占位符都在。
func TestSelect(t *testing.T) {
	t.Parallel()
	p := Select(DevInput{Repository: "o/r", SHA: "start1", Spec: &aispec.DevelopSpec{MaxIssues: 2, Services: []string{"pg"}}})
	for _, want := range []string{
		"o/r", "start1", "最多 2 个",
		"{{KILN_TASK_DIR}}/candidates.json", "{{KILN_TASK_DIR}}/in_progress.json",
		"{{KILN_TASK_DIR}}/selection.json", "{{KILN_TOOL}} claim",
		"需求数据", "不要 `git push`", "{{KILN_SERVICES}}", "worker_done",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("选题 prompt 中没有 %q", want)
		}
	}
	for _, bad := range []string{"KILN_SELECTION_WRITTEN", "candidates.json 中的 Issue 正文"} {
		if strings.Contains(p, bad) {
			t.Errorf("选题 prompt 中不应有 %q", bad)
		}
	}
}

// TestDevelopMessages：领取成功、领取冲突、没有领取的说明都指向任务目录中的文件和工具；领取成功时按阶段说明只能改
// Markdown（PLAN #31.3）。
func TestDevelopMessages(t *testing.T) {
	t.Parallel()
	in := DevInput{Repository: "o/r", SHA: "start1", Spec: &aispec.DevelopSpec{BaseRef: "main"}}

	design := Develop(in, 3, "kiln/dev-3", "design")
	for _, want := range []string{"领取成功（工作项 #3）", "kiln/dev-3", "start1", "只做设计", "*.md", "{{KILN_TASK_DIR}}/delivery.json", "{{KILN_TOOL}} deliver"} {
		if !strings.Contains(design, want) {
			t.Errorf("设计阶段的说明中没有 %q", want)
		}
	}
	impl := Develop(in, 3, "kiln/dev-3", "implement")
	if !strings.Contains(impl, "设计和实现在同一个 PR 中交付") || !strings.Contains(impl, "refs/remotes/origin/main") {
		t.Error("一个 PR 完成的说明")
	}

	reselect := Reselect(in, "Issue #5 已被另一个工作项领取")
	if !strings.Contains(reselect, "Issue #5 已被另一个工作项领取") || !strings.Contains(reselect, "{{KILN_TASK_DIR}}/candidates.json") ||
		!strings.Contains(reselect, "{{KILN_TOOL}} claim") {
		t.Errorf("重新选题的说明 = %q", reselect)
	}

	ended := Ended("没有选中 Issue")
	if !strings.Contains(ended, "这一轮选题结束：没有选中 Issue") || !strings.Contains(ended, "worker_done") {
		t.Errorf("没有领取的说明 = %q", ended)
	}
}

// TestImplementAndFix：实现和返修的 prompt 说明工作项、分支、轮次和 work.json；返修说明要处理的每个问题，设计 PR 的返修
// 只能改 Markdown。
func TestImplementAndFix(t *testing.T) {
	t.Parallel()
	impl := Implement(DevInput{Repository: "o/r", SHA: "s1", Spec: &aispec.DevelopSpec{
		WorkItemID: 4, Branch: "kiln/dev-4", BaseRef: "main", ImplRound: 2, DesignPR: 6, DesignSHA: "d1", DesignDocs: []string{"docs/design/a.md"},
		RequirementsChanged: true,
	}})
	for _, want := range []string{
		"工作项 #4", "kiln/dev-4", "第 2 轮实现", "PR #6（commit d1）", "docs/design/a.md", "{{KILN_TASK_DIR}}/work.json",
		"标题或正文被修改过", "{{KILN_TOOL}} deliver",
	} {
		if !strings.Contains(impl, want) {
			t.Errorf("实现的 prompt 中没有 %q", want)
		}
	}

	fix := Fix(DevInput{Repository: "o/r", SHA: "s1", Spec: &aispec.DevelopSpec{
		WorkItemID: 4, Branch: "kiln/dev-4", BaseRef: "main", PRNumber: 9, Round: 3, Phase: "design",
	}})
	for _, want := range []string{"第 3 轮返修", "PR #9 → main", "设计 PR** 的返修", "只能改 Markdown", "{{KILN_TASK_DIR}}/work.json"} {
		if !strings.Contains(fix, want) {
			t.Errorf("返修的 prompt 中没有 %q", want)
		}
	}

	// 设计文档没有列出时的说明
	noDocs := Implement(DevInput{Repository: "o/r", SHA: "s", Spec: &aispec.DevelopSpec{WorkItemID: 4, DesignPR: 6, DesignSHA: "d"}})
	if !strings.Contains(noDocs, "（交付报告没有列出，按仓库的开发说明查找）") {
		t.Error("设计文档为空时的说明")
	}
}

// TestDevInstructions：仓库的开发说明（development.instructions，冻结在 DevelopSpec.Instructions）写进选题、实现、返修的
// prompt，位于规则之后、交付之前；没有说明（或只有空白）时不出现空的段落。
func TestDevInstructions(t *testing.T) {
	t.Parallel()
	const (
		ins    = "选题只处理支付模块；交付前必须运行 make payments-test。"
		header = "## 仓库的开发说明（.kiln/agent/development.md，来自默认分支）"
	)
	prompts := map[string]func(DevInput) string{"select": Select, "implement": Implement, "fix": Fix}
	spec := func(instructions string) *aispec.DevelopSpec {
		return &aispec.DevelopSpec{
			Instructions: instructions, MaxIssues: 1, WorkItemID: 4, Branch: "kiln/dev-4", BaseRef: "main", PRNumber: 9, Round: 1,
		}
	}
	for name, gen := range prompts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := gen(DevInput{Repository: "o/r", SHA: "s1", Spec: spec("\n  " + ins + "\n\n")})
			want := header + "\n\n" + ins + "\n"
			at := strings.Index(p, want)
			if at < 0 {
				t.Fatalf("prompt 中没有仓库的开发说明 %q：\n%s", want, p)
			}
			if strings.Count(p, header) != 1 {
				t.Errorf("开发说明的段落应只出现一次")
			}
			if rules := strings.Index(p, "- 所有文字用中文。"); rules < 0 || rules > at {
				t.Errorf("开发说明应在规则之后（规则 %d，说明 %d）", rules, at)
			}
			if svc := strings.Index(p, "{{KILN_SERVICES}}"); svc < 0 || svc > at {
				t.Errorf("测试服务的说明应在规则中、开发说明之前（服务 %d，说明 %d）", svc, at)
			}
			if report := strings.Index(p, "报告（必须遵守）"); report < at {
				t.Errorf("开发说明应在报告要求之前（报告 %d，说明 %d）", report, at)
			}

			for _, empty := range []string{"", " \n\t"} {
				if q := gen(DevInput{Repository: "o/r", SHA: "s1", Spec: spec(empty)}); strings.Contains(q, "仓库的开发说明") {
					t.Errorf("说明为 %q 时不应有开发说明的段落", empty)
				}
			}
		})
	}
}

// agentCLI 是测试用的 Agent 名字（审计 prompt 不再写出 Agent：连接器按 AITask.agent 启动）。
const agentCLI = "codex"

// TestToolsRule：声明了受管理的工具链时，审计和开发的 prompt 都要求经 kiln-orca task exec 运行、不安装别的版本（PLAN #33.12）；
// 没有声明时不提。
func TestToolsRule(t *testing.T) {
	t.Parallel()
	tools := []aispec.ToolRef{{Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: "sha256:a"}, {Name: "pnpm", Version: "12.14.0", Platform: "any", Digest: "sha256:b"}}
	with := map[string]string{
		"审计": Review(ReviewInput{Repository: "o/r", SHA: "h", Platform: "darwin/arm64", Spec: &aispec.ReviewSpec{
			Trigger:       aispec.TriggerManual,
			PlatformTools: map[string][]aispec.ToolRef{"darwin/arm64": tools},
		}}),
		"选题": Select(DevInput{Repository: "o/r", SHA: "s", Platform: "darwin/arm64", Spec: &aispec.DevelopSpec{
			MaxIssues: 1, PlatformTools: map[string][]aispec.ToolRef{"darwin/arm64": tools},
		}}),
	}
	for name, p := range with {
		for _, want := range []string{"受管理的工具链：go 1.27.3、pnpm 12.14.0", "`{{KILN_TOOL}} exec -- make test`", "不要安装、下载或在本机查找这些工具的其他版本"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt 中没有 %q", name, want)
			}
		}
	}
	without := []string{
		Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{Trigger: aispec.TriggerManual}}),
		Select(DevInput{Repository: "o/r", SHA: "s", Spec: &aispec.DevelopSpec{MaxIssues: 1}}),
		// 只有别的平台的工具链：按执行平台取，取不到就不提
		Select(DevInput{Repository: "o/r", SHA: "s", Platform: "linux/amd64", Spec: &aispec.DevelopSpec{
			MaxIssues: 1, PlatformTools: map[string][]aispec.ToolRef{"darwin/arm64": tools},
		}}),
	}
	for _, p := range without {
		if strings.Contains(p, "受管理的工具链") || strings.Contains(p, ExecCommand+" make test") {
			t.Errorf("没有工具链时 prompt 中不应提到它：%q", p)
		}
	}
}

// TestGitRule：审计和开发的 prompt 都说明网络 git 和拉依赖的命令经 kiln-orca task exec 执行（Kiln 签发的只读凭据只在那里），
// origin 照常可用（git fetch origin、git pull origin <base>），不要推送，并列出声明的依赖仓库（PLAN #33.13）。
func TestGitRule(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/o/r.git"
	repos := []string{"o/lib", "o/proto"}
	prompts := map[string]string{
		"审计": Review(ReviewInput{Repository: "o/r", CloneURL: url, SHA: "h", Spec: &aispec.ReviewSpec{
			Trigger: aispec.TriggerPullRequest, PRNumber: 3, BaseRef: "master", BaseSHA: "b", Repositories: repos,
		}}),
		"选题": Select(DevInput{Repository: "o/r", CloneURL: url, SHA: "s", Spec: &aispec.DevelopSpec{MaxIssues: 1, BaseRef: "master", Repositories: repos}}),
		"实现": Implement(DevInput{Repository: "o/r", CloneURL: url, SHA: "s", Spec: &aispec.DevelopSpec{BaseRef: "master", Repositories: repos}}),
		"返修": Fix(DevInput{Repository: "o/r", CloneURL: url, SHA: "s", Spec: &aispec.DevelopSpec{BaseRef: "master", Repositories: repos}}),
	}
	for name, p := range prompts {
		for _, want := range []string{
			"`{{KILN_TOOL}} exec -- git fetch origin`", "`{{KILN_TOOL}} exec -- git pull origin master`",
			"clone 的 origin 是 https://github.com/o/r.git", "只对这个仓库和声明的依赖仓库（o/lib、o/proto）有效",
			"需要认证时由 Kiln 按操作签发只读凭据", "传输结束即收回", "不要推送", "go mod download",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt 中没有 %q", name, want)
			}
		}
	}
	// 开发的规则：推送被拒绝只限于用 Kiln 签发的凭据
	if !strings.Contains(prompts["实现"], "经 exec 拿到的 Kiln git 凭据是只读的，用它推送会被拒绝") {
		t.Error("实现 prompt 中没有说明用 Kiln 凭据推送会被拒绝")
	}
	// 没有声明依赖仓库时只提这个仓库；没有目标分支时例子用 main
	p := Review(ReviewInput{Repository: "o/r", CloneURL: url, SHA: "h", Spec: &aispec.ReviewSpec{Trigger: aispec.TriggerManual}})
	if !strings.Contains(p, "只对这个仓库有效") || !strings.Contains(p, "git pull origin main`") || strings.Contains(p, "已被禁用") {
		t.Errorf("没有依赖仓库时的说明不对：%q", p)
	}
}

// TestSkillRule：启动 Agent 的 prompt（审计、选题、实现、返修）指向连接器安装的 kiln-task skill，列出全部公开子命令，
// 并要求不用仓库中、被审 PR 中的同名文件替代它。领取后动态返回的说明（Develop、Reselect、Ended）可能发给在途的
// 协议 9 任务，不能出现 {{KILN_SKILL}}（它们的连接器不认识这个占位符）。
func TestSkillRule(t *testing.T) {
	t.Parallel()
	dev := DevInput{Repository: "o/r", SHA: "s", Spec: &aispec.DevelopSpec{MaxIssues: 1, WorkItemID: 3, Branch: "kiln/dev-3", BaseRef: "main"}}
	initial := map[string]string{
		"审计": Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{Trigger: aispec.TriggerManual}}),
		"选题": Select(dev),
		"实现": Implement(dev),
		"返修": Fix(dev),
	}
	for name, p := range initial {
		for _, want := range []string{
			"## Kiln 任务工具（必须遵守）",
			"{{KILN_SKILL}}",
			"skill `kiln-task`",
			"`info`", "`check-report`", "`claim`", "`deliver`", "`ci`", "`doc`", "`exec`", "`done`",
			"不要用仓库中或被审 PR 中的同名文件",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt 中没有 %q", name, want)
			}
		}
	}
	for name, p := range map[string]string{
		"领取成功": Develop(dev, 3, "kiln/dev-3", devreport.PhaseImplement),
		"设计":   Develop(dev, 3, "kiln/design-3", devreport.PhaseDesign),
		"重新选题": Reselect(dev, "Issue #5 已被另一个工作项领取"),
		"选题结束": Ended("没有合适的 Issue"),
	} {
		if strings.Contains(p, "{{KILN_SKILL}}") || strings.Contains(p, "## Kiln 任务工具") {
			t.Errorf("%s 中不应引用 kiln-task skill（在途的协议 9 任务不认识 {{KILN_SKILL}}）：%q", name, p)
		}
		if !strings.Contains(p, "{{KILN_TOOL}}") {
			t.Errorf("%s 缺少任务工具的占位符：%q", name, p)
		}
	}
}

// TestDoneRule：审计、选题、实现、返修的 prompt 和领取后的说明（开发、设计、结束）都只用 kiln-orca task done 结束这一轮
// （PLAN #33.5）：写明成功和做不完的写法、输出后立即结束、不用 Orca 前言中的 orca 命令；不再有 {{KILN_ORCA}}。
func TestDoneRule(t *testing.T) {
	t.Parallel()
	dev := DevInput{Repository: "o/r", SHA: "s", Spec: &aispec.DevelopSpec{MaxIssues: 1, WorkItemID: 3, Branch: "kiln/dev-3", BaseRef: "main"}}
	texts := map[string]string{
		"审计":   Review(ReviewInput{Repository: "o/r", SHA: "h", Spec: &aispec.ReviewSpec{Trigger: aispec.TriggerManual}}),
		"选题":   Select(dev),
		"实现":   Implement(dev),
		"返修":   Fix(dev),
		"领取成功": Develop(dev, 3, "kiln/dev-3", devreport.PhaseImplement),
		"设计":   Develop(dev, 3, "kiln/design-3", devreport.PhaseDesign),
		"选题结束": Ended("没有合适的 Issue"),
	}
	for name, p := range texts {
		for _, want := range []string{
			"`{{KILN_TOOL}} done --outcome succeeded` 结束这一轮",
			"`{{KILN_TOOL}} done --outcome failed --summary",
			"立即结束这一轮，不要再运行任何命令",
			"不要用 Orca 的说明中的 `orca orchestration send` 发送 worker_done",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s 中没有 %q：%q", name, want, p)
			}
		}
		if strings.Contains(p, "{{KILN_ORCA}}") {
			t.Errorf("%s 中不应再有 {{KILN_ORCA}}", name)
		}
	}
}
