package admin

import (
	"maps"
	"slices"
	"testing"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// dockerScope 是账号 tester 的 Docker Hub 连接所在的作用域。
var dockerScope = &apiv1.Scope{Account: testAccount, Environment: DockerEnvName}

// configRows 返回 environment、变量、secret、审计日志和配置版本的总行数（用来确认失败的操作没有留下任何东西）。
func (e *testEnv) configRows() map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, table := range []string{"environments", "variables", "secrets", "audit_log", "config_versions"} {
		out[table] = e.count(`SELECT count(*) FROM ` + table)
	}
	return out
}

func TestService_SaveDockerConnection(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	// 第一次保存：创建 docker-release（按给定的 ref 策略），写入用户名、命名空间和访问令牌
	e.must(e.svc.SaveDockerConnection(e.ctx, &DockerConnection{
		Account: testAccount, RefPolicy: []string{"refs/tags/v*"}, Username: new("bob"), Namespace: new("team"), Token: "dckr_pat_1",
	}, testActor))
	envs, err := e.svc.ListEnvironments(e.ctx, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].Name != DockerEnvName || envs[0].Repo != "" || !slices.Equal(envs[0].RefPolicy, []string{"refs/tags/v*"}) {
		t.Fatalf("应创建账号级 environment %s：%v", DockerEnvName, envs)
	}
	if got := e.variables(dockerScope); got[DockerUserVar] != "bob" || got[DockerNamespaceVar] != "team" || len(got) != 2 {
		t.Errorf("连接的变量 = %v", got)
	}
	tok := e.secretMeta(dockerScope, DockerTokenSecret)
	if tok == nil || tok.UpdatedBy != testActor {
		t.Fatalf("访问令牌 = %+v", tok)
	}
	// 配置变更与分别调用 UpsertEnvironment、SetVariable、SetSecret 相同
	var ops []string
	for _, c := range e.configChanges() {
		ops = append(ops, str(c["op"])+":"+str(c["name"]))
	}
	want := []string{"environment.upsert:" + DockerEnvName, "variable.set:" + DockerUserVar, "variable.set:" + DockerNamespaceVar, "secret.set:" + DockerTokenSecret}
	if !slices.Equal(ops, want) {
		t.Errorf("配置变更 = %q，应为 %q", ops, want)
	}

	// 再次保存（连接已存在）：只写入有变化的部分；令牌为空表示不修改，ref 策略不变
	before := e.count(`SELECT count(*) FROM config_versions`)
	e.must(e.svc.SaveDockerConnection(e.ctx, &DockerConnection{
		Account: testAccount, Environment: DockerEnvName, RefPolicy: []string{"refs/heads/*"}, Namespace: new("team2"),
	}, "carol"))
	if got := e.variables(dockerScope); got[DockerUserVar] != "bob" || got[DockerNamespaceVar] != "team2" {
		t.Errorf("只修改命名空间后的变量 = %v", got)
	}
	if after := e.secretMeta(dockerScope, DockerTokenSecret); after.Version != tok.Version {
		t.Errorf("令牌为空时不应修改访问令牌")
	}
	if n := e.count(`SELECT count(*) FROM config_versions`) - before; n != 1 {
		t.Errorf("只修改命名空间应只有 1 个配置变更，实际 %d 个", n)
	}
	if envs, _ := e.svc.ListEnvironments(e.ctx, testAccount); !slices.Equal(envs[0].RefPolicy, []string{"refs/tags/v*"}) {
		t.Errorf("连接已存在时不应修改 ref 策略：%v", envs[0].RefPolicy)
	}

	// 只替换令牌
	e.must(e.svc.SaveDockerConnection(e.ctx, &DockerConnection{Account: testAccount, Environment: DockerEnvName, Token: "dckr_pat_2"}, testActor))
	repo, _ := e.repoAndEnv("")
	env, err := store.LookupEnvironment(e.ctx, e.db, repo.ID, DockerEnvName)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.SecretsFor(e.ctx, e.db, testMasterKey, repo, env, []string{DockerTokenSecret})
	if err != nil || got[DockerTokenSecret] != "dckr_pat_2" {
		t.Errorf("替换后的令牌 = %q（%v）", got[DockerTokenSecret], err)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// TestService_SaveDockerConnectionAtomic：保存中途失败时全部回滚，不留下半套配置（environment、变量、secret、审计）。
func TestService_SaveDockerConnectionAtomic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// setup 构造失败：返回要使用的测试环境
		setup func(t *testing.T) *testEnv
		conn  DockerConnection
	}{
		{
			// 写入最后一步（访问令牌）时数据库报错：前面已经创建的 environment 和变量都要回滚
			name: "写入令牌时失败",
			setup: func(t *testing.T) *testEnv {
				t.Helper()
				e := newEnv(t)
				e.failOn("secrets", "INSERT", "true")
				return e
			},
		},
		{
			// 令牌无法加密（master key 无效）
			name:  "加密令牌失败",
			setup: func(t *testing.T) *testEnv { t.Helper(); return newEnvWith(t, envOpts{masterKey: []byte("short")}) },
		},
		{
			// 第二个变量写入时失败
			name: "写入命名空间时失败",
			setup: func(t *testing.T) *testEnv {
				t.Helper()
				e := newEnv(t)
				e.failOn("variables", "INSERT", "NEW.name = '"+DockerNamespaceVar+"'")
				return e
			},
		},
		{
			name:  "账号不存在",
			setup: newEnv,
			conn:  DockerConnection{Account: "nobody"},
		},
		{
			name:  "指定的连接 environment 不存在",
			setup: newEnv,
			conn:  DockerConnection{Environment: "nope"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := c.setup(t)
			before := e.configRows()
			conn := c.conn
			if conn.Account == "" {
				conn.Account = testAccount
			}
			conn.RefPolicy, conn.Username, conn.Namespace, conn.Token = []string{"refs/tags/v*"}, new("bob"), new("team"), "dckr_pat"
			if err := e.svc.SaveDockerConnection(e.ctx, &conn, testActor); err == nil {
				t.Fatal("应失败")
			}
			if after := e.configRows(); !maps.Equal(before, after) {
				t.Errorf("失败后应全部回滚：之前 %v，之后 %v", before, after)
			}
		})
	}
}

