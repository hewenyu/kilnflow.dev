// Package repodir 按需维护 Kiln 的仓库目录：Server 默认不列出 installation 的全部仓库，
// 仓库记录来自已验签的事件和用户显式的"刷新仓库"。installation 事件的 payload 中没有默认分支和 clone 地址，
// 第一次用到前用一个定向请求补齐（Complete）；跨仓库发布的目标仓库、job 的 repositories 不在 Kiln 中时，
// 同样只定向读取这一个仓库（Lookup），并校验它在同一个 installation 的授权范围内。处理事件时发现新仓库、恢复已停用的仓库
// 也用同一个校验（Verify）。
package repodir

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// MetaOf 把 GitHub 返回的仓库信息转换为 store.RepoMeta。
func MetaOf(instID int64, r *gh.Repository) store.RepoMeta {
	return store.RepoMeta{
		ID: r.ID, InstallationID: instID, OwnerID: r.Owner.ID, Owner: r.Owner.Login, Name: r.Name,
		Private: r.Private, DefaultBranch: r.DefaultBranch, CloneURL: r.CloneURL,
	}
}

// ErrNotInstalled 表示仓库不在 installation 的授权范围内（Verify 确认 App 没有安装在它上面，或者它属于别的 installation）。
var ErrNotInstalled = errors.New("仓库不在 installation 的授权范围内")

// NotInstalled 判断 Verify、VerifyName 的错误是否表示仓库不在 installation 的授权范围内（含已删除）。
func NotInstalled(err error) bool { return gh.IsNotFound(err) || errors.Is(err, ErrNotInstalled) }

// Verify 定向确认仓库 id 当前在 installation instID 的授权范围内，返回它在 GitHub 上的当前信息；不在时返回的错误使
// NotInstalled 成立。不列出 installation 或仓库：
//   - 先用 instID 的 token 读取这个仓库（GET /repositories/{id}）。私有仓库只有在授权范围内才读得到，读到即为证明；
//   - 公开仓库任何 token 都能读取，读到不能说明它属于这个 installation，再用 App JWT 定向查询仓库所属的 installation
//     （GET /repos/{owner}/{name}/installation），必须正好是 instID（也就核对了账号：installation 已经按白名单校验过）。
func Verify(ctx context.Context, client *gh.Client, instID, id int64) (*gh.Repository, error) {
	r, err := client.Inst(instID).GetRepoByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, fmt.Errorf("%w：仓库 ID 为 %d，不是 %d", ErrNotInstalled, r.ID, id)
	}
	if err := confirmInstallation(ctx, client, instID, r); err != nil {
		return nil, err
	}
	return r, nil
}

// VerifyName 与 Verify 相同，按全名读取仓库（GET /repos/{owner}/{name}）。
func VerifyName(ctx context.Context, client *gh.Client, instID int64, fullName string) (*gh.Repository, error) {
	r, err := client.Inst(instID).GetRepo(ctx, fullName)
	if err != nil {
		return nil, err
	}
	if err := confirmInstallation(ctx, client, instID, r); err != nil {
		return nil, err
	}
	return r, nil
}

// confirmInstallation 确认用 instID 的 token 读到的仓库 r 属于 installation instID：私有仓库读到即为证明，公开仓库定向查询它的 installation。
func confirmInstallation(ctx context.Context, client *gh.Client, instID int64, r *gh.Repository) error {
	if r.Private {
		return nil
	}
	inst, err := client.GetRepoInstallation(ctx, r.FullName)
	if gh.IsNotFound(err) {
		return fmt.Errorf("%w：App 没有安装在公开仓库 %s 上", ErrNotInstalled, r.FullName)
	}
	if err != nil {
		return fmt.Errorf("查询仓库 %s 的 installation：%w", r.FullName, err)
	}
	if inst.ID != instID {
		return fmt.Errorf("%w：公开仓库 %s 属于 installation %d，不是 %d", ErrNotInstalled, r.FullName, inst.ID, instID)
	}
	return nil
}

// Complete 在仓库记录缺少默认分支或 clone 地址时，用 GET /repositories/{id} 补齐（只请求这一个仓库）。
// 仓库已不在 installation 的授权范围内时返回错误（gh.IsNotFound 成立），不修改记录。client 为 nil 时不做任何事。
func Complete(ctx context.Context, db store.Execer, client *gh.Client, repo *store.Repo) error {
	if !repo.Incomplete() || client == nil {
		return nil
	}
	r, err := client.Inst(repo.InstallationID).GetRepoByID(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("读取仓库 %s 的信息：%w", repo.FullName(), err)
	}
	m := MetaOf(repo.InstallationID, r)
	if err := store.UpsertRepo(ctx, db, m); err != nil {
		return err
	}
	repo.OwnerID, repo.Owner, repo.Name, repo.Private = m.OwnerID, m.Owner, m.Name, m.Private
	repo.DefaultBranch, repo.CloneURL = r.DefaultBranch, r.CloneURL
	return nil
}

// DB 是 Find、Lookup 需要的数据库连接。
type DB interface {
	store.Querier
	store.Execer
}

// Find 按全名查找启用中的仓库，并补齐缺少的信息。
func Find(ctx context.Context, db DB, client *gh.Client, fullName string) (*store.Repo, error) {
	repo, err := store.FindRepo(ctx, db, fullName)
	if err != nil {
		return nil, err
	}
	if err := Complete(ctx, db, client, repo); err != nil {
		return nil, err
	}
	return repo, nil
}

// Lookup 查找 installation instID 可以访问的仓库 fullName。Kiln 中没有时只定向确认这一个仓库（VerifyName）：
// 不在它的授权范围内（私有仓库读不到；公开仓库没有安装本 App 或属于别的 installation）时返回 nil（不是错误），由调用方报告配置错误。
// 找到的仓库属于别的 installation 时也返回它，由调用方判断。fullName 不是 owner/name 的形式时同样返回 nil（不是错误）。
func Lookup(ctx context.Context, db DB, client *gh.Client, instID int64, fullName string) (*store.Repo, error) {
	// 先检查格式（FindRepo 对缺少 / 的名字返回格式错误）：不是 owner/name 的名字不可能是一个仓库，
	// 与不在授权范围内一样返回 nil，由调用方报告配置错误
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, nil
	}
	repo, err := Find(ctx, db, client, fullName)
	switch {
	case err == nil:
		return repo, nil
	case gh.IsNotFound(err):
		return nil, nil
	case !errors.Is(err, store.ErrRepoNotFound) || client == nil:
		return nil, err
	}
	r, err := VerifyName(ctx, client, instID, fullName)
	if NotInstalled(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取仓库 %s：%w", fullName, err)
	}
	if err := store.UpsertRepo(ctx, db, MetaOf(instID, r)); err != nil {
		return nil, err
	}
	return store.GetRepo(ctx, db, r.ID)
}
