package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/randtoken"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 规划 AI 审计（PLAN #30.6）：仓库有 .kiln/agent/config.yml 并启用了审计时，PR 事件和手动触发各建一个审计请求，
// 其中 review.required_platforms 的每一项一个任务（单个平台，或一组可以互相替代的平台，PLAN #26.1）。审计不依赖
// .kiln/workflows/，与 CI 的规划互不影响（任一方的配置错误都不阻断另一方）。

const (
	eventPullRequest = gitref.EventPullRequest
	// eventManual 是手动审计的 event（与手动触发 workflow 的事件名相同，迁移前的记录也是它）。
	eventManual = gitref.EventWorkflowDispatch
	// workflowName 是审计在去重键中的名称（与迁移前 run 的 workflow 相同：重复的事件在迁移前后去重一致）。
	workflowName = "ai-review"
)

// ErrNoGitHubApp 表示 Server 没有配置 GitHub App。
var ErrNoGitHubApp = errors.New("没有配置 GitHub App")

// UpdatedFromFunc 返回合并队列把目标分支 baseRef 合并进 PR 分支之前的 head（PR 的新 head 是它更新出来的时候），否则为空
// （PLAN #28.4）。只有记录的目标分支与 baseRef 相同的更新才算：从 dev 更新期间 PR 改投 master 时，不能沿用 dev 那次更新的
// 旧审计（PLAN #28.5）。
type UpdatedFromFunc func(ctx context.Context, q store.Querier, repoID int64, pr int, head, baseRef string) (string, error)

// Deps 是 Reviews 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 用于读取配置和审计说明；没有配置 GitHub App 时为 nil，此时无法规划。
	GitHub *gh.Client
	Core   *ai.Core
	Git    *gitread.Reader
	// Docs 读取工作项冻结的外部设计文档快照，让审计与开发读到同一份设计依据；为 nil 时审计不带文档。
	Docs *docsync.Service
	// BaseCtx 是服务生命周期 context：取消时立即中断进行中的同步规划（信号退出、HTTP 异常退出、Close）。
	BaseCtx context.Context
	// Track 可选：登记一次进行中的同步操作，返回的 done 在操作结束时调用（Server 关闭数据库前等它们结束）。
	// 服务正在退出时返回错误，调用方不得再开始操作、也不得再查数据库。
	Track func() (done func(), err error)
	// UpdatedFrom 由组装层注入（合并领域提供）。
	UpdatedFrom UpdatedFromFunc
	Log         *slog.Logger
}

// Reviews 规划、重跑和取消 AI 审计请求。
type Reviews struct {
	db          *store.DB
	gh          *gh.Client
	core        *ai.Core
	git         *gitread.Reader
	docs        *docsync.Service
	updatedFrom UpdatedFromFunc
	log         *slog.Logger
	baseCtx     context.Context
	track       func() (done func(), err error)
	// locks 按去重键串行化同一进程内的同步规划，避免并发事件重复拉取同一个文档集。
	locks *keyedLocks
}

// New 返回 Reviews。
func New(d Deps) *Reviews {
	return &Reviews{
		db: d.DB, gh: d.GitHub, core: d.Core, git: d.Git, docs: d.Docs, updatedFrom: d.UpdatedFrom, log: d.Log,
		baseCtx: d.BaseCtx, track: d.Track, locks: newKeyedLocks(),
	}
}

// Event 是一次需要规划审计的事件（PR 事件或手动审计）。
type Event struct {
	Repo     *store.Repo
	Event    string // pull_request | workflow_dispatch（手动）
	Ref      string
	SHA      string
	Action   string // pull_request 的 action
	PRNumber int
	BaseRef  string
	// BaseSHA 是 PR 目标分支当前的 commit（确定改动范围、读取审计说明）。
	BaseSHA string
	HeadRef string
	Sender  string
	// FromSelf 表示事件由本 App 触发（合并队列更新 PR 分支等，PLAN #28.4）。
	FromSelf bool
	Source   string
	// RequestID 是手动审计的幂等键。
	RequestID string
}

func (ev *Event) manual() bool { return ev.Event == eventManual }

