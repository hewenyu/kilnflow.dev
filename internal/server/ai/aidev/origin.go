package aidev

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 开发 PR 的永久归属（PLAN #29.4、#31.3）：Server 创建或核验过的正式 PR 记在 dev_pull_origins，自动打 tag 据此判断
// 一次合并是否来自可信的 AI 开发实现工作项。归属只按 Server 自己写下的事实判断：分支名像正式分支、PR 作者是机器人
// 都不单独作为依据；第三方在正式分支上开的 PR 不会被提升为可信来源。

// AutoTagOriginKind 是一个 PR 的来源类别（自动打 tag 的判定用）。
type AutoTagOriginKind int

const (
	// AutoTagOriginUnknown：没有可信的 AI 开发工作项归属。
	AutoTagOriginUnknown AutoTagOriginKind = iota
	// AutoTagOriginImplementation：可信的实现 PR，可以自动发布。
	AutoTagOriginImplementation
	// AutoTagOriginDesign：设计 PR，不打版本 tag。
	AutoTagOriginDesign
	// AutoTagOriginRecording：工作项的 PR 创建归属还没有确认，等 Server 记下之后可以重试。
	AutoTagOriginRecording
	// AutoTagOriginDenied：有归属但状态、证据或分支不满足自动发布。
	AutoTagOriginDenied
)

// AutoTagOrigin 是一个 PR 的来源及其依据。
type AutoTagOrigin struct {
	Kind           AutoTagOriginKind
	WorkItemID     int64
	Phase          string
	ImplRound      int
	BaseRef        string
	CreatorLogin   string
	DeliveryTaskID int64
	Issues         []int
	// Reason 是不自动发布的中文说明；ReasonCode 是同一个原因的原因码（AutoTagReason*，没有原因时为空），由组装层换成自动打
	// tag 的原因码（Dashboard 按语言显示）。
	Reason     string
	ReasonCode string
}

// AutoTagOrigin.ReasonCode 的取值。
const (
	// AutoTagReasonDesign：工作项的设计 PR。
	AutoTagReasonDesign = "design"
	// AutoTagReasonChecking、AutoTagReasonUnconfirmed：PR 创建归属正在核验、尚未确认（推进中，编号还没记下）。
	AutoTagReasonChecking    = "checking"
	AutoTagReasonUnconfirmed = "unconfirmed"
	// AutoTagReasonHeadMismatch、AutoTagReasonBaseMismatch：PR 的来源分支、目标分支与工作项的记录不一致。
	AutoTagReasonHeadMismatch = "head_mismatch"
	AutoTagReasonBaseMismatch = "base_mismatch"
	// AutoTagReasonStopped、AutoTagReasonNoChange、AutoTagReasonNoRecord：工作项已终止、没有改动、缺少领取或交付记录。
	AutoTagReasonStopped  = "stopped"
	AutoTagReasonNoChange = "no_change"
	AutoTagReasonNoRecord = "no_record"
)

// pullOrigin 是要写入 dev_pull_origins 的一条归属。
type pullOrigin struct {
	RepoID         int64
	PR             int
	WorkItemID     int64
	Phase          string
	ImplRound      int
	HeadRef        string
	BaseRef        string
	CreatorLogin   string
	DeliveryTaskID *int64
	DeliverySHA    string
}

// recordPullOrigin 在事务 tx 中记下 PR 的归属。返修推进同一个 PR 时只更新交付引用，身份列（工作项、阶段、轮次、分支）
// 必须与已有记录一致，不一致时什么也不改。
func recordPullOrigin(ctx context.Context, tx pgx.Tx, o pullOrigin) error {
	_, err := tx.Exec(ctx, `INSERT INTO dev_pull_origins (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref,
		creator_login, delivery_task_id, delivery_sha) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (repo_id, pr_number) DO UPDATE SET delivery_task_id=EXCLUDED.delivery_task_id,
			delivery_sha=EXCLUDED.delivery_sha, updated_at=now()
		WHERE dev_pull_origins.work_item_id=EXCLUDED.work_item_id AND dev_pull_origins.phase=EXCLUDED.phase
			AND dev_pull_origins.impl_round=EXCLUDED.impl_round AND dev_pull_origins.head_ref=EXCLUDED.head_ref
			AND dev_pull_origins.base_ref=EXCLUDED.base_ref`,
		o.RepoID, o.PR, o.WorkItemID, o.Phase, o.ImplRound, o.HeadRef, o.BaseRef, o.CreatorLogin, o.DeliveryTaskID, o.DeliverySHA)
	if err != nil {
		return fmt.Errorf("记下 PR #%d 的 AI 开发归属：%w", o.PR, err)
	}
	return nil
}

