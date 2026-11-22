package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// installation 与仓库目录：白名单校验、installation / installation_repositories / repository 事件的增量更新，
// 以及用户显式的"刷新仓库"（SyncInstallations）。

// installationAllowed 判断 installation 是否属于白名单账号，不属于时返回丢弃的原因。
// 本地没有记录（或已标记为卸载）时只定向读取这一个 installation（GET /app/installations/{id}），不列出全部安装：
// 不存在（已卸载）时丢弃事件；不在白名单中时卸载它（PLAN #16.1）；否则记录下来。
func (i *Ingester) installationAllowed(ctx context.Context, instID int64) (string, error) {
	var login string
	var accountID int64
	err := i.db.QueryRow(ctx, `SELECT account_id, account_login FROM installations WHERE id=$1 AND NOT deleted`, instID).Scan(&accountID, &login)
	if err == nil {
		if !i.accountAllowed(accountID, login) {
			return "installation 不在白名单中，丢弃", nil
		}
		return "", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	inst, err := i.gh.GetInstallation(ctx, instID)
	if gh.IsNotFound(err) {
		return "installation 不存在或已卸载，丢弃", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 installation %d：%w", instID, err)
	}
	if !i.accountAllowed(inst.Account.ID, inst.Account.Login) {
		if err := i.rejectInstallation(ctx, inst); err != nil {
			return "", err
		}
		return "installation 不在白名单中，已卸载", nil
	}
	if err := store.UpsertInstallation(ctx, i.db, installationMeta(inst)); err != nil {
		return "", err
	}
	i.log.Info("定向校验并记录 installation", "installation", inst.ID, "account", inst.Account.Login)
	return "", nil
}

// rejectInstallation 卸载不在白名单中的 installation（PLAN #16.1）。
func (i *Ingester) rejectInstallation(ctx context.Context, inst *gh.Installation) error {
	i.log.Warn("拒绝不在白名单中的安装，卸载", "installation", inst.ID, "account", inst.Account.Login)
	if err := i.gh.DeleteInstallation(ctx, inst.ID); err != nil && !gh.IsNotFound(err) {
		return err
	}
	return nil
}

func installationMeta(inst *gh.Installation) store.InstallationMeta {
	return store.InstallationMeta{
		ID: inst.ID, AccountID: inst.Account.ID, AccountLogin: inst.Account.Login, AccountType: inst.Account.Type,
		Suspended: inst.SuspendedAt != nil,
	}
}

// handleRepositoryEvent 处理已有仓库的改名、转移和修改默认分支（repository.deleted 由 handleRepositoryDeleted 处理）。
func (i *Ingester) handleRepositoryEvent(ctx context.Context, repo *store.Repo, action string, head *eventHead) (string, error) {
	r := head.Repository
	switch {
	case isRepoMoved(action):
		_, err := i.db.Exec(ctx, `UPDATE repos SET owner=$2, name=$3, owner_id=$4, updated_at=now() WHERE id=$1`, r.ID, r.Owner.Login, r.Name, r.Owner.ID)
		return "仓库改名", err
	case isDefaultBranchChanged(action, head):
		return i.handleDefaultBranchChanged(ctx, repo)
	}
	return "", nil
}

// handleDefaultBranchChanged 记录仓库当前的默认分支。默认分支只在第一次接入和"刷新仓库"时读取，不跟随这个事件更新的话，
// 合并队列会按旧分支判断 PR 的目标分支、读取配置和合并，规划的优先级、手动触发的默认 ref 也会用旧分支。
// payload 只是"默认分支改了"的信号，不按它记录：webhook 可能迟到、乱序或重试，本地的接收时间反映不了 GitHub 上的先后，
// 旧事件会把之后改回的值覆盖掉。因此定向读取这一个仓库（GET /repositories/{id}）按 GitHub 的当前值记录。
// 之后不论是否变化都通知合并队列按当前的默认分支重新判断：上次可能在记录之后、通知之前失败；通知是幂等的。
func (i *Ingester) handleDefaultBranchChanged(ctx context.Context, repo *store.Repo) (string, error) {
	r, err := i.gh.Inst(repo.InstallationID).GetRepoByID(ctx, repo.ID)
	if gh.IsNotFound(err) {
		// 仓库已删除或移出 installation：之后的 repository / installation_repositories 事件会停用它
		return "仓库已不在授权范围内，忽略", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取仓库 %s 的默认分支：%w", repo.FullName(), err)
	}
	result := "默认分支已是 " + r.DefaultBranch
	if r.DefaultBranch != "" && r.DefaultBranch != repo.DefaultBranch {
		if _, err := i.db.Exec(ctx, `UPDATE repos SET default_branch=$2, updated_at=now() WHERE id=$1`, repo.ID, r.DefaultBranch); err != nil {
			return "", err
		}
		result = "默认分支改为 " + r.DefaultBranch
	}
	if err := i.notifyQueue(ctx, repo.ID, 0); err != nil {
		return "", err
	}
	return result, nil
}

// handleRepositoryDeleted 停用 GitHub 上已删除的仓库，不请求 GitHub。仓库还不在 Kiln 中时（只要属于白名单账号）也记下一条
// 停用的记录：之后晚到的旧 installation_repositories.added / installation.created 据此定向校验，不会恢复它。
// 停用是安全的方向：仓库在 GitHub 上被恢复（ID 不变）后，它的下一个事件会定向校验并恢复它。
func (i *Ingester) handleRepositoryDeleted(ctx context.Context, head *eventHead) (string, error) {
	r := head.Repository
	if r == nil || r.ID == 0 {
		return "没有仓库，忽略", nil
	}
	if !i.accountAllowed(r.Owner.ID, r.Owner.Login) {
		return "仓库不属于白名单账号，忽略", nil
	}
	m := store.RepoMeta{ID: r.ID, InstallationID: head.Installation.ID, OwnerID: r.Owner.ID, Owner: r.Owner.Login, Name: r.Name, Private: r.Private}
	if err := store.MarkRepoDeleted(ctx, i.db, m); err != nil {
		return "", err
	}
	i.hub.All()
	return "仓库已删除", nil
}

// ghRepoBrief 是 installation / installation_repositories 事件中的仓库（没有默认分支和 clone 地址）。
type ghRepoBrief struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// handleInstallationEvent 按事件增量更新仓库目录，不列出 installation 的全部仓库：
//   - installation.created：记录 installation 和 payload 中的仓库；
//   - installation_repositories.added / removed：记录新增的仓库、停用移除的仓库（本地配置和历史保留）；
//   - installation.deleted：标记卸载、停用它的仓库；其他 action（suspend、new_permissions_accepted 等）只更新 installation。
//
// 生命周期事件可能乱序或晚到：签名只证明来源，不证明事件是最新的，delivery 去重也挡不住不同的旧 delivery。所以
//   - 停用和卸载直接执行；本地还没有记录时也记下一条停用 / 已卸载的记录，供之后晚到的旧事件判断；
//   - 恢复不相信 payload：本地已标记卸载的 installation 用 GET /app/installations/{id}、本地已停用（或属于其他 installation）
//     的仓库用 repodir.Verify 定向确认它在授权范围内，以 GitHub 的当前状态为准。晚到的旧 added / created
//     不会恢复已移除的记录，真正的重新授权照常恢复；
//   - 第一次出现的 installation 和仓库没有与之矛盾的记录，按 payload 记录，不请求 GitHub。
//
// 反过来，晚到的旧 removed / deleted 可能停用一个已经重新授权的仓库：这是安全的方向，仓库的下一个事件会定向校验并恢复它（resolveRepo）。
// payload 中没有默认分支和 clone 地址，第一次用到前补齐（repodir.Complete，或下一个事件的 payload）。
func (i *Ingester) handleInstallationEvent(ctx context.Context, typ, action string, payload []byte) (string, error) {
	var p struct {
		Installation gh.Installation `json:"installation"`
		Repositories []ghRepoBrief   `json:"repositories"`
		Added        []ghRepoBrief   `json:"repositories_added"`
		Removed      []ghRepoBrief   `json:"repositories_removed"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return dropEvent("payload 无法解析：" + err.Error())
	}
	inst := p.Installation
	if inst.ID == 0 {
		return dropEvent("没有 installation，忽略")
	}
	if typ == eventInstallation && action == actionDeleted {
		err := i.db.Tx(ctx, func(tx pgx.Tx) error { return store.MarkInstallationDeleted(ctx, tx, installationMeta(&inst)) })
		if err != nil {
			return "", err
		}
		i.hub.All()
		return "installation 已删除", nil
	}
	if !i.accountAllowed(inst.Account.ID, inst.Account.Login) {
		if err := i.rejectInstallation(ctx, &inst); err != nil {
			return "", err
		}
		return "不在白名单中，已卸载", nil
	}
	added, removed := p.Added, p.Removed
	if typ == eventInstallation && action == actionCreated {
		added = p.Repositories
	}
	state, err := store.GetInstallationState(ctx, i.db, inst.ID)
	if err != nil {
		return "", err
	}
	// 只移除仓库的事件不恢复 installation；其他事件都会把它记为已安装
	onlyRemoves := typ == eventInstallationRepositories && len(added) == 0
	updateInst := state != store.InstallationDeleted
	if state == store.InstallationDeleted && !onlyRemoves {
		// 本地已标记卸载：定向确认它在 GitHub 上还在（并按 GitHub 的当前状态记录），否则是晚到的旧事件
		if reason, err := i.installationAllowed(ctx, inst.ID); err != nil || reason != "" {
			return reason, err
		}
	}
	toAdd, stale, err := i.addedRepos(ctx, &inst, added)
	if err != nil {
		return "", err
	}
	err = i.db.Tx(ctx, func(tx pgx.Tx) error {
		if updateInst {
			if err := store.UpsertInstallation(ctx, tx, installationMeta(&inst)); err != nil {
				return err
			}
		}
		for _, m := range toAdd {
			if err := store.UpsertRepo(ctx, tx, m); err != nil {
				return err
			}
		}
		for _, r := range removed {
			if err := store.DeactivateRepo(ctx, tx, briefRepoMeta(&inst, r)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(toAdd) > 0 || len(removed) > 0 {
		i.hub.All()
	}
	result := fmt.Sprintf("已更新 installation：新增 %d 个仓库，移除 %d 个", len(toAdd), len(removed))
	if stale > 0 {
		result += fmt.Sprintf("；%d 个已不在授权范围内，不恢复（晚到的事件）", stale)
	}
	return result, nil
}

// addedRepos 返回事件中新增的仓库中要记录（启用）的部分，以及因为已不在授权范围内而不恢复的个数。
// 本地没有记录、或者已经属于这个 installation 并启用的仓库按 payload 记录；本地已停用或属于其他 installation 的仓库
// 是恢复，用 repodir.Verify 定向确认它当前在这个 installation 的授权范围内，不在时不恢复。
func (i *Ingester) addedRepos(ctx context.Context, inst *gh.Installation, added []ghRepoBrief) ([]store.RepoMeta, int, error) {
	if len(added) == 0 {
		return nil, 0, nil
	}
	ids := make([]int64, 0, len(added))
	for _, r := range added {
		ids = append(ids, r.ID)
	}
	states, err := store.RepoStates(ctx, i.db, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]store.RepoMeta, 0, len(added))
	stale := 0
	for _, r := range added {
		s, known := states[r.ID]
		if !known || s.Active && s.InstallationID == inst.ID {
			out = append(out, briefRepoMeta(inst, r))
			continue
		}
		gr, err := repodir.Verify(ctx, i.gh, inst.ID, r.ID)
		if repodir.NotInstalled(err) {
			i.log.Info("installation 事件中的仓库已不在授权范围内，不恢复", "repo", r.FullName, "installation", inst.ID, "reason", err)
			stale++
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("校验仓库 %s：%w", r.FullName, err)
		}
		out = append(out, repodir.MetaOf(inst.ID, gr))
	}
	return out, stale, nil
}

// briefRepoMeta 返回 installation 事件中的仓库对应的记录（没有默认分支和 clone 地址）。
func briefRepoMeta(inst *gh.Installation, r ghRepoBrief) store.RepoMeta {
	owner, name, ok := strings.Cut(r.FullName, "/")
	if !ok {
		owner, name = inst.Account.Login, r.Name
	}
	return store.RepoMeta{ID: r.ID, InstallationID: inst.ID, OwnerID: inst.Account.ID, Owner: owner, Name: name, Private: r.Private}
}

// SyncResult 是一次"刷新仓库"的结果：白名单中的 installation 数和启用中的仓库数。
type SyncResult struct {
	Installations, Repos int
}

// SyncInstallations 刷新仓库目录：列出 App 的全部安装和每个安装的仓库（不读取分支、tag、PR）；
// 不在白名单中的安装会被卸载（PLAN #16.1），GitHub 上已经没有的安装和仓库停用（本地配置和历史保留）。
// 只在用户显式操作（Dashboard 的"刷新仓库"、kiln repo sync）时执行，Server 启动和处理事件时不会调用。
func (i *Ingester) SyncInstallations(ctx context.Context) (SyncResult, error) {
	var res SyncResult
	if i.gh == nil {
		return res, errNoGitHubApp
	}
	list, err := i.gh.ListInstallations(ctx)
	if err != nil {
		return res, fmt.Errorf("列出 installation：%w", err)
	}
	seen := map[int64]bool{}
	for _, inst := range list {
		if !i.accountAllowed(inst.Account.ID, inst.Account.Login) {
			if err := i.rejectInstallation(ctx, &inst); err != nil {
				i.log.Error("卸载 installation", "err", err)
			}
			continue
		}
		seen[inst.ID] = true
		if err := i.syncInstallation(ctx, &inst); err != nil {
			return res, err
		}
	}
	known, err := store.QueryIDs(ctx, i.db, `SELECT id FROM installations WHERE NOT deleted`)
	if err != nil {
		return res, err
	}
	for _, id := range known {
		if seen[id] {
			continue
		}
		// 标记卸载和停用仓库在同一事务中：否则中途失败时 installation 已标记卸载、仓库仍然启用，之后的刷新不会再处理它
		if err := i.db.Tx(ctx, func(tx pgx.Tx) error { return store.DeactivateInstallation(ctx, tx, id) }); err != nil {
			return res, err
		}
	}
	i.hub.All()
	err = i.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM installations WHERE NOT deleted), (SELECT count(*) FROM repos WHERE active)`).
		Scan(&res.Installations, &res.Repos)
	return res, err
}

func (i *Ingester) syncInstallation(ctx context.Context, inst *gh.Installation) error {
	if err := store.UpsertInstallation(ctx, i.db, installationMeta(inst)); err != nil {
		return err
	}
	repos, err := i.gh.Inst(inst.ID).ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("列出 installation %d 的仓库：%w", inst.ID, err)
	}
	ids := make([]int64, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
		if err := store.UpsertRepo(ctx, i.db, repodir.MetaOf(inst.ID, &r)); err != nil {
			return err
		}
	}
	_, err = i.db.Exec(ctx, `UPDATE repos SET active=false WHERE installation_id=$1 AND NOT (id = ANY($2))`, inst.ID, ids)
	return err
}
