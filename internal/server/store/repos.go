package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Repo 是 repos 表中的一个仓库。
type Repo struct {
	ID             int64
	InstallationID int64
	OwnerID        int64
	Owner, Name    string
	Private        bool
	DefaultBranch  string
	CloneURL       string
	Active         bool
	CacheSalt      string
	MaxConcurrency int
	BaselineDone   bool
}

// FullName 返回仓库的全名 owner/name。
func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }

// Incomplete 判断仓库记录是否缺少默认分支或 clone 地址（来自 installation 事件、还没有补齐）。
func (r *Repo) Incomplete() bool { return r.DefaultBranch == "" || r.CloneURL == "" }

const repoCols = `id, installation_id, owner_id, owner, name, private, default_branch, clone_url, active, cache_salt, max_concurrency, baseline_done`

func scanRepo(row pgx.Row) (*Repo, error) {
	var r Repo
	err := row.Scan(&r.ID, &r.InstallationID, &r.OwnerID, &r.Owner, &r.Name, &r.Private, &r.DefaultBranch, &r.CloneURL,
		&r.Active, &r.CacheSalt, &r.MaxConcurrency, &r.BaselineDone)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRepo 按 ID 读取仓库。不存在时返回的错误包装了 pgx.ErrNoRows。
func GetRepo(ctx context.Context, q Querier, id int64) (*Repo, error) {
	r, err := scanRepo(q.QueryRow(ctx, `SELECT `+repoCols+` FROM repos WHERE id=$1`, id))
	if err != nil {
		return nil, fmt.Errorf("读取仓库 %d：%w", id, err)
	}
	return r, nil
}

// ReposByIDs 按 ID 批量读取仓库，返回 id → 仓库（不存在的 ID 不在结果中）。供一次处理多个仓库时避免逐个查询。
func ReposByIDs(ctx context.Context, q Querier, ids []int64) (map[int64]*Repo, error) {
	out := make(map[int64]*Repo, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+repoCols+` FROM repos WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

// ErrRepoNotFound 表示仓库没有同步到 Kiln（或已停用）。
var ErrRepoNotFound = errors.New("仓库不在 Kiln 中")

// RepoNotFoundError 是找不到仓库的错误：Name 是原样的全名，errors.Is 匹配 ErrRepoNotFound，说明文字给 CLI 和日志。
type RepoNotFoundError struct{ Name string }

func (e *RepoNotFoundError) Error() string {
	return fmt.Sprintf("%s：%s（App 是否已安装到这个仓库？）", ErrRepoNotFound, e.Name)
}

func (e *RepoNotFoundError) Unwrap() error { return ErrRepoNotFound }

// RepoNameError 表示仓库名没有写成 owner/name；Name 是原样的输入。
type RepoNameError struct{ Name string }

func (e *RepoNameError) Error() string { return "仓库应写成 owner/name：" + e.Name }

// FindRepo 按全名（owner/name，不区分大小写）查找启用中的仓库。不存在时返回 *RepoNotFoundError（包装了 ErrRepoNotFound），
// 名称不是 owner/name 时返回 *RepoNameError。
func FindRepo(ctx context.Context, q Querier, fullName string) (*Repo, error) {
	return findRepo(ctx, q, fullName, true)
}

// FindRepoAny 按全名查找仓库，包括已停用的仓库。只读查看历史配置时使用；配置写入仍用 FindRepo。
func FindRepoAny(ctx context.Context, q Querier, fullName string) (*Repo, error) {
	return findRepo(ctx, q, fullName, false)
}

func findRepo(ctx context.Context, q Querier, fullName string, activeOnly bool) (*Repo, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil, &RepoNameError{Name: fullName}
	}
	where := `lower(owner)=lower($1) AND lower(name)=lower($2)`
	if activeOnly {
		where += ` AND active`
	}
	r, err := scanRepo(q.QueryRow(ctx, `SELECT `+repoCols+` FROM repos WHERE `+where+`
		ORDER BY updated_at DESC LIMIT 1`, owner, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &RepoNotFoundError{Name: fullName}
	}
	if err != nil {
		return nil, fmt.Errorf("查找仓库 %s：%w", fullName, err)
	}
	return r, nil
}

// 仓库目录（installations、repos）的写入：默认事件驱动，按已验签的事件和定向查询逐个更新，
// 只有用户显式"刷新仓库"时才列出全部 installation 和仓库。

// InstallationMeta 是写入 installations 表的信息。
type InstallationMeta struct {
	ID           int64
	AccountID    int64
	AccountLogin string
	AccountType  string
	Suspended    bool
}

// UpsertInstallation 记录（或恢复）一个 installation，不涉及它的仓库。
func UpsertInstallation(ctx context.Context, q Execer, m InstallationMeta) error {
	_, err := q.Exec(ctx, `INSERT INTO installations (id, account_id, account_login, account_type, suspended, deleted)
		VALUES ($1,$2,$3,coalesce(nullif($4,''),'User'),$5,false) ON CONFLICT (id) DO UPDATE SET account_id=EXCLUDED.account_id,
		account_login=EXCLUDED.account_login, account_type=EXCLUDED.account_type, suspended=EXCLUDED.suspended, deleted=false, updated_at=now()`,
		m.ID, m.AccountID, m.AccountLogin, m.AccountType, m.Suspended)
	return err
}

// InstallationState 是 installation 在本地的状态。
type InstallationState int

const (
	// InstallationUnknown：本地没有记录。
	InstallationUnknown InstallationState = iota
	// InstallationActive：已记录，未标记卸载。
	InstallationActive
	// InstallationDeleted：已标记卸载。
	InstallationDeleted
)

// GetInstallationState 返回 installation 在本地的状态。
func GetInstallationState(ctx context.Context, q Querier, id int64) (InstallationState, error) {
	var deleted bool
	err := q.QueryRow(ctx, `SELECT deleted FROM installations WHERE id=$1`, id).Scan(&deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return InstallationUnknown, nil
	case err != nil:
		return InstallationUnknown, err
	case deleted:
		return InstallationDeleted, nil
	}
	return InstallationActive, nil
}

// MarkInstallationDeleted 标记 installation 已卸载并停用它的仓库。本地还没有记录时也记下一条已卸载的记录，
// 之后晚到的旧事件据此知道它已经卸载。
func MarkInstallationDeleted(ctx context.Context, q Execer, m InstallationMeta) error {
	_, err := q.Exec(ctx, `INSERT INTO installations (id, account_id, account_login, account_type, suspended, deleted)
		VALUES ($1,$2,$3,coalesce(nullif($4,''),'User'),$5,true) ON CONFLICT (id) DO UPDATE SET deleted=true, updated_at=now()`,
		m.ID, m.AccountID, m.AccountLogin, m.AccountType, m.Suspended)
	if err != nil {
		return err
	}
	return deactivateRepos(ctx, q, m.ID)
}

// DeactivateInstallation 标记本地已有的 installation 已卸载并停用它的仓库。两条写入应在同一事务中执行，
// 否则 installation 已标记卸载而仓库仍然启用时，之后的刷新不会再处理它。
func DeactivateInstallation(ctx context.Context, q Execer, id int64) error {
	if _, err := q.Exec(ctx, `UPDATE installations SET deleted=true, updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	return deactivateRepos(ctx, q, id)
}

// deactivateRepos 停用 installation 下的全部仓库。
func deactivateRepos(ctx context.Context, q Execer, installationID int64) error {
	_, err := q.Exec(ctx, `UPDATE repos SET active=false, updated_at=now() WHERE installation_id=$1 AND active`, installationID)
	return err
}

// RepoState 是仓库在本地的归属和启用状态。
type RepoState struct {
	InstallationID int64
	Active         bool
}

// RepoStates 返回 ids 中本地已有记录的仓库的状态（没有记录的不在结果中）。
func RepoStates(ctx context.Context, q Querier, ids []int64) (map[int64]RepoState, error) {
	rows, err := q.Query(ctx, `SELECT id, installation_id, active FROM repos WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[int64]RepoState, len(ids))
	for rows.Next() {
		var id int64
		var s RepoState
		if err := rows.Scan(&id, &s.InstallationID, &s.Active); err != nil {
			return nil, err
		}
		states[id] = s
	}
	return states, rows.Err()
}

// DeactivateRepo 停用移出 installation m.InstallationID 的仓库（本地配置和历史保留）；仓库在本地属于其他 installation 时不变。
// 本地还没有记录时记下一条停用的记录，之后晚到的旧 added 事件据此知道它已经移除。
func DeactivateRepo(ctx context.Context, q Execer, m RepoMeta) error {
	_, err := q.Exec(ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url, active)
		VALUES ($1,$2,$3,$4,$5,$6,'','',false) ON CONFLICT (id) DO UPDATE SET active=false, updated_at=now()
		WHERE repos.installation_id=EXCLUDED.installation_id AND repos.active`,
		m.ID, m.InstallationID, m.OwnerID, m.Owner, m.Name, m.Private)
	return err
}

// MarkRepoDeleted 停用在 GitHub 上已删除的仓库（不论它在本地属于哪个 installation，本地配置和历史保留）。
// 本地还没有记录时记下一条停用的记录，之后晚到的旧 added / created 事件据此知道它已经删除。
func MarkRepoDeleted(ctx context.Context, q Execer, m RepoMeta) error {
	_, err := q.Exec(ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url, active)
		VALUES ($1,$2,$3,$4,$5,$6,'','',false) ON CONFLICT (id) DO UPDATE SET active=false, updated_at=now() WHERE repos.active`,
		m.ID, m.InstallationID, m.OwnerID, m.Owner, m.Name, m.Private)
	return err
}

// RepoMeta 是写入 repos 表的仓库信息。DefaultBranch、CloneURL 为空表示不知道（installation 事件的 payload 中没有），
// 已有的值保留；新记录的这两项留空，第一次用到前补齐（repodir.Complete）。
type RepoMeta struct {
	ID             int64
	InstallationID int64
	OwnerID        int64
	Owner, Name    string
	Private        bool
	DefaultBranch  string
	CloneURL       string
}

// UpsertRepo 记录一个仓库并设为启用。cache_salt、max_concurrency、ref_heads 基线等本地配置不变。
func UpsertRepo(ctx context.Context, q Execer, m RepoMeta) error {
	_, err := q.Exec(ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url, active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,true) ON CONFLICT (id) DO UPDATE SET installation_id=EXCLUDED.installation_id,
		owner_id=EXCLUDED.owner_id, owner=EXCLUDED.owner, name=EXCLUDED.name, private=EXCLUDED.private,
		default_branch=coalesce(nullif(EXCLUDED.default_branch,''), repos.default_branch),
		clone_url=coalesce(nullif(EXCLUDED.clone_url,''), repos.clone_url), active=true, updated_at=now()`,
		m.ID, m.InstallationID, m.OwnerID, m.Owner, m.Name, m.Private, m.DefaultBranch, m.CloneURL)
	return err
}

// syncStatusKey 是 kv 表中保存最近一次"刷新仓库"结果的键。
const syncStatusKey = "repos.last_sync"

// SyncStatus 是最近一次显式"刷新仓库"的结果，Dashboard 的仓库页显示。
type SyncStatus struct {
	At            time.Time `json:"at"`
	Actor         string    `json:"actor"`
	Error         string    `json:"error,omitempty"`
	Installations int       `json:"installations"`
	Repos         int       `json:"repos"`
}

// SaveSyncStatus 记录最近一次"刷新仓库"的结果。
func SaveSyncStatus(ctx context.Context, q Execer, s *SyncStatus) error {
	_, err := q.Exec(ctx, `INSERT INTO kv (key, value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`,
		syncStatusKey, string(MustJSON(s)))
	return err
}

// LoadSyncStatus 返回最近一次"刷新仓库"的结果；从没有刷新过时返回 nil。
func LoadSyncStatus(ctx context.Context, q Querier) (*SyncStatus, error) {
	var v string
	err := q.QueryRow(ctx, `SELECT value FROM kv WHERE key=$1`, syncStatusKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s SyncStatus
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return nil, err
	}
	return &s, nil
}