// SelfCheckFunc 判断 login 是否是本 App 的 bot 账号。ready 为假表示 GitHub App 的身份还没有加载出来（启动时读取失败、
// 后台还在重试），此时无法判断作者是谁，调用方必须保留现场、稍后重试。
type SelfCheckFunc func(login string) (self, ready bool)

// errAppIdentityNotReady 表示 GitHub App 身份尚未加载，暂时无法判断 PR 的作者是不是本 App。绝不能把“还不知道”当成
// “不是本 App”：那样会删掉合法的待核验记录、把工作项误转为受阻、丢掉归属。
var errAppIdentityNotReady = errors.New("GitHub App 身份尚未加载，稍后重试")

// verifiedPull 判断平台返回的 PR 是否是工作项本阶段、本轮由本 App 创建的正式 PR：同一仓库、来源分支和目标分支都对得上，
// 作者是本 App 的 bot 账号。不满足时绝不记为可信发布来源；身份未就绪时返回 errAppIdentityNotReady。
func verifiedPull(w *WorkItem, pr *forge.Pull, checkSelf SelfCheckFunc) (bool, error) {
	return verifiedPullRefs(pr, w.Branch(), w.BaseRef, checkSelf)
}

// verifiedPullRefs 与 verifiedPull 相同，身份分支和目标分支由调用方给出：核验 dev_pull_origin_checks 中冻结的身份时，
// 工作项可能已经换了阶段、轮次（正式分支随之改变），必须按冻结的记录核验。
func verifiedPullRefs(pr *forge.Pull, headRef, baseRef string, checkSelf SelfCheckFunc) (bool, error) {
	// fork、分支对不上与 App 身份无关，可以直接判定不是可信来源
	if pr == nil || pr.Fork || pr.HeadRef != headRef || pr.BaseRef != baseRef || pr.Author == "" {
		return false, nil
	}
	if checkSelf == nil {
		return false, errAppIdentityNotReady
	}
	self, ready := checkSelf(pr.Author)
	if !ready {
		return false, errAppIdentityNotReady
	}
	return self, nil
}

// pullOriginCheck 是一条还待核验的 PR 创建归属（dev_pull_origin_checks 的一行）：创建 PR 的响应丢失、合并事件先到时，
// 合并事件趁换阶段、换轮次改写身份之前冻结工作项当时的阶段、轮次、分支和交付引用。
type pullOriginCheck struct {
	RepoID         int64
	PR             int
	WorkItemID     int64
	Phase          string
	ImplRound      int
	HeadRef        string
	BaseRef        string
	DeliveryTaskID *int64
	DeliverySHA    string
}

// queryPullOriginChecks 返回仓库 repoID 待核验的 PR 创建归属，按 PR 编号升序。
func queryPullOriginChecks(ctx context.Context, q store.Querier, repoID int64) ([]pullOriginCheck, error) {
	rows, err := q.Query(ctx, `SELECT repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref, delivery_task_id, delivery_sha
		FROM dev_pull_origin_checks WHERE repo_id=$1 ORDER BY pr_number`, repoID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (pullOriginCheck, error) {
		var c pullOriginCheck
		return c, r.Scan(&c.RepoID, &c.PR, &c.WorkItemID, &c.Phase, &c.ImplRound, &c.HeadRef, &c.BaseRef, &c.DeliveryTaskID, &c.DeliverySHA)
	})
}

// recordPullOriginCheck 在事务 tx 中冻结工作项 w 当前阶段、轮次的 PR 创建归属，等执行者经平台核验后补记永久归属。
// 同一个 PR 已有待核验记录时什么也不改。
func recordPullOriginCheck(ctx context.Context, tx pgx.Tx, w *WorkItem, pr int) error {
	_, err := tx.Exec(ctx, `INSERT INTO dev_pull_origin_checks (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref,
		delivery_task_id, delivery_sha) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (repo_id, pr_number) DO NOTHING`,
		w.RepoID, pr, w.ID, w.Phase, w.ImplRound, w.Branch(), w.BaseRef, w.DeliveryTaskID, w.DeliverySHA)
	if err != nil {
		return fmt.Errorf("记下 PR #%d 的待核验归属：%w", pr, err)
	}
	return nil
}

