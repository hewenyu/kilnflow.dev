package aidev

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// freeDevSlots 返回开发机上还能再执行几个这个仓库的开发任务（agent、platforms 是仓库的开发 Agent 和平台，PLAN #26.7）。
// 只读一次快照、评估一个仓库；一次处理多个仓库时先 loadDevSlots 一次，再用 devSlots.spare 逐个评估，避免重复汇总全局的
// runners、ai_tasks。
func freeDevSlots(ctx context.Context, q store.Querier, agent string, platforms []string) (int, error) {
	s, err := loadDevSlots(ctx, q)
	if err != nil {
		return 0, err
	}
	return s.spare(agent, platforms), nil
}

// devSlots 是一次读取的开发机空闲槽位快照：按 Agent 汇总的每平台空闲槽位，以及排队中的开发任务。
//
// 计入的节点：在线、没有暂停接单、协议版本是当前的、Orca Server 就绪、agent 可用（检测为可用、不在退避中）的开发角色
// AI 执行节点；每台的空闲槽位按 ai.RunnerFreeSlotsSQL（并发上限减去执行中的任务数，下限 0，与领取时的判断是同一条规则），
// 按节点上报的平台汇总。排队中的同一个 agent 的开发任务会先占用槽位，按 spareSlots 的规则扣减：只占它能执行的平台上的
// 槽位。别的 Agent 的任务不扣减：这些节点领不了它们，扣减会让一个 Agent 不可用的仓库挡住别的仓库。
type devSlots struct {
	// byAgent 是 Agent → 平台 → 空闲槽位（节点必须有这个 Agent 可用才计入）。
	byAgent map[string]map[string]int
	// anyAgent 是平台 → 空闲槽位（节点有任意 Agent 可用即计入），用于还没读到 Agent 时的粗筛。
	anyAgent map[string]int
	// pending 是 Agent → 排队中的开发任务（按平台组合分组）。
	pending map[string][]pendingTasks
}

