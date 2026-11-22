package toolchain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ConfigPath 是 runtime 配置文件在仓库中的路径（PLAN #12.3）。
// v0 只支持其中的 [declarations]：显式指定某个 runtime 的版本声明从哪个文件或目录读取。
const ConfigPath = ".kiln/runtimes.toml"

// declFiles 是读取的版本声明文件名。
var declFiles = []string{"go.work", "go.mod", "package.json", ".node-version", ".nvmrc"}

// declFilesHint 用在错误信息中。
const declFilesHint = "go.work、go.mod、package.json 的 packageManager / devEngines / engines、.node-version、.nvmrc"

// declared 是项目文件中声明的 runtime 版本。
type declared struct {
	Spec   string // 版本或范围
	File   string // 声明所在的文件（相对于仓库根目录，/ 分隔）
	Source string // 文件和字段，例如 "app/go.mod toolchain"
}

// packageJSON 是 package.json 中与 runtime 版本有关的字段。
type packageJSON struct {
	PackageManager string          `json:"packageManager"`
	Engines        map[string]any  `json:"engines"`
	DevEngines     json.RawMessage `json:"devEngines"`
}

// runtimeConfig 是 .kiln/runtimes.toml 的内容。
type runtimeConfig struct {
	// Declarations：runtime 名称 → 版本声明所在的文件或目录（相对于仓库根目录）。
	Declarations map[string]string `toml:"declarations"`
}