// resolveOriginSQL 是来源判定的全部依据：永久归属（连同工作项状态）、设计阶段的待核验记录、历史的 design_pr、非设计的
// 待核验记录（归属正在核验）、推进中（delivering、编号还没记下）的工作项、当前设计 PR 的兜底，一次读出优先级最高的一条。
//
// 必须是一条语句：PostgreSQL 里一条语句在同一个一致的 MVCC 快照上执行。拆成多条 SELECT 时，合并事件的处理（aidev 的
// HandlePullClosed 持有另一把仓库锁）可能在两次查询之间提交，把设计阶段的 delivering 工作项换成实现阶段、写下 design_pr
// 和待核验记录——前一次查询看到旧状态、后一次查询看到新状态，设计 PR 就会被误判为未知来源、被人合并时误打 tag 并发布。
// 参数：$1 仓库、$2 PR、$3/$4/$5 由 headRef 解析出的工作项、阶段、轮次（解析不出时 id=0、round=-1，不会
// 匹配任何工作项）。$3 显式转成 bigint：否则 PostgreSQL 会按整数常量 0 把它推导成 int4，之后 w.id=$3 不会把它扩大为
// bigint，超过 MaxInt32 的合法工作项编号（如 kiln/dev-2147483648）在 pgx 编码时就会失败、让 auto_tags 卡在 pending 重试。
const resolveOriginSQL = `
WITH candidates AS (
	SELECT 1 AS priority, o.work_item_id, o.phase, o.impl_round, o.head_ref, o.base_ref, o.creator_login,
		o.delivery_task_id, o.delivery_sha, w.state, w.claim_attempt_id
	FROM dev_pull_origins o JOIN dev_work_items w ON w.id=o.work_item_id
	WHERE o.repo_id=$1 AND o.pr_number=$2
	UNION ALL
	SELECT 2, c.work_item_id, c.phase, c.impl_round, '', '', '', NULL, '', '', 0
	FROM dev_pull_origin_checks c WHERE c.repo_id=$1 AND c.pr_number=$2 AND c.phase='design'
	UNION ALL
	-- design_pr 只在这个工作项从 design 进入 implement 时写入，此时 w.phase 已经是 implement、impl_round>=1；这个 PR 在设计阶段
	-- 合并，身份固定为 design / 第 0 轮，直接用常量，别把当前工作项的 phase、round 读出来拼出自相矛盾的结果。
	SELECT 3, w.id, 'design', 0, '', '', '', NULL, '', '', 0
	FROM dev_work_items w WHERE w.repo_id=$1 AND w.design_pr=$2
	UNION ALL
	SELECT 4, c.work_item_id, c.phase, c.impl_round, '', '', '', NULL, '', '', 0
	FROM dev_pull_origin_checks c WHERE c.repo_id=$1 AND c.pr_number=$2 AND c.phase<>'design'
	UNION ALL
	SELECT 5, w.id, w.phase, w.impl_round, '', '', '', NULL, '', '', 0
	FROM dev_work_items w
	WHERE $3::bigint<>0 AND w.repo_id=$1 AND w.id=$3 AND w.active AND w.state='delivering' AND w.pr_number IS NULL
		AND w.phase=$4 AND w.impl_round=$5
	UNION ALL
	SELECT 6, w.id, w.phase, w.impl_round, '', '', '', NULL, '', '', 0
	FROM dev_work_items w WHERE w.repo_id=$1 AND w.pr_number=$2 AND w.phase='design'
)
SELECT c.priority, c.work_item_id, c.phase, c.impl_round, c.head_ref, c.base_ref, c.creator_login, c.delivery_task_id,
	c.delivery_sha, c.state, c.claim_attempt_id,
	COALESCE(ARRAY(SELECT i.issue_number FROM dev_work_item_issues i WHERE i.work_item_id=c.work_item_id ORDER BY i.issue_number), '{}')
FROM candidates c ORDER BY c.priority LIMIT 1`