// dedupeKey 返回首次审计请求的去重键：PR 审计按 head commit 和目标分支，手动审计按请求。
func dedupeKey(ev *Event) string {
	if ev.manual() {
		return "ai-review:dispatch:" + ev.RequestID
	}
	return fmt.Sprintf("%d:%s:%s:%s:%s:%s", ev.Repo.ID, workflowName, ev.Event, ev.Ref, ev.SHA, ev.BaseRef)
}

// GroupKey 返回 PR 审计的并发组：同一个 PR 只保留最新 head 的审计，旧的取消（审计耗时长、占用额度）。
func GroupKey(pr int) string { return fmt.Sprintf("review:pr:%d", pr) }

// TriggerError 是手动审计的参数错误（Field 是出错的参数）。
type TriggerError struct {
	Field string
	Err   error
}

func (e *TriggerError) Error() string { return e.Err.Error() }
func (e *TriggerError) Unwrap() error { return e.Err }

// PlanEvent 为 PR 事件规划审计，返回新建的请求 ID（没有配置、没有启用、已经存在时为 0）。只处理 pull_request。
func (p *Reviews) PlanEvent(ctx context.Context, ev *Event) (int64, error) {
	if ev.Event != eventPullRequest {
		return 0, nil
	}
	if p.gh == nil {
		return 0, ErrNoGitHubApp
	}
	planned, err := p.plan(ctx, ev)
	if err != nil || planned == nil || !planned.Created {
		return 0, err
	}
	return planned.Request.ID, nil
}

