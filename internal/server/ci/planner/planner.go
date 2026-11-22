// Package planner 是 kiln-server 的规划器（PLAN #7.4）：为事件规划 workflow、展开 job、创建 run 并加入并发组，
// 以及重跑（PLAN #6.6）和手动触发 workflow_dispatch。
package planner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// errNoGitHubApp 表示 Server 没有配置 GitHub App（KILN_GITHUB_APP_ID）。
var errNoGitHubApp = errors.New("没有配置 GitHub App")

// Deps 是 Planner 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 用于读取 git tree、workflow 文件和 commit；没有配置 GitHub App 时为 nil，此时无法规划。
	GitHub *gh.Client
	Engine *engine.Engine
	// Outbox 在创建 run 和 job 时写入 Check Run 同步记录。
	Outbox *outbox.Writer
	// Hub 在新建 run 后唤醒各组件。
	Hub *notify.Hub
	Log *slog.Logger
	// Git 读取并缓存 git 对象（组装层传入与 AI 审计的规划共用的一份）；为 nil 时用新建的。
	Git *gitread.Reader
}

// Planner 为事件规划 workflow 并创建 run，也负责重跑和手动触发。
type Planner struct {
	db     *store.DB
	gh     *gh.Client
	engine *engine.Engine
	outbox *outbox.Writer
	hub    *notify.Hub
	log    *slog.Logger

	git *gitread.Reader
}

// New 返回 Planner。
func New(d Deps) *Planner {
	git := d.Git
	if git == nil {
		git = gitread.New()
	}
	return &Planner{db: d.DB, gh: d.GitHub, engine: d.Engine, outbox: d.Outbox, hub: d.Hub, log: d.Log, git: git}
}

// Request 是一次需要规划的事件。
type Request struct {
	Repo     *store.Repo
	Event    string // push | pull_request | workflow_dispatch
	Ref      string
	SHA      string
	Before   string // 分支 push 的 before
	Action   string // pull_request 的 action
	PRNumber int
	BaseRef  string
	// BaseSHA 是 PR 目标分支当前的 commit（AI 审计用来确定改动范围）。
	BaseSHA     string
	HeadRef     string
	SenderLogin string
	SenderIsBot bool
	// FromSelf 表示事件由本 App 触发（合并队列更新 PR 分支等，PLAN #28.4）。
	FromSelf bool
	Source   string // Source* 之一
	Inputs   map[string]string
	// workflow_dispatch：只规划这一个文件。
	Workflow string
	// RequestID 是 workflow_dispatch 的幂等键（dedupe_key 见 dispatchDedupeKey）。
	RequestID string
}

// run 的来源（runs.source，Request.Source），值见 store.Source*。
const (
	SourceWebhook   = store.SourceWebhook
	SourceReconcile = store.SourceReconcile
	SourceDashboard = store.SourceDashboard
	SourceCLI       = store.SourceCLI
	SourceAutoTag   = store.SourceAutoTag
)

// dispatchDedupeKey 返回手动触发的 run 的 dedupe_key：同一个 RequestID 只创建一个 run。
func dispatchDedupeKey(requestID string) string {
	return "dispatch:" + requestID
}

// 运行优先级：tag push 最高，其次是默认分支的 push，其余最低（PLAN #14.1）。
const (
	priorityTag           = 3
	priorityDefaultBranch = 2
	priorityNormal        = 1
)

func (p *Planner) tree(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) (*gh.Tree, error) {
	return p.git.Tree(ctx, inst, repo, sha)
}

func (p *Planner) blob(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) ([]byte, error) {
	return p.git.Blob(ctx, inst, repo, sha)
}

func (p *Planner) commitTime(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) (int64, error) {
	return p.git.CommitTime(ctx, inst, repo, sha)
}

// workflowFiles 返回 tree 中 workflow 目录（workflow.Dir）下的 workflow 文件（文件名 → blob sha）。
func workflowFiles(t *gh.Tree) map[string]string {
	out := map[string]string{}
	for _, e := range t.Tree {
		if e.Type != "blob" || path.Dir(e.Path) != workflow.Dir {
			continue
		}
		if strings.HasSuffix(e.Path, ".yml") || strings.HasSuffix(e.Path, ".yaml") {
			out[path.Base(e.Path)] = e.SHA
		}
	}
	return out
}

