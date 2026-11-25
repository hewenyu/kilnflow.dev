package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// Runners 页面：连接状态（在线 / 离线 / 已吊销）和接单状态（接单中 / 已暂停接单）分开显示，
// 列出每台 Runner 正在执行的 job；注册新的 Runner 分步骤引导，一次性令牌经 POST → 重定向后只展示一次。
// 暂停接单（drain）、恢复、吊销和生成令牌仍由 admin.Service 执行，语义不变。

// defaultLeaseTTL、defaultAcquireLongPoll 是没有配置 Config.LeaseTTL、Config.AcquireLongPoll 时页面上说明的时长（与 Server 的默认值一致）。
const (
	defaultLeaseTTL        = 60 * time.Second
	defaultAcquireLongPoll = 25 * time.Second
)

// Runner 操作（POST /runners/{name}/{op}）。
const (
	runnerOpDrain   = "drain"
	runnerOpUndrain = "undrain"
	runnerOpRevoke  = "revoke"
	runnerOpDelete  = "delete"
)

// Runners 页面按状态筛选的取值（地址中的 state 参数）。
const (
	runnerStateOnline    = "online"
	runnerStateAccepting = "accepting"
	runnerStatePaused    = "paused"
	runnerStateBusy      = "busy"
	runnerStateOffline   = "offline"
	runnerStateRevoked   = "revoked"
)

// runnerView 是 CI Runners 页面上的一台 CI Runner（AI 执行节点在 AI 执行节点页面，见 ai_runners.go）。
type runnerView struct {
	ID         int64
	Name, Pool string
	// Kind 是 Runner 类型（ci；注册页上的 AI 执行节点为 ai），Role 是 AI 执行节点的角色（review、development，CI Runner 为空），
	// 前端按它们显示类型名称；URL 是它的详情页。
	Kind, Role, URL string
	Platforms       []string
	Executors       []string
	Version         string
	Revoked         bool
	// Drain 表示已暂停接单（Server 上的 drain 标记）。
	Drain bool
	// Online 表示最近 admin.RunnerOfflineAfter 内联系过 Server（见 admin.RunnerOnline）。
	Online   bool
	LastSeen *time.Time
	Jobs     []cistore.RunningJob
	// Capacity 是上报的规格与容量（PLAN #8.6）；旧版 kiln-runner 没有上报时为 nil。
	Capacity *capacityView
	// CacheBytes、CacheReportedAt 是最近一次上报的本地缓存总大小和上报时间（没有上报过时为 0 和 nil）。
	CacheBytes      int64
	CacheReportedAt *time.Time
	// Upgrade 是自升级的状态（PLAN #27.8）；loadRunners 之外构造的为 nil。
	Upgrade *runnerUpgradeView

	// capacity 是上报的容量（计算 Capacity 用，加上正在执行的 job 声明的资源后得到已分配）。
	capacity *store.RunnerCapacity
}

// resourcesView 是一组资源：CPU 是核数（例如"2""1.5"），Memory 是内存（例如"1.9 GiB"），没有时为空；
// 单位"核"和组合的写法由前端按语言显示。
type resourcesView struct{ CPU, Memory string }

// newResourcesView 返回资源的文字（为 0 的一项为空）。
func newResourcesView(r store.Resources) resourcesView {
	var v resourcesView
	if r.CPU > 0 {
		v.CPU = store.FormatCPU(r.CPU)
	}
	if r.Memory > 0 {
		v.Memory = store.FormatMemory(r.Memory)
	}
	return v
}

// limitView 是一种 executor 的固定上限（CPU、Memory 至少有一项）。
type limitView struct {
	Executor string
	resourcesView
}

// capacityView 是 Runner 的规格与容量。规格是静态的，不是实时占用；
// 已分配是正在执行的 job 声明的 resources 之和，可分配是机器规格 - 预留（PLAN #14.1）。
type capacityView struct {
	Machine     resourcesView
	Reserved    resourcesView
	Allocatable resourcesView
	Allocated   resourcesView
	// Limits 是各 executor 的固定上限（按 executor 排序）。
	Limits  []limitView
	MaxJobs int
}

// fullResources 返回规格、预留等总是显示两项的资源：为 0 的一项显示为 0。
func fullResources(r store.Resources) resourcesView {
	return resourcesView{CPU: store.FormatCPU(r.CPU), Memory: store.FormatMemory(r.Memory)}
}

