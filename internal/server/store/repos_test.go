package store

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRepo_FullNameAndIncomplete(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		repo       Repo
		incomplete bool
	}{
		{"信息完整", Repo{DefaultBranch: "main", CloneURL: "https://x"}, false},
		{"缺默认分支", Repo{CloneURL: "https://x"}, true},
		{"缺 clone 地址", Repo{DefaultBranch: "main"}, true},
		{"都缺", Repo{}, true},
	} {
		if got := c.repo.Incomplete(); got != c.incomplete {
			t.Errorf("%s：Incomplete = %v，应为 %v", c.name, got, c.incomplete)
		}
	}
	if got := (&Repo{Owner: "alice", Name: "app"}).FullName(); got != "alice/app" {
		t.Errorf("FullName = %q", got)
	}
}

func TestGetRepo(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url, active, cache_salt, max_concurrency, baseline_done)
		VALUES (10, 1, 100, 'alice', 'app', false, 'trunk', 'https://example.com/alice/app.git', false, 'salt', 3, true)`)
	got, err := GetRepo(t.Context(), db, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := Repo{
		ID: 10, InstallationID: 1, OwnerID: 100, Owner: "alice", Name: "app", DefaultBranch: "trunk",
		CloneURL: "https://example.com/alice/app.git", CacheSalt: "salt", MaxConcurrency: 3, BaselineDone: true,
	}
	if *got != want {
		t.Errorf("GetRepo = %+v，应为 %+v（停用的仓库也能按 ID 读取）", *got, want)
	}
	if _, err := GetRepo(t.Context(), db, 11); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("不存在的仓库应返回 pgx.ErrNoRows：%v", err)
	}
}

func TestFindRepo(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "Alice", "App")
	// 同名的旧仓库已停用（例如删除后重建）：只找启用中的
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, active) VALUES (9, 1, 100, 'alice', 'app', '', false)`)
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, active) VALUES (20, 1, 100, 'alice', 'gone', '', false)`)
	// 两个启用中的同名记录（例如转移后旧记录还没停用）：取最近更新的
	insertRepo(t, db, 30, 1, 100, "bob", "dup")
	insertRepo(t, db, 31, 2, 200, "bob", "dup")
	mustExec(t, db, `UPDATE repos SET updated_at = now() - interval '1 hour' WHERE id=31`)

	for _, c := range []struct {
		name, full string
		wantID     int64
		notFound   bool
		wantErr    string
	}{
		{name: "不区分大小写", full: "alice/app", wantID: 10},
		{name: "原样大小写", full: "Alice/App", wantID: 10},
		{name: "同名记录取最近更新的", full: "bob/dup", wantID: 30},
		{name: "已停用", full: "alice/gone", notFound: true},
		{name: "不存在", full: "alice/none", notFound: true},
		{name: "缺少 owner/", full: "app", wantErr: "owner/name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, err := FindRepo(t.Context(), db, c.full)
			switch {
			case c.notFound:
				if !errors.Is(err, ErrRepoNotFound) || !strings.Contains(err.Error(), c.full) {
					t.Fatalf("应返回包含仓库名的 ErrRepoNotFound：%v", err)
				}
			case c.wantErr != "":
				if err == nil || errors.Is(err, ErrRepoNotFound) || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("格式错误应返回包含 %q 的错误（不是 ErrRepoNotFound）：%v", c.wantErr, err)
				}
			case err != nil:
				t.Fatal(err)
			case r.ID != c.wantID:
				t.Fatalf("FindRepo(%s) 找到 %d，应为 %d", c.full, r.ID, c.wantID)
			}
		})
	}
}

// installationRow 是 installations 表中的一行。
type installationRow struct {
	AccountID          int64
	AccountLogin, Type string
	Suspended, Deleted bool
}

func getInstallation(t *testing.T, db *DB, id int64) *installationRow {
	t.Helper()
	var r installationRow
	err := db.QueryRow(t.Context(), `SELECT account_id, account_login, account_type, suspended, deleted FROM installations WHERE id=$1`, id).
		Scan(&r.AccountID, &r.AccountLogin, &r.Type, &r.Suspended, &r.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func installationState(t *testing.T, db *DB, id int64) InstallationState {
	t.Helper()
	s, err := GetInstallationState(t.Context(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpsertInstallation(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	if s := installationState(t, db, 1); s != InstallationUnknown {
		t.Fatalf("没有记录时状态应为 Unknown，实际 %v", s)
	}

	// 新记录：账号类型为空时记为 User
	if err := UpsertInstallation(ctx, db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got, want := *getInstallation(t, db, 1), (installationRow{AccountID: 100, AccountLogin: "alice", Type: "User"}); got != want {
		t.Errorf("新记录 = %+v，应为 %+v", got, want)
	}
	if s := installationState(t, db, 1); s != InstallationActive {
		t.Errorf("记录后状态应为 Active，实际 %v", s)
	}

	// 更新：账号改名、暂停
	if err := UpsertInstallation(ctx, db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "alice2", AccountType: "Organization", Suspended: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := *getInstallation(t, db, 1), (installationRow{AccountID: 100, AccountLogin: "alice2", Type: "Organization", Suspended: true}); got != want {
		t.Errorf("更新后 = %+v，应为 %+v", got, want)
	}

	// 已标记卸载的 installation 重新安装后恢复
	if err := DeactivateInstallation(ctx, db, 1); err != nil {
		t.Fatal(err)
	}
	if s := installationState(t, db, 1); s != InstallationDeleted {
		t.Fatalf("停用后状态应为 Deleted，实际 %v", s)
	}
	if err := UpsertInstallation(ctx, db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "alice2", AccountType: "Organization"}); err != nil {
		t.Fatal(err)
	}
	if s := installationState(t, db, 1); s != InstallationActive {
		t.Errorf("重新记录后应恢复为 Active，实际 %v", s)
	}
}

// repoActive 返回仓库的 installation 和是否启用；没有记录时 ok 为 false。
func repoActive(t *testing.T, db *DB, ids ...int64) map[int64]RepoState {
	t.Helper()
	s, err := RepoStates(t.Context(), db, ids)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarkInstallationDeleted(t *testing.T) {
	t.Parallel()

	t.Run("本地已有记录：标记卸载并停用它的仓库", func(t *testing.T) {
		t.Parallel()
		db := newDB(t)
		if err := UpsertInstallation(t.Context(), db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "alice", AccountType: "Organization"}); err != nil {
			t.Fatal(err)
		}
		insertRepo(t, db, 10, 1, 100, "alice", "a")
		insertRepo(t, db, 11, 1, 100, "alice", "b")
		insertRepo(t, db, 20, 2, 100, "alice", "c") // 其他 installation 的仓库不受影响
		// payload 中的账号信息不覆盖已有记录
		if err := MarkInstallationDeleted(t.Context(), db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "renamed", Suspended: true}); err != nil {
			t.Fatal(err)
		}
		if got, want := *getInstallation(t, db, 1), (installationRow{AccountID: 100, AccountLogin: "alice", Type: "Organization", Deleted: true}); got != want {
			t.Errorf("installation = %+v，应为 %+v", got, want)
		}
		want := map[int64]RepoState{10: {1, false}, 11: {1, false}, 20: {2, true}}
		if got := repoActive(t, db, 10, 11, 20); !maps.Equal(got, want) {
			t.Errorf("仓库状态 = %v，应为 %v", got, want)
		}
	})

	t.Run("本地没有记录：记下一条已卸载的记录", func(t *testing.T) {
		t.Parallel()
		db := newDB(t)
		if err := MarkInstallationDeleted(t.Context(), db, InstallationMeta{ID: 5, AccountID: 500, AccountLogin: "carol"}); err != nil {
			t.Fatal(err)
		}
		if got, want := *getInstallation(t, db, 5), (installationRow{AccountID: 500, AccountLogin: "carol", Type: "User", Deleted: true}); got != want {
			t.Errorf("installation = %+v，应为 %+v", got, want)
		}
		if s := installationState(t, db, 5); s != InstallationDeleted {
			t.Errorf("晚到的旧事件应能看到已卸载，实际 %v", s)
		}
	})
}

func TestDeactivateInstallation(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	if err := UpsertInstallation(t.Context(), db, InstallationMeta{ID: 1, AccountID: 100, AccountLogin: "alice"}); err != nil {
		t.Fatal(err)
	}
	insertRepo(t, db, 10, 1, 100, "alice", "a")
	insertRepo(t, db, 20, 2, 100, "alice", "b")
	if err := DeactivateInstallation(t.Context(), db, 1); err != nil {
		t.Fatal(err)
	}
	if s := installationState(t, db, 1); s != InstallationDeleted {
		t.Errorf("状态应为 Deleted，实际 %v", s)
	}
	want := map[int64]RepoState{10: {1, false}, 20: {2, true}}
	if got := repoActive(t, db, 10, 20); !maps.Equal(got, want) {
		t.Errorf("仓库状态 = %v，应为 %v", got, want)
	}

	// 本地没有记录时不新建记录（与 MarkInstallationDeleted 不同）
	if err := DeactivateInstallation(t.Context(), db, 9); err != nil {
		t.Fatal(err)
	}
	if r := getInstallation(t, db, 9); r != nil {
		t.Errorf("本地没有记录时不应新建：%+v", r)
	}
}

func TestRepoStates(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "a")
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, active) VALUES (11, 2, 100, 'alice', 'b', '', false)`)
	for _, c := range []struct {
		name string
		ids  []int64
		want map[int64]RepoState
	}{
		{"只返回本地已有记录的仓库", []int64{10, 11, 12}, map[int64]RepoState{10: {1, true}, 11: {2, false}}},
		{"都没有记录", []int64{99}, map[int64]RepoState{}},
		{"空列表", nil, map[int64]RepoState{}},
	} {
		got, err := RepoStates(t.Context(), db, c.ids)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || !maps.Equal(got, c.want) {
			t.Errorf("%s：RepoStates(%v) = %v，应为 %v", c.name, c.ids, got, c.want)
		}
	}
}

