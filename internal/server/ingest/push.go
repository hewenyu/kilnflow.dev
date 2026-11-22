package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// push 和 pull_request 事件：规划 run、记录 ref_heads，删除分支和关闭 PR 时取消 run。

// setRefHead 记录 ref 的最新位置（sha 为空表示 ref 已删除）。base 是 PR 的 head 按哪个目标分支规划过（只用于 PR ref，
// 没有规划过时为空；合并队列据此判断验证结果是否完整，PLAN #28.3）。失败时只记录日志：对账时会再次比较并补上。
func (i *Ingester) setRefHead(ctx context.Context, repoID int64, ref, sha, object, base string) {
	var err error
	if sha == "" || sha == gitref.ZeroSHA {
		_, err = i.db.Exec(ctx, `DELETE FROM ref_heads WHERE repo_id=$1 AND ref=$2`, repoID, ref)
	} else {
		_, err = i.db.Exec(ctx, `INSERT INTO ref_heads (repo_id, ref, sha, object_sha, base_ref) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (repo_id, ref) DO UPDATE SET sha=EXCLUDED.sha, object_sha=EXCLUDED.object_sha, base_ref=EXCLUDED.base_ref, updated_at=now()`,
			repoID, ref, sha, object, base)
	}
	if err != nil {
		i.log.Warn("更新 ref_heads", "repo", repoID, "ref", ref, "err", err)
	}
}

// cancelRuns 在各自的事务中取消一组 run。
func (i *Ingester) cancelRuns(ctx context.Context, ids []int64, reason store.CancelReason) error {
	for _, id := range ids {
		if err := i.db.Tx(ctx, func(tx pgx.Tx) error { return i.engine.CancelRun(ctx, tx, id, reason) }); err != nil {
			return err
		}
	}
	i.hub.All()
	return nil
}

