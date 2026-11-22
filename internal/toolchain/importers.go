package toolchain

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/semver"
)

// artifact 是某个版本、某个平台的上游发行包。
type artifact struct {
	URL    string
	SHA256 string // hex；为空表示上游没有直接给出 sha256，需要下载后计算
	// Integrity 是 npm 的 dist.integrity（sha512-…），用于校验。
	Integrity string
	Format    string // formatTarGz | formatTgz | formatZip
	// Strip 是解包时去掉的顶层目录层数。
	Strip int
}

// EnvSpec 是 runtime 对环境变量的设置（PLAN #12.2）。
type EnvSpec struct {
	PrependPaths []string `json:"prepend_paths,omitempty"`
	Path         string   `json:"path,omitempty"`
	Value        string   `json:"value,omitempty"`
	IsValue      bool     `json:"is_value,omitempty"`
}

// importer 是一种内置 runtime 的来源。
type importer interface {
	// PlatformIndependent 为 true 表示这个版本的发行包与平台无关（lockfile 中只有一个 any 条目）。
	PlatformIndependent(ctx context.Context, version string) (bool, error)
	// ResolveVersion 把项目文件中的版本声明（可能是范围或只有次版本号）解析为精确版本。
	ResolveVersion(ctx context.Context, spec string) (string, error)
	// Artifact 返回发行包；platform 是 lockfile 中的平台（平台无关的条目是 anyPlatform）。
	Artifact(ctx context.Context, version, platform string) (artifact, error)
	// Env 返回解包后的环境变量设置。
	Env() map[string]EnvSpec
	// PostExtract 在解包后做额外处理（例如生成启动脚本）。
	PostExtract(dir string) error
}

// importers 是内置 importer 的列表。
var importers = map[string]importer{
	"go":   goImporter{},
	"node": nodeImporter{},
	"pnpm": pnpmImporter{},
	"bun":  bunImporter{},
}

const (
	// httpTimeout 是访问上游索引和下载发行包的总超时。
	httpTimeout = 10 * time.Minute
	// maxTextSize 是 getText 读取的文本大小上限（SHASUMS256.txt 等）。
	maxTextSize = 16 << 20
	userAgent   = "kiln"
)

var httpClient = &http.Client{Timeout: httpTimeout}

// httpGet 发起 GET 请求，状态码不是 200 时返回错误。成功时调用方负责关闭 Body。
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() // 只需要状态码，关闭失败无影响
		return nil, fmt.Errorf("GET %s：%s", url, resp.Status)
	}
	return resp, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func getText(ctx context.Context, url string) (string, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTextSize))
	return string(b), err
}

// shasum 从 SHASUMS256.txt 中找出某个文件的 sha256。
func shasum(text, file string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			return f[0]
		}
	}
	return ""
}

func splitPlatform(p string) (goos, goarch string, err error) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("无效的平台 %q", p)
	}
	return parts[0], parts[1], nil
}

// ---------- 发行索引 ----------

// indexCache 在进程内缓存上游的发行索引（JSON 数组）。常驻的 Runner 运行期间上游会发布新版本，
// 所以 findInIndex 在缓存中找不到时会强制刷新一次。
type indexCache[T any] struct {
	url string

	mu   sync.Mutex
	data []T
}

// get 返回索引；没有缓存或 refresh 为 true 时从上游获取。
func (c *indexCache[T]) get(ctx context.Context, refresh bool) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && !refresh {
		return c.data, nil
	}
	var rels []T
	if err := getJSON(ctx, c.url, &rels); err != nil {
		return nil, err
	}
	c.data = rels
	return rels, nil
}

// notInIndexError 表示要找的版本不在索引中（索引可能已经过时）。
type notInIndexError struct{ msg string }

func (e *notInIndexError) Error() string { return e.msg }

func notInIndexf(format string, args ...any) error {
	return &notInIndexError{msg: fmt.Sprintf(format, args...)}
}

// findInIndex 用 find 在缓存的索引中查找；find 返回 *notInIndexError 时刷新索引再找一次。
func findInIndex[T, R any](ctx context.Context, c *indexCache[T], find func([]T) (R, error)) (R, error) {
	var zero R
	rels, err := c.get(ctx, false)
	if err != nil {
		return zero, err
	}
	v, err := find(rels)
	if _, ok := errors.AsType[*notInIndexError](err); !ok {
		return v, err
	}
	if rels, err = c.get(ctx, true); err != nil {
		return zero, err
	}
	return find(rels)
}