func githubContext(ev *Request) cistore.GitHubCtx {
	refType, refName := workflow.RefInfo(ev.Ref)
	g := cistore.GitHubCtx{
		EventName:       ev.Event,
		SHA:             ev.SHA,
		Ref:             ev.Ref,
		RefName:         refName,
		RefType:         refType,
		Repository:      ev.Repo.FullName(),
		RepositoryOwner: ev.Repo.Owner,
		Actor:           ev.SenderLogin,
	}
	if ev.Event == workflow.EventPullRequest {
		g.HeadRef, g.BaseRef = ev.HeadRef, ev.BaseRef
	}
	return g
}

// dedupeKey 返回 run 的 dedupe_key。PR 的 run 还按目标分支区分：改了目标分支后重新规划（合并队列只看针对默认分支的 run）。
func dedupeKey(ev *Request, workflowFile string) string {
	switch ev.Event {
	case workflow.EventWorkflowDispatch:
		return dispatchDedupeKey(ev.RequestID)
	case workflow.EventPullRequest:
		return fmt.Sprintf("%d:%s:%s:%s:%s:%s", ev.Repo.ID, workflowFile, ev.Event, ev.Ref, ev.SHA, ev.BaseRef)
	}
	return fmt.Sprintf("%d:%s:%s:%s:%s", ev.Repo.ID, workflowFile, ev.Event, ev.Ref, ev.SHA)
}

// isTrustedAutoTag 判断一次规划是否来自 Server 自动打的版本 tag push（PLAN #29.4）：来源是自动打 tag、事件是 push、
// ref 是 tag。可信的自动 tag push 允许 Bot 执行引用 secret 的签名 job 和发布 job；其他来源（webhook、重跑、手动触发）
// 即使 ref 是 tag 也不可信。
func isTrustedAutoTag(source, event, ref string) bool {
	return source == store.SourceAutoTag && event == gitref.EventPush && strings.HasPrefix(ref, gitref.TagPrefix)
}

func priorityFor(ev *Request) int {
	switch {
	case ev.Event == workflow.EventPush && strings.HasPrefix(ev.Ref, workflow.RefPrefixTag):
		return priorityTag
	case ev.Event == workflow.EventPush && ev.Ref == workflow.RefPrefixBranch+ev.Repo.DefaultBranch:
		return priorityDefaultBranch
	}
	return priorityNormal
}

// plannedRun 是一个 workflow 规划的结果。
type plannedRun struct {
	File         string
	Name         string
	BlobSHA      string
	State        store.RunState // active | skipped
	ConfigError  string
	Warnings     string
	Plan         *workflow.Plan
	InputDigests map[string]string // job_key → 输入摘要
	Jobs         []*workflow.PlannedJob
	CommitTime   int64
}

// changedFileList 是一次变更文件查询的结果。
type changedFileList struct {
	files    []string
	complete bool
}

// changedFilesFunc 返回按需查询、只查询一次的变更文件列表（用于 paths 过滤）。
func changedFilesFunc(ctx context.Context, inst *gh.Inst, ev *Request) func() ([]string, bool, error) {
	once := sync.OnceValues(func() (changedFileList, error) {
		var (
			files    []string
			complete bool
			err      error
		)
		switch ev.Event {
		case workflow.EventPush:
			if ev.Before == "" || ev.Before == gitref.ZeroSHA {
				return changedFileList{}, nil
			}
			files, complete, err = inst.CompareFiles(ctx, ev.Repo.FullName(), ev.Before, ev.SHA)
		case workflow.EventPullRequest:
			files, complete, err = inst.PullFiles(ctx, ev.Repo.FullName(), ev.PRNumber)
		}
		return changedFileList{files: files, complete: complete}, err
	})
	return func() ([]string, bool, error) {
		r, err := once()
		return r.files, r.complete, err
	}
}

// PlanEvent 为事件规划所有被触发的 workflow，返回新建的 run ID。
func (p *Planner) PlanEvent(ctx context.Context, ev *Request) ([]int64, error) {
	if p.gh == nil {
		return nil, errNoGitHubApp
	}
	inst := p.gh.Inst(ev.Repo.InstallationID)
	tree, err := p.tree(ctx, inst, ev.Repo, ev.SHA)
	if err != nil {
		return nil, fmt.Errorf("读取 git tree：%w", err)
	}
	return p.planWorkflows(ctx, inst, ev, tree)
}

