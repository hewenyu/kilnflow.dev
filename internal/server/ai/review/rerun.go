package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 重跑（PLAN #30.6）：都建新请求（新的 Check Run），rerun_of 指向被重跑的请求，family_id 指向首次请求，按任务 key 对应。
// 重跑旧 head 不取代当前 head 的审计。GitHub 触发的重跑按 webhook delivery 幂等：同一个 delivery 对同一个请求族至多重跑一次。
//
// 文档依据固定在请求族根：重跑绝不按当前 PR / 分支重新选择依据。族根是 snapshot_set / work_item 时直接复用，
// none 保持 none（即使仓库后来绑定了 Outline、工作项后来才冻结）；unplanned 要求发起新的审计；legacy 在能核对出唯一依据时
// 一次性固化到族根。

// RerunMode 是重跑的方式。
type RerunMode string

const (
	// RerunAll 重新执行全部任务。
	RerunAll RerunMode = "all"
	// RerunFailed 只重新执行没有成功的任务，成功的沿用。
	RerunFailed RerunMode = "failed"
	// RerunTask 只重新执行一个任务，其余沿用（Check Run 上的 Re-run）。
	RerunTask RerunMode = "task"
)

// RerunRequest 是一次重跑。
type RerunRequest struct {
	RequestID int64
	Mode      RerunMode
	// TaskID 是 Mode 为 task 时要重跑的任务（属于 RequestID）。
	TaskID int64
	Actor  string
	Source string
	// DeliveryID 是触发重跑的 webhook delivery（GitHub 上的 Re-run）；Dashboard、CLI 的重跑为空。
	DeliveryID string
}

// ErrNotFinished 表示请求还没有结束，不能重跑。
var ErrNotFinished = errors.New("AI 请求还没有结束，不能重跑")

// ErrLegacyDocsBasis 表示被重跑的请求属于迁移前的请求族（没有记录文档依据，也无法唯一核对出来）：需要发起新的审计。
var ErrLegacyDocsBasis = errors.New("这次审计属于迁移前的请求，没有记录文档依据：请发起一次新的审计")

// ErrUnplannedDocsBasis 表示请求族根是配置错误、没有进入冻结的请求：需要发起一次新的审计。
var ErrUnplannedDocsBasis = errors.New("这次审计首次因配置错误没有冻结文档：请发起一次新的审计")

