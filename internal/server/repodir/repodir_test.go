package repodir

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const (
	// otherInst 是同一账号下的另一个 installation。
	otherInst = 888
	// accountID 是模拟 GitHub 中账号的 ID（fakegithub 的默认值）。
	accountID = 9001
)

// appKey 是 App 的私钥；生成 RSA 密钥较慢，整个包共用一个。
var appKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// testEnv 是模拟的 GitHub（默认 installation 777，另有 installation 888）和它的客户端。
//
// 仓库：
//   - tester/priv（1）：私有，属于 777
//   - tester/pub（2）：公开，属于 777
//   - tester/other-priv（3）：私有，属于 888
//   - tester/other-pub（4）：公开，属于 888
//   - tester/no-app（5）：公开，没有安装 App
type testEnv struct {
	fake   *fakegithub.Server
	client *gh.Client
	inst   int64
	// overrides 按 "METHOD path" 替换模拟 GitHub 的响应。
	overrides map[string]http.HandlerFunc
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	fake.AddInstallation(otherInst)
	for _, r := range []*fakegithub.Repo{
		{ID: 1, Owner: "tester", Name: "priv", Private: true, Dir: "/src/priv"},
		{ID: 2, Owner: "tester", Name: "pub", Dir: "/src/pub"},
		{ID: 3, Owner: "tester", Name: "other-priv", Private: true, Installation: otherInst},
		{ID: 4, Owner: "tester", Name: "other-pub", Installation: otherInst},
		{ID: 5, Owner: "tester", Name: "no-app", Installation: -1}, // -1：不在任何 installation 中
	} {
		fake.AddRepo(r)
	}

	e := &testEnv{fake: fake, inst: fake.InstallationID, overrides: map[string]http.HandlerFunc{}}
	// 在模拟 GitHub 前加一层，用来注入异常响应
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := e.overrides[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		fake.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	e.client = &gh.Client{AppID: fake.AppID, Key: appKey(), APIURL: proxy.URL, UploadURL: proxy.URL}
	return e
}

// apiCalls 返回模拟 GitHub 收到的 API 请求（不含签发 token）。
func (e *testEnv) apiCalls() []string {
	return slices.DeleteFunc(e.fake.Requests(), func(r string) bool { return strings.HasSuffix(r, "/access_tokens") })
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestMetaOf(t *testing.T) {
	t.Parallel()
	r := &gh.Repository{
		ID: 1, Name: "app", FullName: "alice/app", Owner: gh.Account{Login: "alice", ID: 100}, Private: true,
		DefaultBranch: "main", CloneURL: "https://github.com/alice/app.git",
	}
	want := store.RepoMeta{
		ID: 1, InstallationID: 7, OwnerID: 100, Owner: "alice", Name: "app", Private: true,
		DefaultBranch: "main", CloneURL: "https://github.com/alice/app.git",
	}
	if got := MetaOf(7, r); got != want {
		t.Fatalf("MetaOf = %+v，应为 %+v", got, want)
	}
}

func TestNotInstalled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrNotInstalled", ErrNotInstalled, true},
		{"包装的 ErrNotInstalled", fmt.Errorf("x：%w", ErrNotInstalled), true},
		{"GitHub 404", &gh.APIError{Status: http.StatusNotFound}, true},
		{"包装的 GitHub 404", fmt.Errorf("x：%w", &gh.APIError{Status: http.StatusNotFound}), true},
		{"GitHub 403", &gh.APIError{Status: http.StatusForbidden}, false},
		{"其他错误", errors.New("timeout"), false},
	} {
		if got := NotInstalled(c.err); got != c.want {
			t.Errorf("%s：NotInstalled = %v，应为 %v", c.name, got, c.want)
		}
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	installationPath := func(name string) string { return "GET /repos/tester/" + name + "/installation" }
	cases := []struct {
		name string
		id   int64
		// override 替换一个接口的响应
		override     string
		response     http.HandlerFunc
		wantName     string
		notInstalled bool
		wantErr      bool
		// wantInstallationQuery 表示是否应定向查询仓库所属的 installation
		wantInstallationQuery string
	}{
		{name: "私有仓库读到即在授权范围内", id: 1, wantName: "priv"},
		{name: "公开仓库确认属于本 installation", id: 2, wantName: "pub", wantInstallationQuery: installationPath("pub")},
		{name: "其他 installation 的私有仓库读不到", id: 3, notInstalled: true},
		{name: "其他 installation 的公开仓库", id: 4, notInstalled: true, wantInstallationQuery: installationPath("other-pub")},
		{name: "没有安装 App 的公开仓库", id: 5, notInstalled: true, wantInstallationQuery: installationPath("no-app")},
		{name: "仓库不存在或已删除", id: 99, notInstalled: true},
		{
			name: "返回的仓库 ID 不一致", id: 1, notInstalled: true,
			override: "GET /repositories/1", response: respond(http.StatusOK, `{"id":2,"name":"pub","full_name":"tester/pub","private":true}`),
		},
		{
			name: "读取仓库时 GitHub 出错", id: 1, wantErr: true,
			override: "GET /repositories/1", response: respond(http.StatusBadGateway, `{"message":"Server Error"}`),
		},
		{
			name: "查询 installation 时 GitHub 出错", id: 2, wantErr: true,
			override: installationPath("pub"), response: respond(http.StatusInternalServerError, `{"message":"Server Error"}`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if c.override != "" {
				e.overrides[c.override] = c.response
			}
			r, err := Verify(t.Context(), e.client, e.inst, c.id)
			switch {
			case c.notInstalled:
				if !NotInstalled(err) || r != nil {
					t.Fatalf("应返回 NotInstalled 的错误：%v, %v", r, err)
				}
			case c.wantErr:
				if err == nil || NotInstalled(err) {
					t.Fatalf("GitHub 出错时应返回错误，且不能判为不在授权范围内：%v", err)
				}
			case err != nil:
				t.Fatal(err)
			case r.ID != c.id || r.Name != c.wantName:
				t.Fatalf("Verify = %+v", r)
			}
			queried := slices.ContainsFunc(e.apiCalls(), func(s string) bool { return strings.HasSuffix(s, "/installation") })
			if want := c.wantInstallationQuery != ""; queried != want {
				t.Errorf("是否定向查询 installation：%v，应为 %v（请求 %v）", queried, want, e.apiCalls())
			}
			if c.wantInstallationQuery != "" && !slices.Contains(e.apiCalls(), c.wantInstallationQuery) {
				t.Errorf("应请求 %s，实际 %v", c.wantInstallationQuery, e.apiCalls())
			}
			// 不列出 installation 或仓库
			for _, req := range e.apiCalls() {
				if strings.Contains(req, "/installation/repositories") || strings.HasSuffix(req, "/app/installations") {
					t.Errorf("不应列出 installation 或仓库：%s", req)
				}
			}
		})
	}
}

func TestVerifyName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, full   string
		wantID       int64
		notInstalled bool
	}{
		{name: "私有仓库", full: "tester/priv", wantID: 1},
		{name: "公开仓库（名称不区分大小写）", full: "Tester/PUB", wantID: 2},
		{name: "其他 installation 的私有仓库", full: "tester/other-priv", notInstalled: true},
		{name: "其他 installation 的公开仓库", full: "tester/other-pub", notInstalled: true},
		{name: "没有安装 App 的公开仓库", full: "tester/no-app", notInstalled: true},
		{name: "不存在", full: "tester/none", notInstalled: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			r, err := VerifyName(t.Context(), e.client, e.inst, c.full)
			if c.notInstalled {
				if !NotInstalled(err) {
					t.Fatalf("应返回 NotInstalled 的错误：%v, %v", r, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.ID != c.wantID {
				t.Fatalf("VerifyName = %+v，应为仓库 %d", r, c.wantID)
			}
		})
	}
}

// insertRepo 在本地写入仓库记录；branch、url 为空表示还没有补齐。
func insertRepo(t *testing.T, db *store.DB, m store.RepoMeta) {
	t.Helper()
	if err := store.UpsertRepo(t.Context(), db, m); err != nil {
		t.Fatal(err)
	}
}

func getRepo(t *testing.T, db *store.DB, id int64) *store.Repo {
	t.Helper()
	r, err := store.GetRepo(t.Context(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestComplete(t *testing.T) {
	t.Parallel()

	t.Run("缺少信息时补齐记录和传入的仓库", func(t *testing.T) {
		t.Parallel()
		e, db := newEnv(t), store.New(pgtest.Pool(t))
		// 本地记录来自 installation 事件：名字是旧的，没有默认分支和 clone 地址
		insertRepo(t, db, store.RepoMeta{ID: 1, InstallationID: e.inst, OwnerID: 1, Owner: "old", Name: "old-name", Private: false})
		repo := getRepo(t, db, 1)
		if err := Complete(t.Context(), db, e.client, repo); err != nil {
			t.Fatal(err)
		}
		want := store.Repo{
			ID: 1, InstallationID: e.inst, OwnerID: accountID, Owner: "tester", Name: "priv", Private: true,
			DefaultBranch: "main", CloneURL: "file:///src/priv", Active: true, MaxConcurrency: 8,
		}
		if *repo != want {
			t.Errorf("传入的仓库 = %+v\n应为 %+v", *repo, want)
		}
		if got := getRepo(t, db, 1); *got != want {
			t.Errorf("本地记录 = %+v\n应为 %+v", *got, want)
		}
		if calls := e.apiCalls(); !slices.Equal(calls, []string{"GET /repositories/1"}) {
			t.Errorf("应只请求这一个仓库：%v", calls)
		}
	})

	t.Run("信息完整时不请求 GitHub", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		repo := &store.Repo{ID: 1, InstallationID: e.inst, DefaultBranch: "main", CloneURL: "x"}
		if err := Complete(t.Context(), nil, e.client, repo); err != nil {
			t.Fatal(err)
		}
		if calls := e.apiCalls(); len(calls) != 0 {
			t.Errorf("不应请求 GitHub：%v", calls)
		}
	})

	t.Run("没有 GitHub 客户端时不做任何事", func(t *testing.T) {
		t.Parallel()
		repo := &store.Repo{ID: 1}
		if err := Complete(t.Context(), nil, nil, repo); err != nil || repo.DefaultBranch != "" {
			t.Fatalf("Complete = %v，仓库 %+v", err, repo)
		}
	})

	t.Run("仓库已不在授权范围内", func(t *testing.T) {
		t.Parallel()
		e, db := newEnv(t), store.New(pgtest.Pool(t))
		insertRepo(t, db, store.RepoMeta{ID: 3, InstallationID: e.inst, OwnerID: 1, Owner: "tester", Name: "other-priv", Private: true})
		repo := getRepo(t, db, 3)
		before := *repo
		err := Complete(t.Context(), db, e.client, repo)
		if !gh.IsNotFound(err) || !strings.Contains(err.Error(), "tester/other-priv") {
			t.Fatalf("应返回包含仓库名的 404：%v", err)
		}
		if *repo != before || *getRepo(t, db, 3) != before {
			t.Errorf("出错时不应修改仓库：%+v", repo)
		}
	})
}

func TestFind(t *testing.T) {
	t.Parallel()
	e, db := newEnv(t), store.New(pgtest.Pool(t))
	insertRepo(t, db, store.RepoMeta{ID: 1, InstallationID: e.inst, OwnerID: accountID, Owner: "tester", Name: "priv", Private: true})

	if _, err := Find(t.Context(), db, e.client, "tester/none"); !errors.Is(err, store.ErrRepoNotFound) {
		t.Fatalf("不在 Kiln 中的仓库应返回 ErrRepoNotFound：%v", err)
	}
	if calls := e.apiCalls(); len(calls) != 0 {
		t.Fatalf("Find 不应向 GitHub 查找不在 Kiln 中的仓库：%v", calls)
	}
	repo, err := Find(t.Context(), db, e.client, "TESTER/priv")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != 1 || repo.Incomplete() {
		t.Fatalf("应找到并补齐仓库：%+v", repo)
	}
	// 第二次找到的记录已经完整，不再请求
	if _, err := Find(t.Context(), db, e.client, "tester/priv"); err != nil {
		t.Fatal(err)
	}
	if calls := e.apiCalls(); len(calls) != 1 {
		t.Fatalf("补齐后不应再请求 GitHub：%v", calls)
	}

	// 补齐失败时返回错误
	insertRepo(t, db, store.RepoMeta{ID: 3, InstallationID: e.inst, OwnerID: accountID, Owner: "tester", Name: "other-priv", Private: true})
	if _, err := Find(t.Context(), db, e.client, "tester/other-priv"); !gh.IsNotFound(err) {
		t.Fatalf("补齐时 GitHub 返回 404 应返回错误：%v", err)
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	complete := func(id, inst int64, name string) store.RepoMeta {
		return store.RepoMeta{
			ID: id, InstallationID: inst, OwnerID: accountID, Owner: "tester", Name: name, Private: true,
			DefaultBranch: "main", CloneURL: "https://example.com/" + name,
		}
	}
	cases := []struct {
		name string
		// local 是本地已有的仓库记录
		local    []store.RepoMeta
		full     string
		override string
		response http.HandlerFunc
		// wantID 为 0 表示应返回 nil；wantInst 是返回的仓库所属的 installation
		wantID   int64
		wantInst int64
		wantErr  bool
		// wantCalls 是模拟 GitHub 收到的请求（不含被 override 拦下的；nil 表示没有）
		wantCalls []string
		// wantStored 表示仓库是否应写入本地
		wantStored bool
	}{
		{
			name: "Kiln 中已有、信息完整：不请求 GitHub", local: []store.RepoMeta{complete(1, 777, "priv")},
			full: "tester/priv", wantID: 1, wantInst: 777, wantStored: true,
		},
		{
			name: "Kiln 中已有、属于其他 installation：照样返回，由调用方判断", local: []store.RepoMeta{complete(3, otherInst, "other-priv")},
			full: "tester/other-priv", wantID: 3, wantInst: otherInst, wantStored: true,
		},
		{
			name: "不在 Kiln 中的私有仓库：定向读取后记录", full: "tester/priv", wantID: 1, wantInst: 777, wantStored: true,
			wantCalls: []string{"GET /repos/tester/priv"},
		},
		{
			name: "不在 Kiln 中的公开仓库：确认属于本 installation 后记录", full: "tester/pub", wantID: 2, wantInst: 777, wantStored: true,
			wantCalls: []string{"GET /repos/tester/pub", "GET /repos/tester/pub/installation"},
		},
		{
			name: "不在 Kiln 中、属于其他 installation 的私有仓库", full: "tester/other-priv",
			wantCalls: []string{"GET /repos/tester/other-priv"},
		},
		{
			name: "不在 Kiln 中、属于其他 installation 的公开仓库", full: "tester/other-pub",
			wantCalls: []string{"GET /repos/tester/other-pub", "GET /repos/tester/other-pub/installation"},
		},
		{
			name: "不在 Kiln 中、没有安装 App 的公开仓库", full: "tester/no-app",
			wantCalls: []string{"GET /repos/tester/no-app", "GET /repos/tester/no-app/installation"},
		},
		{name: "GitHub 上也不存在", full: "tester/none", wantCalls: []string{"GET /repos/tester/none"}},
		{
			name: "Kiln 中的记录待补齐，但仓库已不在授权范围内", local: []store.RepoMeta{{ID: 3, InstallationID: 777, OwnerID: 1, Owner: "tester", Name: "other-priv"}},
			full: "tester/other-priv", wantCalls: []string{"GET /repositories/3"}, wantStored: true,
		},
		{
			name: "GitHub 出错", full: "tester/priv", wantErr: true,
			override: "GET /repos/tester/priv", response: respond(http.StatusInternalServerError, `{"message":"boom"}`),
		},
		// 不是 owner/name 形式的名字：不查数据库、不请求 GitHub，直接返回 nil（回归：缺少 / 时曾返回 FindRepo 的格式错误）
		{name: "缺少 /", full: "priv"},
		{name: "缺少仓库名", full: "tester/"},
		{name: "缺少 owner", full: "/priv"},
		{name: "多级路径", full: "tester/priv/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, db := newEnv(t), store.New(pgtest.Pool(t))
			for _, m := range c.local {
				insertRepo(t, db, m)
			}
			if c.override != "" {
				e.overrides[c.override] = c.response
			}
			r, err := Lookup(t.Context(), db, e.client, e.inst, c.full)
			switch {
			case c.wantErr:
				if err == nil {
					t.Fatalf("应返回错误，实际 %+v", r)
				}
			case err != nil:
				t.Fatal(err)
			case c.wantID == 0 && r != nil:
				t.Fatalf("应返回 nil，实际 %+v", r)
			case c.wantID != 0 && (r == nil || r.ID != c.wantID || r.InstallationID != c.wantInst || r.Incomplete() || !r.Active):
				t.Fatalf("Lookup = %+v，应为仓库 %d（installation %d，信息完整、启用）", r, c.wantID, c.wantInst)
			}
			if calls := e.apiCalls(); !slices.Equal(calls, c.wantCalls) {
				t.Errorf("GitHub 请求 = %v，应为 %v", calls, c.wantCalls)
			}
			states, err := store.RepoStates(t.Context(), db, []int64{1, 2, 3, 4, 5})
			if err != nil {
				t.Fatal(err)
			}
			if stored := len(states) > 0; stored != c.wantStored {
				t.Errorf("本地是否有仓库记录：%v，应为 %v（%v）", stored, c.wantStored, states)
			}
		})
	}

	t.Run("没有 GitHub 客户端", func(t *testing.T) {
		t.Parallel()
		db := store.New(pgtest.Pool(t))
		insertRepo(t, db, complete(1, 777, "priv"))
		if r, err := Lookup(t.Context(), db, nil, 777, "tester/priv"); err != nil || r == nil || r.ID != 1 {
			t.Fatalf("Kiln 中已有的仓库：%+v, %v", r, err)
		}
		if _, err := Lookup(t.Context(), db, nil, 777, "tester/none"); !errors.Is(err, store.ErrRepoNotFound) {
			t.Fatalf("不能定向确认时应返回 ErrRepoNotFound：%v", err)
		}
		if r, err := Lookup(t.Context(), db, nil, 777, "none"); err != nil || r != nil {
			t.Fatalf("格式不对的名字应返回 nil：%+v, %v", r, err)
		}
	})
}