// ---------- Go ----------

type goImporter struct{}

type goRelease struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Files   []struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Kind     string `json:"kind"`
	} `json:"files"`
}

// goIndex 是 Go 的发行索引。
var goIndex = &indexCache[goRelease]{url: "https://go.dev/dl/?mode=json&include=all"}

func (goImporter) PlatformIndependent(context.Context, string) (bool, error) { return false, nil }

func (goImporter) ResolveVersion(ctx context.Context, spec string) (string, error) {
	return findInIndex(ctx, goIndex, func(rels []goRelease) (string, error) {
		return resolveGoVersion(rels, spec)
	})
}

// resolveGoVersion 在 Go 的发行列表中解析版本声明：完整版本（1.24.4）和预发布版本（1.25rc1、1.25beta1）
// 必须存在；只有次版本号（1.24）时取最新的正式补丁版。
func resolveGoVersion(rels []goRelease, spec string) (string, error) {
	spec = strings.TrimPrefix(strings.TrimSpace(spec), "go")
	if strings.Count(spec, ".") >= 2 || strings.Contains(spec, "rc") || strings.Contains(spec, "beta") {
		for _, r := range rels {
			if strings.TrimPrefix(r.Version, "go") == spec {
				return spec, nil
			}
		}
		return "", notInIndexf("找不到 Go 版本 %s", spec)
	}
	var versions []string
	for _, r := range rels {
		if r.Stable {
			versions = append(versions, strings.TrimPrefix(r.Version, "go"))
		}
	}
	base, ok := semver.Parse(spec)
	if !ok {
		return "", fmt.Errorf("无法解析 Go 版本 %q", spec)
	}
	v, found := semver.Latest(versions, func(v semver.Version) bool { return v.Major == base.Major && v.Minor == base.Minor && v.Pre == "" })
	if !found {
		return "", notInIndexf("找不到 Go %s 的正式版本", spec)
	}
	return v, nil
}

func (goImporter) Artifact(ctx context.Context, version, platform string) (artifact, error) {
	goos, goarch, err := splitPlatform(platform)
	if err != nil {
		return artifact{}, err
	}
	return findInIndex(ctx, goIndex, func(rels []goRelease) (artifact, error) {
		listed := false
		for _, r := range rels {
			if strings.TrimPrefix(r.Version, "go") != version {
				continue
			}
			listed = true
			for _, f := range r.Files {
				if f.OS == goos && f.Arch == goarch && f.Kind == "archive" && strings.HasSuffix(f.Filename, "."+formatTarGz) {
					return artifact{URL: "https://go.dev/dl/" + f.Filename, SHA256: f.SHA256, Format: formatTarGz, Strip: 1}, nil
				}
			}
		}
		if !listed {
			return artifact{}, notInIndexf("没有 %s 平台的 Go %s 发行包", platform, version)
		}
		return artifact{}, fmt.Errorf("没有 %s 平台的 Go %s 发行包", platform, version)
	})
}

func (goImporter) Env() map[string]EnvSpec {
	return map[string]EnvSpec{
		"PATH":        {PrependPaths: []string{"bin"}},
		"GOROOT":      {Path: "."},
		"GOTOOLCHAIN": {Value: "local", IsValue: true},
	}
}

func (goImporter) PostExtract(string) error { return nil }

// ---------- Node ----------

type nodeImporter struct{}

type nodeRelease struct {
	Version string `json:"version"`
	LTS     any    `json:"lts"`
}

// nodeIndex 是 Node 的发行索引。
var nodeIndex = &indexCache[nodeRelease]{url: "https://nodejs.org/dist/index.json"}

func (nodeImporter) PlatformIndependent(context.Context, string) (bool, error) { return false, nil }

func (nodeImporter) ResolveVersion(ctx context.Context, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	return findInIndex(ctx, nodeIndex, func(rels []nodeRelease) (string, error) {
		return resolveNodeVersion(rels, spec)
	})
}

