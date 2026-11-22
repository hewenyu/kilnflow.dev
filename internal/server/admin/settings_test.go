package admin

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 作用域的写法。
var (
	accountScope = &apiv1.Scope{Account: testAccount}
	repoScope    = &apiv1.Scope{Repo: testRepo}
)

func envScope(repo, env string) *apiv1.Scope {
	if repo != "" {
		return &apiv1.Scope{Repo: repo, Environment: env}
	}
	return &apiv1.Scope{Account: testAccount, Environment: env}
}

// variables 返回作用域中的变量（名称 → 值）。
func (e *testEnv) variables(sc *apiv1.Scope) map[string]string {
	e.t.Helper()
	ref, err := resolveScope(e.ctx, e.db, sc)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := store.ListVariables(e.ctx, e.db, ref)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, v := range list {
		out[v.Name] = v.Value
	}
	return out
}

// secretMeta 返回作用域中名为 name 的 secret 的元数据，不存在时返回 nil。
func (e *testEnv) secretMeta(sc *apiv1.Scope, name string) *store.SecretMeta {
	e.t.Helper()
	ref, err := resolveScope(e.ctx, e.db, sc)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := store.ListSecretMeta(e.ctx, e.db, ref)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// repo 返回仓库 tester/demo 和它的 environment name（name 为空或不存在时为 nil），用来按 job 的视角读取变量和 secret。
func (e *testEnv) repoAndEnv(name string) (*store.Repo, *store.Environment) {
	e.t.Helper()
	repo, err := store.FindRepo(e.ctx, e.db, testRepo)
	if err != nil {
		e.t.Fatal(err)
	}
	if name == "" {
		return repo, nil
	}
	env, err := store.LookupEnvironment(e.ctx, e.db, repo.ID, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return repo, env
}

// configChanges 返回 config_versions 中的全部变更（按版本顺序）。
func (e *testEnv) configChanges() []map[string]any {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT owner_id, change, changed_by FROM config_versions ORDER BY version`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var owner int64
		var change []byte
		var by string
		if err := rows.Scan(&owner, &change, &by); err != nil {
			e.t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(change, &m); err != nil {
			e.t.Fatal(err)
		}
		m["_owner"], m["_by"] = owner, by
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestValidName(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"TOKEN", "a_b1", "_X"} {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", "a-b", "名字", "a b", "a.b", "A$"} {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) 应失败", n)
		}
	}
}

func TestValidRepoFullName(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"a/b", "Owner/repo.name", "o/r-1"} {
		if !ValidRepoFullName(s) {
			t.Errorf("ValidRepoFullName(%q) 应为 true", s)
		}
	}
	// 发布时按全名比较，这些写法永远不会匹配任何仓库
	for _, s := range []string{"", "ab", "/", "a/", "/b", "a/b/c", " a/b", "a/b ", "a /b"} {
		if ValidRepoFullName(s) {
			t.Errorf("ValidRepoFullName(%q) 应为 false", s)
		}
	}
}

func TestEmptyIfNil(t *testing.T) {
	t.Parallel()
	if v := emptyIfNil(nil); v == nil || len(v) != 0 {
		t.Fatalf("emptyIfNil(nil) = %#v", v)
	}
	if v := emptyIfNil([]string{"x"}); len(v) != 1 {
		t.Fatalf("emptyIfNil 不应改变非空切片：%#v", v)
	}
}

// TestService_ScopeResolution：账号、仓库、environment 三级作用域各自写入对应的 scope，名称不区分大小写；
// 无效的作用域报错且不写入任何东西。
func TestService_ScopeResolution(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo}, testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "shared", Account: testAccount}, testActor))

	for _, c := range []struct {
		name     string
		scope    *apiv1.Scope
		kind     store.ScopeKind
		label    string
		wantErr  string
		envNotFd bool
	}{
		{name: "账号级", scope: &apiv1.Scope{Account: "TESTER"}, kind: store.ScopeOwner, label: "TESTER"},
		{name: "仓库级", scope: &apiv1.Scope{Repo: "Tester/Demo"}, kind: store.ScopeRepo, label: testRepo},
		{name: "仓库级优先于账号", scope: &apiv1.Scope{Account: "other", Repo: testRepo}, kind: store.ScopeRepo, label: testRepo},
		{name: "仓库级 environment", scope: &apiv1.Scope{Repo: testRepo, Environment: "RELEASE"}, kind: store.ScopeEnvironment, label: "tester/demo/RELEASE"},
		{name: "账号级 environment", scope: &apiv1.Scope{Account: testAccount, Environment: "shared"}, kind: store.ScopeEnvironment, label: "tester/shared"},
		{name: "没有指定作用域", scope: nil, wantErr: "需要指定账号"},
		{name: "账号不存在", scope: &apiv1.Scope{Account: "nobody"}, wantErr: "账号 nobody 不在 Kiln 中"},
		{name: "仓库不存在", scope: &apiv1.Scope{Repo: "tester/nope"}, wantErr: "仓库不在 Kiln 中"},
		{name: "仓库写法错误", scope: &apiv1.Scope{Repo: "demo"}, wantErr: "owner/name"},
		{name: "environment 不存在", scope: &apiv1.Scope{Repo: testRepo, Environment: "nope"}, wantErr: "environment nope 不存在", envNotFd: true},
		// 账号级 environment 不能用仓库作用域访问
		{name: "仓库中没有账号级 environment", scope: &apiv1.Scope{Repo: testRepo, Environment: "shared"}, wantErr: "不存在", envNotFd: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref, err := resolveScope(e.ctx, e.db, c.scope)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("应失败并说明 %q：%v", c.wantErr, err)
				}
				if c.envNotFd != errors.Is(err, store.ErrEnvironmentNotFound) {
					t.Errorf("errors.Is(ErrEnvironmentNotFound) = %v，应为 %v", !c.envNotFd, c.envNotFd)
				}
				// 写操作同样失败，不写入任何东西
				before := e.count(`SELECT count(*) FROM variables`)
				if err := e.svc.SetVariable(e.ctx, c.scope, "X", "1", testActor); err == nil {
					t.Errorf("无效作用域中 SetVariable 应失败")
				}
				if e.count(`SELECT count(*) FROM variables`) != before {
					t.Errorf("失败的 SetVariable 写入了变量")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ref.Kind != c.kind || ref.Label != c.label || ref.OwnerID != testAccountID {
				t.Errorf("作用域 = %+v，应为 %s %q（owner %d）", ref, c.kind, c.label, testAccountID)
			}
		})
	}
}

func TestService_Variables(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo}, testActor))

	// 三级作用域各写一个同名变量，再各写一个只在本级的变量
	for _, w := range []struct {
		scope *apiv1.Scope
		value string
	}{{accountScope, "account"}, {repoScope, "repo"}, {envScope(testRepo, "release"), "env"}} {
		e.must(e.svc.SetVariable(e.ctx, w.scope, "CHANNEL", w.value, testActor))
		e.must(e.svc.SetVariable(e.ctx, w.scope, "ONLY_"+strings.ToUpper(w.value), "1", testActor))
	}
	if got := e.variables(repoScope); got["CHANNEL"] != "repo" || len(got) != 2 {
		t.Errorf("仓库级变量 = %v", got)
	}

	// job 的视角：environment > 仓库 > 账号
	repo, env := e.repoAndEnv("release")
	for _, c := range []struct {
		name string
		env  *store.Environment
		want string
		n    int
	}{{"没有 environment", nil, "repo", 2}, {"使用 environment", env, "env", 3}} {
		vars, err := store.VariablesFor(e.ctx, e.db, repo, c.env)
		if err != nil {
			t.Fatal(err)
		}
		if vars["CHANNEL"] != c.want || len(vars) != c.n+1 {
			t.Errorf("%s：job 看到的变量 = %v，CHANNEL 应为 %q", c.name, vars, c.want)
		}
	}

	// 覆盖：值被替换，不产生第二行
	e.must(e.svc.SetVariable(e.ctx, repoScope, "CHANNEL", "repo2", testActor))
	if got := e.variables(repoScope)["CHANNEL"]; got != "repo2" {
		t.Errorf("覆盖后的值 = %q", got)
	}

	// 删除：只删除指定作用域中的；删除不存在的不报错
	e.must(e.svc.DeleteVariable(e.ctx, repoScope, "CHANNEL", testActor))
	e.must(e.svc.DeleteVariable(e.ctx, repoScope, "NOPE", testActor))
	if _, ok := e.variables(repoScope)["CHANNEL"]; ok {
		t.Errorf("删除后仓库级变量仍然存在")
	}
	if got := e.variables(accountScope)["CHANNEL"]; got != "account" {
		t.Errorf("删除仓库级变量不应影响账号级：%q", got)
	}

	// 审计和配置版本：记录作用域、名称和值，操作者为登录用户
	sets := e.audits("config.variable.set")
	if len(sets) != 7 {
		t.Fatalf("variable.set 的审计日志有 %d 条，应为 7 条", len(sets))
	}
	if d := sets[2].Detail; sets[2].Actor != testActor || d["scope"] != "tester/demo" || d["name"] != "CHANNEL" || d["value"] != "repo" || d["op"] != "variable.set" {
		t.Errorf("审计日志 = %+v", sets[2])
	}
	dels := e.audits("config.variable.delete")
	if len(dels) != 2 || dels[0].Detail["name"] != "CHANNEL" || dels[0].Detail["scope"] != testRepo {
		t.Errorf("variable.delete 的审计日志 = %+v", dels)
	}
	changes := e.configChanges()
	last := changes[len(changes)-1]
	if last["op"] != "variable.delete" || last["name"] != "NOPE" || last["_by"] != testActor || last["_owner"] != int64(testAccountID) {
		t.Errorf("最后一个配置版本 = %v", last)
	}
}

func TestService_SetVariableInvalidName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, name := range []string{"", "BAD-NAME", "名字"} {
		if err := e.svc.SetVariable(e.ctx, repoScope, name, "v", testActor); err == nil {
			t.Errorf("SetVariable(%q) 应失败", name)
		}
		if err := e.svc.SetSecret(e.ctx, repoScope, name, "v", testActor); err == nil {
			t.Errorf("SetSecret(%q) 应失败", name)
		}
	}
	if n := e.count(`SELECT count(*) FROM variables`) + e.count(`SELECT count(*) FROM secrets`) + e.count(`SELECT count(*) FROM audit_log`); n != 0 {
		t.Errorf("名称无效时不应写入任何东西（%d 行）", n)
	}
}

func TestService_Secrets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo}, testActor))
	const value = "s3cr3t-value-xyz"

	e.must(e.svc.SetSecret(e.ctx, accountScope, "TOKEN", "account-token", "bob"))
	e.must(e.svc.SetSecret(e.ctx, envScope(testRepo, "release"), "TOKEN", value, testActor))

	// 只保存密文，用 master key 和作用域（AAD）能解开；job 的视角 environment 优先
	if n := e.count(`SELECT count(*) FROM secrets WHERE position($1::bytea in ciphertext) > 0`, []byte(value)); n != 0 {
		t.Errorf("secret 以明文保存")
	}
	repo, env := e.repoAndEnv("release")
	for _, c := range []struct {
		name string
		env  *store.Environment
		want string
	}{{"没有 environment", nil, "account-token"}, {"使用 environment", env, value}} {
		got, err := store.SecretsFor(e.ctx, e.db, testMasterKey, repo, c.env, []string{"TOKEN"})
		if err != nil {
			t.Fatal(err)
		}
		if got["TOKEN"] != c.want {
			t.Errorf("%s：TOKEN = %q，应为 %q", c.name, got["TOKEN"], c.want)
		}
	}
	if _, err := store.SecretsFor(e.ctx, e.db, []byte("ffffffffffffffffffffffffffffffff"), repo, env, []string{"TOKEN"}); err == nil {
		t.Errorf("用错误的 master key 不应能解密")
	}

	// 元数据：更新者；每次写入（即使值相同）都换新版本
	m1 := e.secretMeta(envScope(testRepo, "release"), "TOKEN")
	if m1 == nil || m1.UpdatedBy != testActor {
		t.Fatalf("secret 元数据 = %+v", m1)
	}
	e.must(e.svc.SetSecret(e.ctx, envScope(testRepo, "release"), "TOKEN", value, "carol"))
	m2 := e.secretMeta(envScope(testRepo, "release"), "TOKEN")
	if m2.Version == m1.Version || m2.UpdatedBy != "carol" {
		t.Errorf("再次写入后版本 %d → %d、更新者 %q：版本应变化，更新者应为 carol", m1.Version, m2.Version, m2.UpdatedBy)
	}

	// 审计日志只记录作用域和名称，不记录值
	sets := e.audits("config.secret.set")
	if len(sets) != 3 || sets[1].Detail["scope"] != "tester/demo/release" || sets[1].Detail["name"] != "TOKEN" {
		t.Fatalf("secret.set 的审计日志 = %+v", sets)
	}
	for _, a := range e.audits("") {
		if b := mustJSON(t, a.Detail); strings.Contains(string(b), value) {
			t.Errorf("审计日志 %s 中出现了 secret 的值", a.Action)
		}
	}
	if n := e.count(`SELECT count(*) FROM config_versions WHERE change::text LIKE '%'||$1||'%'`, value); n != 0 {
		t.Errorf("配置版本中出现了 secret 的值")
	}

	// 删除只影响指定作用域
	e.must(e.svc.DeleteSecret(e.ctx, envScope(testRepo, "release"), "TOKEN", testActor))
	if e.secretMeta(envScope(testRepo, "release"), "TOKEN") != nil || e.secretMeta(accountScope, "TOKEN") == nil {
		t.Errorf("删除 environment 中的 secret 后：environment 中应没有、账号级应保留")
	}
	e.wantAudit("config.secret.delete", testActor, map[string]any{"scope": "tester/demo/release", "name": "TOKEN"})
	if err := e.svc.DeleteSecret(e.ctx, &apiv1.Scope{Repo: "tester/nope"}, "TOKEN", testActor); err == nil {
		t.Errorf("在不存在的仓库中删除 secret 应失败")
	}
}

func TestService_UpsertEnvironment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addRepo(502, "other")

	// 新建账号级和仓库级 environment；ref 策略按 glob 匹配完整的 ref
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{
		Name: "release", Account: testAccount, RefPolicy: []string{"refs/tags/v*"}, AllowedPublishRepos: []string{"tester/pub"},
	}, testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "Release", Repo: testRepo, RefPolicy: []string{"refs/heads/main"}}, testActor))

	// 仓库级整体取代账号级的同名 environment；其他仓库仍然使用账号级的
	for _, c := range []struct {
		repo           int64
		allowed, block string
		publish        []string
	}{
		{testRepoID, "refs/heads/main", "refs/tags/v1.0.0", []string{}},
		{502, "refs/tags/v1.0.0", "refs/heads/main", []string{"tester/pub"}},
	} {
		env, err := store.LookupEnvironment(e.ctx, e.db, c.repo, "RELEASE")
		if err != nil || env == nil {
			t.Fatalf("仓库 %d 找不到 environment release：%v", c.repo, err)
		}
		if !env.RefAllowed(c.allowed) || env.RefAllowed(c.block) {
			t.Errorf("仓库 %d：ref 策略 %v 应允许 %s、拒绝 %s", c.repo, env.RefPolicy, c.allowed, c.block)
		}
		if !slices.Equal(env.AllowedPublishRepos, c.publish) {
			t.Errorf("仓库 %d：允许发布到的仓库 = %v，应为 %v", c.repo, env.AllowedPublishRepos, c.publish)
		}
	}

	// 整体覆盖：再次保存时 ref 策略和允许发布的仓库被替换（空表示不限制），挂在上面的变量不受影响
	e.must(e.svc.SetVariable(e.ctx, envScope("", "release"), "KEEP", "1", testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "RELEASE", Account: "Tester"}, testActor))
	env, err := store.LookupEnvironment(e.ctx, e.db, 502, "release")
	if err != nil {
		t.Fatal(err)
	}
	if len(env.RefPolicy) != 0 || len(env.AllowedPublishRepos) != 0 || !env.RefAllowed("refs/pull/1/head") {
		t.Errorf("覆盖后的 environment = %+v，ref 策略应为空（不限制）", env)
	}
	if e.count(`SELECT count(*) FROM environments`) != 2 {
		t.Errorf("同名（不区分大小写）的 environment 不应重复创建")
	}
	if got := e.variables(envScope("", "release"))["KEEP"]; got != "1" {
		t.Errorf("覆盖 environment 不应影响它的变量")
	}

	ups := e.audits("config.environment.upsert")
	if len(ups) != 3 {
		t.Fatalf("environment.upsert 的审计日志有 %d 条，应为 3 条", len(ups))
	}
	if d := ups[0].Detail; d["scope"] != testAccount || d["name"] != "release" ||
		string(mustJSON(t, d["ref_policy"])) != `["refs/tags/v*"]` || string(mustJSON(t, d["allowed_publish_repos"])) != `["tester/pub"]` {
		t.Errorf("审计日志 = %v", d)
	}
}

// TestService_UpsertEnvironmentCaseInsensitive 是回归测试：environment 名称在查找时不区分大小写，
// 保存只是大小写不同的名称（例如 CLI 的 kiln env set RELEASE）应更新已有的那个，而不是再建一个查找时无法区分的。
func TestService_UpsertEnvironmentCaseInsensitive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo, RefPolicy: []string{"refs/tags/v*"}}, testActor))
	e.must(e.svc.SetSecret(e.ctx, envScope(testRepo, "release"), "TOKEN", "x", testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "RELEASE", Repo: testRepo, RefPolicy: []string{"refs/heads/main"}}, testActor))

	var names []string
	rows, err := e.db.Query(e.ctx, `SELECT name FROM environments ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"release"}) {
		t.Fatalf("environment = %q，应只有原来的 release", names)
	}
	_, env := e.repoAndEnv("Release")
	if !env.RefAllowed("refs/heads/main") || env.RefAllowed("refs/tags/v1") {
		t.Errorf("ref 策略应更新为 refs/heads/main：%v", env.RefPolicy)
	}
	if e.secretMeta(envScope(testRepo, "RELEASE"), "TOKEN") == nil {
		t.Errorf("挂在 environment 上的 secret 应保留")
	}
}