// newCapacityView 返回容量；allocated 是正在执行的 job 声明的资源之和。
func newCapacityView(c *store.RunnerCapacity, allocated store.Resources) *capacityView {
	v := &capacityView{
		Machine: fullResources(c.Machine), Reserved: fullResources(c.Reserved), Allocatable: fullResources(c.Allocatable()),
		Allocated: fullResources(allocated), MaxJobs: c.MaxJobs,
	}
	for _, k := range slices.Sorted(maps.Keys(c.Limits)) {
		if l := newResourcesView(c.Limits[k]); l != (resourcesView{}) {
			v.Limits = append(v.Limits, limitView{Executor: k, resourcesView: l})
		}
	}
	return v
}

// newRunnerView 返回 Runner r 在 now 时的显示数据（不含正在执行的 job 和已分配的资源）。
func newRunnerView(r *cistore.CIRunner, now time.Time) *runnerView {
	v := &runnerView{
		ID: r.ID, Name: r.Name, Pool: r.Pool, Kind: store.RunnerKindCI, URL: runnerURL(r.Name), Platforms: r.Platforms, Executors: r.Executors, Version: r.Version,
		Revoked: r.Revoked(), Drain: r.Drain, Online: admin.RunnerOnline(&r.Runner, now), LastSeen: r.LastSeen,
		capacity: r.Capacity, CacheReportedAt: r.CacheReportedAt,
	}
	if r.CacheReportedAt != nil {
		v.CacheBytes = admin.CacheBytes(r.CacheReport)
	}
	if r.Capacity != nil {
		v.Capacity = newCapacityView(r.Capacity, store.Resources{})
	}
	return v
}

// Accepting 表示 Runner 在线并且接单：有空闲时会领取新任务。
func (v *runnerView) Accepting() bool { return !v.Revoked && v.Online && !v.Drain }

// ConnClass 返回连接状态的徽章样式（bad：已吊销，ok：在线，warn：离线）；连接状态的文字由前端按 Revoked、Online 显示。
func (v *runnerView) ConnClass() string { return connClass(v.Revoked, v.Online) }

// connClass 是 CI Runner 和 AI 执行节点共用的连接状态样式。
func connClass(revoked, online bool) string {
	switch {
	case revoked:
		return "bad"
	case online:
		return "ok"
	}
	return "warn"
}

// match 判断 Runner 是否符合状态筛选（取值见 runnerStateOptions，空字符串表示不筛选）。
func (v *runnerView) match(state string) bool {
	switch state {
	case runnerStateOnline:
		return !v.Revoked && v.Online
	case runnerStateOffline:
		return !v.Revoked && !v.Online
	case runnerStateAccepting:
		return v.Accepting()
	case runnerStatePaused:
		return !v.Revoked && v.Drain
	case runnerStateBusy:
		return len(v.Jobs) > 0
	case runnerStateRevoked:
		return v.Revoked
	}
	return true
}

// runnerStateOptions 是 Runners 页面按状态筛选的选项。
var runnerStateOptions = []textOption{
	{runnerStateOnline, message(msgRunnerFilterOnline)},
	{runnerStateAccepting, message(msgRunnerFilterAccepting)},
	{runnerStatePaused, message(msgRunnerFilterPaused)},
	{runnerStateBusy, message(msgRunnerFilterBusy)},
	{runnerStateOffline, message(msgRunnerFilterOffline)},
	{runnerStateRevoked, message(msgRunnerFilterRevoked)},
}

// runnerStateLabel 返回状态筛选的文字；不是筛选的取值时为空。
func runnerStateLabel(s string) uiText {
	for _, o := range runnerStateOptions {
		if o.Value == s {
			return o.Label
		}
	}
	return uiText{}
}

// runnerCounts 是全部 Runner（不受筛选影响）的统计。
type runnerCounts struct {
	Total, Online, Accepting, Paused, Offline, Revoked, Jobs int
}

// runnersPage 是 Runners 页面的数据。
type runnersPage struct {
	Runners []*runnerView
	Counts  runnerCounts
	// State、Pool 是筛选条件（已校验，无效的取值视为不筛选）。
	State, Pool string
	States      []textOption
	Pools       []string
	// Self 是当前页面的地址（带筛选条件），操作后回到这里。
	Self string
	Now  time.Time
	// OfflineAfter、LeaseTTL、LongPoll 是页面说明中用到的时长（秒）。
	OfflineAfter, LeaseTTL, LongPoll int64
}

