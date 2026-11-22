package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// 矩阵中有特殊含义的键。
const (
	matrixInclude = "include"
	matrixExclude = "exclude"
)

// maxCartesian 是 exclude 之前笛卡尔积的组合数上限。展开后的 job 数由 MaxJobs 限制，
// 但那是在生成全部组合之后检查的；变量多时组合数按乘积增长，不先限制会在 Server 上耗尽内存。
const maxCartesian = 1 << 16

// maxExcludeWork 是 exclude 时"变体数 × exclude 项数"的上限。组合数和 exclude 项数各自都可以很大，
// 逐一比较的次数是二者的乘积，不限制时一个 workflow 就能让 Server 的规划长时间占满 CPU。
const maxExcludeWork = 1 << 24

// maxIncludeWork 是 include 时"变体数 × include 项数"的上限：每个 include 项都要和全部原始变体比较一次，
// 不限制时一个 workflow 就能让规划长时间占满 CPU。
const maxIncludeWork = 1 << 16

// expandMatrix 按 Actions 的规则展开矩阵：先求各变量的笛卡尔积，再去掉 exclude，最后合并 include。
// 返回全部变体和变量名的显示顺序。
func expandMatrix(j *Job, ctx expr.Context) ([]map[string]any, []string, error) {
	if j.Strategy.Matrix == nil {
		return []map[string]any{{}}, nil, nil
	}
	m, order, err := matrixDef(j, ctx)
	if err != nil {
		return nil, nil, err
	}
	combos, err := cartesian(m, order, ctx)
	if err != nil {
		return nil, nil, err
	}
	if ex, ok := m[matrixExclude]; ok {
		list, err := evalList(ex, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("exclude：%w", err)
		}
		if combos, err = applyExclude(combos, list); err != nil {
			return nil, nil, err
		}
	}
	// include 不会减少变体，超过上限时不必再合并，也不必逐个展开（展开每个变体都要对全部 step 求值）
	if err := checkVariantCount(combos); err != nil {
		return nil, nil, err
	}
	if inc, ok := m[matrixInclude]; ok {
		list, err := evalList(inc, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("include：%w", err)
		}
		if combos, order, err = applyInclude(combos, order, list, j.Strategy.IncludeOrder); err != nil {
			return nil, nil, err
		}
		if err := checkVariantCount(combos); err != nil {
			return nil, nil, err
		}
	}
	return combos, order, nil
}

// checkVariantCount 检查一个 job 的变体数没有超过单个 run 的 job 上限。
func checkVariantCount(combos []map[string]any) error {
	if len(combos) > MaxJobs {
		return fmt.Errorf("展开后有 %d 个变体，超过单个 run 的 job 上限 %d", len(combos), MaxJobs)
	}
	return nil
}

// matrixDef 返回矩阵的定义和变量名顺序。整个矩阵是表达式时先求值，变量名按字母排序。
func matrixDef(j *Job, ctx expr.Context) (map[string]any, []string, error) {
	switch r := j.Strategy.Matrix.(type) {
	case string:
		v, err := expr.InterpolateValue(r, ctx)
		if err != nil {
			return nil, nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("matrix 表达式的结果应为对象")
		}
		var order []string
		for _, k := range slices.Sorted(maps.Keys(m)) {
			// 表达式的结果是数据，其中的字符串不能再当成表达式求值（evalList 对字符串求值，只适用于 YAML 中写的表达式）
			if _, ok := m[k].([]any); !ok {
				return nil, nil, fmt.Errorf("matrix 表达式的结果中 %s 应为列表", k)
			}
			if k != matrixInclude && k != matrixExclude {
				order = append(order, k)
			}
		}
		return m, order, nil
	case map[string]any:
		return r, slices.Clone(j.Strategy.MatrixOrder), nil
	}
	return nil, nil, fmt.Errorf("matrix 的类型不正确：%T", j.Strategy.Matrix)
}

// evalList 返回列表的值；值是表达式时先求值。
func evalList(v any, ctx expr.Context) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		r, err := expr.InterpolateValue(x, ctx)
		if err != nil {
			return nil, err
		}
		l, ok := r.([]any)
		if !ok {
			return nil, fmt.Errorf("应为列表：%s", x)
		}
		return l, nil
	case nil:
		return nil, nil
	}
	return nil, errors.New("应为列表")
}

// cartesian 返回 order 中各变量取值的笛卡尔积。没有任何变量时返回 nil（只靠 include 生成变体）。
func cartesian(m map[string]any, order []string, ctx expr.Context) ([]map[string]any, error) {
	if len(order) == 0 {
		return nil, nil
	}
	combos := []map[string]any{{}}
	for _, k := range order {
		vals, err := evalList(m[k], ctx)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", k, err)
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("%s 不能为空列表", k)
		}
		if len(combos)*len(vals) > maxCartesian {
			return nil, fmt.Errorf("各变量取值的组合超过 %d 个", maxCartesian)
		}
		next := make([]map[string]any, 0, len(combos)*len(vals))
		for _, c := range combos {
			for _, v := range vals {
				n := maps.Clone(c)
				n[k] = v
				next = append(next, n)
			}
		}
		combos = next
	}
	return combos, nil
}