func (i *Ingester) handlePush(ctx context.Context, delivery string, repo *store.Repo, payload []byte, isSelf bool) (string, error) {
	var p struct {
		Ref     string   `json:"ref"`
		Before  string   `json:"before"`
		After   string   `json:"after"`
		Deleted bool     `json:"deleted"`
		Sender  ghSender `json:"sender"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析：" + err.Error())
	}
	if stale, err := i.pushSuperseded(ctx, delivery, repo.ID, p.Ref, p.After); err != nil {
		return "", err
	} else if stale {
		return supersededResult, nil
	}
	tag, isTag := strings.CutPrefix(p.Ref, workflow.RefPrefixTag)
	if p.Deleted {
		i.setRefHead(ctx, repo.ID, p.Ref, "", "", "")
		if isTag {
			return "tag 已删除", nil
		}
		ids, err := store.QueryIDs(ctx, i.db, `SELECT id FROM runs WHERE repo_id=$1 AND ref=$2 AND event='push' AND state IN ('active','queued')`, repo.ID, p.Ref)
		if err != nil {
			return "", err
		}
		if err := i.cancelRuns(ctx, ids, store.CancelRefDeleted); err != nil {
			return "", err
		}
		// 分支删除（任何分支）也可能影响排队中的 PR：让执行者按最新的目标分支重新判断
		if err := i.notifyQueue(ctx, repo.ID, 0); err != nil {
			return "", err
		}
		return fmt.Sprintf("分支已删除，取消 %d 个 run", len(ids)), nil
	}
	sha, object := p.After, p.After
	if isTag {
		var err error
		sha, object, err = i.gh.Inst(repo.InstallationID).ResolveTag(ctx, repo.FullName(), tag)
		if gh.IsNotFound(err) {
			return "tag 已不存在", nil
		}
		if err != nil {
			return "", fmt.Errorf("解析 tag：%w", err)
		}
	}
	if isSelf && isTag {
		i.setRefHead(ctx, repo.ID, p.Ref, sha, object, "")
		return "本 App 创建的 tag，只更新 ref_heads", nil
	}
	ev := &planner.Request{
		Repo: repo, Event: workflow.EventPush, Ref: p.Ref, SHA: sha, SenderLogin: p.Sender.Login,
		SenderIsBot: p.Sender.Type == senderTypeBot && !isSelf, Source: planner.SourceWebhook,
	}
	if !isTag {
		before, err := i.pushChangesBase(ctx, delivery, repo.ID, p.Ref, p.Before)
		if err != nil {
			return "", err
		}
		ev.Before = before
	}
	n, err := i.planEvent(ctx, ev)
	if err != nil {
		// 不更新 ref_heads，对账时会重试
		return "", err
	}
	i.setRefHead(ctx, repo.ID, p.Ref, sha, object, "")
	if strings.HasPrefix(p.Ref, workflow.RefPrefixBranch) {
		// 任何分支前进了：队列中的 PR（无论目标是哪条允许合并的分支）都可能落后了。队列为空时 KickMergeQueue 的
		// EXISTS 守卫不会写通知，也不会请求平台（PLAN #28.5）
		if err := i.notifyQueue(ctx, repo.ID, 0); err != nil {
			return "", err
		}
	}
	return n.String(), nil
}

// joinResult 把附加的说明接到事件结果后面（附加为空时不变）。
func joinResult(result, extra string) string {
	if extra == "" {
		return result
	}
	return result + "；" + extra
}

// handleIssues 处理 issues 事件（AI 开发，PLAN #26.11）：PR 也会产生 issues 事件之外的 pull_request 事件，这里忽略 PR。
func (i *Ingester) handleIssues(ctx context.Context, repo *store.Repo, action string, payload []byte, isSelf bool) (string, error) {
	if i.aiDev == nil {
		return "", nil
	}
	var p struct {
		Issue struct {
			Number      int                     `json:"number"`
			Title       string                  `json:"title"`
			Body        *string                 `json:"body"`
			State       string                  `json:"state"`
			Labels      []struct{ Name string } `json:"labels"`
			HTMLURL     string                  `json:"html_url"`
			UpdatedAt   time.Time               `json:"updated_at"`
			PullRequest *json.RawMessage        `json:"pull_request"`
		} `json:"issue"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
		Sender ghSender `json:"sender"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析：" + err.Error())
	}
	if p.Issue.PullRequest != nil {
		return "PR 的 issues 事件，忽略", nil
	}
	body := ""
	if p.Issue.Body != nil {
		body = *p.Issue.Body
	}
	labels := make([]string, 0, len(p.Issue.Labels))
	for _, l := range p.Issue.Labels {
		labels = append(labels, l.Name)
	}
	return i.aiDev.IssueEvent(ctx, repo.ID, IssueEvent{
		Action: action, Number: p.Issue.Number, Title: p.Issue.Title, Body: body, Open: p.Issue.State == "open", Labels: labels,
		URL: p.Issue.HTMLURL, UpdatedAt: p.Issue.UpdatedAt, Label: p.Label.Name, Sender: p.Sender.Login, FromSelf: isSelf,
	})
}

// pullClosed 处理 PR 被关闭（事件没有过时）：取消它的 run，通知合并队列；没有合并时终止 AI 开发的工作项。autoTag、aiDev
// 是之前处理合并得到的说明（自动打 tag、AI 开发）。
func (i *Ingester) pullClosed(ctx context.Context, repo *store.Repo, number int, headRef string, merged bool, sender, autoTag, aiDev string) (string, error) {
	i.setRefHead(ctx, repo.ID, workflow.PullRef(number), "", "", "")
	ids, err := store.QueryIDs(ctx, i.db, `SELECT id FROM runs WHERE repo_id=$1 AND pr_number=$2 AND state IN ('active','queued')`, repo.ID, number)
	if err != nil {
		return "", err
	}
	if err := i.cancelRuns(ctx, ids, store.CancelPRClosed); err != nil {
		return "", err
	}
	reviews := 0
	if i.reviews != nil {
		if reviews, err = i.reviews.CancelPR(ctx, repo.ID, number); err != nil {
			return "", err
		}
	}
	if err := i.notifyQueue(ctx, repo.ID, number); err != nil {
		return "", err
	}
	if !merged {
		if aiDev, err = i.aiDevPullClosed(ctx, repo.ID, number, headRef, "", false, "", sender); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("PR 已关闭，取消 %d 个 run", len(ids))
	if reviews > 0 {
		msg += fmt.Sprintf("、%d 个 AI 审计请求", reviews)
	}
	return joinResult(msg+autoTag, aiDev), nil
}

// aiDevPullClosed 通知 AI 开发 PR 被关闭（没有配置 AI 开发时什么也不做），返回处理结果的说明。
func (i *Ingester) aiDevPullClosed(ctx context.Context, repoID int64, pr int, headRef, headSHA string, merged bool, mergeSHA, sender string) (string, error) {
	if i.aiDev == nil {
		return "", nil
	}
	return i.aiDev.PullClosed(ctx, repoID, pr, headRef, headSHA, merged, mergeSHA, sender)
}

// aiDevPullHead 通知 AI 开发 PR 的 head 变了（没有配置 AI 开发时什么也不做）。
func (i *Ingester) aiDevPullHead(ctx context.Context, repoID int64, pr int, head string) error {
	if i.aiDev == nil {
		return nil
	}
	return i.aiDev.PullHead(ctx, repoID, pr, head)
}

// notifyQueue 在 PR pr（0 表示任意 PR）在合并队列中时通知队列的执行者。
func (i *Ingester) notifyQueue(ctx context.Context, repoID int64, pr int) error {
	if i.queue == nil {
		return nil
	}
	return i.queue.Notify(ctx, repoID, pr)
}

func (i *Ingester) handlePullRequest(ctx context.Context, delivery string, repo *store.Repo, action string, payload []byte, isSelf bool) (string, error) {
	var p struct {
		Number      int `json:"number"`
		PullRequest struct {
			Number int        `json:"number"`
			State  string     `json:"state"`
			Head   gh.PRRef   `json:"head"`
			Base   gh.PRRef   `json:"base"`
			User   gh.Account `json:"user"`
			// 以下只在 closed 时使用（自动打 tag，PLAN #29.4）
			Merged         bool        `json:"merged"`
			MergeCommitSHA string      `json:"merge_commit_sha"`
			MergedBy       *gh.Account `json:"merged_by"`
			Labels         []gh.Label  `json:"labels"`
		} `json:"pull_request"`
		Changes struct {
			Base *json.RawMessage `json:"base"`
		} `json:"changes"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
		// Repository 只用 default_branch：合并那一刻的默认分支（自动打 tag 判断"合入当时的默认分支"，#29.4）
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Sender ghSender `json:"sender"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析：" + err.Error())
	}
	pr := p.PullRequest
	ref := workflow.PullRef(pr.Number)
	autoTag, aiDev := "", ""
	// 来源分支只用于关联 AI 开发的工作项（正式分支 kiln/dev-<id>），fork 来的同名分支不算
	headRef := pr.Head.Ref
	if gh.IsFork(pr.Head, pr.Base) {
		headRef = ""
	}
	if action == actionClosed && pr.Merged {
		// 合并是不会撤销的事实：在判断事件是否过时之前记录
		var err error
		// 按事件中的默认分支判断（合并那一刻的快照）：事件晚到、期间默认分支改了时，不按现在的默认分支误判。
		// payload 中没有时用仓库现在的
		defaultBranch := p.Repository.DefaultBranch
		if defaultBranch == "" {
			defaultBranch = repo.DefaultBranch
		}
		if autoTag, err = i.recordMerged(ctx, repo, defaultBranch, pr.Number, headRef, pr.Base.Ref, pr.MergeCommitSHA, pr.MergedBy, &pr.User,
			pr.Labels, gh.IsFork(pr.Head, pr.Base), isSelf); err != nil {
			return "", err
		}
		// AI 开发的工作项按阶段推进（PLAN #26.11、#31.3）
		if aiDev, err = i.aiDevPullClosed(ctx, repo.ID, pr.Number, headRef, pr.Head.SHA, true, pr.MergeCommitSHA, p.Sender.Login); err != nil {
			return "", err
		}
	}
	if action == actionClosed || prTriggersBuild(action, p.Changes.Base != nil) {
		// 关闭（取消 run）和规划都以 PR 最新的状态为准
		s := prSnapshot{Number: pr.Number, State: pr.State, Head: pr.Head.SHA, Base: pr.Base.Ref}
		if stale, err := i.prSuperseded(ctx, delivery, repo.ID, s); err != nil {
			return "", err
		} else if stale {
			return supersededResult, nil
		}
	}
	if action == actionClosed {
		return i.pullClosed(ctx, repo, pr.Number, headRef, pr.Merged, p.Sender.Login, autoTag, aiDev)
	}
	if prTriggersBuild(action, p.Changes.Base != nil) {
		// 工作项的 PR：以新的 head 为准判断返修（人推送了新 commit）
		if err := i.aiDevPullHead(ctx, repo.ID, pr.Number, pr.Head.SHA); err != nil {
			return "", err
		}
	}
	// fork 来的 PR 一律丢弃（PLAN #16.1）
	if gh.IsFork(pr.Head, pr.Base) {
		return "fork 来的 PR，丢弃", nil
	}
	if isQueueAction(action) {
		return i.handleQueueAction(ctx, repo, action, pr.Number, pr.Head.SHA, pr.Base.Ref, p.Label.Name, p.Sender.Login)
	}
	if !prTriggersBuild(action, p.Changes.Base != nil) {
		return "", nil
	}
	if action == actionEdited {
		// 修改了目标分支：按推送处理，重新规划
		action = actionSynchronize
	}
	ev := &planner.Request{
		Repo: repo, Event: workflow.EventPullRequest, Ref: ref, SHA: pr.Head.SHA, Action: action, PRNumber: pr.Number,
		BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA, HeadRef: pr.Head.Ref, SenderLogin: p.Sender.Login,
		SenderIsBot: (p.Sender.Type == senderTypeBot && !isSelf) || pr.User.Type == senderTypeBot, FromSelf: isSelf, Source: planner.SourceWebhook,
	}
	n, err := i.planEvent(ctx, ev)
	if err != nil {
		return "", err
	}
	i.setRefHead(ctx, repo.ID, ref, pr.Head.SHA, pr.Head.SHA, pr.Base.Ref)
	if err := i.notifyQueue(ctx, repo.ID, pr.Number); err != nil {
		return "", err
	}
	if action == actionReadyForReview && i.queue != nil {
		// draft 转为 ready：run 可能早已结束（按 dedupe_key 不会重新规划），要在这里提名自动入队
		if err := i.queue.Nominate(ctx, repo.ID, pr.Number); err != nil {
			return "", err
		}
	}
	return n.String(), nil
}

// recordMerged 记下经过 PR 合入的 commit，由自动打 tag 的执行者处理（PLAN #29.4，T4）。返回附在事件结果后的说明。
// 任何目标分支的合并都记下（合并进开发分支的只会在自动打 tag 中跳过，不发布版本，PLAN #28.5）；是否发版按记录的目标分支
// 和合并那一刻的默认分支裁决。
//
// 发布的真实发起者：合并者是人是发送者；为空或是机器人时取 PR 作者；都是机器人或未知时照实记下 sender_is_bot，交给
// 自动打 tag 统一裁决（Kiln 的 Bot 合并 AI 开发的实现 PR 可以发布，第三方 Bot 的 PR 仍然不发布）。
//
// 不记录：fork 来的 PR（PLAN #16.1）。由本 App（合并队列）合并的：队列在合并的同一事务中已经按请求
// 合并时的 label 记下，这里不重复记（会用未冻结的 label）；但队列没有记录的本 App 合并（结果不明、后来才看到已合并）
// 仍然在这里记，不能静默丢失。
//
// 是否交给合并队列判断，不能依赖 isSelf 已经加载：Server 重启后、App 信息读取失败时 isSelf 暂时返回 false，队列恢复落库
// 之前收到的合并 webhook 会以为是自己以外的合并者，用未冻结的 label 抢先记下（队列随后按冻结的 label 写时被唯一约束忽略，
// release:skip 等标记永久丢失）。所以只有明确的"人合并"才在这里按事件快照记录，其余（本 App、其他机器人、合并者未知）
// 都先查队列是否拥有这次合并。这与合并队列自己的判断互补：process.go 里 action=merging 且合并者是人时不写 auto_tags，
// 交给这里按合并那一刻的 label 记录，人合并不会丢。
//
// defaultBranch 是合并那一刻的默认分支（事件中的 repository.default_branch）。
func (i *Ingester) recordMerged(ctx context.Context, repo *store.Repo, defaultBranch string, pr int, headRef, base, sha string,
	by, author *gh.Account, labels []gh.Label, fork, isSelf bool,
) (string, error) {
	if i.autoTag == nil || sha == "" || fork {
		return "", nil
	}
	if i.queue != nil && (isSelf || !isHumanAccount(i.isSelf, by)) {
		own, err := mergequeue.OwnsMerge(ctx, i.db, repo.ID, pr, sha)
		if err != nil {
			return "", fmt.Errorf("判断合并是否由合并队列完成：%w", err)
		}
		if own {
			return "", nil
		}
	}
	sender, senderIsBot := webhookReleaseSender(i.isSelf, by, author)
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	added, err := i.autoTag.Record(ctx, mqevents.AutoTagRecord{
		RepoID: repo.ID, SHA: sha, PR: pr, Labels: names, MergedBy: sender, SenderIsBot: senderIsBot,
		HeadRef: headRef, BaseRef: base, DefaultBranchAtMerge: defaultBranch, Source: mqevents.AutoTagFromWebhook,
	})
	if err != nil {
		return "", fmt.Errorf("记录合并的 commit：%w", err)
	}
	if added {
		return "；已记下合并的 commit，等待自动打 tag", nil
	}
	return "", nil
}

// webhookReleaseSender 返回合并事件中发布的真实发起者和它是否是机器人：合并者是人时是合并者；否则是 PR 作者；合并者和
// PR 作者都是机器人或未知时把已知的 login（可能为空）照实记下并标记为机器人，不静默跳过。
func webhookReleaseSender(isSelf func(string) bool, by, author *gh.Account) (sender string, isBot bool) {
	switch {
	case isHumanAccount(isSelf, by):
		return by.Login, false
	case isHumanAccount(isSelf, author):
		return author.Login, false
	}
	return cmp.Or(accountLogin(author), accountLogin(by)), true
}

// isHumanAccount 判断账号是不是一个可以代表发布的人：有 login、不是机器人类型、没有 [bot] 后缀，且不是本 App 自己。
// isSelf 为 nil（App 身份未知）时只按机器人标记判断；调用方不能因此把"本 App 合并"误判成人，见 recordMerged。
func isHumanAccount(isSelf func(string) bool, a *gh.Account) bool {
	return a != nil && a.Login != "" && a.Type != senderTypeBot && !strings.HasSuffix(a.Login, "[bot]") &&
		(isSelf == nil || !isSelf(a.Login))
}

// accountLogin 返回账号的 login（账号为 nil 时为空）。
func accountLogin(a *gh.Account) string {
	if a == nil {
		return ""
	}
	return a.Login
}

// handleQueueAction 处理只与合并队列有关的 PR 事件（PLAN #28.2）：加上 kiln:merge 即入队；去掉 label、转为 draft 时
// 通知执行者（由它出队）。本 App 自己加、去 label 产生的事件同样处理，入队和出队都是幂等的。
func (i *Ingester) handleQueueAction(ctx context.Context, repo *store.Repo, action string, pr int, head, base, label, sender string) (string, error) {
	if i.queue == nil {
		return "", nil
	}
	if action == actionLabeled && label == mergequeue.Label {
		// 记下事件中的目标分支：执行者仍以平台上的 PR 为准回填
		added, err := i.queue.Enqueue(ctx, repo.ID, pr, head, base, githubActor(sender))
		if err != nil {
			return "", err
		}
		if added {
			return "加入合并队列", nil
		}
		return "已经在合并队列中", nil
	}
	if action == actionUnlabeled && label == mergequeue.HoldLabel {
		// 去掉 kiln:hold：验证已经通过的 PR 不会再有 run 结束，要在这里提名自动入队
		if err := i.queue.Nominate(ctx, repo.ID, pr); err != nil {
			return "", err
		}
	}
	return "", i.notifyQueue(ctx, repo.ID, pr)
}