// Filtered 表示设置了筛选条件。
func (p *runnersPage) Filtered() bool { return p.State != "" || p.Pool != "" }

// StateLabel 返回状态筛选的文字。
func (p *runnersPage) StateLabel() uiText { return runnerStateLabel(p.State) }

// runnersURL 返回带筛选条件的 Runners 页面地址。
func runnersURL(state, pool string) string {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if pool != "" {
		q.Set("pool", pool)
	}
	if len(q) == 0 {
		return "/runners"
	}
	return "/runners?" + q.Encode()
}

// loadRunners 查询满足条件的 Runner 及其正在执行的 job，按名称排序。
func (d *Dashboard) loadRunners(ctx context.Context, f store.RunnerFilter) ([]*runnerView, error) {
	runners, err := cistore.ListCIRunners(ctx, d.db, f)
	if err != nil || len(runners) == 0 {
		return nil, err
	}
	now := time.Now()
	list := make([]*runnerView, len(runners))
	ids := make([]int64, len(runners))
	byID := make(map[int64]*runnerView, len(runners))
	for i, r := range runners {
		list[i] = newRunnerView(r, now)
		ids[i] = r.ID
		byID[r.ID] = list[i]
	}
	// 与 admin.ListRunners 的"运行中"计数使用同一个查询
	jobs, err := cistore.RunningJobs(ctx, d.db, ids)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		byID[j.RunnerID].Jobs = append(byID[j.RunnerID].Jobs, j)
	}
	alloc, err := cistore.RunnerAllocations(ctx, d.db, ids)
	if err != nil {
		return nil, err
	}
	// 与 admin.ListRunners（CLI kiln runner list）使用同一个查询
	upgrades, err := upgradectl.RunnerInfos(ctx, d.db, ids)
	if err != nil {
		return nil, err
	}
	for _, v := range list {
		if v.capacity != nil {
			v.Capacity = newCapacityView(v.capacity, alloc[v.ID])
		}
		v.Upgrade = newRunnerUpgradeView(upgrades[v.ID])
	}
	return list, nil
}

// runnerUpgradeView 是一台 Runner 的自升级状态（PLAN #27.12–#27.14）。
type runnerUpgradeView struct {
	// State 是 Runner 上报的状态（idle、downloading、ready、draining、switching、confirmed、failed、rolled_back、manual）；
	// 为空表示 Runner 没有上报过（版本不支持自升级）。Target、Error 是上报的目标版本和原因。
	State, Target, Error string
	// Launcher 表示经过启动器运行（可以切换版本）；Disabled 表示 config.toml 中 upgrade = false。
	Launcher, Disabled bool
	// Current、Previous 是启动器记录的当前版本和上一个版本（回滚用）。
	Current, Previous string
	// Package 是对应的 npm 包；Latest 是 Runner 查到的最新正式版本，CheckedAt 是最后一次查询成功的时间，
	// CheckError 是最后一次查询的错误；CheckPending 表示已经请求检查、Runner 还没有处理。
	Package, Latest string
	CheckedAt       *time.Time
	CheckError      string
	CheckPending    bool
	// CanUpgrade 表示现在可以请求升级到 Latest；不能时 Reason 说明原因（upgradectl 的原因码映射的消息）。
	CanUpgrade bool
	Reason     uiText
	// Request 是最近一次升级请求（没有时为 nil）。
	Request *upgradeRequestView
}

// upgradeRequestView 是一次按台升级的请求。
type upgradeRequestView struct {
	ID int64
	// Version 是目标版本，From 是请求时 Runner 的版本（不带 v）。
	Version, From string
	// State 是 pending、succeeded、failed 或 cancelled。
	State, Error, Actor string
	Created             time.Time
	Finished            *time.Time
}

func newUpgradeRequestView(r *apiv1.RunnerUpgradeRequest) *upgradeRequestView {
	if r == nil {
		return nil
	}
	return &upgradeRequestView{
		ID: r.Id, Version: r.Version, From: r.FromVersion, State: r.State, Error: r.Error, Actor: r.Actor,
		Created: time.Unix(r.CreatedAt, 0), Finished: unixTime(r.FinishedAt),
	}
}