// resolveNodeVersion 在 Node 的发行列表中解析版本声明：范围、nvm 的 LTS 别名（见 resolveNodeLTS），
// 以及表示最新正式版本的 nvm 别名 node、stable、current。
func resolveNodeVersion(rels []nodeRelease, spec string) (string, error) {
	if alias, ok := strings.CutPrefix(spec, "lts"); ok && (alias == "" || alias[0] == '/') {
		return resolveNodeLTS(rels, strings.TrimPrefix(alias, "/"))
	}
	switch spec {
	case "node", "stable", "current":
		spec = "*"
	}
	var versions []string
	for _, r := range rels {
		versions = append(versions, strings.TrimPrefix(r.Version, "v"))
	}
	v, ok := semver.Latest(versions, func(v semver.Version) bool { return semver.MatchRange(spec, v) })
	if !ok {
		return "", notInIndexf("找不到满足 %q 的 Node 版本", spec)
	}
	return v, nil
}

// resolveNodeLTS 解析 nvm 的 LTS 别名（lts/ 之后的部分）：空或 *（最新的 LTS 版本），
// 代号（例如 iron，不区分大小写，该 LTS 系列的最新版本），-N（往前数第 N 个 LTS 系列，例如 -1 是上一个系列）。
func resolveNodeLTS(rels []nodeRelease, alias string) (string, error) {
	// 每个 LTS 系列（按代号区分）的最新版本
	latest := map[string]semver.Version{}
	for _, r := range rels {
		name, ok := r.LTS.(string)
		if !ok || name == "" {
			continue
		}
		v, ok := semver.Parse(strings.TrimPrefix(r.Version, "v"))
		if !ok {
			continue
		}
		key := strings.ToLower(name)
		if cur, ok := latest[key]; !ok || v.Compare(cur) > 0 {
			latest[key] = v
		}
	}
	if len(latest) == 0 {
		return "", notInIndexf("找不到 Node LTS 版本")
	}
	// 从新到旧
	lines := slices.SortedFunc(maps.Values(latest), func(a, b semver.Version) int { return b.Compare(a) })
	switch {
	case alias == "" || alias == "*":
		return lines[0].Raw, nil
	case strings.HasPrefix(alias, "-"):
		n, err := strconv.Atoi(alias[1:])
		if err != nil || n < 0 {
			return "", fmt.Errorf("无效的 Node LTS 别名 lts/%s", alias)
		}
		if n >= len(lines) {
			return "", notInIndexf("找不到 Node lts/%s（只有 %d 个 LTS 系列）", alias, len(lines))
		}
		return lines[n].Raw, nil
	}
	v, ok := latest[strings.ToLower(alias)]
	if !ok {
		return "", notInIndexf("找不到代号为 %s 的 Node LTS 版本", alias)
	}
	return v.Raw, nil
}

func nodePlatform(platform string) (string, error) {
	goos, goarch, err := splitPlatform(platform)
	if err != nil {
		return "", err
	}
	var arch string
	switch goarch {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "x64"
	}
	if arch == "" || (goos != "darwin" && goos != "linux") {
		return "", fmt.Errorf("没有 %s 平台的 Node 发行包", platform)
	}
	return goos + "-" + arch, nil
}

func (nodeImporter) Artifact(ctx context.Context, version, platform string) (artifact, error) {
	np, err := nodePlatform(platform)
	if err != nil {
		return artifact{}, err
	}
	file := fmt.Sprintf("node-v%s-%s.%s", version, np, formatTarGz)
	sums, err := getText(ctx, fmt.Sprintf("https://nodejs.org/dist/v%s/SHASUMS256.txt", version))
	if err != nil {
		return artifact{}, err
	}
	sum := shasum(sums, file)
	if sum == "" {
		return artifact{}, fmt.Errorf("SHASUMS256.txt 中没有 %s", file)
	}
	return artifact{URL: fmt.Sprintf("https://nodejs.org/dist/v%s/%s", version, file), SHA256: sum, Format: formatTarGz, Strip: 1}, nil
}

func (nodeImporter) Env() map[string]EnvSpec {
	return map[string]EnvSpec{"PATH": {PrependPaths: []string{"bin"}}}
}

func (nodeImporter) PostExtract(string) error { return nil }

// ---------- pnpm ----------

