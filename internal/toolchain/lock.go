// Package toolchain 处理 runtime（工具链）：lockfile、内置 importer（Go、Node、pnpm、Bun）、下载与解包（PLAN #12）。
//
// v0 的简化：lockfile 中的 digest 是上游发行包的 sha256，Runner 直接从上游下载并校验，
// 不经过 Kiln 的 CAS（PLAN #25）。
package toolchain

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// LockPath 是 lockfile 在仓库中的路径。
const LockPath = ".kiln/runtimes.lock"

// anyPlatform 表示平台无关的 runtime（例如 pnpm）。
const anyPlatform = "any"

// Lock 是 .kiln/runtimes.lock 的内容。
type Lock struct {
	Entries map[string]*LockEntry
}

// LockEntry 是一个 runtime 的锁定版本和各平台的 digest。
type LockEntry struct {
	Name      string
	Version   string
	Platforms map[string]string // 平台 → "sha256:…"
}

// ParseLock 解析 lockfile。
func ParseLock(data []byte) (*Lock, error) {
	var raw map[string]map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", LockPath, err)
	}
	l := &Lock{Entries: map[string]*LockEntry{}}
	for name, tbl := range raw {
		e := &LockEntry{Name: name, Platforms: map[string]string{}}
		for k, v := range tbl {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s：[%s].%s 应为字符串", LockPath, name, k)
			}
			if k == "version" {
				e.Version = s
				continue
			}
			if !strings.HasPrefix(s, cas.DigestPrefix) {
				return nil, fmt.Errorf("%s：[%s].%q 应为 sha256:…", LockPath, name, k)
			}
			e.Platforms[k] = s
		}
		if e.Version == "" {
			return nil, fmt.Errorf("%s：[%s] 缺少 version", LockPath, name)
		}
		l.Entries[name] = e
	}
	return l, nil
}

// Resolve 返回 runtime 在某个平台上的条目：平台、digest。
func (l *Lock) Resolve(name, platform string) (version, plat, digest string, err error) {
	if l == nil {
		return "", "", "", fmt.Errorf("job 使用了 runtime %s，但仓库中没有 %s（运行 kiln runtime lock 生成）", name, LockPath)
	}
	e := l.Entries[name]
	if e == nil {
		return "", "", "", fmt.Errorf("runtime %s 不在 %s 中（运行 kiln runtime lock）", name, LockPath)
	}
	base := platformBase(platform)
	if d, ok := e.Platforms[base]; ok {
		return e.Version, base, d, nil
	}
	if d, ok := e.Platforms[anyPlatform]; ok {
		return e.Version, anyPlatform, d, nil
	}
	return "", "", "", fmt.Errorf("runtime %s 在 %s 中没有平台 %s 的条目（运行 kiln runtime lock）", name, LockPath, base)
}

// platformBase 去掉 abi 部分：linux/arm64/gnu → linux/arm64。
func platformBase(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		return parts[0] + "/" + parts[1]
	}
	return p
}

// Format 输出 lockfile 内容（键排序，结果稳定）。
func (l *Lock) Format() []byte {
	var b bytes.Buffer
	b.WriteString("# 由 `kiln runtime lock` 根据 go.mod、package.json 等文件生成，请勿手工修改（PLAN #12.3）\n")
	b.WriteString("# v0：digest 为上游发行包的 sha256\n")
	for _, n := range slices.Sorted(maps.Keys(l.Entries)) {
		e := l.Entries[n]
		fmt.Fprintf(&b, "\n[%s]\nversion = %q\n", n, e.Version)
		for _, p := range slices.Sorted(maps.Keys(e.Platforms)) {
			fmt.Fprintf(&b, "%q = %q\n", p, e.Platforms[p])
		}
	}
	return b.Bytes()
}