// loadDevSlots 一次读取开发机的空闲槽位快照（三次查询，与仓库数量无关）。
func loadDevSlots(ctx context.Context, q store.Querier) (*devSlots, error) {
	s := &devSlots{byAgent: map[string]map[string]int{}, anyAgent: map[string]int{}, pending: map[string][]pendingTasks{}}
	// 按可用的 Agent 展开：一台节点的每个可用 Agent 各计一次它的空闲槽位（与按 agent 过滤后汇总等价）。
	rows, err := q.Query(ctx, `SELECT g.agent, ar.platform, sum(`+ai.RunnerFreeSlotsSQL+`)::bigint
		 FROM runners r JOIN ai_runners ar ON ar.runner_id=r.id JOIN ai_runner_agents g ON g.runner_id=r.id
		 WHERE r.state='active' AND r.kind=$1 AND ar.role=$2 AND NOT r.drain
		   AND r.last_seen_at > now() - make_interval(secs => $3)
		   AND ar.protocol = $4 AND ar.orca_state='ready'
		   AND g.state=$5 AND g.installed IS NOT false AND (g.backoff_until IS NULL OR g.backoff_until <= now())
		 GROUP BY g.agent, ar.platform`,
		store.RunnerKindAI, store.RunnerRoleDevelopment, store.RunnerOnlineWindow.Seconds(), agentcfg.AIProtocol,
		agentcfg.AgentAvailable)
	if err != nil {
		return nil, err
	}
	var agent, platform string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&agent, &platform, &n}, func() error {
		if s.byAgent[agent] == nil {
			s.byAgent[agent] = map[string]int{}
		}
		s.byAgent[agent][platform] = n
		return nil
	}); err != nil {
		return nil, err
	}
	// 还没读到 Agent 时的粗筛：节点只要有一个可用 Agent 就计入。
	rows, err = q.Query(ctx, `SELECT ar.platform, sum(`+ai.RunnerFreeSlotsSQL+`)::bigint FROM runners r JOIN ai_runners ar ON ar.runner_id=r.id
		 WHERE r.state='active' AND r.kind=$1 AND ar.role=$2 AND NOT r.drain
		   AND r.last_seen_at > now() - make_interval(secs => $3)
		   AND ar.protocol = $4 AND ar.orca_state='ready'
		   AND EXISTS (SELECT 1 FROM ai_runner_agents g WHERE g.runner_id=r.id AND g.state=$5 AND g.installed IS NOT false
		               AND (g.backoff_until IS NULL OR g.backoff_until <= now()))
		 GROUP BY ar.platform`,
		store.RunnerKindAI, store.RunnerRoleDevelopment, store.RunnerOnlineWindow.Seconds(), agentcfg.AIProtocol,
		agentcfg.AgentAvailable)
	if err != nil {
		return nil, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&platform, &n}, func() error {
		s.anyAgent[platform] = n
		return nil
	}); err != nil {
		return nil, err
	}
	// 排队中的开发任务（所有 Agent 一次取回）。
	rows, err = q.Query(ctx, `SELECT agent, platforms, count(*) FROM ai_tasks WHERE state='pending' AND type=$1 GROUP BY agent, platforms`,
		agentcfg.TaskDevelop)
	if err != nil {
		return nil, err
	}
	var g pendingTasks
	if _, err := pgx.ForEachRow(rows, []any{&agent, &g.platforms, &g.n}, func() error {
		s.pending[agent] = append(s.pending[agent], g)
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// spare 返回快照中 agent、platforms 的开发任务还能再匹配几个空闲槽位（agent 为空时数任何 Agent 可用的节点、不扣减；
// platforms 为 nil 时任何平台都算）。规则见 spareSlots。
func (s *devSlots) spare(agent string, platforms []string) int {
	free := s.anyAgent
	var pending []pendingTasks
	if agent != "" {
		free = s.byAgent[agent]
		pending = s.pending[agent]
	}
	return spareSlots(free, pending, platforms)
}

// pendingTasks 是排队中的、可以在同一组平台上执行的 n 个任务。
type pendingTasks struct {
	platforms []string
	n         int
}

// spareSlots 是排队任务占用槽位的规则（PLAN #33.8），freeDevSlots 按它扣减。free 是每个平台的空闲槽位，pending 是排队中的
// 任务，want 是要规划的任务可以执行的平台（nil 表示任何平台）。
//
// 一个任务只能占用它可以执行的平台上的槽位，每个槽位最多给一个任务。先让排队任务占用尽可能多的槽位（任务与槽位的最大
// 匹配），再看在不减少排队任务占用数的前提下，还能为 want 的任务再匹配几个槽位，就是返回值（最大流的增量）。所以：
//   - 只能在没有空闲槽位的平台上执行的排队任务不扣减（例如 macOS 的开发机都暂停了，只能在 macOS 上执行的排队任务不占
//     Linux 开发机的槽位）；
//   - 可以在一组平台上执行的排队任务，能放到 want 之外的平台时不占 want 中的槽位；
//   - 排队任务确实需要的槽位照样扣减，不会因为规划了新任务让本来能执行的排队任务没有槽位。
//
// 返回值大于 0 表示存在一种分配：排队任务能执行的都照常执行，同时有 want 中的空闲槽位留给新任务。实际领取的顺序不一定是
// 这种分配（例如一组平台的排队任务先被 want 中的开发机领走），这时新任务等下一个空闲槽位，只是稍晚执行；不会出现 want 中
// 有空闲的开发机、却因为领不走的排队任务一直不规划。
func spareSlots(free map[string]int, pending []pendingTasks, want []string) int {
	// 有空闲槽位的平台；没有槽位的平台与结果无关
	var plats []string
	total := 0
	for p, n := range free {
		if n > 0 {
			plats = append(plats, p)
			total += n
		}
	}
	slices.Sort(plats)
	// 流网络：源点 → 任务组（容量为任务数）→ 能执行的平台（容量不限）→ 汇点（容量为平台的空闲槽位）。
	// 节点编号：0 源点，1..len(pending) 排队任务组，len(pending)+1 是要规划的任务，之后是平台，最后是汇点。
	wantNode := len(pending) + 1
	platNode := func(i int) int { return wantNode + 1 + i }
	sink := platNode(len(plats))
	g := newFlowGraph(sink + 1)
	link := func(from int, platforms []string) {
		for i, p := range plats {
			if platforms == nil || slices.Contains(platforms, p) {
				g.cap[from][platNode(i)] = total
			}
		}
	}
	for i, t := range pending {
		g.cap[0][i+1] = t.n
		link(i+1, t.platforms)
	}
	link(wantNode, want)
	for i, p := range plats {
		g.cap[platNode(i)][sink] = free[p]
	}
	g.maxFlow(0, sink)
	// 再接上要规划的任务（数量不限）继续增广：增广路不经过源点的其它出边，排队任务已占用的数量不会减少
	g.cap[0][wantNode] = total
	return g.maxFlow(0, sink)
}

// flowGraph 是邻接矩阵表示的流网络的剩余容量（节点数很少：排队任务按平台组合分组，平台只有几个）。
type flowGraph struct{ cap [][]int }

func newFlowGraph(n int) *flowGraph {
	g := &flowGraph{cap: make([][]int, n)}
	for i := range g.cap {
		g.cap[i] = make([]int, n)
	}
	return g
}

// maxFlow 在当前的剩余容量上从 s 到 t 增广到没有增广路为止（Edmonds–Karp：每次沿 BFS 找到的最短增广路），返回增加的流量。
func (g *flowGraph) maxFlow(s, t int) int {
	added := 0
	for {
		prev := make([]int, len(g.cap))
		for i := range prev {
			prev[i] = -1
		}
		prev[s] = s
		queue := []int{s}
		for len(queue) > 0 && prev[t] < 0 {
			u := queue[0]
			queue = queue[1:]
			for v, c := range g.cap[u] {
				if c > 0 && prev[v] < 0 {
					prev[v] = u
					queue = append(queue, v)
				}
			}
		}
		if prev[t] < 0 {
			return added
		}
		f := -1
		for v := t; v != s; v = prev[v] {
			if c := g.cap[prev[v]][v]; f < 0 || c < f {
				f = c
			}
		}
		for v := t; v != s; v = prev[v] {
			g.cap[prev[v]][v] -= f
			g.cap[v][prev[v]] += f
		}
		added += f
	}
}