func TestDeactivateRepoAndMarkRepoDeleted(t *testing.T) {
	t.Parallel()
	meta := func(id, inst int64) RepoMeta {
		return RepoMeta{ID: id, InstallationID: inst, OwnerID: 100, Owner: "alice", Name: "app", Private: true}
	}
	cases := []struct {
		name string
		// existing 是已有记录的 installation 和是否启用；nil 表示本地没有记录
		existing *RepoState
		op       func(ctx context.Context, q Execer, m RepoMeta) error
		event    int64 // 事件中的 installation
		want     RepoState
	}{
		{"移出：同一 installation 的仓库停用", &RepoState{1, true}, DeactivateRepo, 1, RepoState{1, false}},
		{"移出：仓库属于其他 installation 时不变", &RepoState{2, true}, DeactivateRepo, 1, RepoState{2, true}},
		{"移出：本地没有记录时记下停用的记录", nil, DeactivateRepo, 1, RepoState{1, false}},
		{"移出：已停用的保持停用", &RepoState{1, false}, DeactivateRepo, 1, RepoState{1, false}},
		{"删除：同一 installation 的仓库停用", &RepoState{1, true}, MarkRepoDeleted, 1, RepoState{1, false}},
		{"删除：不论属于哪个 installation 都停用", &RepoState{2, true}, MarkRepoDeleted, 1, RepoState{2, false}},
		{"删除：本地没有记录时记下停用的记录", nil, MarkRepoDeleted, 1, RepoState{1, false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := newDB(t)
			if c.existing != nil {
				insertRepo(t, db, 10, c.existing.InstallationID, 100, "alice", "app")
				mustExec(t, db, `UPDATE repos SET active=$1, cache_salt='keep' WHERE id=10`, c.existing.Active)
			}
			if err := c.op(t.Context(), db, meta(10, c.event)); err != nil {
				t.Fatal(err)
			}
			if got := repoActive(t, db, 10)[10]; got != c.want {
				t.Fatalf("仓库状态 = %+v，应为 %+v", got, c.want)
			}
			r, err := GetRepo(t.Context(), db, 10)
			if err != nil {
				t.Fatal(err)
			}
			if c.existing != nil && r.CacheSalt != "keep" {
				t.Errorf("停用时应保留本地配置，cache_salt = %q", r.CacheSalt)
			}
			if c.existing == nil && (r.DefaultBranch != "" || r.CloneURL != "" || r.FullName() != "alice/app") {
				t.Errorf("新记录应只有事件中的信息：%+v", r)
			}
		})
	}
}

