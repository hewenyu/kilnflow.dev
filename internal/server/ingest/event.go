package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 事件的分派与过滤（PLAN #7.2、#16.1）。

// 除 push、pull_request（workflow.EventPush、workflow.EventPullRequest）以外要处理的 webhook 事件（X-GitHub-Event）。
const (
	eventPing                     = "ping"
	eventInstallation             = "installation"
	eventInstallationRepositories = "installation_repositories"
	eventRepository               = "repository"
	eventCheckRun                 = "check_run"
	eventCheckSuite               = "check_suite"
	// eventIssues 是 Issue 的 label、内容、状态变化（AI 开发，PLAN #26.11）。
	eventIssues = "issues"
)

// 要处理的 webhook action（payload 中的 action）。
const (
	actionOpened         = "opened"
	actionSynchronize    = "synchronize"
	actionReopened       = "reopened"
	actionReadyForReview = "ready_for_review"
	actionEdited         = "edited"
	actionClosed         = "closed"
	actionRerequested    = "rerequested"
	actionCreated        = "created"
	actionDeleted        = "deleted"
	actionRenamed        = "renamed"
	actionTransferred    = "transferred"
	// 合并队列关心的 PR action（PLAN #28.3）：label 的增减（kiln:merge 入队、出队）和转为 draft。
	actionLabeled          = "labeled"
	actionUnlabeled        = "unlabeled"
	actionConvertedToDraft = "converted_to_draft"
)

type ghSender struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghRepoRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Owner    struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"owner"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
}

type eventHead struct {
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository *ghRepoRef `json:"repository"`
	Sender     ghSender   `json:"sender"`
	// Deleted 是 push 事件中 ref 是否被删除。
	Deleted bool `json:"deleted"`
	// Changes 是 edited 事件中被修改的字段。
	Changes eventChanges `json:"changes"`
}

// eventChanges 是 edited 事件中被修改的字段（只关心是否存在）：pull_request 的 base，repository 的 default_branch。
type eventChanges struct {
	Base          *json.RawMessage `json:"base"`
	DefaultBranch *json.RawMessage `json:"default_branch"`
}

// repoNeed 是事件对仓库的要求。
type repoNeed int

const (
	// needNone：与构建无关的事件（例如本 App 自己的 check_run.created / completed），记录后忽略，不读取 GitHub。
	needNone repoNeed = iota
	// needKnown：只处理 Kiln 中已有的仓库（取消、改名、重跑等）；仓库不在 Kiln 中时直接忽略，不读取 GitHub。
	needKnown
	// needAny：可能触发构建；第一次出现的仓库定向校验后接入（不要求用户先刷新仓库）。
	needAny
)

// prTriggersBuild 判断 pull_request 事件的 action 是否触发构建：打开、推送、重新打开、转为 ready，
// 以及修改了目标分支（baseChanged）的 edited。
func prTriggersBuild(action string, baseChanged bool) bool {
	switch action {
	case actionOpened, actionSynchronize, actionReopened, actionReadyForReview:
		return true
	case actionEdited:
		return baseChanged
	}
	return false
}

// isQueueAction 判断 pull_request 事件的 action 是否只与合并队列有关（不触发构建）。
func isQueueAction(action string) bool {
	return action == actionLabeled || action == actionUnlabeled || action == actionConvertedToDraft
}

// isRepoMoved 判断 repository 事件的 action 是否是改名或转移（只需要更新仓库的 owner 和 name）。
func isRepoMoved(action string) bool {
	return action == actionRenamed || action == actionTransferred
}

// isDefaultBranchChanged 判断 repository 事件是否修改了默认分支（edited 且 changes.default_branch 存在）。
func isDefaultBranchChanged(action string, head *eventHead) bool {
	return action == actionEdited && head.Changes.DefaultBranch != nil
}

// repoNeedOf 返回事件对仓库的要求。
func repoNeedOf(typ, action string, head *eventHead) repoNeed {
	switch typ {
	case workflow.EventPush:
		if head.Deleted {
			return needKnown
		}
		return needAny
	case workflow.EventPullRequest:
		if prTriggersBuild(action, head.Changes.Base != nil) {
			return needAny
		}
		if action == actionClosed || isQueueAction(action) {
			return needKnown
		}
	case eventCheckRun, eventCheckSuite:
		if action == actionRerequested {
			return needKnown
		}
	case eventIssues:
		switch action {
		case actionOpened, actionLabeled, actionUnlabeled, actionEdited, actionClosed, actionReopened:
			return needKnown // 新建时就带 kiln:ready 的 Issue 只有 opened 事件
		}
	case eventRepository:
		if isRepoMoved(action) || isDefaultBranchChanged(action, head) {
			return needKnown
		}
	}
	return needNone
}