// readConfig 读取 .kiln/runtimes.toml；不存在时返回零值。
func readConfig(repoDir string) (runtimeConfig, error) {
	var cfg runtimeConfig
	b, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(ConfigPath)))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	md, err := toml.Decode(string(b), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("解析 %s：%w", ConfigPath, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return cfg, fmt.Errorf("%s：不支持 %q（v0 只支持 [declarations]，见 PLAN #12.3）", ConfigPath, u[0].String())
	}
	for name, p := range cfg.Declarations {
		if _, ok := importers[name]; !ok {
			return cfg, fmt.Errorf("%s：[declarations] 中的 %s 不是内置 runtime（v0 只支持 go、node、pnpm、bun）", ConfigPath, name)
		}
		if p == "" || !filepath.IsLocal(filepath.FromSlash(p)) || strings.Contains(p, `\`) {
			return cfg, fmt.Errorf("%s：[declarations] %s = %q 应为仓库中的相对路径（用 / 分隔，不能以 / 开头或包含 ..）", ConfigPath, name, p)
		}
	}
	return cfg, nil
}

// detectDir 读取目录 dir（相对于仓库根目录，"." 为根目录）中的版本声明；only 非空时只读这个文件名。
// 同一目录中的优先级：go.work > go.mod（各自 toolchain > go）；package.json 的 packageManager；
// devEngines.runtime > .node-version > .nvmrc > engines.node。
func detectDir(repoDir, dir, only string) map[string]declared {
	read := func(name string) ([]byte, string, bool) {
		if only != "" && name != only {
			return nil, "", false
		}
		rel := path.Join(dir, name)
		b, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(rel)))
		return b, rel, err == nil
	}
	out := map[string]declared{}
	for _, f := range []string{"go.work", "go.mod"} {
		if b, rel, ok := read(f); ok {
			if d, ok := parseGoVersion(b, rel); ok {
				out["go"] = d
				break
			}
		}
	}
	var pkg packageJSON
	pkgRel := ""
	if b, rel, ok := read("package.json"); ok && json.Unmarshal(b, &pkg) == nil {
		pkgRel = rel
		detectPackageManager(pkg.PackageManager, rel, out)
		detectDevEngines(pkg.DevEngines, rel, out)
	}
	if _, ok := out["node"]; !ok {
		for _, f := range []string{".node-version", ".nvmrc"} {
			b, rel, ok := read(f)
			if !ok {
				continue
			}
			if s := strings.TrimSpace(string(b)); s != "" {
				out["node"] = declared{Spec: strings.TrimPrefix(s, "v"), File: rel, Source: rel}
				break
			}
		}
	}
	if _, ok := out["node"]; !ok {
		if s, ok := pkg.Engines["node"].(string); ok && s != "" {
			out["node"] = declared{Spec: s, File: pkgRel, Source: pkgRel + " engines.node"}
		}
	}
	return out
}

// parseGoVersion 读取 go.mod / go.work 的 toolchain 行，没有时用 go 行。
func parseGoVersion(b []byte, rel string) (declared, bool) {
	var goLine, toolchain string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "go":
			goLine = f[1]
		case "toolchain":
			toolchain = strings.TrimPrefix(f[1], "go")
		}
	}
	switch {
	case toolchain != "":
		return declared{Spec: toolchain, File: rel, Source: rel + " toolchain"}, true
	case goLine != "":
		return declared{Spec: goLine, File: rel, Source: rel + " go"}, true
	}
	return declared{}, false
}

// detectPackageManager 读取 "pnpm@9.12.0+sha512.…" 形式的 packageManager。
func detectPackageManager(pm, rel string, out map[string]declared) {
	if pm == "" {
		return
	}
	name, ver, _ := strings.Cut(pm, "@")
	ver, _, _ = strings.Cut(ver, "+")
	if name == "pnpm" || name == "bun" {
		out[name] = declared{Spec: ver, File: rel, Source: rel + " packageManager"}
	}
}

// detectDevEngines 读取 devEngines.runtime（对象或数组）。node 的声明优先于其他来源，
// bun 的声明不覆盖 packageManager。
func detectDevEngines(raw json.RawMessage, rel string, out map[string]declared) {
	if len(raw) == 0 {
		return
	}
	var de struct {
		Runtime json.RawMessage `json:"runtime"`
	}
	if json.Unmarshal(raw, &de) != nil || len(de.Runtime) == 0 {
		return
	}
	type rt struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	var list []rt
	var one rt
	if json.Unmarshal(de.Runtime, &one) == nil && one.Name != "" {
		list = []rt{one}
	} else if err := json.Unmarshal(de.Runtime, &list); err != nil {
		return // 格式不认识时忽略，与没有声明一样
	}
	for _, r := range list {
		if (r.Name != "node" && r.Name != "bun") || r.Version == "" {
			continue
		}
		if _, ok := out[r.Name]; !ok || r.Name == "node" {
			out[r.Name] = declared{Spec: r.Version, File: rel, Source: rel + " devEngines.runtime"}
		}
	}
}

// declFinder 按 PLAN #12.3 的规则查找 runtime 的版本声明：
//  1. .kiln/runtimes.toml 的 [declarations] 显式指定的文件或目录；
//  2. 根目录中的声明（单模块仓库与之前完全一样）；
//  3. 根目录没有时，所有子目录中的声明：只看 git 跟踪的和未被忽略的文件，
//     跳过隐藏目录、node_modules、vendor、testdata。
//
// 返回的候选不止一个时，由调用方确认它们解析出同一个版本，否则报歧义。
type declFinder struct {
	ctx     context.Context
	repoDir string
	cfg     runtimeConfig
	root    map[string]declared
	subDirs []map[string]declared // 按目录排序；第一次需要时才扫描
	scanned bool
}

func newDeclFinder(ctx context.Context, repoDir string) (*declFinder, error) {
	cfg, err := readConfig(repoDir)
	if err != nil {
		return nil, err
	}
	return &declFinder{ctx: ctx, repoDir: repoDir, cfg: cfg, root: detectDir(repoDir, ".", "")}, nil
}

// find 返回 runtime name 的版本声明候选；没有任何声明时报错。
func (f *declFinder) find(name string) ([]declared, error) {
	if p, ok := f.cfg.Declarations[name]; ok {
		d, err := f.explicit(name, p)
		if err != nil {
			return nil, err
		}
		return []declared{d}, nil
	}
	if d, ok := f.root[name]; ok {
		return []declared{d}, nil
	}
	if !f.scanned {
		if err := f.scan(); err != nil {
			return nil, err
		}
	}
	var out []declared
	for _, m := range f.subDirs {
		if d, ok := m[name]; ok {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("找不到 runtime %s 的版本声明（根目录和子目录中的 %s）；声明在其他文件中时，在 %s 的 [declarations] 中指定，例如 %s = \"app/%s\"",
			name, declFilesHint, ConfigPath, name, exampleFile(name))
	}
	return out, nil
}

// explicit 读取 [declarations] 中指定的文件或目录。
func (f *declFinder) explicit(name, p string) (declared, error) {
	where := fmt.Sprintf("%s 的 [declarations] %s = %q", ConfigPath, name, p)
	st, err := os.Stat(filepath.Join(f.repoDir, filepath.FromSlash(p)))
	if err != nil {
		return declared{}, fmt.Errorf("%s：%w", where, err)
	}
	dir, only := path.Clean(p), ""
	if !st.IsDir() {
		dir, only = path.Split(dir)
		if !slices.Contains(declFiles, only) {
			return declared{}, fmt.Errorf("%s：不是版本声明文件（%s），也可以写目录", where, declFilesHint)
		}
	}
	if dir == "" {
		dir = "."
	}
	d, ok := detectDir(f.repoDir, dir, only)[name]
	if !ok {
		return declared{}, fmt.Errorf("%s：其中没有 %s 的版本声明", where, name)
	}
	return d, nil
}

// scan 读取所有子目录中的版本声明。
func (f *declFinder) scan() error {
	files, err := projectFiles(f.ctx, f.repoDir)
	if err != nil {
		return fmt.Errorf("列出仓库中的文件：%w", err)
	}
	var dirs []string
	for _, rel := range files {
		dir, base := path.Split(rel)
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" || !slices.Contains(declFiles, base) || skippedDir(dir) || slices.Contains(dirs, dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	for _, dir := range dirs {
		if m := detectDir(f.repoDir, dir, ""); len(m) > 0 {
			f.subDirs = append(f.subDirs, m)
		}
	}
	f.scanned = true
	return nil
}

// skippedDir 判断扫描时是否跳过目录 dir（任何一级是隐藏目录、node_modules、vendor 或 testdata）。
func skippedDir(dir string) bool {
	for part := range strings.SplitSeq(dir, "/") {
		if strings.HasPrefix(part, ".") || part == "node_modules" || part == "vendor" || part == "testdata" {
			return true
		}
	}
	return false
}

// projectFiles 返回仓库中的文件（相对路径，/ 分隔）：git 仓库中为跟踪的和未被忽略的文件
// （与 Server 从 git tree 中看到的一致，不含构建产物和第三方下载），否则遍历目录。
func projectFiles(ctx context.Context, repoDir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if out, err := cmd.Output(); err == nil {
		var files []string
		for f := range strings.SplitSeq(string(out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skippedDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

// exampleFile 是错误信息中示例用的声明文件名。
func exampleFile(name string) string {
	if name == "go" {
		return "go.mod"
	}
	return "package.json"
}

// sameVersion 确认所有候选解析出同一个精确版本，返回该版本；不一致时报歧义，并提示如何显式选择。
func sameVersion(ctx context.Context, imp importer, name string, cands []declared) (string, error) {
	resolved := make([]string, len(cands))
	cache := map[string]string{}
	for i, d := range cands {
		v, ok := cache[d.Spec]
		if !ok {
			var err error
			if v, err = imp.ResolveVersion(ctx, d.Spec); err != nil {
				return "", fmt.Errorf("%s（%s = %q）：%w", name, d.Source, d.Spec, err)
			}
			cache[d.Spec] = v
		}
		resolved[i] = v
	}
	if !slices.ContainsFunc(resolved, func(v string) bool { return v != resolved[0] }) {
		return resolved[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runtime %s 的版本声明不一致，不能确定用哪一个：", name)
	for i, d := range cands {
		fmt.Fprintf(&b, "\n  %s = %q → %s", d.Source, d.Spec, resolved[i])
	}
	fmt.Fprintf(&b, "\n改成一致的版本，或在 %s 中指定用哪一个，例如：\n  [declarations]\n  %s = %q", ConfigPath, name, cands[0].File)
	return "", errors.New(b.String())
}

// describe 用于进度输出：每个候选的来源和声明的版本。
func describe(cands []declared) string {
	parts := make([]string, len(cands))
	for i, d := range cands {
		parts[i] = fmt.Sprintf("%s %q", d.Source, d.Spec)
	}
	return strings.Join(parts, "、")
}