func TestService_UpsertEnvironmentInvalid(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, c := range []struct {
		name string
		env  *apiv1.Environment
		want string
	}{
		{"nil", nil, "需要 environment 名称"},
		{"没有名称", &apiv1.Environment{Repo: testRepo}, "需要 environment 名称"},
		{"名称只有空白", &apiv1.Environment{Name: " ", Repo: testRepo}, "需要 environment 名称"},
		{"名称首尾有空白", &apiv1.Environment{Name: "release ", Repo: testRepo}, "不能以空白开头或结尾"},
		{"允许发布的仓库写法错误", &apiv1.Environment{Name: "x", Repo: testRepo, AllowedPublishRepos: []string{"pub"}}, "owner/name"},
		{"允许发布的仓库缺少名称", &apiv1.Environment{Name: "x", Repo: testRepo, AllowedPublishRepos: []string{"tester/"}}, "owner/name"},
		{"仓库不存在", &apiv1.Environment{Name: "x", Repo: "tester/nope"}, "仓库不在 Kiln 中"},
		{"没有作用域", &apiv1.Environment{Name: "x"}, "需要指定账号"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := e.svc.UpsertEnvironment(e.ctx, c.env, testActor)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应失败并说明 %q：%v", c.want, err)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM environments`) + e.count(`SELECT count(*) FROM audit_log`); n != 0 {
		t.Errorf("校验失败时不应写入任何东西")
	}
}

func TestService_DeleteEnvironment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo}, testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "staging", Repo: testRepo}, testActor))
	for _, env := range []string{"release", "staging"} {
		e.must(e.svc.SetVariable(e.ctx, envScope(testRepo, env), "V", env, testActor))
		e.must(e.svc.SetSecret(e.ctx, envScope(testRepo, env), "S", env, testActor))
	}
	e.must(e.svc.SetVariable(e.ctx, repoScope, "V", "repo", testActor))

	e.must(e.svc.DeleteEnvironment(e.ctx, envScope(testRepo, "RELEASE"), testActor))
	// environment 连同它的变量和 secret 一起删除；其他 environment 和仓库级的不受影响
	if n := e.count(`SELECT count(*) FROM environments WHERE name='release'`); n != 0 {
		t.Errorf("environment 没有删除")
	}
	if n := e.count(`SELECT count(*) FROM variables WHERE scope_kind='environment'`); n != 1 {
		t.Errorf("应只剩 staging 的变量，实际 %d 个", n)
	}
	if n := e.count(`SELECT count(*) FROM secrets WHERE scope_kind='environment'`); n != 1 {
		t.Errorf("应只剩 staging 的 secret，实际 %d 个", n)
	}
	if e.variables(repoScope)["V"] != "repo" {
		t.Errorf("仓库级变量不应受影响")
	}
	e.wantAudit("config.environment.delete", testActor, map[string]any{"scope": "tester/demo/RELEASE"})

	// 不是 environment 的作用域、不存在的 environment：失败，不删除任何东西
	for _, sc := range []*apiv1.Scope{repoScope, envScope(testRepo, "release")} {
		if err := e.svc.DeleteEnvironment(e.ctx, sc, testActor); err == nil {
			t.Errorf("DeleteEnvironment(%v) 应失败", sc)
		}
	}
	if e.count(`SELECT count(*) FROM environments`) != 1 || len(e.audits("config.environment.delete")) != 1 {
		t.Errorf("失败的删除不应改变任何东西")
	}
}

