package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// repoRoot 返回当前 git 仓库的根目录（不在仓库中时为当前目录）。
func repoRoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return os.Getwd()
}

func workflowFiles(root string, args []string) []string {
	if len(args) > 0 {
		return args
	}
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(workflow.Dir), pat)) // 模式是常量，不会出错
		files = append(files, m...)
	}
	slices.Sort(files)
	return files
}

// localPlans 解析并按几种典型事件展开 workflow，用于 lint 和 runtime lock。
func localPlans(root string, files []string, lock *toolchain.Lock, checkRuntime bool) ([]*workflow.Plan, workflow.Diags) {
	var plans []*workflow.Plan
	var diags workflow.Diags
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G703：文件来自命令行参数或仓库内的 .kiln/workflows
		if err != nil {
			diags = append(diags, workflow.Diag{File: f, Msg: err.Error()})
			continue
		}
		w, d := workflow.Parse(displayPath(root, f), data)
		diags = append(diags, d...)
		if d.HasErrors() {
			continue
		}
		seen := map[string]bool{}
		for _, gctx := range lintContexts() {
			pc := workflow.PlanContext{GitHub: gctx.Map(), Inputs: map[string]string{}, Vars: func(string) map[string]string { return map[string]string{} }}
			if checkRuntime {
				pc.CheckRuntime = func(name, platform string) error {
					_, _, _, err := lock.Resolve(name, platform)
					return err
				}
			}
			p := workflow.Expand(w, pc)
			for _, d := range p.Diags {
				if !seen[d.String()] {
					seen[d.String()] = true
					diags = append(diags, d)
				}
			}
			plans = append(plans, p)
		}
	}
	return plans, diags
}