// applyExclude 去掉与 exclude 中任意一项部分匹配的变体。
func applyExclude(combos []map[string]any, list []any) ([]map[string]any, error) {
	excludes := make([]map[string]any, 0, len(list))
	for _, e := range list {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, errors.New("exclude 的每一项应为对象")
		}
		excludes = append(excludes, em)
	}
	if len(combos)*len(excludes) > maxExcludeWork {
		return nil, fmt.Errorf("exclude 前有 %d 个组合、%d 个 exclude 项，比较次数超过上限 %d", len(combos), len(excludes), maxExcludeWork)
	}
	var kept []map[string]any
	for _, c := range combos {
		if !slices.ContainsFunc(excludes, func(em map[string]any) bool { return partialMatch(c, em) }) {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// applyInclude 合并 include：每一项合并到所有不冲突（不改变原有变量取值）的原始变体中；
// 没有可以合并的变体时作为新变体追加。include 引入的新变量追加到 order 末尾。
func applyInclude(combos []map[string]any, order []string, list []any, includeOrder []string) ([]map[string]any, []string, error) {
	baseKeys := map[string]bool{}
	for _, k := range order {
		baseKeys[k] = true
	}
	origLen := len(combos)
	// 只有 include 的矩阵 origLen 为 0，但每个 include 项仍会追加一个变体，仍按"至少一个变体"计算工作量，
	// 并在追加时按 MaxJobs 早停（见下）。
	if max(origLen, 1)*len(list) > maxIncludeWork {
		return nil, nil, fmt.Errorf("include 前有 %d 个变体、%d 个 include 项，比较次数超过上限 %d", origLen, len(list), maxIncludeWork)
	}
	for _, e := range list {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, nil, errors.New("include 的每一项应为对象")
		}
		for _, k := range includeKeys(em, includeOrder) {
			if !baseKeys[k] && !slices.Contains(order, k) {
				order = append(order, k)
			}
		}
		added := false
		for _, c := range combos[:origLen] {
			if conflicts(c, em, baseKeys) {
				continue
			}
			maps.Copy(c, em)
			added = true
		}
		if !added {
			combos = append(combos, maps.Clone(em))
			// 只有 include 的矩阵没有原始变体，这里逐个追加：同样按 MaxJobs 早停，不先展开出超大的列表。
			if err := checkVariantCount(combos); err != nil {
				return nil, nil, err
			}
		}
	}
	return combos, order, nil
}

// includeKeys 返回 include 项的变量名：先按它们在 YAML 中首次出现的顺序，其余按字母排序。
func includeKeys(em map[string]any, includeOrder []string) []string {
	keys := make([]string, 0, len(em))
	for _, k := range includeOrder {
		if _, ok := em[k]; ok {
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range em {
		if !slices.Contains(keys, k) {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	return append(keys, rest...)
}

// conflicts 判断 include 项是否会改变变体中原有变量的取值。
func conflicts(c, em map[string]any, baseKeys map[string]bool) bool {
	for k, v := range em {
		if baseKeys[k] && !valueEqual(c[k], v) {
			return true
		}
	}
	return false
}

func partialMatch(c, sel map[string]any) bool {
	for k, v := range sel {
		if !valueEqual(c[k], v) {
			return false
		}
	}
	return true
}

// valueEqual 比较两个矩阵取值：先比较结构，再比较 JSON 形式（例如 YAML 的 1 与表达式的 1.0）。
// 无法序列化的值视为不相等。字符串、数字、布尔值和 null 直接比较（与比较 JSON 形式的结果相同），
// 避免 exclude、include 逐一比较时每次都序列化。
func valueEqual(a, b any) bool {
	if eq, ok := scalarEqual(a, b); ok {
		return eq
	}
	if reflect.DeepEqual(a, b) {
		return true
	}
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}

// scalarEqual 比较两个标量（字符串、数字、布尔值、null）；ok 为 false 表示其中有数组或对象，需要按结构比较。
// 类型不同的标量不相等（JSON 形式也不同），NaN 与任何值都不相等（无法序列化）。
func scalarEqual(a, b any) (eq, ok bool) {
	na, aNum := number(a)
	nb, bNum := number(b)
	switch {
	case !isScalar(a) || !isScalar(b):
		return false, false
	case aNum || bNum:
		return aNum && bNum && na == nb, true
	}
	return a == b, true
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64, int:
		return true
	}
	return false
}

// number 返回数字类型的值（YAML 和 fromJSON 得到 float64，Go 代码中构造的可能是 int）。
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
