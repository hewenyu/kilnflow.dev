package fakegithub

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// 写请求按 token 限定的仓库和权限校验；已经吊销的 token 不能再用；不限定的 token（共用客户端）照常。
func TestTokenScope(t *testing.T) {
	f := newFixture(t)
	other := &Repo{ID: 2, Owner: "tester", Name: "other", Dir: newRepo(t), Private: true}
	f.s.AddRepo(other)
	f.s.SetPull(f.repo, Pull{Number: 3, HeadRef: "feature", HeadSHA: gitOut(t, f.dir, "rev-parse", "HEAD")})
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	issue := func(t *testing.T, repoID int64, perms map[string]string) *github.Inst {
		t.Helper()
		tok, err := f.c.CreateToken(t.Context(), f.s.InstallationID, []int64{repoID}, perms)
		if err != nil {
			t.Fatal(err)
		}
		return f.c.InstWithToken(f.s.InstallationID, tok.Token)
	}
	createRef := func(inst *github.Inst, name string) error {
		return inst.CreateRef(t.Context(), demo, "refs/heads/"+name, head)
	}
	addLabel := func(inst *github.Inst) error { return inst.AddLabels(t.Context(), demo, 3, "x") }
	forbidden := func(err error) bool {
		ae, ok := errors.AsType[*github.APIError](err)
		return ok && ae.Status == http.StatusForbidden
	}

	tests := []struct {
		name      string
		repoID    int64
		perms     map[string]string
		refOK     bool
		labelOK   bool
		getRepoOK bool
	}{
		{"本仓库 contents: write", 1, map[string]string{"contents": "write"}, true, false, true},
		{"本仓库 contents: read", 1, map[string]string{"contents": "read"}, false, false, true},
		{"本仓库 pull_requests: write", 1, map[string]string{"pull_requests": "write"}, false, true, true},
		{"别的仓库 contents: write", 2, map[string]string{"contents": "write"}, false, false, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := issue(t, tt.repoID, tt.perms)
			err := createRef(inst, "b"+string(rune('a'+i)))
			if (err == nil) != tt.refOK || (err != nil && !forbidden(err)) {
				t.Errorf("建分支：%v，应 %v", err, tt.refOK)
			}
			err = addLabel(inst)
			if (err == nil) != tt.labelOK || (err != nil && !forbidden(err)) {
				t.Errorf("加 label：%v，应 %v", err, tt.labelOK)
			}
			if _, err := inst.GetRepo(t.Context(), demo); err != nil {
				t.Errorf("读请求不按权限校验：%v", err)
			}
		})
	}

	// 不限定仓库和权限的 token（共用客户端）照常
	if err := createRef(f.inst, "shared"); err != nil {
		t.Errorf("共用客户端建分支：%v", err)
	}

	// 吊销后再用：401
	tok, err := f.c.CreateToken(t.Context(), f.s.InstallationID, []int64{1}, map[string]string{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.RevokeToken(t.Context(), f.s.InstallationID, tok.Token); err != nil {
		t.Fatal(err)
	}
	inst := f.c.InstWithToken(f.s.InstallationID, tok.Token)
	for name, err := range map[string]error{"写": createRef(inst, "after-revoke"), "读": func() error { _, err := inst.GetRepo(t.Context(), demo); return err }()} {
		if ae, ok := errors.AsType[*github.APIError](err); !ok || ae.Status != http.StatusUnauthorized {
			t.Errorf("吊销后的%s请求应返回 401：%v", name, err)
		}
	}
	if got, ok := f.s.IssuedToken(tok.Token); !ok || !got.Revoked || got.Permissions["contents"] != "write" || len(got.RepoIDs) != 1 {
		t.Errorf("签发记录 %+v", got)
	}
}

// 注入的故障：签发已生效但响应丢失（客户端结果不明，模拟中记有这枚 token）、签发失败、吊销失败（token 仍然有效）。
func TestTokenFaults(t *testing.T) {
	f := newFixture(t)
	route := CreateTokenRoute(f.s.InstallationID)

	f.s.SetFault(route, Fault{Lost: true})
	_, err := f.c.CreateToken(t.Context(), f.s.InstallationID, []int64{1}, map[string]string{"contents": "read"})
	if err == nil || github.NotApplied(err) {
		t.Fatalf("响应丢失应是结果不明：%v", err)
	}
	toks := f.s.IssuedTokens()
	if len(toks) != 1 || toks[0].Revoked {
		t.Fatalf("响应丢失时模拟中应有一枚有效的 token：%+v", toks)
	}

	f.s.SetFault(route, Fault{Status: http.StatusInternalServerError})
	if _, err := f.c.CreateToken(t.Context(), f.s.InstallationID, nil, nil); err == nil {
		t.Fatal("注入的签发失败应返回错误")
	}
	if n := len(f.s.IssuedTokens()); n != 1 {
		t.Errorf("签发失败不应记下 token，实际 %d 枚", n)
	}

	f.s.SetFault(RevokeTokenRoute, Fault{Status: http.StatusInternalServerError})
	if err := f.c.RevokeToken(t.Context(), f.s.InstallationID, toks[0].Token); err == nil {
		t.Fatal("注入的吊销失败应返回错误")
	}
	if got, _ := f.s.IssuedToken(toks[0].Token); got.Revoked {
		t.Error("吊销失败后 token 不应被吊销")
	}
	if err := f.c.RevokeToken(t.Context(), f.s.InstallationID, toks[0].Token); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.IssuedToken(toks[0].Token); !got.Revoked {
		t.Error("故障去掉后应能吊销")
	}
}

// installation 的归属：签发时显式列出的、本模拟登记过的仓库必须属于这个 installation，写请求的目标仓库必须属于 token 的
// installation（不限定仓库的 token 也一样）。
func TestTokenInstallation(t *testing.T) {
	f := newFixture(t)
	const otherInst = 888
	f.s.AddInstallation(otherInst)
	foreign := &Repo{ID: 2, Owner: "tester", Name: "foreign", Dir: newRepo(t), Private: true, Installation: otherInst}
	f.s.AddRepo(foreign)
	unprocessable := func(err error) bool {
		ae, ok := errors.AsType[*github.APIError](err)
		return ok && ae.Status == http.StatusUnprocessableEntity
	}
	for name, ids := range map[string][]int64{"别的 installation 的仓库": {foreign.ID}, "混有别的 installation": {f.repo.ID, foreign.ID}} {
		if _, err := f.c.CreateToken(t.Context(), f.s.InstallationID, ids, map[string]string{"contents": "write"}); !unprocessable(err) {
			t.Errorf("%s：签发应被拒绝（422）：%v", name, err)
		}
	}
	if n := len(f.s.IssuedTokens()); n != 0 {
		t.Errorf("被拒绝的签发不应记下 token，实际 %d 枚", n)
	}
	head := gitOut(t, foreign.Dir, "rev-parse", "HEAD")
	// 本 installation 的不限定仓库的 token 不能写别的 installation 的仓库
	err := f.inst.CreateRef(t.Context(), foreign.FullName(), "refs/heads/x", head)
	if ae, ok := errors.AsType[*github.APIError](err); !ok || ae.Status != http.StatusForbidden {
		t.Errorf("跨 installation 写应返回 403：%v", err)
	}
	// 那个 installation 自己的 token 可以
	if err := f.c.Inst(otherInst).CreateRef(t.Context(), foreign.FullName(), "refs/heads/x", head); err != nil {
		t.Errorf("所属 installation 写自己的仓库：%v", err)
	}
}