// ResolveAutoTagOrigin 判断仓库 repoID 的 PR pr 是否可以自动打版本 tag（只读）。headRef、baseRef 是合并事件中的分支
// （未知时为空，不做对应的一致性检查）。必须使用调用方传入的 q：自动打 tag 的执行者持有仓库锁，池上限为 1 时再借连接
// 会死锁。判定只发起一条 SQL 语句（见 resolveOriginSQL），在一次一致的快照上完成。
func ResolveAutoTagOrigin(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (AutoTagOrigin, error) {
	b, ok := devreport.ParseWorkItemBranch(headRef)
	if !ok {
		b = devreport.BranchRef{Round: -1} // id=0、阶段为空：不匹配任何工作项
	}
	var (
		o          AutoTagOrigin
		priority   int
		storedHead string
		state      string
		claim      int64
		deliveryID *int64
		delivery   string
		issues     []int32
	)
	err := q.QueryRow(ctx, resolveOriginSQL, repoID, pr, b.ID, b.Phase, b.Round).
		Scan(&priority, &o.WorkItemID, &o.Phase, &o.ImplRound, &storedHead, &o.BaseRef, &o.CreatorLogin, &deliveryID,
			&delivery, &state, &claim, &issues)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutoTagOrigin{Kind: AutoTagOriginUnknown, Reason: "没有可信的 AI 开发工作项归属"}, nil
	}
	if err != nil {
		return AutoTagOrigin{}, err
	}
	o.Issues = make([]int, len(issues))
	for i, n := range issues {
		o.Issues[i] = int(n)
	}
	if priority != 1 {
		// 没有永久归属：设计 PR（待核验记录冻结的设计阶段、历史或当前的 design_pr）不打 tag——绝不能把已知的设计身份遮成
		// “归属正在核验”；非设计的待核验记录、推进中的工作项返回 Recording，等执行者核验、Server 记下之后再重试。
		switch priority {
		case 2, 3, 6:
			o.Kind, o.ReasonCode = AutoTagOriginDesign, AutoTagReasonDesign
			o.Reason = designReason(o.WorkItemID)
		case 4:
			o.Kind, o.ReasonCode = AutoTagOriginRecording, AutoTagReasonChecking
			o.Reason = fmt.Sprintf("AI 开发工作项 #%d 的 PR 创建归属正在核验，确认后可重试", o.WorkItemID)
		default: // 5：工作项已经建了 PR、编号还没记下（推进中）
			o.Kind, o.ReasonCode = AutoTagOriginRecording, AutoTagReasonUnconfirmed
			o.Reason = fmt.Sprintf("AI 开发工作项 #%d 的 PR 创建归属尚未确认，确认后可重试", o.WorkItemID)
		}
		return o, nil
	}
	if deliveryID != nil {
		o.DeliveryTaskID = *deliveryID
	}
	if o.Phase == PhaseDesign {
		o.Kind, o.ReasonCode = AutoTagOriginDesign, AutoTagReasonDesign
		o.Reason = designReason(o.WorkItemID)
		return o, nil
	}
	if headRef != "" && storedHead != headRef {
		o.Kind, o.ReasonCode = AutoTagOriginDenied, AutoTagReasonHeadMismatch
		o.Reason = fmt.Sprintf("PR 的来源分支与 AI 开发工作项 #%d 的记录不一致，不自动发布", o.WorkItemID)
		return o, nil
	}
	if baseRef != "" && o.BaseRef != baseRef {
		o.Kind, o.ReasonCode = AutoTagOriginDenied, AutoTagReasonBaseMismatch
		o.Reason = fmt.Sprintf("PR 的目标分支与 AI 开发工作项 #%d 的记录不一致，不自动发布", o.WorkItemID)
		return o, nil
	}
	switch {
	case state == StateStopped:
		o.Kind, o.ReasonCode = AutoTagOriginDenied, AutoTagReasonStopped
		o.Reason = fmt.Sprintf("AI 开发工作项 #%d 已终止，不自动发布", o.WorkItemID)
	case state == StateNoChange:
		o.Kind, o.ReasonCode = AutoTagOriginDenied, AutoTagReasonNoChange
		o.Reason = fmt.Sprintf("AI 开发工作项 #%d 没有改动，不自动发布", o.WorkItemID)
	case claim == 0 || (deliveryID == nil && delivery == ""):
		o.Kind, o.ReasonCode = AutoTagOriginDenied, AutoTagReasonNoRecord
		o.Reason = fmt.Sprintf("AI 开发工作项 #%d 缺少领取或交付记录，不自动发布", o.WorkItemID)
	default:
		o.Kind = AutoTagOriginImplementation
	}
	return o, nil
}

// designReason 是设计 PR 不自动发布的原因。
func designReason(id int64) string {
	return fmt.Sprintf("这是 AI 开发工作项 #%d 的设计 PR，不打版本 tag", id)
}