func newRunnerUpgradeView(ri upgradectl.RunnerInfo) *runnerUpgradeView {
	i := ri.Info
	if i == nil {
		return &runnerUpgradeView{}
	}
	return &runnerUpgradeView{
		State: i.State, Target: i.Target, Error: i.Error, Launcher: i.Launcher, Disabled: i.Disabled, Current: i.Current, Previous: i.Previous,
		Package: i.Package, Latest: i.Latest, CheckedAt: unixTime(i.CheckedAt), CheckError: i.CheckError, CheckPending: i.CheckPending,
		CanUpgrade: i.CanUpgrade, Reason: upgradeReasonText(ri.Reason), Request: newUpgradeRequestView(i.Request),
	}
}

// runnerByName 返回名为 name 的 Runner，不存在时返回 nil。
func (d *Dashboard) runnerByName(ctx context.Context, name string) (*runnerView, error) {
	list, err := d.loadRunners(ctx, store.RunnerFilter{Name: name})
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

func (d *Dashboard) leaseTTL() time.Duration {
	if d.cfg.LeaseTTL > 0 {
		return d.cfg.LeaseTTL
	}
	return defaultLeaseTTL
}

func (d *Dashboard) longPoll() time.Duration {
	if d.cfg.AcquireLongPoll > 0 {
		return d.cfg.AcquireLongPoll
	}
	return defaultAcquireLongPoll
}

func (d *Dashboard) pageRunners(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	p := &runnersPage{
		States: runnerStateOptions, Now: time.Now(),
		OfflineAfter: seconds(admin.RunnerOfflineAfter), LeaseTTL: seconds(d.leaseTTL()), LongPoll: seconds(d.longPoll()),
	}
	if s := q.Get("state"); !runnerStateLabel(s).IsZero() {
		p.State = s
	}
	all, err := d.loadRunners(r.Context(), store.RunnerFilter{})
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.Counts = countRunners(all)
	pools := map[string]bool{}
	for _, v := range all {
		pools[v.Pool] = true
	}
	p.Pools = slices.Sorted(maps.Keys(pools))
	if pool := strings.TrimSpace(q.Get("pool")); pools[pool] {
		p.Pool = pool
	}
	for _, v := range all {
		if v.match(p.State) && (p.Pool == "" || v.Pool == p.Pool) {
			p.Runners = append(p.Runners, v)
		}
	}
	p.Self = runnersURL(p.State, p.Pool)
	d.renderPage(w, r, "runners", message(msgRunnersTitle), p, http.StatusOK)
}

// ---- 注册新的 Runner ----

// enrollNamePattern 是 Dashboard 中可以填写的 pool 和 Runner 名称（会写进页面上给出的 shell 命令）。
var enrollNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// enrollStatus 是注册令牌的当前状态。
type enrollStatus string

const (
	enrollUnused  enrollStatus = "unused"
	enrollUsed    enrollStatus = "used"
	enrollExpired enrollStatus = "expired"
)

// enrollView 是一次生成的注册令牌（一次性展示页的数据）。
type enrollView struct {
	ID string
	// Token 只在第一次打开展示页时有值；之后为空，页面说明令牌已经显示过。
	Token      string
	Pool, Name string
	// Kind 是令牌注册出的 Runner 类型：ci（kiln-runner）或 ai（执行机器上的 kiln-orca 连接器）；Role 是 AI 执行节点的角色
	// （review、development）。
	Kind, Role string
	Created    time.Time
	Expires    time.Time
	Status     enrollStatus
	// UsedAt、Runner 是使用了这个令牌的 Runner（Status 为 used 时）。
	UsedAt *time.Time
	Runner *runnerView
	// Existing 是已经存在的同名 Runner（填写了名称、令牌还没有使用时）：注册会替换它的身份。
	Existing *runnerView
}

// runnerNewPage 是注册新的 Runner 页面的数据。
type runnerNewPage struct {
	Form *formState
	// RequestID 是生成令牌表单的请求 ID：同一个表单重复提交只生成一个令牌。
	RequestID     string
	PublicURL     string
	ServerVersion string
	Pools         []string
	// Reveal 是生成后的一次性展示页；为 nil 时显示生成令牌的表单。
	Reveal *enrollView
	// Missing 表示地址中的令牌找不到（属于其他用户、已清理或 Server 重启过）。
	Missing bool
	// RegenID 是"重新生成"表单的请求 ID。
	RegenID string
}

// Command 返回在 Runner 机器上执行的注册命令（Token 为空时返回空字符串）。
func (p *runnerNewPage) Command() string {
	v := p.Reveal
	if v == nil || v.Token == "" {
		return ""
	}
	// AI 执行节点的连接器（kiln-orca，npm 包）有自己的一套命令（PLAN #33.3）
	bin := "kiln-runner"
	if v.Kind == store.RunnerKindAI {
		bin = "kiln-orca"
	}
	cmd := bin + " enroll --server " + p.PublicURL + " --token " + v.Token
	if v.Name != "" {
		cmd += " --name " + v.Name
	}
	return cmd + "\n" + bin + " doctor\n" + bin + " service install"
}

// enrollReveal 是一个等待展示的注册令牌，只保存在内存中（Server 重启后丢失，令牌本身仍然有效）。
type enrollReveal struct {
	login string
	// tokenHash 是令牌在数据库中的哈希，用于查询令牌的状态。
	tokenHash  string
	token      string
	pool, name string
	// kind 是令牌注册出的 Runner 类型（ci 或 ai），role 是 AI 执行节点的角色（review 或 development）。
	kind, role string
	created    time.Time
	expires    time.Time
}

// enrollReveals 保存最近生成的注册令牌，键是生成者和生成表单的请求 ID。令牌只在第一次打开展示页时返回，之后清空；
// 条目保留到令牌过期后 revealKeep，用于显示令牌的状态。
type enrollReveals struct {
	// gen 串行化"检查是否已生成 → 生成 → 保存"（见 ensure）：并发重复提交同一个表单时只生成一个令牌。
	gen sync.Mutex
	mu  sync.Mutex
	m   map[string]*enrollReveal
}

// revealKey 是条目的键：不同用户即使用了相同的请求 ID 也互不影响。
func revealKey(id, login string) string { return login + "\x00" + id }

const (
	// enrollCreateTimeout 是生成一个注册令牌（写入令牌和审计）的最长时间，不受请求取消影响。
	enrollCreateTimeout = time.Minute
	revealKeep          = time.Hour
	// maxReveals 是内存中保存的条目上限，超过时先清理最早生成的。
	maxReveals = 256
)

func newEnrollReveals() *enrollReveals { return &enrollReveals{m: map[string]*enrollReveal{}} }

// prune 清理过期的条目；调用方持有锁。
func (s *enrollReveals) prune(now time.Time) {
	for id, v := range s.m {
		if now.After(v.expires.Add(revealKeep)) {
			delete(s.m, id)
		}
	}
	for len(s.m) >= maxReveals {
		var oldest string
		for id, v := range s.m {
			if oldest == "" || v.created.Before(s.m[oldest].created) {
				oldest = id
			}
		}
		delete(s.m, oldest)
	}
}

// has 判断 login 是否已经用请求 ID id 生成过令牌。
func (s *enrollReveals) has(id, login string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[revealKey(id, login)] != nil
}

func (s *enrollReveals) put(id string, v *enrollReveal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	s.m[revealKey(id, v.login)] = v
}

// ensure 保证 login 的请求 ID id 只生成一个令牌：已经生成过时直接返回；否则调用 create 生成并保存。
// 检查、生成和保存在同一把锁（gen）内完成，并发的重复提交等待第一个完成后复用它的结果。create 出错时不保存，可以用同一个请求 ID 重试。
func (s *enrollReveals) ensure(id, login string, create func() (*enrollReveal, error)) error {
	s.gen.Lock()
	defer s.gen.Unlock()
	if s.has(id, login) {
		return nil
	}
	v, err := create()
	if err != nil {
		return err
	}
	v.login = login
	s.put(id, v)
	return nil
}

// take 返回 login 用请求 ID id 生成的令牌的副本，并清空保存的令牌：令牌只返回一次。
func (s *enrollReveals) take(id, login string) (enrollReveal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.m[revealKey(id, login)]
	if v == nil {
		return enrollReveal{}, false
	}
	c := *v
	v.token = ""
	return c, true
}

// enrollPools 返回已有的 pool（含 default），供表单提示。
func (d *Dashboard) enrollPools(ctx context.Context) ([]string, error) {
	rows, err := d.db.Query(ctx, `SELECT pool FROM ci_runners UNION SELECT $1::text ORDER BY 1`, store.DefaultPool)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (d *Dashboard) newRunnerPage(ctx context.Context, f *formState) (*runnerNewPage, error) {
	pools, err := d.enrollPools(ctx)
	if err != nil {
		return nil, err
	}
	return &runnerNewPage{
		Form: f, RequestID: newRequestID(), RegenID: newRequestID(), PublicURL: d.cfg.PublicURL,
		ServerVersion: buildinfo.Version(), Pools: pools,
	}, nil
}

// renderRunnerNew 渲染注册页面；页面可能含有一次性令牌，不允许缓存。
func (d *Dashboard) renderRunnerNew(w http.ResponseWriter, r *http.Request, p *runnerNewPage, status int) {
	w.Header().Set("Cache-Control", "no-store")
	d.renderPage(w, r, "runner_new", message(msgRunnerNewTitle), p, status)
}

// pageRunnerNew 是注册新的 Runner 的第一步：准备条件和生成令牌的表单。地址中的 pool、name、kind 用于预填（例如重新注册被吊销的
// Runner；kind 取注册表单的取值 ci、ai、ai-dev）。
func (d *Dashboard) pageRunnerNew(w http.ResponseWriter, r *http.Request, _ *session) {
	f := newForm()
	q := r.URL.Query()
	for _, k := range []string{"pool", "name"} {
		if v := strings.TrimSpace(q.Get(k)); enrollNamePattern.MatchString(v) {
			f.Values[k] = v
		}
	}
	if v := q.Get("kind"); v != "" {
		if _, _, ok := runnerKindRole(v); ok {
			f.Values["kind"] = v
		}
	}
	p, err := d.newRunnerPage(r.Context(), f)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.renderRunnerNew(w, r, p, http.StatusOK)
}

// enrolledRunner 返回满足条件（ID 或名称）的 Runner：CI Runner，或换成同样摘要的 AI 执行节点（Runner 名称在两种 Runner 中唯一）；
// 不存在时返回 nil。
func (d *Dashboard) enrolledRunner(ctx context.Context, f store.RunnerFilter) (*runnerView, error) {
	list, err := d.loadRunners(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		return list[0], nil
	}
	var id int64
	err = d.db.QueryRow(ctx, `SELECT id FROM runners WHERE kind=$1 AND ($2::bigint=0 OR id=$2) AND ($3::text='' OR name=$3)`,
		store.RunnerKindAI, f.ID, f.Name).Scan(&id)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ai, err := d.loadAIRunners(ctx, id)
	if err != nil || len(ai) == 0 {
		return nil, err
	}
	return ai[0].asRunnerView(), nil
}

// pageRunnerEnroll 是生成令牌后的一次性展示页：第一次打开时显示令牌和命令，之后（刷新、返回）只显示令牌的状态，
// 不再显示令牌，也不会重新生成。
func (d *Dashboard) pageRunnerEnroll(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	id := r.PathValue("id")
	p, err := d.newRunnerPage(ctx, newForm())
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	rv, ok := enrollReveal{}, false
	if requestIDPattern.MatchString(id) {
		rv, ok = d.reveals.take(id, se.Login)
	}
	if !ok {
		p.Missing = true
		d.renderRunnerNew(w, r, p, http.StatusNotFound)
		return
	}
	v := &enrollView{ID: id, Token: rv.token, Pool: rv.pool, Name: rv.name, Kind: rv.kind, Role: rv.role, Created: rv.created, Expires: rv.expires}
	var runnerID *int64
	err = d.db.QueryRow(ctx, `SELECT expires_at, used_at, runner_id FROM enrollment_tokens WHERE token_hash=$1`, rv.tokenHash).
		Scan(&v.Expires, &v.UsedAt, &runnerID)
	// 过期的令牌按保留策略删除（retention），可能早于这里的条目（保留到过期后 revealKeep）：按已过期显示
	gone := isNotFound(err)
	if err != nil && !gone {
		d.serverError(w, r, err)
		return
	}
	switch {
	case gone:
		v.Status = enrollExpired
	case v.UsedAt != nil:
		v.Status = enrollUsed
	case !v.Expires.After(time.Now()):
		v.Status = enrollExpired
	default:
		v.Status = enrollUnused
	}
	if v.Status != enrollUnused {
		v.Token = "" // 已使用或已过期的令牌不再有用，不显示
	}
	switch {
	case runnerID != nil:
		if v.Runner, err = d.enrolledRunner(ctx, store.RunnerFilter{ID: *runnerID}); err != nil {
			d.serverError(w, r, err)
			return
		}
	case v.Status == enrollUnused && v.Name != "":
		if v.Existing, err = d.enrolledRunner(ctx, store.RunnerFilter{Name: v.Name}); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	p.Reveal = v
	p.Form.Values["pool"], p.Form.Values["name"] = v.Pool, v.Name
	d.renderRunnerNew(w, r, p, http.StatusOK)
}