// Rerun 重跑一个已结束的审计请求，返回新的请求（同一个 delivery 已经重跑过时返回那次的请求）。
func (p *Reviews) Rerun(ctx context.Context, req RerunRequest) (*ai.Request, error) {
	if p.gh == nil {
		return nil, ErrNoGitHubApp
	}
	prev, err := ai.GetRequest(ctx, p.db, req.RequestID, false)
	if err != nil {
		return nil, err
	}
	if prev.Type != agentcfg.TaskReview {
		return nil, ai.ErrNotRerunnable
	}
	// 先于读取 GitHub 和"还没有结束"的检查：同一个 delivery 重试时不把已处理的请求当作失败
	if req.DeliveryID != "" {
		if r, err := ai.RerunByDelivery(ctx, p.db, prev.FamilyID, req.DeliveryID); err != nil || r != nil {
			return r, err
		}
	}
	if !prev.State.Terminal() {
		return nil, fmt.Errorf("AI 审计请求 #%d：%w", prev.ID, ErrNotFinished)
	}
	tasks, err := ai.RequestTasks(ctx, p.db, prev.ID, false)
	if err != nil {
		return nil, err
	}
	if req.Mode == RerunTask && !hasTask(tasks, req.TaskID) {
		return nil, fmt.Errorf("任务 #%d 不属于 AI 审计请求 #%d", req.TaskID, prev.ID)
	}
	ev, err := p.eventOf(ctx, prev, req)
	if err != nil {
		return nil, err
	}
	nr := p.newRequest(ev, "")
	nr.RerunOf, nr.DeliveryID = prev, req.DeliveryID
	inst := p.gh.Inst(ev.Repo.InstallationID)
	tree, err := p.git.Tree(ctx, inst, ev.Repo, prev.SHA)
	if err != nil {
		return nil, fmt.Errorf("读取 git tree：%w", err)
	}
	cfgBlob := gitread.FindBlob(tree, agentcfg.ConfigPath)
	if cfgBlob == "" {
		return nil, fmt.Errorf("commit %s 中没有 %s，无法重跑 AI 审计", prev.SHA, agentcfg.ConfigPath)
	}
	cfg, cfgErr, err := p.readConfig(ctx, inst, ev.Repo, cfgBlob)
	switch {
	case err != nil:
		return nil, err
	case cfgErr != "":
		// 配置错误（没有任务）：保留族根已有的 canonical 文档依据引用，不退回 unplanned。
		fixed, err := p.resolveFamily(ctx, ev.Repo.ID, prev.FamilyID, false)
		if err != nil {
			return nil, err
		}
		nr.ConfigError, nr.Detail = cfgErr, p.detail(ev, nil, nil, &fixed)
	case !cfg.ReviewEnabled():
		return nil, fmt.Errorf("commit %s 的 %s 关闭了审计，无法重跑 AI 审计", prev.SHA, agentcfg.ConfigPath)
	default:
		tools, toolsErr, err := p.tools(ctx, inst, ev.Repo, tree, cfg)
		if err != nil {
			return nil, err
		}
		if toolsErr != "" {
			fixed, err := p.resolveFamily(ctx, ev.Repo.ID, prev.FamilyID, false)
			if err != nil {
				return nil, err
			}
			nr.ConfigError, nr.Detail = toolsErr, p.detail(ev, nil, nil, &fixed)
			break
		}
		basis, err := p.resolveFamily(ctx, ev.Repo.ID, prev.FamilyID, true)
		if err != nil {
			return nil, err
		}
		// 工作项只提供上一轮问题、审计方式等与重跑有关的信息；文档依据和 work_item_id 固定用族根的（见 resolveFamily）。
		wi, err := workItemReview(ctx, p.db, ev.Repo.ID, ev.PRNumber, ev.HeadRef)
		if err != nil {
			return nil, err
		}
		b, err := p.build(ctx, inst, ev, tree, cfg, tools, wi, basis)
		if err != nil {
			return nil, err
		}
		nr.Warnings, nr.Tasks, nr.Detail = b.warnings, b.tasks, p.detail(ev, b, nil, nil)
		nr.Warnings = joinWarnings(nr.Warnings, reuse(nr.Tasks, tasks, req))
	}
	planned, err := p.core.Plan(ctx, nr)
	if err != nil {
		return nil, err
	}
	if planned.Created {
		p.audit(ctx, req.Actor, "ai_request.rerun", map[string]any{
			"ai_request_id": prev.ID, "rerun": planned.Request.ID, "mode": string(req.Mode), "task_id": req.TaskID,
		})
	}
	return planned.Request, nil
}

func hasTask(tasks []*ai.Task, id int64) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// reuse 按重跑方式把新任务改为沿用上一次的结果：只重跑失败的沿用成功的任务；重跑单个任务沿用其余的任务（不论结果）。
//
// 沿用还要求任务可以执行的平台相同、审计规则摘要（含文档依据）相同：平台或规则变了就必须重新执行，绝不仍写"沿用上一次
// 的结果"。这样的任务返回一条说明（写进请求的 warnings），让重新执行的原因看得见。
func reuse(tasks []ai.NewTask, prev []*ai.Task, req RerunRequest) []string {
	byKey := make(map[string]*ai.Task, len(prev))
	for _, t := range prev {
		byKey[t.Key] = t
	}
	var warnings []string
	for i := range tasks {
		pt, ok := byKey[tasks[i].Key]
		if !ok || !pt.State.Terminal() {
			continue
		}
		switch {
		case req.Mode == RerunFailed && pt.State == ai.TaskSucceeded,
			req.Mode == RerunTask && pt.ID != req.TaskID:
		default:
			continue
		}
		cur, _ := tasks[i].Spec.(*aispec.ReviewSpec)
		var old aispec.ReviewSpec
		switch {
		case cur == nil || pt.DecodeSpec(&old) != nil || old.PolicyDigest == "" || old.PolicyDigest != cur.PolicyDigest:
			warnings = append(warnings, fmt.Sprintf("任务 %s 的审计规则或文档依据与上一次不同，重新执行（不沿用上一次的结果）", tasks[i].Key))
		case !samePlatforms(pt.Platforms, tasks[i].Platforms):
			warnings = append(warnings, fmt.Sprintf("任务 %s 可以执行的平台与上一次不同，重新执行（不沿用上一次的结果）", tasks[i].Key))
		default:
			tasks[i].ReuseFrom = pt
			tasks[i].ReuseReason = fmt.Sprintf("沿用上一次的结果（任务 #%d）", ai.ReuseSource(pt))
		}
	}
	return warnings
}

