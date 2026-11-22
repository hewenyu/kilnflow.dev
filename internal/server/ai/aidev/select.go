package aidev

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 不开始选题的原因（PlanSelect 返回，记进日志）。
const (
	whyNoForge      = "没有配置 GitHub App"
	whyRepoInactive = "仓库已停用"
	whyPaused       = "仓库已暂停 AI 开发（kiln ai dev resume 恢复）"
	whyNoCandidates = "任务池中没有可选的 Issue（打开、有 " + LabelReady + "、没有 " + LabelSkip + "、不在进行中的工作项中，并且没有因为不适合、无需改代码、被终止而跳过）"
	whyExamined     = "任务池在上次选题之后没有变化"
	whyNoFreeSlot   = "开发机没有空闲槽位（在线、这个仓库的开发 Agent 可用、平台在 development.platform 中的开发角色 AI 执行节点，" +
		"执行中的任务没有到并发上限，并且排队中的同一个 Agent 的开发任务占用它们能执行的平台上的槽位之后仍有空闲）"
)

// devConfig 是从默认分支读到的开发配置。
type devConfig struct {
	head         string
	cfg          *agentcfg.Config
	instructions string
}

// loadDevConfig 读取默认分支 head 的 .kiln/agent/config.yml 和开发说明（PLAN #26.7：只从默认分支读取）。
// 没有启用开发时 why 说明原因。
func loadDevConfig(ctx context.Context, f forge.Repo, repo *store.Repo) (dc *devConfig, why string, err error) {
	head, err := f.BranchHead(ctx, repo.DefaultBranch)
	if err != nil {
		return nil, "", fmt.Errorf("读取默认分支 %s：%w", repo.DefaultBranch, err)
	}
	if head == "" {
		return nil, "默认分支 " + repo.DefaultBranch + " 不存在", nil
	}
	data, err := f.ReadFile(ctx, head, agentcfg.ConfigPath)
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s：%w", agentcfg.ConfigPath, err)
	}
	if data == nil {
		return nil, "默认分支没有 " + agentcfg.ConfigPath, nil
	}
	cfg, perr := agentcfg.Parse(data)
	switch {
	case perr != nil:
		// 配置错误是给人看的原因，不是处理失败
		return nil, "默认分支的 " + agentcfg.ConfigPath + " 有错误：" + perr.Error(), nil //nolint:nilerr // 见上
	case !cfg.DevelopmentEnabled():
		return nil, "默认分支的 " + agentcfg.ConfigPath + " 没有启用 development", nil
	}
	dc = &devConfig{head: head, cfg: cfg}
	ins, err := f.ReadFile(ctx, head, cfg.DevInstructionsPath())
	if err != nil {
		return nil, "", fmt.Errorf("读取开发说明 %s：%w", cfg.DevInstructionsPath(), err)
	}
	dc.instructions = cutText(string(ins), agentcfg.MaxInstructionsSize)
	return dc, "", nil
}

// devTools 按 commit sha（任务的起点）上的 .kiln/runtimes.lock 解析开发任务使用的工具链（requirements.tools，PLAN #33.12）：
// 开发可以执行的每个平台（development.platform）各一份（Runner 平台 → 工具）。没有声明时为 nil。没有 lockfile、lockfile
// 无效、缺少条目或任何一个平台时 why 说明原因并写明平台（配置错误：不规划，不换版本、不取最新）；只有读取失败时返回 err。
func devTools(ctx context.Context, f forge.Repo, sha string, cfg *agentcfg.Config) (tools map[string][]aispec.ToolRef, why string, err error) {
	names := cfg.Tools()
	if len(names) == 0 {
		return nil, "", nil
	}
	data, err := f.ReadFile(ctx, sha, toolchain.LockPath)
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s：%w", toolchain.LockPath, err)
	}
	var lock *toolchain.Lock
	if data != nil {
		if lock, err = toolchain.ParseLock(data); err != nil {
			return nil, "requirements.tools：" + err.Error(), nil //nolint:nilerr // 配置错误是给人看的原因，不是处理失败
		}
	}
	tools, err = aispec.ResolvePlatformTools(lock, names, cfg.DevPlatforms())
	if err != nil {
		return nil, err.Error(), nil //nolint:nilerr // 同上
	}
	return tools, "", nil
}

// cutText 把 s 截断到最多 n 字节（不切开多字节字符），并去掉 Postgres 文本不能保存的 NUL 字节。
func cutText(s string, n int) string {
	return shorten.Prefix(strings.ReplaceAll(s, "\x00", ""), n)
}

// devIssue 把平台上的 Issue 转换为快照（正文截断，摘要按完整内容计算）。
func devIssue(is *forge.IssueContent) aispec.DevIssue {
	return aispec.DevIssue{
		Number: is.Number, Title: is.Title, Body: cutText(is.Body, issueBodyLimit), Labels: slices.Clone(is.Labels),
		Digest: IssueDigest(is.Title, is.Body), URL: is.URL,
	}
}