// processEvent 处理一个事件；delivery 是它的 X-GitHub-Delivery，重跑请求以它作为幂等键（同一个事件重试时不重复重跑）。
// 只读取这个事件需要的信息：与构建无关的事件不请求 GitHub；本地没有的 installation、第一次出现的仓库
// 各用一个定向请求校验，不列出全部 installation 或仓库。
func (i *Ingester) processEvent(ctx context.Context, delivery, typ, action string, payload []byte) (string, error) {
	var head eventHead
	if err := json.Unmarshal(payload, &head); err != nil {
		return dropEvent("payload 无法解析，忽略：" + err.Error())
	}
	switch typ {
	case eventPing:
		return "ping", nil
	case eventInstallation, eventInstallationRepositories:
		return i.handleInstallationEvent(ctx, typ, action, payload)
	case eventRepository:
		if action == actionDeleted {
			return i.handleRepositoryDeleted(ctx, &head)
		}
	}
	need := repoNeedOf(typ, action, &head)
	if need == needNone {
		return "", nil
	}
	if head.Installation.ID == 0 {
		return "没有 installation，忽略", nil
	}
	if head.Repository == nil {
		return "没有仓库，忽略", nil
	}
	if need == needKnown {
		// 先看本地：仓库不在 Kiln 中时这类事件没有要处理的东西，不必校验 installation
		if repo, err := i.knownRepo(ctx, head.Installation.ID, head.Repository.ID); err != nil || repo == nil {
			return "仓库不在 Kiln 中，忽略", err
		}
	}
	// 过滤 1：installation 所属账号必须在白名单中（PLAN #16.1）
	if reason, err := i.installationAllowed(ctx, head.Installation.ID); err != nil || reason != "" {
		return reason, err
	}
	// 过滤 3：仓库必须在 installation 的授权范围内
	repo, reason, err := i.resolveRepo(ctx, head.Installation.ID, head.Repository, need == needAny)
	if err != nil || repo == nil {
		return reason, err
	}
	// 过滤 2：本 App 的 bot 账号推送的 tag 只更新 ref_heads（PLAN #7.2）；本 App 的分支 push 和 PR 事件
	// （合并队列更新 PR 分支、合并）照常规划，不算 Bot（PLAN #28.4）
	isSelf := i.isSelf(head.Sender.Login)
	switch typ {
	case workflow.EventPush:
		return i.handlePush(ctx, delivery, repo, payload, isSelf)
	case workflow.EventPullRequest:
		return i.handlePullRequest(ctx, delivery, repo, action, payload, isSelf)
	case eventCheckRun:
		return i.handleCheckRunRerequest(ctx, delivery, repo, payload, head.Sender.Login)
	case eventCheckSuite:
		return i.handleCheckSuiteRerequest(ctx, delivery, repo, payload, head.Sender.Login)
	case eventRepository:
		return i.handleRepositoryEvent(ctx, repo, action, &head)
	case eventIssues:
		return i.handleIssues(ctx, repo, action, payload, isSelf)
	}
	return "", nil
}

// knownRepo 返回 Kiln 中属于 installation instID、启用中的仓库；没有时返回 nil。
func (i *Ingester) knownRepo(ctx context.Context, instID, repoID int64) (*store.Repo, error) {
	repo, err := store.GetRepo(ctx, i.db, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !repo.Active || repo.InstallationID != instID {
		return nil, nil
	}
	return repo, nil
}

// resolveRepo 返回事件所属的仓库；不处理时返回 nil 和原因。
// Kiln 中已有（属于这个 installation、启用中）时直接使用，缺少的默认分支和 clone 地址用 payload 补上；
// 否则 discover 为 true 时定向确认这一个仓库在 installation 的授权范围内（repodir.Verify：私有仓库用 installation token 读取，
// 公开仓库再定向查询它所属的 installation）才记录并处理；已删除、已移出 installation 或属于别的 installation 的仓库，事件丢弃，
// 不会因为晚到或重复的事件恢复它。
func (i *Ingester) resolveRepo(ctx context.Context, instID int64, r *ghRepoRef, discover bool) (*store.Repo, string, error) {
	repo, err := i.knownRepo(ctx, instID, r.ID)
	if err != nil {
		return nil, "", err
	}
	if repo != nil {
		if repo.Incomplete() && r.CloneURL != "" && r.DefaultBranch != "" {
			m := store.RepoMeta{
				ID: repo.ID, InstallationID: instID, OwnerID: repo.OwnerID, Owner: repo.Owner, Name: repo.Name, Private: repo.Private,
				DefaultBranch: r.DefaultBranch, CloneURL: r.CloneURL,
			}
			if err := store.UpsertRepo(ctx, i.db, m); err != nil {
				return nil, "", err
			}
			repo.DefaultBranch, repo.CloneURL = r.DefaultBranch, r.CloneURL
		}
		return repo, "", nil
	}
	if !discover {
		return nil, "仓库不在 Kiln 中，忽略", nil
	}
	gr, err := repodir.Verify(ctx, i.gh, instID, r.ID)
	if repodir.NotInstalled(err) {
		i.log.Info("仓库不在 installation 的授权范围内，丢弃事件", "repo", r.FullName, "installation", instID, "reason", err)
		return nil, "仓库不在 installation 的授权范围内（或已删除），丢弃", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("校验仓库 %s：%w", r.FullName, err)
	}
	if err := store.UpsertRepo(ctx, i.db, repodir.MetaOf(instID, gr)); err != nil {
		return nil, "", err
	}
	i.log.Info("第一次收到仓库的事件，定向校验后记录到 Kiln", "repo", gr.FullName, "installation", instID)
	repo, err = store.GetRepo(ctx, i.db, r.ID)
	if err != nil {
		return nil, "", err
	}
	return repo, "", nil
}