// planWorkflows 规划 workflow 目录中被事件触发的 workflow（ev.Workflow 非空时只规划这一个），返回新建的 run ID。
func (p *Planner) planWorkflows(ctx context.Context, inst *gh.Inst, ev *Request, tree *gh.Tree) ([]int64, error) {
	files := workflowFiles(tree)
	names := make([]string, 0, len(files))
	for n := range files {
		if ev.Workflow != "" && n != ev.Workflow {
			continue
		}
		names = append(names, n)
	}
	slices.Sort(names)
	if ev.Workflow != "" && len(names) == 0 {
		return nil, &DispatchError{
			Field: "workflow", Code: DispatchWorkflowMissing, Subject: ev.Workflow, SHA: ev.SHA,
			Err: fmt.Errorf("%s 中没有 %s/%s", shorten.Hash(ev.SHA), workflow.Dir, ev.Workflow),
		}
	}
	if len(names) > 0 {
		// 记录来自 installation 事件、还缺 clone 地址时先补齐（Runner 检出源码需要）
		if err := repodir.Complete(ctx, p.db, p.gh, ev.Repo); err != nil {
			return nil, err
		}
	}
	changedFiles := changedFilesFunc(ctx, inst, ev)
	var runIDs []int64
	for _, file := range names {
		id, err := p.planWorkflow(ctx, inst, ev, tree, file, files[file], changedFiles)
		if err != nil {
			return runIDs, err
		}
		if id != 0 {
			runIDs = append(runIDs, id)
		}
	}
	return runIDs, nil
}