// SelectResult 是 PlanSelect 的结果：建了（或已有）选题请求时 RequestID 非 0，否则 Reason 说明原因。
type SelectResult struct {
	RequestID int64
	Reason    string
}

// PlanSelect 为仓库规划一次选题（PLAN #26.11 调度）。条件：
//
//   - 仓库开启了开发、没有暂停；
//   - 这个仓库没有正在选题的请求（选题轮次：同一个仓库同时只有一台机器在选题，它领取成功、没有领取或者请求结束时
//     这一轮结束）；
//   - 仓库没有被占用：没有未结束的工作项（I14，serial.go）——同一个仓库按完整的工作项串行，不同仓库并行；
//   - 任务池中有可选的 Issue，并且任务池在上次选题之后有变化；
//   - 开发机有空闲槽位：排队中的同一个 Agent 的 AI 开发任务占用它们能执行的平台上的槽位之后，development.platform 中的
//     平台上还有空闲槽位（freeDevSlots）。Server 不预先排选题请求，能同时开发几个仓库取决于开发机一共有几个空闲槽位（一个
//     节点可以同时执行多个开发任务，PLAN #33.8）。
//
// 候选不在这里确定：Runner 准备好后才向 Server 要（Offer）。sender 是触发者（日志）。
func (s *Service) PlanSelect(ctx context.Context, repoID int64, sender string) (*SelectResult, error) {
	if s.forge == nil {
		return &SelectResult{Reason: whyNoForge}, nil
	}
	repo, err := store.GetRepo(ctx, s.db, repoID)
	if err != nil {
		return nil, err
	}
	g, err := s.selectGate(ctx, repo)
	if err != nil || g.why != "" || g.requestID != 0 {
		if err != nil {
			return nil, err
		}
		return &SelectResult{RequestID: g.requestID, Reason: g.why}, nil
	}
	// 空闲槽位快照读一次，读到配置前后各评估一次：全局的 runners、ai_tasks 不重复汇总。
	slots, err := loadDevSlots(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if slots.spare(g.agent, g.platforms) <= 0 {
		return &SelectResult{Reason: whyNoFreeSlot}, nil
	}
	dc, why, err := loadDevConfig(ctx, s.forge(repo), repo)
	if err != nil {
		return nil, err
	}
	if why != "" {
		// 记下这个任务池状态已经看过：有空闲槽位的开发机反复来领取时不再读取配置，任务池变化或者手动同步后再判断
		return &SelectResult{Reason: why}, setExamined(ctx, s.db, repo.ID, g.fingerprint)
	}
	cfg := dc.cfg
	// 记下读到的 Agent 和平台：之后开发机来领取时按它们判断（不读 GitHub）。能用这个 Agent、平台在其中的开发机没有空闲
	// 槽位时不规划，也不记"已经看过"：这样的开发机有空闲槽位时再来
	agent, platforms := cfg.Development.Agent, cfg.DevPlatforms()
	if err := setDevTarget(ctx, s.db, repo.ID, agent, platforms); err != nil {
		return nil, err
	}
	if slots.spare(agent, platforms) <= 0 {
		return &SelectResult{Reason: whyNoFreeSlot}, nil
	}
	tools, why, err := devTools(ctx, s.forge(repo), dc.head, cfg)
	if err != nil {
		return nil, err
	}
	if why != "" {
		return &SelectResult{Reason: "默认分支的工具链配置有错误：" + why}, setExamined(ctx, s.db, repo.ID, g.fingerprint)
	}
	r, _, err := s.plan(ctx, &devRequest{
		repo: repo, ref: gitref.BranchPrefix + repo.DefaultBranch, sha: dc.head, platforms: platforms,
		dedupeKey: selectKey(repo.ID, g.seq), sender: sender, timeout: int(cfg.DevTimeout().Minutes()),
		spec: &aispec.DevelopSpec{
			Mode: aispec.DevSelect, Agent: cfg.Development.Agent, Instructions: dc.instructions, ConfigSHA: dc.head,
			SelectSeq: g.seq, MaxIssues: cfg.MaxIssues(), DryRun: cfg.Development.DryRun, MaxFixRounds: cfg.MaxFixRounds(),
			BaseRef: repo.DefaultBranch, Services: cfg.Services(), PlatformTools: tools, Repositories: cfg.Repositories(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("规划选题：%w", err)
	}
	// 记下这个任务池状态已经交给选题：选题请求失败（准备失败、Agent 出错、超时）后不自动重来，任务池变化或手动同步后
	// 再选，不会在持续的故障上反复空转。Runner 要候选时按它实际看到的状态再记一次。
	if err := setExamined(ctx, s.db, repo.ID, g.fingerprint); err != nil {
		return nil, err
	}
	s.log.Info("AI 开发：已规划选题", "repo", repo.FullName(), "request", r.ID, "turn", g.seq, "candidates", g.candidates, "sender", sender)
	return &SelectResult{RequestID: r.ID}, nil
}

// selectGate 是不读 GitHub 就能判断的选题条件（PlanSelect、RunnerIdle 共用）。
type selectGate struct {
	// why 是不能选题的原因；requestID 是这一轮已有的选题请求（正在选题）。
	why       string
	requestID int64
	// seq 是当前的选题轮次；fingerprint、candidates 是任务池的状态和可选的 Issue 数。agent 是上次从默认分支读到的
	// 开发 Agent（还没读过时为空），platforms 是同时读到的开发平台（还没读过时为 nil）。
	agent       string
	platforms   []string
	seq         int
	fingerprint string
	candidates  int
}

// selectGate 判断仓库现在能不能开始一轮选题。这一轮的选题请求已经结束（领取前失败、超时、取消、没有候选）时结束这一轮。
func (s *Service) selectGate(ctx context.Context, repo *store.Repo) (*selectGate, error) {
	if !repo.Active {
		return &selectGate{why: whyRepoInactive}, nil
	}
	rs, err := readRepoState(ctx, s.db, repo.ID)
	if err != nil {
		return nil, err
	}
	return s.gateWithState(ctx, repo, rs)
}

// gateWithState 与 selectGate 相同，但使用已经读到的仓库状态（批量处理多个仓库时避免逐个读取）。
func (s *Service) gateWithState(ctx context.Context, repo *store.Repo, rs *repoState) (*selectGate, error) {
	if !repo.Active {
		return &selectGate{why: whyRepoInactive}, nil
	}
	if rs.paused {
		return &selectGate{why: whyPaused}, nil
	}
	g := selectGate{seq: rs.seq, agent: rs.agent, platforms: rs.platforms}
	examined := rs.examined
	r, err := ai.RequestByDedupeKey(ctx, s.db, selectKey(repo.ID, g.seq))
	switch {
	case err != nil:
		return nil, err
	case r == nil:
	case !r.State.Terminal():
		g.requestID = r.ID
		g.why = fmt.Sprintf("AI 开发请求 #%d 正在选题（同一个仓库同时只有一台机器在选题）", r.ID)
		return &g, nil
	default:
		if err := endTurn(ctx, s.db, repo.ID, g.seq); err != nil {
			return nil, err
		}
		g.seq++
	}
	// 仓库被占用（I14）时不选题，也不记"已经看过"：占用者结束时 transition 通知执行者，任务池的状态也包含未结束的
	// 工作项，结束后自然重新判断
	occ, err := occupancy(ctx, s.db, repo.ID)
	if err != nil {
		return nil, err
	}
	if why := occ.Why(); why != "" {
		g.why = why
		return &g, nil
	}
	st, err := readPool(ctx, s.db, repo.ID)
	if err != nil {
		return nil, err
	}
	g.fingerprint, g.candidates = st.fingerprint(), len(st.candidates)
	switch {
	case g.candidates == 0:
		g.why = whyNoCandidates
	case g.fingerprint == examined:
		g.why = whyExamined
	}
	return &g, nil
}

// RunnerIdle 在 AI 执行节点空闲（来领取却没有可领的任务）时调用（ai.Core 的空闲钩子）：开发角色时通知可以开始选题的
// 仓库的执行者（PLAN #26.11 调度，由执行者规划选题请求，开发机在长轮询中领走）。只读数据库，不请求 GitHub。
func (s *Service) RunnerIdle(ctx context.Context, role string) error {
	if s.forge == nil || role != store.RunnerRoleDevelopment {
		return nil
	}
	ids, err := store.QueryIDs(ctx, s.db, `SELECT r.id FROM repos r LEFT JOIN ai_repo_state s ON s.repo_id=r.id
		WHERE r.active AND NOT coalesce(s.dev_paused, false) AND EXISTS (SELECT 1 FROM dev_pool p WHERE p.repo_id=r.id AND p.in_pool) ORDER BY r.id`)
	if err != nil || len(ids) == 0 {
		return err
	}
	// 仓库、状态、空闲槽位各批量读取一次，避免按仓库重复汇总全局的 runners、ai_tasks。
	repos, err := store.ReposByIDs(ctx, s.db, ids)
	if err != nil {
		return err
	}
	states, err := readRepoStates(ctx, s.db, ids)
	if err != nil {
		return err
	}
	slots, err := loadDevSlots(ctx, s.db)
	if err != nil {
		return err
	}
	for _, id := range ids {
		repo := repos[id]
		if repo == nil {
			continue // 已被删除（与查询之间存在竞态）
		}
		st := states[id]
		if st == nil {
			st = &repoState{}
		}
		g, err := s.gateWithState(ctx, repo, st)
		if err != nil {
			return err
		}
		if g.why == "" && g.requestID == 0 && slots.spare(g.agent, g.platforms) > 0 {
			if err := s.Enqueue(ctx, s.db, id); err != nil {
				return err
			}
		}
	}
	return nil
}
