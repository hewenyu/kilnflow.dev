package toolchain

import (
	"fmt"
	"maps"
	"slices"
)

// BuiltinNames 返回内置 runtime 的名称（排序）。
func BuiltinNames() []string { return slices.Sorted(maps.Keys(importers)) }

// Builtin 判断 name 是不是内置 runtime。
func Builtin(name string) bool {
	_, ok := importers[name]
	return ok
}

// ResolveRefs 按 lockfile 解析 runtime 列表在 platform 上的版本和 digest（顺序与 names 相同），用于 AI 任务的
// requirements.tools（PLAN #33.12）。lock 为 nil（仓库中没有 lockfile）、缺少条目或平台时返回错误，不换版本、不取最新。
func ResolveRefs(lock *Lock, names []string, platform string) ([]Ref, error) {
	if lock == nil {
		return nil, fmt.Errorf("仓库中没有 %s（运行 kiln runtime lock 生成）", LockPath)
	}
	refs := make([]Ref, 0, len(names))
	for _, name := range names {
		version, plat, digest, err := lock.Resolve(name, platform)
		if err != nil {
			return nil, err
		}
		refs = append(refs, Ref{Name: name, Version: version, Platform: plat, Digest: digest})
	}
	return refs, nil
}