// joinWarnings 把说明追加到已有的 warnings（空的部分不留下多余空行）。
func joinWarnings(base string, extra []string) string {
	parts := make([]string, 0, len(extra)+1)
	if strings.TrimSpace(base) != "" {
		parts = append(parts, base)
	}
	for _, w := range extra {
		if strings.TrimSpace(w) != "" {
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, "\n")
}

// eventOf 按被重跑的请求和它的明细还原审计的上下文（PR、分支、base）。
func (p *Reviews) eventOf(ctx context.Context, prev *ai.Request, req RerunRequest) (*Event, error) {
	repo, err := store.GetRepo(ctx, p.db, prev.RepoID)
	if err != nil {
		return nil, err
	}
	ev := &Event{Repo: repo, Event: prev.Event, Ref: prev.Ref, SHA: prev.SHA, Sender: req.Actor, Source: req.Source}
	var pr *int
	err = p.db.QueryRow(ctx, `SELECT pr_number, head_ref, base_ref, base_sha FROM ai_review_requests WHERE request_id=$1`, prev.ID).
		Scan(&pr, &ev.HeadRef, &ev.BaseRef, &ev.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("读取 AI 审计请求 #%d 的明细：%w", prev.ID, err)
	}
	if pr != nil {
		ev.PRNumber = *pr
	}
	return ev, nil
}

// ---------- 请求族固定的文档依据 ----------

// familyRootDocs 是请求族根记录的文档依据（还没有解析成可执行的 Basis）。
type familyRootDocs struct {
	source         string
	workItemID     int64
	snapshotSetID  int64
	manifestDigest string
}

// resolveFamily 返回请求族根固定的文档依据。buildTasks 表示这次重跑会建任务（配置和工具链都正常）：这时 unplanned 需要
// 发起新审计，legacy 要尝试一次性固化；不会建任务时（配置错误）保留族根已有的 canonical 引用即可。
func (p *Reviews) resolveFamily(ctx context.Context, repoID, familyID int64, buildTasks bool) (docBasis, error) {
	root, err := p.familyRoot(ctx, p.db, familyID)
	if err != nil {
		return docBasis{}, err
	}
	return p.resolveRoot(ctx, p.db, repoID, familyID, root, buildTasks)
}

// resolveRoot 按族根的文档依据解析出本次重跑使用的 Basis；legacy 交给 legacyFamily（会开启自己的事务与族锁）。
func (p *Reviews) resolveRoot(ctx context.Context, q store.Querier, repoID, familyID int64, root familyRootDocs, buildTasks bool) (docBasis, error) {
	switch root.source {
	case docsync.SourceSnapshotSet:
		if p.docs == nil {
			return docBasis{}, errors.New("没有配置文档服务，无法读取审计的文档快照")
		}
		basis := docsync.Basis{Source: docsync.SourceSnapshotSet, RepoID: repoID, SnapshotSetID: root.snapshotSetID}
		docs, err := p.docs.ListBasis(ctx, q, basis)
		if err != nil {
			return docBasis{}, err
		}
		return docBasis{Source: root.source, SnapshotSetID: root.snapshotSetID, ManifestDigest: root.manifestDigest, Docs: docs}, nil
	case docsync.SourceWorkItem:
		if p.docs == nil {
			return docBasis{}, errors.New("没有配置文档服务，无法读取审计的文档快照")
		}
		b, err := p.docs.WorkItemBasis(ctx, q, repoID, root.workItemID)
		if err != nil {
			return docBasis{}, err
		}
		return docBasis{Source: root.source, WorkItemID: root.workItemID, ManifestDigest: root.manifestDigest, Docs: b.Docs}, nil
	case docsync.SourceNone:
		digest := root.manifestDigest
		if digest == "" {
			digest = docsync.NoneManifestDigest()
		}
		return docBasis{Source: root.source, WorkItemID: root.workItemID, ManifestDigest: digest}, nil
	case docsync.SourceUnplanned:
		if buildTasks {
			return docBasis{}, ErrUnplannedDocsBasis
		}
		return docBasis{Source: root.source}, nil
	case docsync.SourceLegacy:
		return p.legacyFamily(ctx, repoID, familyID, root, buildTasks)
	}
	return docBasis{}, fmt.Errorf("未知的文档依据来源 %q", root.source)
}

// familyRoot 读取请求族根的文档依据。
func (p *Reviews) familyRoot(ctx context.Context, q store.Querier, familyID int64) (familyRootDocs, error) {
	var (
		root familyRootDocs
		wi   *int64
		set  *int64
	)
	err := q.QueryRow(ctx, `SELECT docs_source, work_item_id, doc_snapshot_set_id, docs_manifest_digest
		FROM ai_review_requests WHERE request_id=$1`, familyID).Scan(&root.source, &wi, &set, &root.manifestDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return root, fmt.Errorf("审计请求族根 #%d 没有明细：%w", familyID, ErrLegacyDocsBasis)
	}
	if err != nil {
		return root, err
	}
	root.workItemID, root.snapshotSetID = deref64(wi), deref64(set)
	return root, nil
}

// legacyFamily 处理迁移前的请求族：族根没有记录文档依据。只会按族内已有任务的 spec 唯一核对出一种依据并一次性固化到
// 族根（不改原任务 spec / verdict / 来源列），核对不出时返回 ErrLegacyDocsBasis。不会把后来才出现的非空快照塞进旧家族。
func (p *Reviews) legacyFamily(ctx context.Context, repoID, familyID int64, root familyRootDocs, buildTasks bool) (docBasis, error) {
	if !buildTasks {
		// 配置错误的重跑：沿用族根的 canonical 值（legacy、空摘要），不尝试固化。
		return docBasis{Source: docsync.SourceLegacy, WorkItemID: root.workItemID}, nil
	}
	if root.workItemID == 0 {
		return docBasis{}, ErrLegacyDocsBasis
	}
	var out docBasis
	err := p.db.Tx(ctx, func(tx pgx.Tx) error {
		// 同一个请求族的重跑串行：并发重跑只有一个做固化，其余读到固化后的值。
		if err := store.LockXact(ctx, tx, familyLockKey(familyID)); err != nil {
			return err
		}
		// 拿锁后重新读族根：可能已经被并发的重跑固化。
		cur, err := p.familyRoot(ctx, tx, familyID)
		if err != nil {
			return err
		}
		if cur.source != docsync.SourceLegacy {
			out, err = p.resolveRoot(ctx, tx, repoID, familyID, cur, true)
			return err
		}
		refs, err := familyDocs(ctx, tx, familyID)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			// 原任务没有文档依据：固化 none，不把后来才出现的快照塞进旧家族。
			if _, err := tx.Exec(ctx, `UPDATE ai_review_requests SET docs_source='none', docs_manifest_digest=$2
				WHERE request_id=$1 AND docs_source='legacy'`, familyID, docsync.NoneManifestDigest()); err != nil {
				return err
			}
			out = docBasis{Source: docsync.SourceNone, WorkItemID: root.workItemID, ManifestDigest: docsync.NoneManifestDigest()}
			return nil
		}
		if p.docs == nil {
			return ErrLegacyDocsBasis
		}
		basis, err := p.docs.WorkItemBasis(ctx, tx, repoID, root.workItemID)
		if err != nil {
			return err
		}
		if basis.Source != docsync.SourceWorkItem || !docsync.SameDocRefs(refs, basis.Docs) {
			return ErrLegacyDocsBasis
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_review_requests SET docs_source='work_item', docs_manifest_digest=$2
			WHERE request_id=$1 AND docs_source='legacy'`, familyID, basis.ManifestDigest); err != nil {
			return err
		}
		out = docBasis{Source: docsync.SourceWorkItem, WorkItemID: root.workItemID, ManifestDigest: basis.ManifestDigest, Docs: basis.Docs}
		return nil
	})
	if err != nil {
		return docBasis{}, err
	}
	return out, nil
}

// familyDocs 返回请求族中所有已有任务的文档引用；族内记录互相矛盾、没有任务或 spec 解析失败时返回 ErrLegacyDocsBasis。
func familyDocs(ctx context.Context, q store.Querier, familyID int64) ([]aispec.DocRef, error) {
	rows, err := q.Query(ctx, `SELECT t.spec FROM ai_tasks t JOIN ai_requests r ON r.id=t.request_id
		WHERE r.family_id=$1 ORDER BY t.id`, familyID)
	if err != nil {
		return nil, err
	}
	specs, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, ErrLegacyDocsBasis
	}
	var refs []aispec.DocRef
	key := ""
	for _, raw := range specs {
		var spec aispec.ReviewSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, ErrLegacyDocsBasis
		}
		k := docRefsKey(spec.Docs)
		if key == "" {
			key, refs = k, spec.Docs
			continue
		}
		if k != key {
			return nil, ErrLegacyDocsBasis
		}
	}
	return refs, nil
}

// docRefsKey 返回一组文档引用的规范化表示（按 doc_id 排序），用于判断族内记录是否一致。
func docRefsKey(refs []aispec.DocRef) string {
	list := append([]aispec.DocRef(nil), refs...)
	slices.SortFunc(list, func(a, b aispec.DocRef) int { return strings.Compare(a.ID, b.ID) })
	b, _ := json.Marshal(list) //nolint:errchkjson // 只有字符串和整数，不会失败
	return string(b)
}

// familyLockKey 是按请求族串行化 legacy 依据固化的 advisory lock 键。
func familyLockKey(familyID int64) string { return fmt.Sprintf("kiln:ai-review-family:%d", familyID) }

func deref64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// RerunHead 处理 Check Suite 上的 "Re-run all checks"：重跑 commit sha 上已结束的审计（每个请求族最新的一次，只重跑失败的）。
// 返回重跑的请求数。限流时返回错误（事件稍后重试，已经由这个 delivery 重跑过的按 delivery 跳过）。
func (p *Reviews) RerunHead(ctx context.Context, repoID int64, sha, actor, delivery string) (int, error) {
	ids, err := store.QueryIDs(ctx, p.db, `SELECT DISTINCT ON (family_id) id FROM ai_requests
		WHERE type=$1 AND repo_id=$2 AND sha=$3 ORDER BY family_id, id DESC`, agentcfg.TaskReview, repoID, sha)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		r, err := ai.GetRequest(ctx, p.db, id, false)
		if err != nil {
			return n, err
		}
		if !r.State.Terminal() || r.State == ai.RequestSkipped {
			continue
		}
		if _, err := p.Rerun(ctx, RerunRequest{RequestID: id, Mode: RerunFailed, Actor: actor, Source: store.SourceWebhook, DeliveryID: delivery}); err != nil {
			if _, ok := gh.RateLimited(err); ok {
				return n, err
			}
			p.log.Warn("重跑 AI 审计", "request", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// Rerequest 处理 Check Run 上的 "Re-run"：任务的 Check Run 重跑这个任务，请求的 Check Run 重跑没有成功的任务。target 来自
// ai.ResolveExternalID；对象必须属于事件的仓库 repoID，否则返回 found=false（不让一个仓库的事件重跑另一个仓库的审计）。
func (p *Reviews) Rerequest(ctx context.Context, repoID int64, target ai.CheckTarget, actor, delivery string) (r *ai.Request, found bool, err error) {
	req := RerunRequest{RequestID: target.RequestID, Mode: RerunFailed, Actor: actor, Source: store.SourceWebhook, DeliveryID: delivery}
	if target.TaskID != 0 {
		t, err := ai.GetTask(ctx, p.db, target.TaskID, false)
		if errors.Is(err, ai.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		req.RequestID, req.Mode, req.TaskID = t.RequestID, RerunTask, t.ID
	}
	prev, err := ai.GetRequest(ctx, p.db, req.RequestID, false)
	if errors.Is(err, ai.ErrNotFound) || (err == nil && prev.RepoID != repoID) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r, err = p.Rerun(ctx, req)
	return r, true, err
}