func TestService_ListEnvironments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Repo: testRepo, RefPolicy: []string{"refs/tags/v*"}}, testActor))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "docker-release", Account: testAccount}, testActor))
	// 另一个账号
	e.must(storeInstallation(e, 778, 9002, "other"))
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Name: "release", Account: "other"}, testActor))

	all, err := e.svc.ListEnvironments(e.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, env := range all {
		got = append(got, env.Account+"|"+env.Repo+"|"+env.Name)
	}
	// 按账号、仓库、名称排序；账号级的仓库为空
	want := []string{"other||release", "tester||docker-release", "tester|tester/demo|release"}
	if !slices.Equal(got, want) {
		t.Errorf("ListEnvironments = %q，应为 %q", got, want)
	}
	if !slices.Equal(all[2].RefPolicy, []string{"refs/tags/v*"}) || all[1].RefPolicy == nil {
		t.Errorf("ref 策略 = %v / %v", all[2].RefPolicy, all[1].RefPolicy)
	}
	mine, err := e.svc.ListEnvironments(e.ctx, "TESTER")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Errorf("按账号（不区分大小写）筛选应有 2 个：%v", mine)
	}

	// 账号改名后重新安装：旧的 installation 已删除，显示和筛选都用新的账号名（与 ResolveScope 一致）。
	// 修复前任取一个 installation，可能是已删除的旧名字
	e.exec(`UPDATE installations SET deleted=true WHERE id=$1`, testInstallation)
	e.must(storeInstallation(e, testInstallation+1000, testAccountID, "tester-renamed"))
	renamed, err := e.svc.ListEnvironments(e.ctx, "tester-renamed")
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 2 || renamed[0].Account != "tester-renamed" {
		t.Errorf("改名后按新名字筛选应有 2 个：%v", renamed)
	}
}

