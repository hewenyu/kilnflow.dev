package toolchain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Mounted 是一个已挂载的 runtime。
type Mounted struct {
	Mount string // runtime 在 job 中可见的路径
	Meta  *Meta
}

// Env 是一组 runtime 对环境变量的设置（PLAN #12.2）：Set 中的变量直接设置；Prepend 中的变量（例如 PATH）把这些路径
// 按顺序加到原值之前；Order 是 Prepend 中变量第一次出现的顺序（结果稳定）。
type Env struct {
	Set     map[string]string   `json:"set"`
	Prepend map[string][]string `json:"prepend"`
	Order   []string            `json:"-"`
}

// Environment 根据 runtime 的挂载点计算环境变量设置。runtimes 按声明的顺序给出；prepend_paths 按顺序叠加（先声明的
// 在前），path / value 冲突时报错。CI 的 job（ApplyEnv）和 AI 执行节点的任务（kiln-runner toolchain ensure，PLAN #33.12）
// 都用这一条规则。
func Environment(mounts []Mounted) (*Env, error) {
	e := &Env{Set: map[string]string{}, Prepend: map[string][]string{}}
	for _, m := range mounts {
		for name, spec := range m.Meta.Env {
			switch {
			case len(spec.PrependPaths) > 0:
				if _, ok := e.Prepend[name]; !ok {
					e.Order = append(e.Order, name)
				}
				for _, p := range spec.PrependPaths {
					e.Prepend[name] = append(e.Prepend[name], joinMount(m.Mount, p))
				}
			case spec.IsValue:
				if old, ok := e.Set[name]; ok && old != spec.Value {
					return nil, fmt.Errorf("runtime 之间的环境变量 %s 冲突", name)
				}
				e.Set[name] = spec.Value
			default:
				v := joinMount(m.Mount, spec.Path)
				if old, ok := e.Set[name]; ok && old != v {
					return nil, fmt.Errorf("runtime 之间的环境变量 %s 冲突", name)
				}
				e.Set[name] = v
			}
		}
	}
	return e, nil
}

// Apply 把设置合并进 env：Set 覆盖原值，Prepend 的路径用 ":" 加在原值之前（原值为空时只有这些路径）。
func (e *Env) Apply(env map[string]string) {
	maps.Copy(env, e.Set)
	for _, name := range e.Order {
		parts := e.Prepend[name]
		if old := env[name]; old != "" {
			parts = append(slices.Clip(parts), old)
		}
		env[name] = strings.Join(parts, ":")
	}
}

// ApplyEnv 根据 runtime 的挂载点计算环境变量并合并进 env（见 Environment、Env.Apply）。
func ApplyEnv(env map[string]string, mounts []Mounted) error {
	e, err := Environment(mounts)
	if err != nil {
		return err
	}
	e.Apply(env)
	return nil
}

func joinMount(mount, rel string) string {
	if rel == "" || rel == "." {
		return mount
	}
	return strings.TrimRight(mount, "/") + "/" + strings.TrimPrefix(rel, "./")
}
