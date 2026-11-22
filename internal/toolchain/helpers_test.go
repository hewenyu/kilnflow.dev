package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// fakeUpstream 是假的上游（go.dev、nodejs.org、registry.npmjs.org、github.com）。
//
// 测试期间把包级的 httpClient 换成改写地址的客户端：请求仍然带着原来的 Host（例如 go.dev），
// 但都发到本地的 httptest 服务，由它按 "Host + 路径" 返回预先登记的内容。
// 因为替换的是包级变量，使用 fakeUpstream 的测试不能调用 t.Parallel()。
type fakeUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string][]byte // "host/path" → 响应内容
	hits   map[string]int    // "host/path" → 请求次数
	// gate 非 nil 时，对 gated 路径的请求先关闭 arrived（只关一次），再等待 gate 关闭：
	// 测试可以等第一个下载请求到达后再放行，让其他并发的安装在这段时间里排队。
	gate        chan struct{}
	gated       string
	arrived     chan struct{}
	arrivedOnce sync.Once
}

// holdRequests 让对 rawURL 的请求在到达后阻塞，返回"第一个请求已到达"和"放行"两个操作。
func (f *fakeUpstream) holdRequests(t *testing.T, rawURL string) (arrived <-chan struct{}, release func()) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	f.gated = u.Host + u.Path
	f.arrived = make(chan struct{})
	gate := f.gate
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return f.arrived, release
}

// newFakeUpstream 启动假的上游并替换 httpClient，测试结束时恢复，并清空发行索引的缓存。
func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{routes: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	target, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	old := httpClient
	httpClient = &http.Client{Transport: rewriteTransport{base: f.srv.Client().Transport, target: target}}
	resetIndexes()
	t.Cleanup(func() {
		httpClient = old
		resetIndexes()
		f.srv.Close()
	})
	return f
}

// resetIndexes 清空进程内缓存的发行索引，避免测试之间互相影响。
func resetIndexes() {
	for _, c := range []interface{ reset() }{goIndex, nodeIndex} {
		c.reset()
	}
}

func (c *indexCache[T]) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = nil
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Host + r.URL.Path
	f.mu.Lock()
	f.hits[key]++
	body, ok := f.routes[key]
	gate, arrived := f.gate, f.arrived
	gated := key == f.gated
	f.mu.Unlock()
	if gated && gate != nil {
		f.arrivedOnce.Do(func() { close(arrived) })
		<-gate
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

// set 登记 rawURL 的响应内容。
func (f *fakeUpstream) set(t *testing.T, rawURL string, body []byte) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[u.Host+u.Path] = body
}

// setJSON 登记 rawURL 的 JSON 响应。
func (f *fakeUpstream) setJSON(t *testing.T, rawURL string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	f.set(t, rawURL, b)
}

// hitCount 返回 rawURL 被请求的次数（不计查询参数）。
func (f *fakeUpstream) hitCount(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[u.Host+u.Path]
}

// rewriteTransport 把所有请求发到 target，保留原来的 Host 头。
type rewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	return rt.base.RoundTrip(r)
}

// ---------- 归档 ----------

