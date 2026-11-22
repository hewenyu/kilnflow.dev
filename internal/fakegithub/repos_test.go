package fakegithub

import (
	"slices"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

func TestAppAndInstallations(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	app, err := f.c.GetApp(ctx)
	if err != nil || app.ID != f.s.AppID || app.Slug != f.s.AppSlug {
		t.Fatalf("GetApp = %+v %v", app, err)
	}

	f.s.AddInstallation(888)
	ids := func() []int64 {
		t.Helper()
		list, err := f.c.ListInstallations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, in := range list {
			if in.Account.Login != "tester" || in.Account.ID != f.s.AccountID {
				t.Errorf("installation 的账号：%+v", in.Account)
			}
			out = append(out, in.ID)
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []int64{777, 888}) {
		t.Errorf("ListInstallations = %v", got)
	}
	if in, err := f.c.GetInstallation(ctx, 888); err != nil || in.ID != 888 {
		t.Errorf("GetInstallation(888) = %+v %v", in, err)
	}

	// 仓库移到 888 后再卸载 888：installation 和它的仓库都消失
	f.s.MoveRepo(f.repo, 888)
	if in, err := f.c.GetRepoInstallation(ctx, demo); err != nil || in.ID != 888 {
		t.Errorf("GetRepoInstallation = %+v %v", in, err)
	}
	f.s.RemoveInstallation(888)
	if got := ids(); !slices.Equal(got, []int64{777}) {
		t.Errorf("卸载后 ListInstallations = %v", got)
	}
	if _, err := f.c.GetInstallation(ctx, 888); !github.IsNotFound(err) {
		t.Errorf("卸载后应返回 404：%v", err)
	}
	if _, err := f.c.GetRepoInstallation(ctx, demo); !github.IsNotFound(err) {
		t.Errorf("卸载后仓库也应消失：%v", err)
	}
}

// 仓库的授权范围：私有仓库只有所属 installation 的 token 能读取，公开仓库任何 token 都能读取；
// GET /repos/{owner}/{repo}/installation 只认仓库实际所属的 installation。
func TestRepoVisibility(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.s.AddInstallation(888)
	public := &Repo{ID: 2, Owner: "tester", Name: "pub", Dir: f.dir, Installation: 888}
	f.s.AddRepo(public)
	if public.DefaultBranch != "main" {
		t.Errorf("默认分支应为 main：%q", public.DefaultBranch)
	}
	if f.repo.Installation != f.s.InstallationID {
		t.Errorf("没有指定 installation 时应属于默认 installation：%d", f.repo.Installation)
	}
	other := f.c.Inst(888)

	names := func(inst *github.Inst) []string {
		t.Helper()
		repos, err := inst.ListRepos(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range repos {
			out = append(out, r.FullName)
		}
		return out
	}
	if got := names(f.inst); !slices.Equal(got, []string{demo}) {
		t.Errorf("默认 installation 的仓库：%v", got)
	}
	if got := names(other); !slices.Equal(got, []string{"tester/pub"}) {
		t.Errorf("888 的仓库：%v", got)
	}

	tests := []struct {
		name    string
		inst    *github.Inst
		repo    string
		id      int64
		visible bool
	}{
		{"所属 installation 读取私有仓库", f.inst, demo, 1, true},
		{"其他 installation 读不到私有仓库", other, demo, 1, false},
		{"任何 installation 都能读取公开仓库", f.inst, "tester/pub", 2, true},
		{"仓库名不区分大小写", f.inst, "Tester/Demo", 1, true},
		{"不存在的仓库", f.inst, "tester/none", 99, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := tt.inst.GetRepo(ctx, tt.repo)
			if tt.visible != (err == nil) || !tt.visible && !github.IsNotFound(err) {
				t.Errorf("GetRepo：%+v %v", r, err)
			}
			r, err = tt.inst.GetRepoByID(ctx, tt.id)
			if tt.visible != (err == nil) || !tt.visible && !github.IsNotFound(err) {
				t.Errorf("GetRepoByID：%+v %v", r, err)
			}
			if tt.visible && (r.ID != tt.id || r.CloneURL != "file://"+f.dir || r.DefaultBranch != "main") {
				t.Errorf("仓库信息：%+v", r)
			}
		})
	}
	if in, err := f.c.GetRepoInstallation(ctx, "tester/pub"); err != nil || in.ID != 888 {
		t.Errorf("公开仓库所属的 installation：%+v %v", in, err)
	}

	// 移出全部 installation：仓库仍然存在，但不属于任何 installation
	f.s.MoveRepo(f.repo, -1)
	if _, err := f.inst.GetRepo(ctx, demo); !github.IsNotFound(err) {
		t.Errorf("移出后私有仓库应不可见：%v", err)
	}
	if _, err := f.c.GetRepoInstallation(ctx, demo); !github.IsNotFound(err) {
		t.Errorf("移出后 GetRepoInstallation 应返回 404：%v", err)
	}
	if got := names(f.inst); len(got) != 0 {
		t.Errorf("移出后默认 installation 的仓库：%v", got)
	}

	// 删除仓库
	f.s.RemoveRepo(public)
	if _, err := f.inst.GetRepo(ctx, "tester/pub"); !github.IsNotFound(err) {
		t.Errorf("删除后应返回 404：%v", err)
	}
}

// 每个请求都记录为 "METHOD path"，没有路由的请求单独列出。
func TestRequestLog(t *testing.T) {
	f := newFixture(t)
	if _, err := f.inst.GetRepo(t.Context(), demo); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = f.inst.Do(t.Context(), "GET", "/nope?x=1", nil, &out)
	want := []string{"POST /app/installations/777/access_tokens", "GET /repos/tester/demo", "UNHANDLED GET /nope?x=1"}
	if got := f.s.Requests(); !slices.Equal(got, want) {
		t.Errorf("Requests = %v，应为 %v", got, want)
	}
	if got := f.s.UnhandledRequests(); !slices.Equal(got, []string{"GET /nope?x=1"}) {
		t.Errorf("UnhandledRequests = %v", got)
	}
	// 返回的是副本
	f.s.Requests()[0] = "changed"
	if f.s.Requests()[0] == "changed" {
		t.Error("修改 Requests 的结果影响了记录")
	}
}