func TestUpsertRepo(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	m := RepoMeta{ID: 10, InstallationID: 1, OwnerID: 100, Owner: "alice", Name: "app", Private: true}

	// installation 事件的 payload 中没有默认分支和 clone 地址：新记录留空，等待补齐
	if err := UpsertRepo(ctx, db, m); err != nil {
		t.Fatal(err)
	}
	r, err := GetRepo(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Active || !r.Incomplete() || r.DefaultBranch != "" || r.CloneURL != "" {
		t.Fatalf("新记录应启用且缺少默认分支和 clone 地址：%+v", r)
	}

	// 补齐信息
	m.DefaultBranch, m.CloneURL = "main", "https://example.com/alice/app.git"
	if err := UpsertRepo(ctx, db, m); err != nil {
		t.Fatal(err)
	}
	// 本地配置
	mustExec(t, db, `UPDATE repos SET cache_salt='s1', max_concurrency=2, baseline_done=true, active=false WHERE id=10`)

	// 之后的事件没有这两项时保留已有的值；改名、换 installation、公开、重新启用；本地配置不变
	if err := UpsertRepo(ctx, db, RepoMeta{ID: 10, InstallationID: 2, OwnerID: 200, Owner: "org", Name: "app2"}); err != nil {
		t.Fatal(err)
	}
	r, err = GetRepo(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := Repo{
		ID: 10, InstallationID: 2, OwnerID: 200, Owner: "org", Name: "app2", DefaultBranch: "main",
		CloneURL: "https://example.com/alice/app.git", Active: true, CacheSalt: "s1", MaxConcurrency: 2, BaselineDone: true,
	}
	if *r != want {
		t.Errorf("更新后 = %+v\n应为 %+v", *r, want)
	}

	// 有新的默认分支时覆盖
	if err := UpsertRepo(ctx, db, RepoMeta{ID: 10, InstallationID: 2, OwnerID: 200, Owner: "org", Name: "app2", DefaultBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}
	if r, err := GetRepo(ctx, db, 10); err != nil || r.DefaultBranch != "trunk" || r.CloneURL == "" {
		t.Errorf("默认分支应更新为 trunk、clone 地址保留：%+v, %v", r, err)
	}
}

func TestSyncStatus(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	s, err := LoadSyncStatus(ctx, db)
	if err != nil || s != nil {
		t.Fatalf("从没有刷新过时应返回 nil：%+v, %v", s, err)
	}

	at := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	for _, want := range []SyncStatus{
		{At: at, Actor: "alice", Installations: 2, Repos: 7},
		// 再次保存时覆盖
		{At: at.Add(time.Hour), Actor: "bob", Error: "GitHub 限流"},
	} {
		if err := SaveSyncStatus(ctx, db, &want); err != nil {
			t.Fatal(err)
		}
		got, err := LoadSyncStatus(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || !got.At.Equal(want.At) || got.Actor != want.Actor || got.Error != want.Error ||
			got.Installations != want.Installations || got.Repos != want.Repos {
			t.Errorf("LoadSyncStatus = %+v，应为 %+v", got, want)
		}
	}

	mustExec(t, db, `UPDATE kv SET value='not json' WHERE key=$1`, syncStatusKey)
	if _, err := LoadSyncStatus(ctx, db); err == nil {
		t.Error("损坏的记录应返回错误")
	}
}