func TestService_DeleteDockerConnection(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.SaveDockerConnection(e.ctx, &DockerConnection{
		Account: testAccount, RefPolicy: []string{"refs/tags/v*"}, Username: new("bob"), Namespace: new("team"), Token: "dckr_pat",
	}, testActor))
	// docker-release 中的其他变量和 secret 不属于连接
	e.must(e.svc.SetVariable(e.ctx, dockerScope, "EXTRA", "1", testActor))
	e.must(e.svc.SetSecret(e.ctx, dockerScope, "OTHER", "x", testActor))

	e.must(e.svc.DeleteDockerConnection(e.ctx, testAccount, DockerEnvName, "carol"))
	if got := e.variables(dockerScope); len(got) != 1 || got["EXTRA"] != "1" {
		t.Errorf("删除后应只剩 EXTRA：%v", got)
	}
	if e.secretMeta(dockerScope, DockerTokenSecret) != nil || e.secretMeta(dockerScope, "OTHER") == nil {
		t.Errorf("应只删除访问令牌")
	}
	envs, err := e.svc.ListEnvironments(e.ctx, testAccount)
	if err != nil || len(envs) != 1 || !slices.Equal(envs[0].RefPolicy, []string{"refs/tags/v*"}) {
		t.Errorf("environment 及其 ref 策略应保留：%v %v", envs, err)
	}
	for _, action := range []string{"config.variable.delete", "config.secret.delete"} {
		for _, a := range e.audits(action) {
			if a.Actor != "carol" || a.Detail["scope"] != "tester/"+DockerEnvName {
				t.Errorf("%s 的审计日志 = %+v", action, a)
			}
		}
	}
	if n := len(e.audits("config.variable.delete")); n != 2 {
		t.Errorf("应删除 2 个变量，审计日志 %d 条", n)
	}
}

// TestService_DeleteDockerConnectionAtomic：删除中途失败时全部回滚，用户名、命名空间和令牌都还在。
func TestService_DeleteDockerConnectionAtomic(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.must(e.svc.SaveDockerConnection(e.ctx, &DockerConnection{
		Account: testAccount, Username: new("bob"), Namespace: new("team"), Token: "dckr_pat",
	}, testActor))
	// 删除最后一步（访问令牌）时失败
	e.failOn("secrets", "DELETE", "true")
	before := e.configRows()

	if err := e.svc.DeleteDockerConnection(e.ctx, testAccount, DockerEnvName, testActor); err == nil {
		t.Fatal("应失败")
	}
	if after := e.configRows(); !maps.Equal(before, after) {
		t.Errorf("失败后应全部回滚：之前 %v，之后 %v", before, after)
	}
	if got := e.variables(dockerScope); got[DockerUserVar] != "bob" || got[DockerNamespaceVar] != "team" {
		t.Errorf("用户名和命名空间应保留：%v", got)
	}
	if err := e.svc.DeleteDockerConnection(e.ctx, testAccount, "nope", testActor); err == nil {
		t.Errorf("environment 不存在时应失败")
	}
}
