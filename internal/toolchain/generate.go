package toolchain

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// Generate 为 needs 中的每个 runtime（名称 → 平台列表）生成 lockfile。
// 版本来自项目文件中的声明（查找规则见 declFinder）；old 中版本相同、平台已存在的条目直接沿用。
func Generate(ctx context.Context, repoDir string, needs map[string][]string, old *Lock, logf func(string, ...any)) (*Lock, error) {
	finder, err := newDeclFinder(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	lock := &Lock{Entries: map[string]*LockEntry{}}
	for _, name := range slices.Sorted(maps.Keys(needs)) {
		imp, ok := importers[name]
		if !ok {
			return nil, fmt.Errorf("runtime %s 不是内置 runtime（v0 只支持 go、node、pnpm、bun）", name)
		}
		cands, err := finder.find(name)
		if err != nil {
			return nil, err
		}
		version, err := sameVersion(ctx, imp, name, cands)
		if err != nil {
			return nil, err
		}
		logf("%s %s（来自 %s）", name, version, describe(cands))
		var prev *LockEntry
		if old != nil {
			if p := old.Entries[name]; p != nil && p.Version == version {
				prev = p
			}
		}
		plats := needs[name]
		independent, err := imp.PlatformIndependent(ctx, version)
		if err != nil {
			return nil, err
		}
		if independent {
			plats = []string{anyPlatform}
		}
		e, err := lockEntry(ctx, imp, name, version, plats, prev, logf)
		if err != nil {
			return nil, err
		}
		lock.Entries[name] = e
	}
	return lock, nil
}

// lockEntry 计算一个 runtime 在各平台上的 digest；prev 中已有的平台直接沿用。
func lockEntry(ctx context.Context, imp importer, name, version string, plats []string, prev *LockEntry, logf func(string, ...any)) (*LockEntry, error) {
	e := &LockEntry{Name: name, Version: version, Platforms: map[string]string{}}
	for _, p := range plats {
		p = platformBase(p)
		if prev != nil && prev.Platforms[p] != "" {
			e.Platforms[p] = prev.Platforms[p]
			continue
		}
		art, err := imp.Artifact(ctx, version, p)
		if err != nil {
			return nil, err
		}
		digest := cas.DigestPrefix + strings.ToLower(art.SHA256)
		if art.SHA256 == "" {
			logf("  下载 %s 计算 sha256 …", art.URL)
			if digest, err = hashURL(ctx, art.URL, art.Integrity); err != nil {
				return nil, err
			}
		}
		// 上游索引给出的 sha256 格式不对时，写进 lockfile 的条目在安装时才会报错，这里提前拒绝
		if !cas.ValidDigest(digest) {
			return nil, fmt.Errorf("%s 的 sha256 %q 无效", art.URL, art.SHA256)
		}
		e.Platforms[p] = digest
		logf("  %s = %s", p, digest)
	}
	return e, nil
}
