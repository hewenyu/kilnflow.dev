package planner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// expandInput 是展开一个 workflow 需要的全部输入。
type expandInput struct {
	Inst        *gh.Inst
	Repo        *store.Repo
	Tree        *gh.Tree
	Workflow    *workflow.Workflow
	GitHub      cistore.GitHubCtx
	Inputs      map[string]string
	SenderIsBot bool
	// TrustedAutoTag 由 isTrustedAutoTag 派生（PLAN #29.4）：可信的自动 tag push 允许 Bot 执行 secret 和发布 job。
	TrustedAutoTag bool
}

// envLookup 在一次规划中缓存 environment 的查询结果。
type envLookup struct {
	db     *store.DB
	repoID int64
	cache  map[string]*store.Environment
}

func (l *envLookup) get(ctx context.Context, name string) (*store.Environment, error) {
	if e, ok := l.cache[name]; ok {
		return e, nil
	}
	e, err := store.LookupEnvironment(ctx, l.db, l.repoID, name)
	if err != nil {
		return nil, err
	}
	l.cache[name] = e
	return e, nil
}

// planDiags 收集展开过程中的配置错误和警告。
type planDiags struct {
	errs, warns []string
}

func (d *planDiags) errorf(format string, args ...any) {
	d.errs = append(d.errs, fmt.Sprintf(format, args...))
}

func (d *planDiags) warnf(format string, args ...any) {
	d.warns = append(d.warns, fmt.Sprintf(format, args...))
}

// expandRun 展开 workflow：求值、矩阵、校验 environment、计算输入摘要、解析 runtime。
// 也用于重跑时按当前配置重新规划（PLAN #6.6）。
func (p *Planner) expandRun(ctx context.Context, in *expandInput, pr *plannedRun) error {
	if in.Tree.Truncated {
		pr.ConfigError = "仓库的 git tree 过大（GitHub 返回 truncated），v0 暂不支持"
		return nil
	}
	ct, err := p.commitTime(ctx, in.Inst, in.Repo, in.GitHub.SHA)
	if err != nil {
		return fmt.Errorf("读取 commit：%w", err)
	}
	pr.CommitTime = ct
	lock, lockErr, err := p.runtimeLock(ctx, in)
	if err != nil {
		return err
	}
	envs := &envLookup{db: p.db, repoID: in.Repo.ID, cache: map[string]*store.Environment{}}
	var varsErr error
	pc := workflow.PlanContext{
		GitHub:         in.GitHub.Map(),
		Inputs:         in.Inputs,
		CommitTime:     ct,
		SenderIsBot:    in.SenderIsBot,
		TrustedAutoTag: in.TrustedAutoTag,
		Vars: func(env string) map[string]string {
			var e *store.Environment
			if env != "" {
				var err error
				if e, err = envs.get(ctx, env); err != nil {
					varsErr = err
					return nil
				}
			}
			v, err := store.VariablesFor(ctx, p.db, in.Repo, e)
			if err != nil {
				varsErr = err
			}
			return v
		},
		CheckRuntime: func(name, platform string) error {
			if lockErr != nil {
				return lockErr
			}
			_, _, _, err := lock.Resolve(name, platform)
			return err
		},
	}
	plan := workflow.Expand(in.Workflow, pc)
	if varsErr != nil {
		return varsErr
	}
	pr.Plan = plan
	pr.Name = in.Workflow.Name
	var d planDiags
	if plan.Diags.HasErrors() {
		d.errs = append(d.errs, plan.Diags.Errors().Error())
	}
	if w := plan.Diags.Warnings(); len(w) > 0 {
		d.warns = append(d.warns, w.Error())
	}
	entries, allPaths := treeEntries(in.Tree)
	pr.InputDigests = map[string]string{}
	emptyWarned := map[string]bool{}
	for _, j := range plan.Jobs {
		if j.Spec == nil {
			continue
		}
		ok, err := p.checkJobEnvironment(ctx, in, envs, j, &d)
		if err != nil {
			return err
		}
		if !ok || j.Kind != workflow.JobKindNormal {
			continue
		}
		pr.InputDigests[j.Key] = jobInputDigest(in.Workflow.File, j, entries, allPaths, emptyWarned, &d)
		resolveRuntimes(in.Workflow.File, j, lock, &d)
	}
	if len(d.errs) > 0 {
		pr.ConfigError = strings.Join(dedupStrings(d.errs), "\n")
	}
	pr.Warnings = strings.Join(dedupStrings(d.warns), "\n")
	pr.Jobs = plan.Jobs
	return nil
}

// runtimeLock 读取仓库中的 runtime lockfile。lockfile 不存在时返回 nil；
// 内容无效时返回解析错误（只有用到 runtime 的 job 才会报告它）。
func (p *Planner) runtimeLock(ctx context.Context, in *expandInput) (lock *toolchain.Lock, lockErr, err error) {
	for _, e := range in.Tree.Tree {
		if e.Path == toolchain.LockPath && e.Type == "blob" {
			data, err := p.blob(ctx, in.Inst, in.Repo, e.SHA)
			if err != nil {
				return nil, nil, err
			}
			lock, lockErr = toolchain.ParseLock(data)
		}
	}
	return lock, lockErr, nil
}

// treeEntries 返回 tree 中参与输入摘要的条目（文件和子模块）及其路径。
func treeEntries(t *gh.Tree) ([]actionkey.TreeEntry, []string) {
	var entries []actionkey.TreeEntry
	var paths []string
	for _, e := range t.Tree {
		if e.Type == "blob" || e.Type == "commit" {
			entries = append(entries, actionkey.TreeEntry{Path: e.Path, Mode: e.Mode, SHA: e.SHA})
			paths = append(paths, e.Path)
		}
	}
	return entries, paths
}