// tarGzBytes 返回由 entries 组成的 tar.gz 内容。
func tarGzBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	b, err := os.ReadFile(writeTarGz(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// zipEntry 描述测试 zip 中的一个条目。
type zipEntry struct {
	name string
	mode fs.FileMode
	body string
}

// zipBytes 返回由 entries 组成的 zip 内容。
func zipBytes(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTemp 把内容写入临时文件并返回路径。
func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sha512Integrity(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

// ---------- 假的发行数据 ----------

// fakeRelease 是登记到假上游的一个发行包：URL、内容和 lockfile 中应有的 digest。
type fakeRelease struct {
	url    string
	body   []byte
	digest string
}

func newRelease(u string, body []byte) fakeRelease {
	return fakeRelease{url: u, body: body, digest: cas.Digest(body)}
}

// fakeReleases 是 Go、Node、pnpm、Bun 各一个版本的假发行包。
type fakeReleases struct {
	goLinux, goDarwin fakeRelease // go 1.24.4
	nodeLinux         fakeRelease // node 22.11.0
	pnpm              fakeRelease // pnpm 9.12.0（与平台无关的 JavaScript 包）
	pnpmWrapper       fakeRelease // pnpm 12.14.0 的包装（pnpm 包，里面没有程序）
	pnpmLinuxArm      fakeRelease // pnpm 12.14.0 的 linux/arm64 原生包（@pnpm/exe.linux-arm64）
	bunLinux          fakeRelease // bun 1.1.30
}

// goTarball 是 Go 发行包的内容：顶层目录 go/，bin/go 可执行。
func goTarball(t *testing.T, version string) []byte {
	t.Helper()
	return tarGzBytes(t, []tarEntry{
		{name: "go/", typ: tar.TypeDir, mode: 0o755},
		{name: "go/bin/go", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho go" + version + "\n"},
		{name: "go/VERSION", typ: tar.TypeReg, body: "go" + version},
	})
}

// registerAll 在假上游中登记 Go、Node、pnpm、Bun 的索引和发行包。
func registerAll(t *testing.T, f *fakeUpstream) fakeReleases {
	t.Helper()
	var r fakeReleases

	// Go：索引自带 sha256；1.24 的最新补丁版是 1.24.4，另有一个预发布版本
	r.goLinux = newRelease("https://go.dev/dl/go1.24.4.linux-amd64.tar.gz", goTarball(t, "1.24.4"))
	r.goDarwin = newRelease("https://go.dev/dl/go1.24.4.darwin-arm64.tar.gz", goTarball(t, "1.24.4-darwin"))
	type goFile struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Kind     string `json:"kind"`
	}
	type goRel struct {
		Version string   `json:"version"`
		Stable  bool     `json:"stable"`
		Files   []goFile `json:"files"`
	}
	f.setJSON(t, "https://go.dev/dl/", []goRel{
		{Version: "go1.25rc1", Files: []goFile{{Filename: "go1.25rc1.linux-amd64.tar.gz", OS: "linux", Arch: "amd64", SHA256: "00", Kind: "archive"}}},
		{Version: "go1.24.4", Stable: true, Files: []goFile{
			{Filename: "go1.24.4.src.tar.gz", Kind: "source", SHA256: "11"},
			{Filename: "go1.24.4.darwin-arm64.pkg", OS: "darwin", Arch: "arm64", Kind: "installer", SHA256: "22"},
			{Filename: "go1.24.4.darwin-arm64.tar.gz", OS: "darwin", Arch: "arm64", Kind: "archive", SHA256: sha256Hex(r.goDarwin.body)},
			{Filename: "go1.24.4.linux-amd64.tar.gz", OS: "linux", Arch: "amd64", Kind: "archive", SHA256: sha256Hex(r.goLinux.body)},
		}},
		{Version: "go1.24.3", Stable: true},
		{Version: "go1.23.10", Stable: true},
	})
	for _, rel := range []fakeRelease{r.goLinux, r.goDarwin} {
		f.set(t, rel.url, rel.body)
	}

	// Node：index.json + 每个版本的 SHASUMS256.txt
	r.nodeLinux = newRelease("https://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-x64.tar.gz", tarGzBytes(t, []tarEntry{
		{name: "node-v22.11.0-linux-x64/bin/node", typ: tar.TypeReg, mode: 0o755, body: "node"},
		{name: "node-v22.11.0-linux-x64/bin/npm", typ: tar.TypeSymlink, link: "../lib/node_modules/npm/bin/npm-cli.js"},
		{name: "node-v22.11.0-linux-x64/lib/node_modules/npm/bin/npm-cli.js", typ: tar.TypeReg, body: "npm"},
	}))
	f.setJSON(t, "https://nodejs.org/dist/index.json", []map[string]any{
		{"version": "v23.1.0", "lts": false},
		{"version": "v22.11.0", "lts": "Jod"},
		{"version": "v22.10.0", "lts": false},
		{"version": "v20.18.0", "lts": "Iron"},
	})
	f.set(t, "https://nodejs.org/dist/v22.11.0/SHASUMS256.txt", fmt.Appendf(nil,
		"%s  node-v22.11.0-darwin-arm64.tar.gz\n%s  node-v22.11.0-linux-x64.tar.gz\n", sha256Hex([]byte("other")), sha256Hex(r.nodeLinux.body)))
	f.set(t, r.nodeLinux.url, r.nodeLinux.body)

	// pnpm：npm registry 的包元数据和版本元数据（只有 sha512 integrity）
	r.pnpm = newRelease("https://registry.npmjs.org/pnpm/-/pnpm-9.12.0.tgz", tarGzBytes(t, []tarEntry{
		{name: "package/package.json", typ: tar.TypeReg, body: `{"name":"pnpm"}`},
		{name: "package/bin/pnpm.cjs", typ: tar.TypeReg, body: "pnpm"},
		{name: "package/bin/pnpx.cjs", typ: tar.TypeReg, body: "pnpx"},
	}))
	f.setJSON(t, "https://registry.npmjs.org/pnpm", map[string]any{
		"versions": map[string]any{"8.15.9": map[string]any{}, "9.11.0": map[string]any{}, "9.12.0": map[string]any{}, "10.0.0-rc.1": map[string]any{}},
	})
	f.setJSON(t, "https://registry.npmjs.org/pnpm/9.12.0", map[string]any{
		"dist": map[string]any{"tarball": r.pnpm.url, "integrity": sha512Integrity(r.pnpm.body)},
	})
	f.set(t, r.pnpm.url, r.pnpm.body)
	// pnpm 12.14.0：按平台发行原生可执行文件，pnpm 包只是包装
	const wrapperManifest = `{"name":"pnpm","optionalDependencies":{"@pnpm/exe.linux-arm64":"12.14.0","@pnpm/exe.darwin-arm64":"12.14.0"}}`
	r.pnpmWrapper = newRelease("https://registry.npmjs.org/pnpm/-/pnpm-12.14.0.tgz", tarGzBytes(t, []tarEntry{
		{name: "package/package.json", typ: tar.TypeReg, body: wrapperManifest},
		{name: "package/bin/pnpm.mjs", typ: tar.TypeReg, body: "download the native binary"},
		{name: "package/pnpm", typ: tar.TypeReg, mode: 0o755, body: "placeholder"},
	}))
	f.setJSON(t, "https://registry.npmjs.org/pnpm/12.14.0", map[string]any{
		"dist":                 map[string]any{"tarball": r.pnpmWrapper.url, "integrity": sha512Integrity(r.pnpmWrapper.body)},
		"optionalDependencies": map[string]any{"@pnpm/exe.linux-arm64": "12.14.0", "@pnpm/exe.darwin-arm64": "12.14.0"},
	})
	f.set(t, r.pnpmWrapper.url, r.pnpmWrapper.body)
	r.pnpmLinuxArm = newRelease("https://registry.npmjs.org/@pnpm/exe.linux-arm64/-/exe.linux-arm64-12.14.0.tgz", tarGzBytes(t, []tarEntry{
		{name: "package/package.json", typ: tar.TypeReg, body: `{"name":"@pnpm/exe.linux-arm64"}`},
		{name: "package/pnpm", typ: tar.TypeReg, mode: 0o755, body: "native pnpm"},
	}))
	f.setJSON(t, "https://registry.npmjs.org/@pnpm/exe.linux-arm64/12.14.0", map[string]any{
		"dist": map[string]any{"tarball": r.pnpmLinuxArm.url, "integrity": sha512Integrity(r.pnpmLinuxArm.body)},
	})
	f.set(t, r.pnpmLinuxArm.url, r.pnpmLinuxArm.body)

	// Bun：GitHub Releases 的 SHASUMS256.txt 和 zip
	base := "https://github.com/oven-sh/bun/releases/download/bun-v1.1.30/"
	r.bunLinux = newRelease(base+"bun-linux-x64.zip", zipBytes(t, []zipEntry{
		{name: "bun-linux-x64/", mode: fs.ModeDir | 0o755},
		{name: "bun-linux-x64/bun", mode: 0o755, body: "bun"},
	}))
	f.set(t, base+"SHASUMS256.txt", fmt.Appendf(nil, "%s  bun-linux-x64.zip\n%s  bun-darwin-aarch64.zip\n", sha256Hex(r.bunLinux.body), sha256Hex([]byte("x"))))
	f.set(t, r.bunLinux.url, r.bunLinux.body)
	f.setJSON(t, "https://registry.npmjs.org/bun", map[string]any{
		"versions": map[string]any{"1.0.0": map[string]any{}, "1.1.29": map[string]any{}, "1.1.30": map[string]any{}},
	})
	return r
}

// writeRepo 在临时目录中写入项目文件（相对路径 → 内容，/ 分隔），返回目录。
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// discardf 是不输出的 logf。
func discardf(string, ...any) {}

// installDir 调用 Install，返回目录和元数据（多数测试不关心是否复用）。
func installDir(ctx context.Context, root string, ref Ref) (string, *Meta, error) {
	in, err := Install(ctx, root, ref)
	if err != nil {
		return "", nil, err
	}
	return in.Dir, in.Meta, nil
}
