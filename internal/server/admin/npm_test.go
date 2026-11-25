package admin

import (
	"errors"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestNpmAccount(t *testing.T) {
	e := newEnv(t)
	const token = "npm_s3cr3t-token-value"
	acct, err := e.svc.SetNpmAccount(e.ctx, testAccount, "acme", "", " "+token+"\n", "alice")
	if err != nil {
		t.Fatal(err)
	}
	// scope 补上 @，registry 默认 npmjs
	if acct.Scope != "@acme" || acct.Registry != "https://registry.npmjs.org/" || acct.Account != testAccount || acct.UpdatedBy != "alice" {
		t.Errorf("配置 %+v", acct)
	}
	repo, err := store.FindRepo(e.ctx, e.db, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := store.ResolveNpmCredentials(e.ctx, e.db, testMasterKey, repo.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || creds.Token != token || creds.Scope != "@acme" {
		t.Fatalf("解密后的配置 %+v", creds)
	}
	first := creds.Version

	// 覆盖：registry 补上结尾的 /，版本号变化
	if _, err := e.svc.SetNpmAccount(e.ctx, testAccount, "@acme", "https://npm.example.com/r", "t2", "bob"); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListNpmAccounts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Registry != "https://npm.example.com/r/" || list[0].UpdatedBy != "bob" {
		t.Errorf("列表 %+v", list)
	}
	if creds, _ = store.ResolveNpmCredentials(e.ctx, e.db, testMasterKey, repo.OwnerID); creds.Token != "t2" || creds.Version == first {
		t.Errorf("覆盖后 %+v", creds)
	}

	// 配置变更审计不含 token
	var ops []string
	for _, c := range e.configChanges() {
		op, _ := c["op"].(string)
		ops = append(ops, op)
		for k, v := range c {
			if s, ok := v.(string); ok && strings.Contains(s, token) {
				t.Errorf("审计中出现了 token：%s=%s", k, s)
			}
		}
	}
	if strings.Join(ops, ",") != "npm.set,npm.set" {
		t.Errorf("配置变更 %v", ops)
	}

	if err := e.svc.DeleteNpmAccount(e.ctx, testAccount, "alice"); err != nil {
		t.Fatal(err)
	}
	if creds, _ = store.ResolveNpmCredentials(e.ctx, e.db, testMasterKey, repo.OwnerID); creds != nil {
		t.Errorf("删除后仍有配置 %+v", creds)
	}
	err = e.svc.DeleteNpmAccount(e.ctx, testAccount, "alice")
	if err == nil || !strings.Contains(err.Error(), "没有配置 npm") {
		t.Errorf("再次删除：%v", err)
	}
	if ne, ok := errors.AsType[*NpmNotConfiguredError](err); !ok || ne.Account != testAccount {
		t.Errorf("再次删除应是 *NpmNotConfiguredError：%#v", err)
	}
}

func TestNpmAccountInvalid(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name, account, scope, registry, token, want string
		// is 判断错误的类型（Dashboard 按它显示当前语言的说明）
		is func(error) bool
	}{
		{"scope 大写", testAccount, "@Bad", "", "t", "scope", func(err error) bool {
			se, ok := errors.AsType[*npm.ScopeError](err)
			return ok && se.Scope == "@Bad" && se.Part != nil && se.Part.Problem == npm.PartUppercase
		}},
		{"scope 为空", testAccount, "", "", "t", "scope", func(err error) bool {
			se, ok := errors.AsType[*npm.ScopeError](err)
			return ok && se.Part != nil && se.Part.Problem == npm.PartEmpty
		}},
		{"registry 不是 https", testAccount, "@ok", "http://npm.example.com/", "t", "https", func(err error) bool {
			ee, ok := errors.AsType[*endpoint.Error](err)
			return ok && ee.Problem == endpoint.ProblemScheme && ee.Raw == "http://npm.example.com/"
		}},
		{"token 为空", testAccount, "@ok", "", "  ", "token 为空", isErr(ErrNpmTokenEmpty)},
		{"token 太长", testAccount, "@ok", "", strings.Repeat("t", maxNpmToken+1), "token 太长", isErr(ErrNpmTokenTooLong)},
		{"账号不存在", "nobody", "@ok", "", "t", "不在 Kiln 中", func(err error) bool {
			ae, ok := errors.AsType[*store.AccountNotFoundError](err)
			return ok && ae.Account == "nobody"
		}},
		{"没有账号", "", "@ok", "", "t", "需要指定账号", isErr(store.ErrScopeRequired)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.SetNpmAccount(e.ctx, tc.account, tc.scope, tc.registry, tc.token, "alice")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %v，期望包含 %q", err, tc.want)
			}
			if err != nil && !tc.is(err) {
				t.Errorf("错误的类型不对：%#v", err)
			}
		})
	}
	if list, _ := e.svc.ListNpmAccounts(e.ctx); len(list) != 0 {
		t.Errorf("不应保存：%+v", list)
	}
}