// checkJobEnvironment 校验 job 引用的 environment（PLAN #16.4）和发布 job 的约束（PLAN #13.4、#13.6）。
// 返回 false 表示 job 有配置错误（已记录到 d），不需要继续处理它。
func (p *Planner) checkJobEnvironment(ctx context.Context, in *expandInput, envs *envLookup, j *workflow.PlannedJob, d *planDiags) (bool, error) {
	sp := j.Spec
	file, ref := in.Workflow.File, in.GitHub.Ref
	if sp.Environment != "" && j.SkipReason == "" {
		e, err := envs.get(ctx, sp.Environment)
		if err != nil {
			return false, err
		}
		if e == nil {
			d.errorf("%s: 错误：job %s 引用的 environment %s 不存在。Kiln 不会自动创建，也不读取 GitHub Settings："+
				"先在 Kiln 中创建（Dashboard 设置 → 环境，或 kiln env set %s --repo %s --ref <允许的 ref 模式>），再重跑这个 run",
				file, j.JobID, sp.Environment, sp.Environment, in.Repo.FullName())
			return false, nil
		}
		if !e.RefAllowed(ref) && j.FailReason == "" {
			j.FailReason = fmt.Sprintf("environment %s 的 ref 策略不允许 %s", e.Name, ref)
		}
	}
	if j.Kind != workflow.JobKindPublish || j.FailReason != "" || j.SkipReason != "" {
		return true, nil
	}
	if in.GitHub.EventName != workflow.EventPush || !strings.HasPrefix(ref, workflow.RefPrefixTag) {
		j.FailReason = "发布只能由 tag push 触发（PLAN #13.4）"
	}
	return p.checkCrossRepoPublish(ctx, in, envs, j, d)
}

// checkCrossRepoPublish 检查发布到其他仓库的 job（PLAN #13.6）。返回 false 表示有配置错误。
func (p *Planner) checkCrossRepoPublish(ctx context.Context, in *expandInput, envs *envLookup, j *workflow.PlannedJob, d *planDiags) (bool, error) {
	sp := j.Spec
	file := in.Workflow.File
	if sp.Release == nil {
		return true, nil
	}
	g := sp.Release.GitHub
	if g == nil || g.Repository == "" || strings.EqualFold(g.Repository, in.Repo.FullName()) {
		return true, nil
	}
	if sp.Environment == "" {
		d.errorf("%s: 错误：job %s 发布到其他仓库 %s，必须声明 environment（PLAN #13.6）", file, j.JobID, g.Repository)
		return false, nil
	}
	e, err := envs.get(ctx, sp.Environment)
	if err != nil {
		return false, err
	}
	if e == nil || !slices.ContainsFunc(e.AllowedPublishRepos, func(r string) bool { return strings.EqualFold(r, g.Repository) }) {
		d.errorf("%s: 错误：%s 不在 environment %s 的允许发布仓库列表中（kiln env set %s … --publish-to %s）", file, g.Repository, sp.Environment, sp.Environment, g.Repository)
		return false, nil
	}
	// 目标仓库不在 Kiln 中时，用源仓库 installation 的 token 定向读取这一个仓库（不依赖启动时同步全部仓库）；
	// 读不到与没有安装 App 是同一个配置错误
	target, err := repodir.Lookup(ctx, p.db, p.gh, in.Repo.InstallationID, g.Repository)
	if err != nil {
		return false, err
	}
	if target == nil || target.InstallationID != in.Repo.InstallationID {
		d.errorf("%s: 错误：App 没有安装在目标仓库 %s 上（在 GitHub 的 App 安装设置中添加它）", file, g.Repository)
		return false, nil
	}
	if g.NotesFrom == "" {
		d.errorf("%s: 错误：跨仓库发布时必须显式填写 release.github.notes.from（PLAN #13.6）", file)
		return false, nil
	}
	return true, nil
}

// jobInputDigest 计算普通 job 的输入摘要，并对没有匹配任何文件的 inputs 模式给出警告（每个 job 只警告一次）。
func jobInputDigest(file string, j *workflow.PlannedJob, entries []actionkey.TreeEntry, allPaths []string, warned map[string]bool, d *planDiags) string {
	var inputs []string
	if !j.Spec.InputsDefault {
		inputs = j.Spec.Inputs
	}
	m := workflow.NewInputMatcher(inputs)
	var sel []actionkey.TreeEntry
	for _, e := range entries {
		if m.Match(e.Path) {
			sel = append(sel, e)
		}
	}
	if !warned[j.JobID] {
		warned[j.JobID] = true
		for _, p := range m.EmptyPatterns(allPaths) {
			d.warnf("%s: 警告：job %s 的 inputs 中 %q 没有匹配任何文件", file, j.JobID, p)
		}
	}
	return actionkey.InputDigest(sel)
}

// resolveRuntimes 按 lockfile 解析 job 声明的 runtime。
func resolveRuntimes(file string, j *workflow.PlannedJob, lock *toolchain.Lock, d *planDiags) {
	sp := j.Spec
	for _, name := range sp.Runtimes {
		version, plat, digest, err := lock.Resolve(name, sp.Platform)
		if err != nil {
			if j.FailReason == "" && j.SkipReason == "" {
				d.errorf("%s: 错误：job %s：%v", file, j.JobID, err)
			}
			continue
		}
		sp.RuntimeRefs = append(sp.RuntimeRefs, workflow.RuntimeRef{Name: name, Version: version, Platform: plat, Digest: digest})
	}
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