// displayPath 返回诊断中显示的文件名：仓库内的文件用相对仓库根目录的路径；
// 命令行给出的仓库外的文件（或无法换算的路径）按用户给出的写法显示。
func displayPath(root, f string) string {
	if abs, err := filepath.Abs(f); err == nil {
		if rel, err := filepath.Rel(root, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(f)
}

// readLock 读取 runtime lockfile；不存在时返回 nil。
func readLock(root string) (*toolchain.Lock, error) {
	b, err := os.ReadFile(filepath.Join(root, toolchain.LockPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return toolchain.ParseLock(b)
}

// lint 检查 workflow；问题输出到 stdout，有错误时返回非 nil。
func (a *app) lint(ctx context.Context, args []string) error {
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	mergeCfg, mergeErr, err := a.lintMergeConfig(root, len(args) == 0)
	if err != nil {
		return err
	}
	toolsErr, err := a.lintAgentTools(root, len(args) == 0)
	if err != nil {
		return err
	}
	files := workflowFiles(root, args)
	if len(files) == 0 {
		fmt.Fprintln(a.stdout, "没有找到 "+workflow.Dir+"/*.yml")
		if mergeCfg != nil && mergeErr == nil {
			// 没有 workflow 时 required 引用的 workflow 都不存在，ai-review 仍要求有 AI 审计配置
			mergeErr = a.lintRequired(root, mergeCfg, nil)
		}
		return errors.Join(mergeErr, toolsErr)
	}
	lock, err := readLock(root)
	if err != nil {
		return fmt.Errorf("%s：%w", toolchain.LockPath, err)
	}
	plans, diags := localPlans(root, files, lock, true)
	for _, d := range diags {
		fmt.Fprintln(a.stdout, d.String())
	}
	for _, p := range plans {
		for _, j := range p.Jobs {
			if j.FailReason != "" {
				fmt.Fprintf(a.stdout, "%s: 错误：job %s：%s\n", p.Workflow.File, j.DisplayName, j.FailReason)
				diags = append(diags, workflow.Diag{File: p.Workflow.File, Msg: j.FailReason})
			}
		}
	}
	if mergeCfg != nil && mergeErr == nil && !diags.HasErrors() {
		mergeErr = a.lintRequired(root, mergeCfg, plans)
	}
	if diags.HasErrors() {
		return errors.Join(errors.New("workflow 检查未通过"), toolsErr)
	}
	if err := errors.Join(mergeErr, toolsErr); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "✓ %d 个 workflow 没有问题\n", len(files))
	return nil
}

// lintMergeConfig 检查合并配置 .kiln/merge.yml（PLAN #28.5，只在检查整个仓库时检查，文件可以不存在）。
// 返回解析后的配置（没有检查或文件不存在时为 nil）；配置有错误时输出原因并返回 check 非空；读取失败时返回 err。
func (a *app) lintMergeConfig(root string, all bool) (cfg *mergecfg.Config, check, err error) {
	if !all {
		return nil, nil, nil
	}
	b, err := os.ReadFile(filepath.Join(root, mergecfg.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if cfg, err = mergecfg.Parse(b); err != nil {
		fmt.Fprintf(a.stdout, "错误：%v\n", err)
		return nil, errors.New(mergecfg.Path + " 检查未通过"), nil
	}
	return cfg, nil, nil
}

// lintRequired 检查 required 引用的验证在仓库中存在（PLAN #28.3）：workflow 文件和其中的 job，ai-review 要求有
// AI 审计配置。写错的名字在合并时会让每个 PR 都被移出队列，这里提前报告。
func (a *app) lintRequired(root string, cfg *mergecfg.Config, plans []*workflow.Plan) error {
	wfs := map[string]*workflow.Workflow{}
	for _, p := range plans {
		wfs[workflow.BaseName(filepath.Base(p.Workflow.File))] = p.Workflow // File 是相对仓库根目录的路径
	}
	bad := false
	for _, r := range cfg.Requirements {
		var why string
		switch w := wfs[r.Workflow]; {
		case r.Workflow == mergecfg.AIReview:
			why = aiReviewRequiredProblem(root, r)
		case w == nil:
			why = fmt.Sprintf("%s/%s.yml（或 .yaml）不存在", workflow.Dir, r.Workflow)
		case w.On.PullRequest == nil:
			why = filepath.Base(w.File) + " 不在 pull_request 时触发，PR 不会有这个 run"
		case r.Job != "" && w.Job(r.Job) == nil:
			why = fmt.Sprintf("%s 中没有 job %s", filepath.Base(w.File), r.Job)
		}
		if why != "" {
			bad = true
			fmt.Fprintf(a.stdout, "错误：%s：required 的 %s：%s\n", mergecfg.Path, r, why)
		}
	}
	// 多目标分支（PLAN #28.5）：必需 workflow 的 pull_request 分支过滤必须覆盖每个允许合并的目标分支，否则这些分支上的
	// PR 缺少必需的验证，上线后会被合并队列移出。同一个 workflow 的多个 job（ci/build、ci/test）只按 workflow 报一次。
	if len(cfg.Branches) > 0 {
		seen := make(map[string]bool, len(cfg.Requirements))
		for _, r := range cfg.Requirements {
			w := wfs[r.Workflow]
			if r.Workflow == mergecfg.AIReview || w == nil || w.On.PullRequest == nil || seen[r.Workflow] {
				continue
			}
			seen[r.Workflow] = true
			for _, b := range cfg.Branches {
				if !w.On.PullRequest.MatchesBranch(b) {
					bad = true
					fmt.Fprintf(a.stdout, "错误：%s：branches 中的 %s 不会触发必需 workflow %s 的 pull_request（它的 branches / branches-ignore 排除了这个分支），这些 PR 会缺少验证\n",
						mergecfg.Path, b, filepath.Base(w.File))
				}
			}
		}
	}
	if bad {
		return errors.New(mergecfg.Path + " 检查未通过")
	}
	return nil
}

// aiReviewRequiredProblem 检查 required 中的 ai-review：不能带 job，仓库要有 AI 审计配置并在 PR 时审计。
func aiReviewRequiredProblem(root string, r mergecfg.Requirement) string {
	if r.Job != "" {
		return "ai-review 没有 job，只能写 ai-review"
	}
	b, err := os.ReadFile(filepath.Join(root, agentcfg.ConfigPath))
	if err != nil {
		return "仓库没有 " + agentcfg.ConfigPath + "（没有开启 AI 审计）"
	}
	c, err := agentcfg.Parse(b)
	switch {
	case err != nil:
		return agentcfg.ConfigPath + " 无效：" + err.Error()
	case !c.PullRequestTrigger():
		return agentcfg.ConfigPath + " 没有开启 PR 审计（review.triggers.pull_request），PR 不会有审计 run"
	}
	return ""
}

// runtime 处理 runtime 子命令：目前只有 lock。
func (a *app) runtime(ctx context.Context, args []string) error {
	if sub, _ := splitSub(args); sub != "lock" {
		return usageErrorf("用法：kiln runtime lock")
	}
	return a.runtimeLock(ctx)
}

// runtimeLock 根据 workflow 用到的 runtime 生成 .kiln/runtimes.lock；进度输出到 stderr。
func (a *app) runtimeLock(ctx context.Context) error {
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	files := workflowFiles(root, nil)
	old, err := readLock(root)
	if err != nil {
		return fmt.Errorf("%s：%w", toolchain.LockPath, err)
	}
	plans, diags := localPlans(root, files, nil, false)
	if diags.HasErrors() {
		for _, d := range diags.Errors() {
			fmt.Fprintln(a.stderr, d.String())
		}
		return errors.New("workflow 有错误，先修复（kiln lint）")
	}
	platforms, err := lockPlatforms(root, plans)
	if err != nil {
		return err
	}
	if len(platforms) == 0 {
		fmt.Fprintln(a.stdout, "workflow 中没有 job 使用 runtimes，"+agentcfg.ConfigPath+" 也没有 requirements.tools，不需要 lockfile")
		return nil
	}
	progress := func(format string, args ...any) { fmt.Fprintf(a.stderr, format+"\n", args...) }
	lock, err := toolchain.Generate(ctx, root, platforms, old, progress)
	if err != nil {
		return err
	}
	p := filepath.Join(root, toolchain.LockPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, lock.Format(), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已写入", toolchain.LockPath)
	return nil
}

// lockPlatforms 返回 lockfile 要锁定的 runtime 和平台：workflow 的 runtimes 与 AI 任务的工具链（.kiln/agent/config.yml 的
// requirements.tools，PLAN #33.12）共用这份 lockfile，同一个 runtime 的平台取并集（排序、去重）。
func lockPlatforms(root string, plans []*workflow.Plan) (map[string][]string, error) {
	platforms := workflow.RuntimePlatforms(plans)
	tools, err := agentToolPlatforms(root)
	if err != nil {
		return nil, err
	}
	for name, plats := range tools {
		platforms[name] = slices.Compact(slices.Sorted(slices.Values(append(platforms[name], plats...))))
	}
	return platforms, nil
}

// agentToolPlatforms 返回 AI 任务的工具链需要锁定的平台（.kiln/agent/config.yml 的 requirements.tools，见
// agentcfg.Config.ToolPlatforms）；没有这个文件时为 nil，文件无效时返回错误。
func agentToolPlatforms(root string) (map[string][]string, error) {
	b, err := os.ReadFile(filepath.Join(root, agentcfg.ConfigPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := agentcfg.Parse(b)
	if err != nil {
		return nil, err
	}
	return c.ToolPlatforms(), nil
}

// lintAgentTools 检查 AI 任务的工具链（只在检查整个仓库时检查，all）在 lockfile 中都有各平台的条目：缺少时 Server
// 规划的任务以配置错误失败。问题输出到 stdout 并返回 check 非空；读取失败时返回 err。.kiln/agent/config.yml 不存在或无效
// 时不在这里报告（无效由 Server 规划时说明）。
func (a *app) lintAgentTools(root string, all bool) (check, err error) {
	if !all {
		return nil, nil
	}
	tools, cfgErr := agentToolPlatforms(root)
	if cfgErr != nil || len(tools) == 0 {
		return nil, nil //nolint:nilerr // 配置本身的错误不在这里报告
	}
	lock, err := readLock(root)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", toolchain.LockPath, err)
	}
	bad := false
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		for _, plat := range tools[name] {
			if _, err := toolchain.ResolveRefs(lock, []string{name}, plat); err != nil {
				bad = true
				fmt.Fprintf(a.stdout, "错误：%s：requirements.tools 的 %s（%s）：%v\n", agentcfg.ConfigPath, name, plat, err)
			}
		}
	}
	if bad {
		return errors.New(agentcfg.ConfigPath + " 的工具链检查未通过（运行 kiln runtime lock）"), nil
	}
	return nil, nil
}

// lintContexts 返回 lint 时用来展开 workflow 的几种典型事件：推送分支、推送 tag、PR。
// 与 Server 一样构造完整的 github 上下文，workflow 引用任何字段都不会误报。
func lintContexts() []*workflow.GitHubContext {
	events := []struct{ event, ref, head, base string }{
		{workflow.EventPush, workflow.RefPrefixBranch + "main", "", ""},
		{workflow.EventPush, workflow.RefPrefixTag + "v0.0.0", "", ""},
		{workflow.EventPullRequest, workflow.PullRef(1), "feature", "main"},
	}
	out := make([]*workflow.GitHubContext, 0, len(events))
	for _, e := range events {
		refType, refName := workflow.RefInfo(e.ref)
		out = append(out, &workflow.GitHubContext{
			EventName: e.event, SHA: strings.Repeat("0", 40), Ref: e.ref, RefName: refName, RefType: refType,
			Repository: "owner/repo", RepositoryOwner: "owner", HeadRef: e.head, BaseRef: e.base, Actor: "kiln",
		})
	}
	return out
}