// plan 规划 ev 的审计（手动审计没有配置或不允许手动触发时返回 *TriggerError）；没有启用时返回 nil。
func (p *Reviews) plan(ctx context.Context, ev *Event) (*ai.Planned, error) {
	key := dedupeKey(ev)
	if r, err := ai.RequestByDedupeKey(ctx, p.db, key); err != nil || r != nil {
		return &ai.Planned{Request: r}, err
	}
	inst := p.gh.Inst(ev.Repo.InstallationID)
	tree, err := p.git.Tree(ctx, inst, ev.Repo, ev.SHA)
	if err != nil {
		return nil, fmt.Errorf("读取 git tree：%w", err)
	}
	cfgBlob := gitread.FindBlob(tree, agentcfg.ConfigPath)
	if cfgBlob == "" {
		if ev.manual() {
			return nil, &TriggerError{Field: "ref", Err: fmt.Errorf("%s 中没有 %s", ev.SHA, agentcfg.ConfigPath)}
		}
		return nil, nil
	}
	cfg, cfgErr, err := p.readConfig(ctx, inst, ev.Repo, cfgBlob)
	if err != nil {
		return nil, err
	}
	nr := p.newRequest(ev, key)
	if cfgErr != "" {
		// 配置错误：建一个失败的请求，在 Dashboard 和 Check Run 中说明
		nr.ConfigError = cfgErr
		nr.Detail = p.detail(ev, nil, nil, nil)
		return p.core.Plan(ctx, nr)
	}
	switch {
	case ev.manual() && !cfg.ManualTrigger():
		return nil, &TriggerError{Field: "ref", Err: fmt.Errorf("%s 没有启用审计或不允许手动触发", agentcfg.ConfigPath)}
	case !ev.manual() && !cfg.PullRequestTrigger():
		return nil, nil
	}
	tools, toolsErr, err := p.tools(ctx, inst, ev.Repo, tree, cfg)
	if err != nil {
		return nil, err
	}
	if toolsErr != "" {
		nr.ConfigError = toolsErr
		nr.Detail = p.detail(ev, nil, nil, nil)
		return p.core.Plan(ctx, nr)
	}
	// 从这里开始要读取外部文档并可能提交快照集：用一份独立的服务端预算（不受调用方取消影响、随服务生命周期取消），
	// 并在进程内按去重键串行。拿锁后二次查库，已提交的请求直接复用，不再重复拉取 Outline。
	opCtx, cancel, err := p.opContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	unlock, err := p.locks.acquire(opCtx, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if r, err := ai.RequestByDedupeKey(opCtx, p.db, key); err != nil || r != nil {
		return &ai.Planned{Request: r}, err
	}
	wi, err := workItemReview(opCtx, p.db, ev.Repo.ID, ev.PRNumber, ev.HeadRef)
	if err != nil {
		return nil, err
	}
	basis, err := p.planBasis(opCtx, ev.Repo.ID, wi)
	if err != nil {
		return nil, err
	}
	b, err := p.build(opCtx, inst, ev, tree, cfg, tools, wi, basis)
	if err != nil {
		return nil, err
	}
	nr.Warnings, nr.Tasks = b.warnings, b.tasks
	reused, err := p.queueReuse(opCtx, inst, ev, nr.Tasks)
	if err != nil {
		return nil, err
	}
	nr.Detail = p.detail(ev, b, reused, nil)
	return p.core.Plan(opCtx, nr)
}

// docBasis 是本次规划固定的外部设计文档依据：独立审计由 PrepareFreeze 拉取（Prepared），工作项 PR 直接读工作项快照，
// 重跑沿用请求族根的依据。Docs 只有元数据（正文由 Agent 经 kiln-orca task doc 读取）。
type docBasis struct {
	Source         string
	WorkItemID     int64
	SnapshotSetID  int64
	ManifestDigest string
	Docs           []docsync.Snapshot
	// Prepared 是独立审计新冻结的整批文档（Bound=false 表示没有绑定，提交时记 none）。重跑沿用已有依据时为 nil。
	Prepared *docsync.PreparedFreeze
}

// planBasis 决定这次规划的文档依据：关联工作项时读工作项已冻结的快照（没有冻结就是 none，不为工作项新建冻结）；
// 独立 PR / 手动审计自己冻结一份快照集（没有绑定时是 none）。拉取失败时返回错误，不带空快照继续。
func (p *Reviews) planBasis(ctx context.Context, repoID int64, wi *prWorkItem) (docBasis, error) {
	if p.docs == nil {
		return docBasis{Source: docsync.SourceNone, ManifestDigest: docsync.NoneManifestDigest(), WorkItemID: workItemID(wi)}, nil
	}
	if wi != nil {
		b, err := p.docs.WorkItemBasis(ctx, p.db, repoID, wi.id)
		if err != nil {
			return docBasis{}, err
		}
		return docBasis{Source: b.Source, WorkItemID: wi.id, ManifestDigest: b.ManifestDigest, Docs: b.Docs}, nil
	}
	prepared, err := p.docs.PrepareFreeze(ctx, repoID)
	if err != nil {
		return docBasis{}, err
	}
	source := docsync.SourceSnapshotSet
	if !prepared.Bound {
		source = docsync.SourceNone
	}
	return docBasis{Source: source, ManifestDigest: prepared.ManifestDigest, Docs: metadata(prepared.Docs), Prepared: prepared}, nil
}

// workItemID 返回工作项的编号（没有工作项时为 0）。
func workItemID(wi *prWorkItem) int64 {
	if wi == nil {
		return 0
	}
	return wi.id
}

// metadata 去掉快照中的正文（写进任务 spec 的只有引用）。
func metadata(docs []docsync.Snapshot) []docsync.Snapshot {
	out := make([]docsync.Snapshot, 0, len(docs))
	for _, d := range docs {
		out = append(out, docsync.Snapshot{DocID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	return out
}

// newRequest 返回 ev 的审计请求（还没有任务）。
func (p *Reviews) newRequest(ev *Event, key string) *ai.NewRequest {
	trigger := aispec.TriggerPullRequest
	if ev.manual() {
		trigger = aispec.TriggerManual
	}
	nr := &ai.NewRequest{
		Type: agentcfg.TaskReview, RepoID: ev.Repo.ID, Event: ev.Event, Ref: ev.Ref, SHA: ev.SHA, Trigger: trigger, Source: ev.Source,
		Sender: ev.Sender, DedupeKey: key,
	}
	if ev.PRNumber != 0 {
		nr.GroupKey = GroupKey(ev.PRNumber)
	}
	return nr
}

// detail 写审计请求的明细（ai_review_requests）。b 为 nil 时（配置错误）没有审计规则、也没有进入冻结；
// fixed 不为 nil 时（重跑）沿用请求族根固定的文档依据。reused 是沿用审计结论的来源请求。
//
// 独立审计的快照集在这里原子提交：CommitSetTx 只写数据库，不请求 Outline；提交后回填任务 spec 的
// doc_snapshot_set_id / docs_source / docs_manifest_digest，保证 spec 与数据库关系一致。事务重试会重新执行本回调，
// 因此 setID 绝不缓存（每次都是本次事务新建的那一行）。
func (p *Reviews) detail(ev *Event, b *built, reused *int64, fixed *docBasis) func(ctx context.Context, tx pgx.Tx, id int64) error {
	return func(ctx context.Context, tx pgx.Tx, id int64) error {
		var pr *int
		if ev.PRNumber != 0 {
			pr = &ev.PRNumber
		}
		var digest string
		source := docsync.SourceUnplanned
		workItem := int64(0)
		var prepared *docsync.PreparedFreeze
		setID := int64(0)
		manifestDigest := ""
		// lock 表示这次请求附着的是**已经存在**的快照集或工作项依据（不是本事务刚提交的），
		// 必须加锁并与保留清理串行；正文是否仍可读只在新任务真正执行时要求。
		lock := false
		switch {
		case b != nil:
			digest, workItem = b.digest, b.workItem
			source, manifestDigest, prepared, setID = b.docsSource, b.docsManifestDigest, b.prepared, b.snapshotSetID
			switch source {
			case docsync.SourceSnapshotSet:
				// 新冻结的独立审计：提交快照集，回填任务 spec；沿用已有的快照集时加锁并核对正文。
				if prepared != nil && prepared.Bound {
					basis, err := p.docs.CommitSetTx(ctx, tx, ev.Repo.ID, prepared)
					if err != nil {
						return err
					}
					setID, manifestDigest, workItem = basis.SnapshotSetID, basis.ManifestDigest, 0
					for i := range b.tasks {
						if s, ok := b.tasks[i].Spec.(*aispec.ReviewSpec); ok {
							s.DocSnapshotSetID, s.DocsSource, s.DocsManifestDigest = basis.SnapshotSetID, basis.Source, basis.ManifestDigest
						}
					}
				} else {
					lock = true
				}
			case docsync.SourceWorkItem:
				lock = true
			}
		case fixed != nil:
			// 重跑遇到配置错误（没有任务）：保留族根已有的 canonical 文档依据引用，不退回 unplanned。
			source, workItem = fixed.Source, fixed.WorkItemID
			manifestDigest, setID = fixed.ManifestDigest, fixed.SnapshotSetID
			if source == docsync.SourceSnapshotSet {
				workItem = 0
			}
			// 只要附着了已有的依据就一律加锁，哪怕是配置错误、这次没有任何任务执行：
			// 依据本身仍被这次请求引用，保留清理不能在同一时刻把它清掉。
			switch source {
			case docsync.SourceSnapshotSet, docsync.SourceWorkItem:
				lock = true
			}
		}
		basis := docsync.Basis{Source: source, RepoID: ev.Repo.ID, WorkItemID: workItem, SnapshotSetID: setID}
		if lock {
			if err := docsync.LockBasisTx(ctx, tx, basis); err != nil {
				return err
			}
			// 没有任务真正执行（全部沿用结论或配置错误重跑）时不要求正文可读：不会读取快照，
			// 而正文可能已经过了保留期。
			if p.docs != nil && anyNewTask(b) {
				if err := p.docs.RequireReadableTx(ctx, tx, ev.Repo.ID, basis); err != nil {
					return err
				}
			}
		}
		var workItemCol, setCol *int64
		if workItem != 0 {
			workItemCol = &workItem
		}
		if setID != 0 {
			setCol = &setID
		}
		_, err := tx.Exec(ctx, `INSERT INTO ai_review_requests (request_id, pr_number, head_ref, base_ref, base_sha, config_sha, policy_digest,
			work_item_id, reused_from, doc_snapshot_set_id, docs_source, docs_manifest_digest)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			id, pr, ev.HeadRef, ev.BaseRef, ev.BaseSHA, ev.SHA, digest, workItemCol, reused, setCol, source, manifestDigest)
		return err
	}
}

// anyNewTask 返回是否有需要在本次真正执行（不是沿用结论）的任务。
func anyNewTask(b *built) bool {
	if b == nil {
		return false
	}
	return slices.ContainsFunc(b.tasks, func(t ai.NewTask) bool { return t.ReuseFrom == nil })
}

// readConfig 读取并解析 commit 中的 .kiln/agent/config.yml（blob 为 cfgBlob）。配置有错误时 cfg 为 nil，错误在 cfgErr 中。
func (p *Reviews) readConfig(ctx context.Context, inst *gh.Inst, repo *store.Repo, cfgBlob string) (cfg *agentcfg.Config, cfgErr string, err error) {
	data, err := p.git.Blob(ctx, inst, repo, cfgBlob)
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s：%w", agentcfg.ConfigPath, err)
	}
	cfg, perr := agentcfg.Parse(data)
	if perr != nil {
		return nil, perr.Error(), nil //nolint:nilerr // 配置错误是规划的结果（失败的请求），不是读取的错误
	}
	return cfg, "", nil
}

// tools 按被审 commit 上的 .kiln/runtimes.lock 解析各审计任务的工具链（requirements.tools，PLAN #33.12）：平台 id →
// Runner 平台 → 工具，一组平台（any_of）时组内每个平台各一份。没有声明工具链时为 nil。没有 lockfile、lockfile 无效、
// 缺少条目或任何一个平台都是配置错误（cfgErr，请求直接失败并说明是哪个平台），不换版本、不取最新；只有读取失败时返回 err。
func (p *Reviews) tools(ctx context.Context, inst *gh.Inst, repo *store.Repo, tree *gh.Tree, cfg *agentcfg.Config) (map[string]map[string][]aispec.ToolRef, string, error) {
	names := cfg.Tools()
	if len(names) == 0 {
		return nil, "", nil
	}
	var lock *toolchain.Lock
	if blob := gitread.FindBlob(tree, toolchain.LockPath); blob != "" {
		data, err := p.git.Blob(ctx, inst, repo, blob)
		if err != nil {
			return nil, "", fmt.Errorf("读取 %s：%w", toolchain.LockPath, err)
		}
		if lock, err = toolchain.ParseLock(data); err != nil {
			return nil, "requirements.tools：" + err.Error(), nil //nolint:nilerr // 配置错误是给人看的原因，不是处理失败
		}
	}
	out := map[string]map[string][]aispec.ToolRef{}
	for _, plat := range cfg.Review.RequiredPlatforms {
		byPlatform, err := aispec.ResolvePlatformTools(lock, names, plat.RunnerPlatforms())
		if err != nil {
			return nil, err.Error(), nil //nolint:nilerr // 同上
		}
		out[plat.ID] = byPlatform
	}
	return out, "", nil
}

// built 是按配置规划出的审计任务。
type built struct {
	tasks    []ai.NewTask
	warnings string
	digest   string
	workItem int64
	// 文档依据：来源、摘要、独立审计的待提交快照集、沿用快照集的编号。
	docsSource         string
	docsManifestDigest string
	prepared           *docsync.PreparedFreeze
	snapshotSetID      int64
}

// build 按配置为 ev 规划审计任务（review.required_platforms 的每一项一个，一组平台时组内任一平台的节点都可以领取）。
// 新建和重跑共用，触发方式由 ev.Event 决定。tools 是各任务的工具链（见 Reviews.tools），wi 是 PR 关联的工作项，
// basis 是本次固定的文档依据（重跑时来自请求族根，绝不按当前 PR 重新选择）。
func (p *Reviews) build(ctx context.Context, inst *gh.Inst, ev *Event, tree *gh.Tree, cfg *agentcfg.Config, tools map[string]map[string][]aispec.ToolRef, wi *prWorkItem, basis docBasis) (*built, error) {
	rules, err := p.baseRules(ctx, inst, ev, tree, cfg)
	if err != nil {
		return nil, err
	}
	instructions := rules.instructions
	trigger := aispec.TriggerPullRequest
	if ev.manual() {
		trigger = aispec.TriggerManual
	}
	var prior []aispec.PriorFinding
	var phase, phaseNote, designSHA string
	var designPR int
	if wi != nil {
		if phase, phaseNote, err = p.reviewPhase(ctx, inst, ev, wi); err != nil {
			return nil, err
		}
		// DesignPR、DesignSHA 只表示已合并的设计（实现 PR 按它核对）；被审的是设计 PR 本身时为空
		prior, designPR, designSHA = wi.prior, wi.mergedPR, wi.mergedSHA
	}
	agent := reviewAgent(cfg.ReviewAgents(), wi)
	b := &built{
		warnings: rules.warning, digest: policyDigest(agent, instructions, phase, basis.ManifestDigest),
		workItem: basis.WorkItemID, docsSource: basis.Source, docsManifestDigest: basis.ManifestDigest,
		prepared: basis.Prepared, snapshotSetID: basis.SnapshotSetID,
	}
	var docs []aispec.DocRef
	for _, d := range basis.Docs {
		docs = append(docs, aispec.DocRef{ID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	for _, plat := range cfg.Review.RequiredPlatforms {
		b.tasks = append(b.tasks, ai.NewTask{
			Key: "review-" + plat.ID, Platforms: plat.RunnerPlatforms(), Agent: agent,
			DisplayName: fmt.Sprintf("%s · %s", agent, plat.ID), TimeoutMinutes: int(cfg.ReviewTimeout().Minutes()),
			Spec: &aispec.ReviewSpec{
				Agent: agent, Trigger: trigger, PRNumber: ev.PRNumber, BaseRef: ev.BaseRef, BaseSHA: ev.BaseSHA, HeadRef: ev.HeadRef,
				Instructions: instructions, Services: cfg.Services(), PlatformTools: tools[plat.ID], Repositories: rules.repositories, PlatformID: plat.ID,
				ReviewComments: ev.PRNumber != 0 && cfg.PublishReviewComments(), MaxConcurrency: cfg.Review.MaxConcurrency,
				ConfigSHA: ev.SHA, InstructionsFrom: rules.from, PolicyDigest: b.digest, WorkItemID: b.workItem, PriorFindings: prior,
				Phase: phase, DesignPR: designPR, DesignSHA: designSHA, PhaseNote: phaseNote, Docs: docs,
				DocsSource: basis.Source, DocSnapshotSetID: basis.SnapshotSetID, DocsManifestDigest: basis.ManifestDigest,
			},
		})
	}
	// 仓库记录还缺 clone 地址时先补齐（AI 执行节点按它找到仓库）
	return b, repodir.Complete(ctx, p.db, p.gh, ev.Repo)
}

// rules 是审计从 base 读取的规则（PLAN #29.6、#33.13）：审计说明（内容、警告、读取的 commit）和依赖仓库
// （requirements.repositories，决定读凭据覆盖哪些仓库）。
type rules struct {
	instructions, warning, from string
	repositories                []string
}

// baseRules 读取审计说明和依赖仓库。
//
// PR 审计从 PR 的 base commit 读取（PLAN #29.6）：base 中的 .kiln/agent/config.yml 决定说明文件的路径
// （review.instructions，默认 .kiln/agent/review.md）和依赖仓库，说明的内容也从 base 读取——PR 改不了审计自己的说明，否则
// 可以让审计总是给出 no_issues，再经自动入队、自动打 tag 一路发布；也不能靠改配置给审计加读权限（读凭据覆盖的仓库，
// #33.13）。base 中没有 AI 配置（例如开启审计的那个 PR）或配置有错误时不带仓库的审计说明、没有依赖仓库。手动审计没有 base，
// 按被审的 commit（tree、cfg）读取。显式指定的说明文件不存在时返回警告（不阻止审计）。
func (p *Reviews) baseRules(ctx context.Context, inst *gh.Inst, ev *Event, tree *gh.Tree, cfg *agentcfg.Config) (rules, error) {
	out := rules{from: ev.SHA}
	if ev.PRNumber != 0 {
		if ev.BaseSHA == "" {
			return rules{warning: "PR 的 base commit 未知，没有使用仓库的审计说明"}, nil
		}
		out.from = ev.BaseSHA
		var err error
		tree, err = p.git.Tree(ctx, inst, ev.Repo, ev.BaseSHA)
		if gh.IsNotFound(err) {
			// base commit 已经不在了（例如目标分支被强制推送后清理）：不退回 head 的说明（PR 可以改它）
			return rules{warning: fmt.Sprintf("PR 的 base %s 不存在，没有使用仓库的审计说明", shorten.Hash(ev.BaseSHA))}, nil
		}
		if err != nil {
			return rules{}, fmt.Errorf("读取 base %s 的 git tree：%w", shorten.Hash(ev.BaseSHA), err)
		}
		cfgBlob := gitread.FindBlob(tree, agentcfg.ConfigPath)
		if cfgBlob == "" {
			return out, nil
		}
		baseCfg, cfgErr, err := p.readConfig(ctx, inst, ev.Repo, cfgBlob)
		if err != nil {
			return rules{}, err
		}
		if cfgErr != "" {
			out.warning = fmt.Sprintf("base %s 的 %s 有错误，没有使用仓库的审计说明", shorten.Hash(ev.BaseSHA), agentcfg.ConfigPath)
			return out, nil
		}
		cfg = baseCfg
	}
	out.repositories = cfg.Repositories()
	path := cfg.InstructionsPath()
	blob := gitread.FindBlob(tree, path)
	if blob == "" {
		if cfg.Review.Instructions != "" {
			out.warning = fmt.Sprintf("review.instructions 指定的 %s 不存在（在 %s 中）", path, shorten.Hash(out.from))
		}
		return out, nil
	}
	data, err := p.git.Blob(ctx, inst, ev.Repo, blob)
	if err != nil {
		return rules{}, fmt.Errorf("读取 %s：%w", path, err)
	}
	if len(data) > agentcfg.MaxInstructionsSize {
		// 按字符边界截断：审计说明多为中文，切开的字符会变成乱码
		out.instructions = shorten.Prefix(string(data), agentcfg.MaxInstructionsSize)
		out.warning = fmt.Sprintf("%s 超过 %d KiB，只使用前 %d KiB", path, agentcfg.MaxInstructionsSize>>10, agentcfg.MaxInstructionsSize>>10)
		return out, nil
	}
	out.instructions = string(data)
	return out, nil
}

// reviewAgent 按 PLAN #33.7 选审计这个 PR 的 Agent：配置中的第一个没有开发过这个工作项的 Agent。都被开发用过时仍然用
// 第一个配置的 Agent（任务等待另一个审计 Agent，原因由任务类型的 Mismatch 给出，不偷偷换成开发过的 Agent 自审）。
func reviewAgent(agents []string, wi *prWorkItem) string {
	if len(agents) == 0 {
		return agentcfg.AgentCodex
	}
	for _, a := range agents {
		if wi == nil || !slices.Contains(wi.devAgents, a) {
			return a
		}
	}
	return agents[0]
}

// policyDigest 是审计规则的摘要（PLAN #29.6）：agent、审计说明、报告版本和文档依据。合并队列更新分支后沿用旧审计时
// 要求相同，规则或文档依据变了就重新审计。版本 2 起纳入文档依据摘要（工作项审计与独立审计都计入），因此迁移前的
// v1 审计不会被误认为可以沿用。
func policyDigest(agent, instructions, phase, docsManifestDigest string) string {
	b, _ := json.Marshal(struct { //nolint:errchkjson // 只有字符串和整数，不会失败
		V            int    `json:"v"`
		Agent        string `json:"agent"`
		Instructions string `json:"instructions"`
		Report       int    `json:"report"`
		// Phase 是工作项 PR 的审计方式（设计审计与实现审计的结论不互相沿用）；不是工作项的 PR 时为空。
		Phase string `json:"phase,omitempty"`
		// DocsDigest 是本次审计的文档依据摘要：文档、来源或绑定代次变化后结论不再沿用。
		DocsDigest string `json:"docs_digest,omitempty"`
	}{2, agent, instructions, aireport.Version, phase, docsManifestDigest})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// reviewPhase 读取被审 head 相对 PR base 的完整文件列表，返回工作项的 PR 的审计方式（PLAN #31.5、I12）。只有设计 PR
// 需要读取；读取失败时返回错误（规划重试），不按设计审计。
func (p *Reviews) reviewPhase(ctx context.Context, inst *gh.Inst, ev *Event, w *prWorkItem) (phase, note string, err error) {
	if !w.designPR {
		phase, note = reviewPhase(w, nil, true)
		return phase, note, nil
	}
	base := ev.BaseSHA
	if base == "" {
		base = ev.BaseRef
	}
	cmp, err := inst.CompareDetail(ctx, ev.Repo.FullName(), base, ev.SHA)
	if err != nil {
		return "", "", fmt.Errorf("读取设计 PR #%d 的改动文件：%w", ev.PRNumber, err)
	}
	phase, note = reviewPhase(w, cmp.Files, cmp.FilesComplete)
	return phase, note, nil
}

// TriggerRequest 是一次手动触发的 AI 审计（CLI 和 Dashboard 共用）。
type TriggerRequest struct {
	Repo string // owner/name
	Ref  string // 分支、tag、完整的 ref 或 commit sha；空表示默认分支
	// RequestID 用于幂等：同一个 RequestID 只建一个请求。空时自动生成。
	RequestID string
	Actor     string
	Source    string
}

// Trigger 手动触发一次 AI 审计，返回请求 ID 和被审计的 commit。
func (p *Reviews) Trigger(ctx context.Context, req *TriggerRequest) (int64, string, error) {
	if p.gh == nil {
		return 0, "", ErrNoGitHubApp
	}
	if req.RequestID == "" {
		req.RequestID = randtoken.New(16)
	}
	ev := &Event{Event: eventManual, RequestID: req.RequestID}
	if r, err := ai.RequestByDedupeKey(ctx, p.db, dedupeKey(ev)); err != nil || r != nil {
		if r == nil {
			return 0, "", err
		}
		return r.ID, r.SHA, nil
	}
	repo, err := repodir.Find(ctx, p.db, p.gh, req.Repo)
	if err != nil {
		return 0, "", err
	}
	ref, sha, err := gitread.ResolveRefOrCommit(ctx, p.gh.Inst(repo.InstallationID), repo, req.Ref)
	if gitread.IsRefError(err) {
		return 0, "", &TriggerError{Field: "ref", Err: err}
	}
	if err != nil {
		return 0, "", err
	}
	ev.Repo, ev.Ref, ev.SHA, ev.Sender, ev.Source = repo, ref, sha, req.Actor, req.Source
	planned, err := p.plan(ctx, ev)
	if err != nil {
		return 0, "", err
	}
	id := planned.Request.ID
	p.audit(ctx, req.Actor, "ai_review.trigger", map[string]any{"ai_request_id": id, "repo": repo.FullName(), "ref": ref, "sha": sha})
	return id, planned.Request.SHA, nil
}

// audit 写一条审计日志。它使用与规划相同的、受服务生命周期追踪且有限预算的服务端 context：
// 调用方（HTTP / CLI / webhook）已经取消时不能用一个没有预算的 context 继续写库，服务退出时也不能在数据库关闭之后写。
// 日志写入失败只记警告，不改变已经完成的业务结果。
func (p *Reviews) audit(ctx context.Context, actor, action string, detail map[string]any) {
	actx, cancel, err := p.opContext(ctx)
	if err != nil {
		p.log.Warn("记录审计日志失败", "action", action, "err", err)
		return
	}
	err = store.Audit(actx, p.db, actor, action, detail)
	cancel()
	if err != nil {
		p.log.Warn("记录审计日志失败", "action", action, "err", err)
	}
}

// CancelPR 取消 PR 的未结束审计请求（PR 关闭）。
func (p *Reviews) CancelPR(ctx context.Context, repoID int64, pr int) (int, error) {
	ids, err := store.QueryIDs(ctx, p.db, `SELECT r.id FROM ai_requests r JOIN ai_review_requests d ON d.request_id=r.id
		WHERE r.repo_id=$1 AND d.pr_number=$2 AND r.state IN ('queued','active') ORDER BY r.id`, repoID, pr)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := p.core.Cancel(ctx, id, ai.CancelPRClosed, ""); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