// pnpm 有两种发行方式：
//
//   - 旧版本是一个与平台无关的 JavaScript 包（npm 的 pnpm），运行在 node 之上：lockfile 中只有 any 条目，
//     启动脚本用 PATH 中的 node 执行 bin/pnpm.cjs（或 bin/pnpm.mjs）。
//   - 新版本的程序是各平台的原生可执行文件，在 npm 的 @pnpm/exe.<os>-<arch> 中；pnpm 包只是一层包装，
//     自己找不到原生文件时会在运行时下载。Kiln 直接锁定并安装原生包（glibc 的 linux、darwin），不经过包装。
//
// 按版本区分：pnpm 包的 optionalDependencies 中有 @pnpm/exe.* 的是新的发行方式。
type pnpmImporter struct{}

// pnpmExePrefix 是 pnpm 各平台原生包的名称前缀（@pnpm/exe.linux-arm64 等）。
const pnpmExePrefix = "@pnpm/exe."

// pnpmManifest 是 npm registry 中一个版本的元数据里用到的部分。
type pnpmManifest struct {
	Dist struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// native 判断这个版本是不是按平台发行原生可执行文件。
func (m *pnpmManifest) native() bool {
	for name := range m.OptionalDependencies {
		if strings.HasPrefix(name, pnpmExePrefix) {
			return true
		}
	}
	return false
}

func pnpmVersionManifest(ctx context.Context, pkg, version string) (*pnpmManifest, error) {
	var m pnpmManifest
	if err := getJSON(ctx, "https://registry.npmjs.org/"+pkg+"/"+version, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (pnpmImporter) PlatformIndependent(ctx context.Context, version string) (bool, error) {
	m, err := pnpmVersionManifest(ctx, "pnpm", version)
	if err != nil {
		return false, err
	}
	return !m.native(), nil
}

func (pnpmImporter) ResolveVersion(ctx context.Context, spec string) (string, error) {
	return npmResolve(ctx, "pnpm", spec)
}

func npmResolve(ctx context.Context, pkg, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if exactVersion(spec) {
		return strings.TrimPrefix(spec, "v"), nil
	}
	var meta struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := getJSON(ctx, "https://registry.npmjs.org/"+pkg, &meta); err != nil {
		return "", err
	}
	versions := make([]string, 0, len(meta.Versions))
	for v := range meta.Versions {
		versions = append(versions, v)
	}
	v, ok := semver.Latest(versions, func(v semver.Version) bool { return semver.MatchRange(spec, v) })
	if !ok {
		return "", fmt.Errorf("找不到满足 %q 的 %s 版本", spec, pkg)
	}
	return v, nil
}

// pnpmExePackage 返回平台对应的原生包名（只有 darwin 和 glibc 的 linux，arm64 / amd64）。
func pnpmExePackage(platform string) (string, error) {
	goos, goarch, err := splitPlatform(platform)
	if err != nil {
		return "", err
	}
	var arch string
	switch goarch {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "x64"
	}
	if arch == "" || (goos != "darwin" && goos != "linux") {
		return "", fmt.Errorf("没有 %s 平台的 pnpm 发行包", platform)
	}
	return pnpmExePrefix + goos + "-" + arch, nil
}

func (pnpmImporter) Artifact(ctx context.Context, version, platform string) (artifact, error) {
	pkg := "pnpm"
	if platform != anyPlatform {
		exe, err := pnpmExePackage(platform)
		if err != nil {
			return artifact{}, err
		}
		m, err := pnpmVersionManifest(ctx, "pnpm", version)
		if err != nil {
			return artifact{}, err
		}
		pkg = exe
		if _, ok := m.OptionalDependencies[pkg]; !ok {
			if !m.native() {
				return artifact{}, fmt.Errorf("pnpm %s 的发行包与平台无关，lockfile 中应为 %s 条目（运行 kiln runtime lock）", version, anyPlatform)
			}
			return artifact{}, fmt.Errorf("没有 %s 平台的 pnpm %s 发行包", platform, version)
		}
	}
	m, err := pnpmVersionManifest(ctx, pkg, version)
	if err != nil {
		return artifact{}, err
	}
	return artifact{URL: m.Dist.Tarball, Integrity: m.Dist.Integrity, Format: formatTarGz, Strip: 1}, nil
}

func (pnpmImporter) Env() map[string]EnvSpec {
	return map[string]EnvSpec{"PATH": {PrependPaths: []string{shimDir}}}
}

// pnpmNativeBin 是原生包中可执行文件的名称。
const pnpmNativeBin = "pnpm"

// pnpmScriptExts 是 JavaScript 发行包中入口脚本的扩展名（bin/pnpm.cjs、bin/pnpm.mjs），按顺序查找。
var pnpmScriptExts = []string{".cjs", ".mjs"}

// PostExtract 在 shimDir 中生成 pnpm、pnpx 的入口：原生包中是到可执行文件的链接（pnpx 即 pnpm dlx）；
// JavaScript 包中是用 node 执行入口脚本的启动脚本（pnpm 运行在 node runtime 之上）。
func (pnpmImporter) PostExtract(dir string) error {
	isFile := func(rel string) bool {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil && info.Mode().IsRegular()
	}
	if m, err := readPackageOptional(dir); err == nil && m.native() {
		// 包装（pnpm 包）被当成平台无关的发行包装了进来：里面没有程序，运行时会去下载
		return fmt.Errorf("这个版本的 pnpm 按平台发行原生可执行文件，lockfile 中的 %s 条目不能用（运行 kiln runtime lock）", anyPlatform)
	}
	for _, ext := range pnpmScriptExts {
		if isFile("bin/pnpm" + ext) {
			return writeShims(dir, map[string]string{"pnpm": "bin/pnpm" + ext, "pnpx": "bin/pnpx" + ext})
		}
	}
	if !isFile(pnpmNativeBin) {
		return errors.New("发行包中没有 pnpm 的可执行文件或入口脚本")
	}
	bin := filepath.Join(dir, shimDir)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	if err := os.Symlink("../"+pnpmNativeBin, filepath.Join(bin, "pnpm")); err != nil {
		return err
	}
	pnpx := "#!/bin/sh\nexec \"$(dirname \"$0\")/pnpm\" dlx \"$@\"\n"
	return os.WriteFile(filepath.Join(bin, "pnpx"), []byte(pnpx), 0o755)
}

// readPackageOptional 读取解包目录中 package.json 的 optionalDependencies。
func readPackageOptional(dir string) (*pnpmManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var m pnpmManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------- Bun ----------

type bunImporter struct{}

func (bunImporter) PlatformIndependent(context.Context, string) (bool, error) { return false, nil }

func (bunImporter) ResolveVersion(ctx context.Context, spec string) (string, error) {
	return npmResolve(ctx, "bun", spec)
}

// exactVersion 判断版本声明是否是完整的版本号（major.minor.patch，可以带预发布段），
// 是则直接使用，不查询 registry。以前按点的个数判断，1.2.3-rc.1 这样的预发布版本被当成范围，
// 解析成了正式版本 1.2.3（或者找不到）。
func exactVersion(spec string) bool {
	_, ok, _ := semver.IsRelease(spec)
	return ok
}

func bunTarget(platform string) (string, error) {
	goos, goarch, err := splitPlatform(platform)
	if err != nil {
		return "", err
	}
	var arch string
	switch goarch {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x64"
	}
	if arch == "" || (goos != "darwin" && goos != "linux") {
		return "", fmt.Errorf("没有 %s 平台的 Bun 发行包", platform)
	}
	t := "bun-" + goos + "-" + arch
	if strings.HasSuffix(platform, "/musl") {
		t += "-musl"
	}
	return t, nil
}

func (bunImporter) Artifact(ctx context.Context, version, platform string) (artifact, error) {
	t, err := bunTarget(platform)
	if err != nil {
		return artifact{}, err
	}
	base := fmt.Sprintf("https://github.com/oven-sh/bun/releases/download/bun-v%s/", version)
	sums, err := getText(ctx, base+"SHASUMS256.txt")
	if err != nil {
		return artifact{}, err
	}
	file := t + "." + formatZip
	sum := shasum(sums, file)
	if sum == "" {
		return artifact{}, fmt.Errorf("SHASUMS256.txt 中没有 %s", file)
	}
	return artifact{URL: base + file, SHA256: sum, Format: formatZip, Strip: 1}, nil
}

func (bunImporter) Env() map[string]EnvSpec {
	return map[string]EnvSpec{"PATH": {PrependPaths: []string{"."}}}
}

func (bunImporter) PostExtract(dir string) error {
	return writeShims(dir, map[string]string{"bunx": ""})
}