// planWorkflow 规划一个 workflow 文件。返回 0 表示没有新建 run（已存在或没有被触发）。
func (p *Planner) planWorkflow(ctx context.Context, inst *gh.Inst, ev *Request, tree *gh.Tree, file, blobSHA string,
	changedFiles func() ([]string, bool, error),
) (int64, error) {
	key := dedupeKey(ev, file)
	var exists bool
	if err := p.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE dedupe_key=$1)`, key).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return 0, nil
	}
	data, err := p.blob(ctx, inst, ev.Repo, blobSHA)
	if err != nil {
		return 0, fmt.Errorf("读取 %s：%w", file, err)
	}
	wf, diags := workflow.Parse(file, data)
	pr := &plannedRun{File: file, Name: wf.Name, BlobSHA: blobSHA, State: store.RunActive}
	onParsed := wf.On.Push != nil || wf.On.PullRequest != nil || wf.On.WorkflowDispatch != nil || len(wf.On.Schedule) > 0
	if onParsed || !diags.HasErrors() {
		m, err := wf.Match(workflow.Event{Name: ev.Event, Ref: ev.Ref, Action: ev.Action, BaseRef: ev.BaseRef, ChangedFiles: changedFiles})
		if err != nil {
			return 0, fmt.Errorf("获取变更文件：%w", err)
		}
		switch m {
		case workflow.NoMatch:
			if ev.Event == workflow.EventWorkflowDispatch {
				return 0, &DispatchError{
					Field: "workflow", Code: DispatchNotDispatchable, Subject: file, Err: fmt.Errorf("%s 没有声明 on.workflow_dispatch", file),
				}
			}
			return 0, nil
		case workflow.PathsExcluded:
			pr.State = store.RunSkipped
		}
	}
	if diags.HasErrors() {
		pr.ConfigError = diags.Errors().Error()
	} else if pr.State == store.RunActive {
		in := &expandInput{
			Inst: inst, Repo: ev.Repo, Tree: tree, Workflow: wf, GitHub: githubContext(ev), Inputs: ev.Inputs,
			SenderIsBot: ev.SenderIsBot, TrustedAutoTag: isTrustedAutoTag(ev.Source, ev.Event, ev.Ref),
		}
		if err := p.expandRun(ctx, in, pr); err != nil {
			return 0, err
		}
	}
	if w := diags.Warnings(); len(w) > 0 {
		pr.Warnings = strings.TrimSpace(w.Error() + "\n" + pr.Warnings)
	}
	return p.createRun(ctx, ev, key, pr)
}

// createRun 在一个事务中插入 run 和 job，处理并发组，并推进没有上游的 job。返回 0 表示 run 已存在。
func (p *Planner) createRun(ctx context.Context, ev *Request, key string, pr *plannedRun) (int64, error) {
	return p.createRunWith(ctx, ev, key, pr, nil)
}

// createRunWith 同 createRun；opt 非空时，key 在 opt.reuse 中的 job 直接沿用其他 job 的结果（合并队列沿用审计，PLAN #28.4）。
func (p *Planner) createRunWith(ctx context.Context, ev *Request, key string, pr *plannedRun, opt *rerunOptions) (int64, error) {
	var runID int64
	err := p.db.Tx(ctx, func(tx pgx.Tx) error {
		runID = 0
		state := pr.State
		if pr.ConfigError != "" {
			state = store.RunFailed
		}
		concKey, cancelInProgress := runConcurrency(pr, state)
		var prNum *int
		if ev.PRNumber != 0 {
			prNum = &ev.PRNumber
		}
		inputs := ev.Inputs
		if inputs == nil {
			inputs = map[string]string{}
		}
		err := tx.QueryRow(ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number,
			sender_login, sender_is_bot, concurrency_key, cancel_in_progress, state, config_error, warnings, github_ctx, inputs,
			commit_time, workflow_blob_sha, priority, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,nullif($15,''),nullif($16,''),$17,$18,$19,$20,$21, CASE WHEN $22 THEN now() END)
			ON CONFLICT (dedupe_key) DO NOTHING RETURNING id`,
			ev.Repo.ID, pr.File, pr.Name, ev.Event, ev.Ref, ev.SHA, ev.Source, key, prNum,
			ev.SenderLogin, ev.SenderIsBot, concKey, cancelInProgress, state, pr.ConfigError, pr.Warnings, store.MustJSON(githubContext(ev)), store.MustJSON(inputs),
			pr.CommitTime, pr.BlobSHA, priorityFor(ev), state.Terminal()).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_reruns (run_id, rerun, state, conclusion, finished_at) VALUES ($1, 0, $2, $3, CASE WHEN $4 THEN now() END)`,
			runID, state, state.ConclusionOrNil(), state.Terminal()); err != nil {
			return err
		}
		if state != store.RunActive {
			return cicheck.EnqueueRun(ctx, p.outbox, tx, runID, 0)
		}
		if err := p.insertJobs(ctx, tx, runID, 0, pr, opt); err != nil {
			return err
		}
		if concKey != nil {
			if err := p.joinConcurrencyGroup(ctx, tx, ev.Repo.ID, runID, 0, *concKey, cancelInProgress); err != nil {
				return err
			}
		}
		if err := cicheck.EnqueueRun(ctx, p.outbox, tx, runID, 0); err != nil {
			return err
		}
		return p.engine.AdvanceRun(ctx, tx, runID)
	})
	if err == nil && runID != 0 {
		p.hub.All()
		p.log.Info("新建 run", "run", runID, "repo", ev.Repo.FullName(), "workflow", pr.File, "event", ev.Event, "ref", ev.Ref, "sha", ev.SHA)
	}
	return runID, err
}

// runConcurrency 返回这次运行要加入的并发组（PLAN #6.5）：只有执行中的运行才加入，没有声明时为 nil。
func runConcurrency(pr *plannedRun, state store.RunState) (*string, bool) {
	if pr.Plan != nil && pr.Plan.ConcurrencyGroup != "" && state == store.RunActive {
		return &pr.Plan.ConcurrencyGroup, pr.Plan.CancelInProgress
	}
	return nil, false
}

// joinConcurrencyGroup 让新建的 run（或 run 新的一次运行 rerun）加入并发组（PLAN #6.5、#6.6）：
// cancel-in-progress 时取消组内其他 run；否则取消组内排队的 run，组内已有 active 的 run 时新 run 排队。
// 调用时 run 应处于 active。
func (p *Planner) joinConcurrencyGroup(ctx context.Context, tx pgx.Tx, repoID, runID int64, rerun int, key string, cancelInProgress bool) error {
	if err := cistore.LockConcurrencyGroup(ctx, tx, repoID, key); err != nil {
		return err
	}
	type openRun struct {
		ID    int64
		State store.RunState
	}
	rows, err := tx.Query(ctx, `SELECT id, state FROM runs WHERE repo_id=$1 AND concurrency_key=$2 AND state IN ('queued','active') AND id<>$3 ORDER BY id`,
		repoID, key, runID)
	if err != nil {
		return err
	}
	open, err := pgx.CollectRows(rows, pgx.RowToStructByPos[openRun])
	if err != nil {
		return err
	}
	hasActive := false
	for _, r := range open {
		switch {
		case cancelInProgress || r.State == store.RunQueued:
			if err := p.engine.CancelRun(ctx, tx, r.ID, store.CancelByConcurrency); err != nil {
				return err
			}
		case r.State == store.RunActive:
			hasActive = true
		}
	}
	if !hasActive {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET state=$2 WHERE id=$1`, runID, store.RunQueued); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE run_reruns SET state=$3 WHERE run_id=$1 AND rerun=$2`, runID, rerun, store.RunQueued)
	return err
}

// insertJobs 插入一次 rerun 的全部 job。opt 非空时，key 在 opt.reuse 中的 job 直接沿用上一次的结果。
func (p *Planner) insertJobs(ctx context.Context, tx pgx.Tx, runID int64, rerun int, pr *plannedRun, opt *rerunOptions) error {
	var priority int
	if err := tx.QueryRow(ctx, `SELECT priority FROM runs WHERE id=$1`, runID).Scan(&priority); err != nil {
		return err
	}
	keyToID := map[string]int64{}
	for _, j := range pr.Jobs {
		state, reason := store.JobWaiting, ""
		switch {
		case j.SkipReason != "":
			state, reason = store.JobSkipped, j.SkipReason
		case j.FailReason != "":
			state, reason = store.JobFailed, j.FailReason
		}
		var reusedFrom *int64
		var resultManifest *string
		var cachedFrom *int64
		var actionKey *string
		force := false
		if opt != nil {
			if prev, ok := opt.reuse[j.Key]; ok {
				format := "沿用上一次运行的结果（job #%d）"
				if opt.reuseReason != "" {
					format = opt.reuseReason
				}
				state, reason = prev.State, fmt.Sprintf(format, prev.ID)
				reusedFrom, resultManifest, cachedFrom, actionKey = &prev.ID, prev.ResultManifest, prev.CachedFrom, prev.ActionKey
			} else if opt.force[j.Key] {
				force = true
			}
		}
		var inputDigest *string
		if d, ok := pr.InputDigests[j.Key]; ok {
			inputDigest = &d
		}
		var env *string
		if j.Spec.Environment != "" {
			env = &j.Spec.Environment
		}
		matrix := []byte("null")
		if len(j.Matrix) > 0 {
			matrix = store.MustJSON(j.Matrix)
		}
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, matrix, spec, platform, executor_kind,
			input_digest, environment, state, reason, priority, effects, force_execute, reused_from_job_id, result_manifest, cached_from_job_id,
			action_key, keep_on_failure, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,nullif($14,''),$15,$16,$17,$18,$19,$20,$21,$22,
			CASE WHEN $23 THEN now() END) RETURNING id`,
			j.Kind, runID, rerun, j.Key, j.JobID, j.DisplayName, matrix, store.MustJSON(j.Spec), j.Spec.Platform, j.Spec.Executor,
			inputDigest, env, state, reason, priority, j.Spec.Effects, force, reusedFrom, resultManifest, cachedFrom,
			actionKey, opt != nil && opt.keepOnFailure, state.Terminal()).Scan(&id)
		if err != nil {
			return fmt.Errorf("插入 job %s：%w", j.Key, err)
		}
		keyToID[j.Key] = id
	}
	for _, j := range pr.Jobs {
		if len(j.NeedsKeys) == 0 {
			continue
		}
		needs := make([]int64, 0, len(j.NeedsKeys))
		for _, k := range j.NeedsKeys {
			needs = append(needs, keyToID[k])
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET needs=$2 WHERE id=$1`, keyToID[j.Key], needs); err != nil {
			return err
		}
	}
	jobs, err := cistore.RerunJobs(ctx, tx, runID, rerun, false)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := cicheck.EnqueueJob(ctx, p.outbox, tx, j); err != nil {
			return err
		}
	}
	return nil
}

// auditBestEffort 记录审计日志；用于操作本身已经完成、审计失败不应让请求失败的场景（只记录告警）。
func (p *Planner) auditBestEffort(ctx context.Context, actor, action string, detail any) {
	if err := store.Audit(ctx, p.db, actor, action, detail); err != nil {
		p.log.Warn("记录审计日志失败", "action", action, "actor", actor, "err", err)
	}
}
