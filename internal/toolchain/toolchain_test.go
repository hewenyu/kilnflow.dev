package toolchain

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

func TestResolveGoVersion(t *testing.T) {
	t.Parallel()
	rels := []goRelease{
		{Version: "go1.25rc1"},
		{Version: "go1.24.4", Stable: true},
		{Version: "go1.24.3", Stable: true},
		{Version: "go1.23.10", Stable: true},
	}
	cases := []struct {
		spec, want string
		wantErr    bool
	}{
		{spec: "1.24", want: "1.24.4"},
		{spec: "go1.24.3", want: "1.24.3"},
		{spec: "1.25rc1", want: "1.25rc1"},
		{spec: "1.25rc2", wantErr: true},
		{spec: "1.22", wantErr: true},
		{spec: "1.24.9", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			t.Parallel()
			got, err := resolveGoVersion(rels, c.spec)
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("resolveGoVersion(%q) = %q, %v", c.spec, got, err)
			}
		})
	}
}

func TestLockRoundTrip(t *testing.T) {
	t.Parallel()
	l := &Lock{Entries: map[string]*LockEntry{
		"go":   {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/arm64": "sha256:aa"}},
		"pnpm": {Name: "pnpm", Version: "9.12.0", Platforms: map[string]string{anyPlatform: "sha256:bb"}},
	}}
	l2, err := ParseLock(l.Format())
	if err != nil {
		t.Fatal(err)
	}
	if _, p, d, err := l2.Resolve("pnpm", "darwin/arm64"); err != nil || p != anyPlatform || d != "sha256:bb" {
		t.Fatal(p, d, err)
	}
	if _, p, d, err := l2.Resolve("go", "linux/arm64/gnu"); err != nil || p != "linux/arm64" || d != "sha256:aa" {
		t.Fatal(p, d, err)
	}
	if _, _, _, err := l2.Resolve("go", "darwin/arm64"); err == nil {
		t.Fatal("期望缺少平台的错误")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.24\n\ntoolchain go1.24.4\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{"packageManager":"pnpm@9.12.0+sha512.abc","engines":{"node":">=20"}}`)
	writeFile(t, filepath.Join(dir, ".nvmrc"), "v22.11.0\n")
	d := detectDir(dir, ".", "")
	if d["go"].Spec != "1.24.4" || d["pnpm"].Spec != "9.12.0" || d["node"].Spec != "22.11.0" {
		t.Fatal(d)
	}
}

func TestDetectDirDevEngines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.24\n")
	writeFile(t, filepath.Join(dir, "package.json"),
		`{"packageManager":"bun@1.1.0","devEngines":{"runtime":[{"name":"node","version":"^22"},{"name":"bun","version":"1.2.0"}]},"engines":{"node":">=20"}}`)
	writeFile(t, filepath.Join(dir, ".nvmrc"), "20\n")
	d := detectDir(dir, ".", "")
	if d["go"].Spec != "1.24" || d["node"].Spec != "^22" || d["bun"].Spec != "1.1.0" {
		t.Fatal(d)
	}
}

func TestHashURL(t *testing.T) {
	t.Parallel()
	body := []byte("pnpm tarball")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	sum := sha512.Sum512(body)
	good := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	ctx := context.Background()

	d, err := hashURL(ctx, srv.URL, good)
	if err != nil || d != cas.Digest(body) {
		t.Fatalf("hashURL = %s, %v", d, err)
	}
	for _, integrity := range []string{"", "sha1-abc", "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))} {
		if _, err := hashURL(ctx, srv.URL, integrity); err == nil {
			t.Errorf("integrity %q：期望校验失败", integrity)
		}
	}
}

// 常驻的 Runner 启动后上游才发布的版本：缓存中找不到时刷新一次索引。
func TestIndexCacheRefresh(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		requests int
		rels     = []goRelease{{Version: "go1.24.4", Stable: true}}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		_ = json.NewEncoder(w).Encode(rels)
	}))
	t.Cleanup(srv.Close)
	idx := &indexCache[goRelease]{url: srv.URL}
	ctx := context.Background()
	resolve := func(spec string) (string, error) {
		return findInIndex(ctx, idx, func(rels []goRelease) (string, error) { return resolveGoVersion(rels, spec) })
	}
	requestCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}

	if v, err := resolve("1.24"); err != nil || v != "1.24.4" {
		t.Fatalf("resolve(1.24) = %q, %v", v, err)
	}
	if v, err := resolve("1.24.4"); err != nil || v != "1.24.4" || requestCount() != 1 {
		t.Fatalf("resolve(1.24.4) = %q, %v；请求 %d 次，期望命中缓存", v, err, requestCount())
	}

	mu.Lock()
	rels = append([]goRelease{{Version: "go1.24.5", Stable: true}}, rels...)
	mu.Unlock()
	if v, err := resolve("1.24.5"); err != nil || v != "1.24.5" || requestCount() != 2 {
		t.Fatalf("resolve(1.24.5) = %q, %v；请求 %d 次，期望刷新一次", v, err, requestCount())
	}
	// 刷新后的索引留在缓存中
	if v, err := resolve("1.24"); err != nil || v != "1.24.5" || requestCount() != 2 {
		t.Fatalf("resolve(1.24) = %q, %v；请求 %d 次", v, err, requestCount())
	}
	// 刷新后仍然找不到：返回错误，只刷新一次
	if _, err := resolve("1.99.0"); err == nil || requestCount() != 3 {
		t.Fatalf("resolve(1.99.0) 期望失败且只刷新一次：%v；请求 %d 次", err, requestCount())
	}
	// 不是"找不到"的错误不刷新
	if _, err := resolve("x.y"); err == nil || requestCount() != 3 {
		t.Fatalf("resolve(x.y) 期望失败且不刷新：%v；请求 %d 次", err, requestCount())
	}
}
