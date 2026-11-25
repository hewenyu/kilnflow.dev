package store

import (
	"bytes"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// 三级作用域的测试数据：账号 alice（owner_id 100）下有仓库 app（10）和 web（20）；
// 账号 bob（owner_id 300）下有仓库 tools（30）。
//
//	environment：release 在账号级（alice）和 app 仓库级各有一个；staging 只在账号级；bob 也有一个账号级的 release
type scopeFixture struct {
	db                                *DB
	app, web, tools                   *Repo
	ownerRelease, appRelease, staging *Environment
	bobRelease                        *Environment
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	db := newDB(t)
	mustExec(t, db, `INSERT INTO installations (id, account_id, account_login) VALUES (1, 100, 'alice'), (2, 300, 'bob')`)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRepo(t, db, 20, 1, 100, "alice", "web")
	insertRepo(t, db, 30, 2, 300, "bob", "tools")
	f := &scopeFixture{db: db}
	for _, r := range []struct {
		id  int64
		dst **Repo
	}{{10, &f.app}, {20, &f.web}, {30, &f.tools}} {
		repo, err := GetRepo(t.Context(), db, r.id)
		if err != nil {
			t.Fatal(err)
		}
		*r.dst = repo
	}
	env := func(id, owner int64, repo *int64, name, policy string, publish []string) *Environment {
		mustExec(t, db, `INSERT INTO environments (id, owner_id, repo_id, name, ref_policy, allowed_publish_repos) VALUES ($1,$2,$3,$4,$5,$6)`,
			id, owner, repo, name, policy, publish)
		e, err := scanEnv(db.QueryRow(t.Context(), `SELECT `+envCols+` FROM environments WHERE id=$1`, id))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	f.ownerRelease = env(1, 100, nil, "release", `["refs/tags/v*"]`, []string{"alice/downloads"})
	f.appRelease = env(2, 100, new(int64(10)), "release", `["refs/tags/app-v*","refs/heads/main"]`, []string{})
	f.staging = env(3, 100, nil, "Staging", `[]`, []string{})
	f.bobRelease = env(4, 300, nil, "release", `[]`, []string{})
	return f
}

func (f *scopeFixture) setVar(t *testing.T, kind ScopeKind, id int64, name, value string) {
	t.Helper()
	mustExec(t, f.db, `INSERT INTO variables (scope_kind, scope_id, name, value) VALUES ($1,$2,$3,$4)`, string(kind), id, name, value)
}

var testMasterKey = bytes.Repeat([]byte{7}, secretbox.KeySize)

// setSecret 用 testMasterKey 加密并写入 secret，返回它的版本。
func (f *scopeFixture) setSecret(t *testing.T, kind ScopeKind, id int64, name, value string) int64 {
	t.Helper()
	sealed, err := secretbox.Seal(testMasterKey, secretbox.AAD(string(kind), id, name), []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	err = f.db.QueryRow(t.Context(), `INSERT INTO secrets (scope_kind, scope_id, name, ciphertext, wrapped_dek, updated_by) VALUES ($1,$2,$3,$4,$5,'admin')
		RETURNING version`, string(kind), id, name, sealed.Ciphertext, sealed.WrappedDEK).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestLookupEnvironment(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	for _, c := range []struct {
		name   string
		repo   int64
		env    string
		wantID int64 // 0 表示不存在
	}{
		{"仓库级整体取代账号级", 10, "release", f.appRelease.ID},
		{"没有仓库级时用账号级", 20, "release", f.ownerRelease.ID},
		{"名称不区分大小写", 10, "RELEASE", f.appRelease.ID},
		{"账号级（大小写不同）", 10, "staging", f.staging.ID},
		{"其他账号的仓库只看到自己账号的", 30, "release", f.bobRelease.ID},
		{"其他账号的 environment 不可见", 30, "staging", 0},
		{"不存在", 10, "prod", 0},
		{"不存在的仓库", 99, "release", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, err := LookupEnvironment(t.Context(), f.db, c.repo, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantID == 0 {
				if e != nil {
					t.Fatalf("应返回 nil，实际 %+v", e)
				}
				return
			}
			if e == nil || e.ID != c.wantID {
				t.Fatalf("LookupEnvironment = %+v，应为 id %d", e, c.wantID)
			}
		})
	}

	t.Run("字段", func(t *testing.T) {
		t.Parallel()
		e, err := LookupEnvironment(t.Context(), f.db, 20, "release")
		if err != nil {
			t.Fatal(err)
		}
		want := &Environment{ID: 1, OwnerID: 100, Name: "release", RefPolicy: []string{"refs/tags/v*"}, AllowedPublishRepos: []string{"alice/downloads"}}
		if !reflect.DeepEqual(e, want) {
			t.Fatalf("LookupEnvironment = %+v，应为 %+v", e, want)
		}
		e, err = LookupEnvironment(t.Context(), f.db, 10, "release")
		if err != nil {
			t.Fatal(err)
		}
		if e.RepoID == nil || *e.RepoID != 10 || !slices.Equal(e.RefPolicy, []string{"refs/tags/app-v*", "refs/heads/main"}) || len(e.AllowedPublishRepos) != 0 {
			t.Fatalf("仓库级 environment = %+v", e)
		}
	})

	t.Run("ref 策略无法解析", func(t *testing.T) {
		t.Parallel()
		mustExec(t, f.db, `INSERT INTO environments (id, owner_id, repo_id, name, ref_policy) VALUES (99, 100, 20, 'broken', '{"x":1}')`)
		if _, err := LookupEnvironment(t.Context(), f.db, 20, "broken"); err == nil || !strings.Contains(err.Error(), "ref 策略无法解析") {
			t.Fatalf("应返回解析错误：%v", err)
		}
	})
}

func TestEnvironment_RefAllowed(t *testing.T) {
	t.Parallel()
	e := &Environment{RefPolicy: []string{"refs/tags/v*", "refs/heads/main", "refs/heads/release/**"}}
	for ref, want := range map[string]bool{
		"refs/tags/v1.2.0":           true,
		"refs/heads/main":            true,
		"refs/heads/release/1.x/fix": true,
		"refs/heads/mainline":        false,
		"refs/pull/1/head":           false, // PR 只有显式写了 refs/pull/* 才匹配
		"main":                       false, // 与完整 ref 匹配
		"refs/tags/x1":               false,
	} {
		if got := e.RefAllowed(ref); got != want {
			t.Errorf("RefAllowed(%q) = %v，应为 %v", ref, got, want)
		}
	}
}

func TestVariablesFor(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	f.setVar(t, ScopeOwner, 100, "A", "owner")
	f.setVar(t, ScopeOwner, 100, "B", "owner")
	f.setVar(t, ScopeOwner, 100, "C", "owner")
	f.setVar(t, ScopeRepo, 10, "B", "repo")
	f.setVar(t, ScopeRepo, 10, "C", "repo")
	f.setVar(t, ScopeEnvironment, f.appRelease.ID, "C", "env")
	// 不相关的作用域
	f.setVar(t, ScopeRepo, 20, "A", "web")
	f.setVar(t, ScopeOwner, 300, "A", "bob")
	f.setVar(t, ScopeEnvironment, f.ownerRelease.ID, "D", "owner-env")
	// 作用域 ID 相同、类型不同不会混淆：仓库 ID 与 owner ID 都是 100 的情况
	f.setVar(t, ScopeRepo, 100, "A", "repo-100")

	for _, c := range []struct {
		name string
		repo *Repo
		env  *Environment
		want map[string]string
	}{
		{"environment > 仓库 > 账号", f.app, f.appRelease, map[string]string{"A": "owner", "B": "repo", "C": "env"}},
		{"没有 environment", f.app, nil, map[string]string{"A": "owner", "B": "repo", "C": "repo"}},
		{"只有账号级", f.web, nil, map[string]string{"A": "web", "B": "owner", "C": "owner"}},
		{"账号级 environment", f.web, f.ownerRelease, map[string]string{"A": "web", "B": "owner", "C": "owner", "D": "owner-env"}},
		{"其他账号", f.tools, nil, map[string]string{"A": "bob"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := VariablesFor(t.Context(), f.db, c.repo, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, c.want) {
				t.Fatalf("VariablesFor = %v，应为 %v", got, c.want)
			}
		})
	}

	t.Run("没有变量时返回空 map", func(t *testing.T) {
		t.Parallel()
		got, err := VariablesFor(t.Context(), f.db, &Repo{ID: 99, OwnerID: 999}, nil)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("VariablesFor = %v, %v", got, err)
		}
	})
}

func TestListVariablesAndSecretMeta(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	f.setVar(t, ScopeRepo, 10, "ZETA", "z")
	f.setVar(t, ScopeRepo, 10, "ALPHA", "a")
	f.setVar(t, ScopeRepo, 20, "OTHER", "o")
	v1 := f.setSecret(t, ScopeRepo, 10, "TOKEN", "t")
	v2 := f.setSecret(t, ScopeRepo, 10, "API_KEY", "k")
	f.setSecret(t, ScopeOwner, 10, "SAME_ID_OTHER_KIND", "x")

	ref := &ScopeRef{Kind: ScopeRepo, ID: 10}
	vars, err := ListVariables(t.Context(), f.db, ref)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Variable{{"ALPHA", "a"}, {"ZETA", "z"}}; !slices.Equal(vars, want) {
		t.Errorf("ListVariables = %v，应为 %v", vars, want)
	}
	metas, err := ListSecretMeta(t.Context(), f.db, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0].Name != "API_KEY" || metas[0].Version != v2 || metas[1].Name != "TOKEN" || metas[1].Version != v1 ||
		metas[0].UpdatedBy != "admin" || metas[0].UpdatedAt.IsZero() {
		t.Errorf("ListSecretMeta = %+v", metas)
	}
	if v1 == v2 {
		t.Errorf("每个 secret 的版本应不同：%d", v1)
	}
	empty, err := ListVariables(t.Context(), f.db, &ScopeRef{Kind: ScopeEnvironment, ID: 1})
	if err != nil || len(empty) != 0 {
		t.Errorf("空作用域：%v, %v", empty, err)
	}
}

func TestResolveSecrets(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	f.setSecret(t, ScopeOwner, 100, "A", "owner-a")
	f.setSecret(t, ScopeOwner, 100, "B", "owner-b")
	vRepoB := f.setSecret(t, ScopeRepo, 10, "B", "repo-b")
	f.setSecret(t, ScopeRepo, 10, "C", "repo-c")
	vEnvC := f.setSecret(t, ScopeEnvironment, f.appRelease.ID, "C", "env-c")
	f.setSecret(t, ScopeRepo, 20, "A", "web-a")

	t.Run("优先级与只解密引用的名字", func(t *testing.T) {
		t.Parallel()
		got, err := ResolveSecrets(t.Context(), f.db, testMasterKey, f.app, f.appRelease, []string{"A", "B", "C", "MISSING"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got["A"].Value != "owner-a" || got["B"] != (ResolvedSecret{"repo-b", vRepoB}) || got["C"] != (ResolvedSecret{"env-c", vEnvC}) {
			t.Fatalf("ResolveSecrets = %+v", got)
		}
		values, err := SecretsFor(t.Context(), f.db, testMasterKey, f.app, nil, []string{"B", "C"})
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"B": "repo-b", "C": "repo-c"}; !maps.Equal(values, want) {
			t.Fatalf("SecretsFor（没有 environment）= %v，应为 %v", values, want)
		}
		values, err = SecretsFor(t.Context(), f.db, testMasterKey, f.web, nil, []string{"A", "B", "C"})
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"A": "web-a", "B": "owner-b"}; !maps.Equal(values, want) {
			t.Fatalf("SecretsFor（web）= %v，应为 %v", values, want)
		}
	})

	t.Run("没有引用任何 secret", func(t *testing.T) {
		t.Parallel()
		for _, names := range [][]string{nil, {}} {
			got, err := ResolveSecrets(t.Context(), f.db, testMasterKey, f.app, f.appRelease, names)
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("ResolveSecrets(%v) = %v, %v", names, got, err)
			}
		}
	})

	t.Run("master key 不对", func(t *testing.T) {
		t.Parallel()
		wrong := bytes.Repeat([]byte{8}, secretbox.KeySize)
		_, err := SecretsFor(t.Context(), f.db, wrong, f.app, nil, []string{"A"})
		if err == nil || !strings.Contains(err.Error(), "解密 secret A") {
			t.Fatalf("应返回解密错误：%v", err)
		}
	})
}

// 密文绑定作用域和名字：挪到别的作用域或改名后不能解密。
func TestResolveSecrets_CiphertextBoundToScope(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, sql string
	}{
		{"挪到其他作用域", `UPDATE secrets SET scope_kind='repo', scope_id=10`},
		{"改名", `UPDATE secrets SET name='B'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newScopeFixture(t)
			f.setSecret(t, ScopeOwner, 100, "A", "secret")
			mustExec(t, f.db, c.sql)
			_, err := ResolveSecrets(t.Context(), f.db, testMasterKey, f.app, nil, []string{"A", "B"})
			if err == nil {
				t.Fatal("密文换了位置后不应能解密")
			}
		})
	}
}

func TestResolveScope(t *testing.T) {
	t.Parallel()
	f := newScopeFixture(t)
	// carol 的 installation 已卸载，仍有仓库记录：按仓库的 owner 找到账号
	mustExec(t, f.db, `INSERT INTO installations (id, account_id, account_login, deleted) VALUES (3, 500, 'carol', true)`)
	mustExec(t, f.db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, active) VALUES (50, 3, 500, 'carol', 'old', '', false)`)
	// dave 的 installation 已卸载、没有仓库
	mustExec(t, f.db, `INSERT INTO installations (id, account_id, account_login, deleted) VALUES (4, 600, 'dave', true)`)

	for _, c := range []struct {
		name               string
		account, repo, env string
		want               *ScopeRef
		wantErr            error
		wantMsg            string
	}{
		{name: "账号", account: "alice", want: &ScopeRef{OwnerID: 100, Kind: ScopeOwner, ID: 100, Label: "alice"}},
		{name: "账号不区分大小写，标签保留输入", account: "ALICE", want: &ScopeRef{OwnerID: 100, Kind: ScopeOwner, ID: 100, Label: "ALICE"}},
		{name: "installation 已卸载时按仓库的 owner 找", account: "carol", want: &ScopeRef{OwnerID: 500, Kind: ScopeOwner, ID: 500, Label: "carol"}},
		{name: "仓库", repo: "alice/app", want: &ScopeRef{OwnerID: 100, Kind: ScopeRepo, ID: 10, Label: "alice/app"}},
		{name: "仓库优先于账号", account: "bob", repo: "Alice/App", want: &ScopeRef{OwnerID: 100, Kind: ScopeRepo, ID: 10, Label: "alice/app"}},
		{name: "账号级 environment", account: "alice", env: "Release", want: &ScopeRef{OwnerID: 100, Kind: ScopeEnvironment, ID: f.ownerRelease.ID, Label: "alice/Release"}},
		{name: "仓库级 environment", repo: "alice/app", env: "release", want: &ScopeRef{OwnerID: 100, Kind: ScopeEnvironment, ID: f.appRelease.ID, Label: "alice/app/release"}},
		{name: "其他账号的同名 environment", account: "bob", env: "release", want: &ScopeRef{OwnerID: 300, Kind: ScopeEnvironment, ID: f.bobRelease.ID, Label: "bob/release"}},
		// 仓库作用域只找仓库级 environment，不回退到账号级
		{name: "仓库没有自己的 environment", repo: "alice/web", env: "release", wantErr: ErrEnvironmentNotFound, wantMsg: "kiln env set"},
		{name: "environment 不存在", account: "alice", env: "prod", wantErr: ErrEnvironmentNotFound, wantMsg: "environment prod 不存在"},
		{name: "仓库不存在", repo: "alice/none", wantErr: ErrRepoNotFound},
		{name: "仓库名格式不对", repo: "alice", wantMsg: "仓库应写成 owner/name：alice"},
		{name: "账号不存在", account: "nobody", wantMsg: "账号 nobody 不在 Kiln 中"},
		{name: "installation 已卸载且没有仓库", account: "dave", wantMsg: "账号 dave 不在 Kiln 中"},
		{name: "都没有指定", wantErr: ErrScopeRequired, wantMsg: "需要指定账号"},
		{name: "只指定 environment", env: "release", wantErr: ErrScopeRequired, wantMsg: "需要指定账号"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveScope(t.Context(), f.db, c.account, c.repo, c.env)
			if c.want != nil {
				if err != nil {
					t.Fatal(err)
				}
				if *got != *c.want {
					t.Fatalf("ResolveScope = %+v，应为 %+v", *got, *c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("应返回错误，实际 %+v", got)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Errorf("错误应包装 %v：%v", c.wantErr, err)
			}
			if c.wantErr == nil && (errors.Is(err, ErrRepoNotFound) || errors.Is(err, ErrEnvironmentNotFound)) {
				t.Errorf("不应包装 ErrRepoNotFound / ErrEnvironmentNotFound：%v", err)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("错误应包含 %q：%v", c.wantMsg, err)
			}
			// Dashboard 按错误的类型和原样的名称显示当前语言的说明
			switch {
			case errors.Is(err, ErrEnvironmentNotFound):
				if e, ok := errors.AsType[*EnvironmentNotFoundError](err); !ok || e.Name != c.env {
					t.Errorf("应是带 environment 名称 %q 的 *EnvironmentNotFoundError：%#v", c.env, err)
				}
			case errors.Is(err, ErrRepoNotFound):
				if e, ok := errors.AsType[*RepoNotFoundError](err); !ok || e.Name != c.repo {
					t.Errorf("应是带仓库名 %q 的 *RepoNotFoundError：%#v", c.repo, err)
				}
			case strings.Contains(c.wantMsg, "owner/name"):
				if e, ok := errors.AsType[*RepoNameError](err); !ok || e.Name != c.repo {
					t.Errorf("应是带仓库名 %q 的 *RepoNameError：%#v", c.repo, err)
				}
			case strings.Contains(c.wantMsg, "不在 Kiln 中"):
				if e, ok := errors.AsType[*AccountNotFoundError](err); !ok || e.Account != c.account {
					t.Errorf("应是带账号 %q 的 *AccountNotFoundError：%#v", c.account, err)
				}
			}
		})
	}
}