func storeInstallation(e *testEnv, id, account int64, login string) error {
	return store.UpsertInstallation(e.ctx, e.db, store.InstallationMeta{ID: id, AccountID: account, AccountLogin: login})
}

// TestUpsertEnvironmentRejectsRemovedFields：旧版 CLI 的 --reviewer、--wait 写在已删除的字段 5、6 中，
// Server 拒绝而不是静默忽略（不能让人误以为设置了审批）；没有这两个字段的旧请求照常处理。
func TestUpsertEnvironmentRejectsRemovedFields(t *testing.T) {
	t.Parallel()
	base, err := proto.Marshal(&apiv1.Environment{Name: "prod", Repo: "tester/demo", RefPolicy: []string{"refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := protowire.AppendString(protowire.AppendTag(base, 5, protowire.BytesType), "alice")
	wait := protowire.AppendVarint(protowire.AppendTag(base, 6, protowire.VarintType), 5)
	for name, b := range map[string][]byte{"reviewers": reviewer, "wait_timer_minutes": wait} {
		var e apiv1.Environment
		if err := proto.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		if e.Name != "prod" || len(e.RefPolicy) != 1 {
			t.Fatalf("%s：已知字段解析错误：%v", name, &e)
		}
		_, err := (&rpcAPI{}).UpsertEnvironment(t.Context(), connect.NewRequest(&apiv1.UpsertEnvironmentRequest{Environment: &e}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !errors.Is(err, errRemovedEnvFields) {
			t.Errorf("%s：应以 InvalidArgument 拒绝：%v", name, err)
		}
	}
}

// TestUpsertEnvironmentConcurrentCaseInsensitive 是回归测试：并发的 "Prod" / "prod" 创建由大小写不敏感的唯一索引
// environments_scope_name_ci 原子合并，只留下一个 environment（保留先写入的写法），不会因为两个事务的 UPDATE 都落空而各插一行。
func TestUpsertEnvironmentConcurrentCaseInsensitive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		name := "Prod"
		if i%2 == 1 {
			name = "prod"
		}
		wg.Go(func() {
			errs <- e.db.Tx(e.ctx, func(tx pgx.Tx) error {
				return upsertEnvironmentTx(e.ctx, tx, &apiv1.Environment{Name: name, Account: testAccount}, testActor)
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发创建同名（仅大小写不同）的 environment：%v", err)
		}
	}
	if c := e.count(`SELECT count(*) FROM environments WHERE owner_id IS NOT NULL AND lower(name)='prod'`); c != 1 {
		t.Fatalf("仅大小写不同的并发创建留下了 %d 个 environment，应合并为 1 个", c)
	}
	if c := e.count(`SELECT count(DISTINCT lower(name)) FROM environments`); c != 1 {
		t.Fatalf("environments 的不同名称（小写）数 = %d，应为 1", c)
	}
}
