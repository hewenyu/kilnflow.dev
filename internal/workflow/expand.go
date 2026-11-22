package workflow

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// 表达式中的上下文名（expr.Context 的键）。
const (
	ctxGitHub  = "github"
	ctxInputs  = "inputs"
	ctxVars    = "vars"
	ctxMatrix  = "matrix"
	ctxKiln    = "kiln"
	ctxSecrets = "secrets"
)

// WorkspacePlaceholder 是 ${{ kiln.workspace }} 在规划后的形式，由 Runner 执行时替换（PLAN #17.1）。
const WorkspacePlaceholder = "${{ kiln.workspace }}"

// MaxJobs 是单个 run 的 job 上限（与 Actions 相同）。
const MaxJobs = 256

var platformRe = regexp.MustCompile(`^(darwin|linux)/(arm64|amd64)(/(gnu|musl))?$`)

// ValidPlatform 判断是否是已知平台。
func ValidPlatform(p string) bool { return platformRe.MatchString(p) }

// expander 保存展开一个 workflow 时的共享状态。
type expander struct {
	w      *Workflow
	pc     PlanContext
	p      *Plan
	github expr.Strict
	inputs expr.Lenient
	kiln   expr.Strict
}

// Expand 对 workflow 求值并展开矩阵。返回的 Diags 有错误时整个 workflow 规划失败。
func Expand(w *Workflow, pc PlanContext) *Plan {
	x := newExpander(w, pc)
	x.expandConcurrency()
	variants := x.expandJobs()
	x.checkUnique()
	x.resolveDeps(variants)
	return x.p
}

func newExpander(w *Workflow, pc PlanContext) *expander {
	if pc.Vars == nil {
		pc.Vars = func(string) map[string]string { return nil }
	}
	return &expander{
		w:      w,
		pc:     pc,
		p:      &Plan{Workflow: w},
		github: expr.Strict{Name: ctxGitHub, Fields: pc.GitHub},
		inputs: dispatchInputs(w, pc),
		kiln: expr.Strict{Name: ctxKiln, Fields: map[string]any{
			"workspace":   WorkspacePlaceholder,
			"commit_time": float64(pc.CommitTime),
		}},
	}
}

// dispatchInputs 构造 inputs 上下文：没有传入的输入取默认值（非 workflow_dispatch 事件取空字符串），
// boolean 类型转换成布尔值。
func dispatchInputs(w *Workflow, pc PlanContext) expr.Lenient {
	inputs := expr.Lenient{}
	for k, v := range pc.Inputs {
		inputs[k] = v
	}
	if w.On.WorkflowDispatch == nil {
		return inputs
	}
	isDispatch := pc.GitHub["event_name"] == EventWorkflowDispatch
	for _, in := range w.On.WorkflowDispatch.Inputs {
		if _, ok := inputs[in.Name]; !ok {
			if isDispatch {
				inputs[in.Name] = in.Default
			} else {
				inputs[in.Name] = ""
			}
		}
		if in.Type == InputBoolean && isDispatch {
			inputs[in.Name] = strings.EqualFold(expr.ToString(inputs[in.Name]), "true")
		}
	}
	return inputs
}

func (x *expander) errf(pos Pos, format string, args ...any) {
	x.p.Diags = append(x.p.Diags, Diag{File: x.w.File, Line: pos.Line, Col: pos.Col, Msg: fmt.Sprintf(format, args...)})
}

// errs 把 err 报告为诊断；err 由 errors.Join 合并而成时每个错误单独一条。
func (x *expander) errs(pos Pos, prefix string, err error) {
	var list []error
	// 只展开顶层的 errors.Join（interpolator 收集的错误），不深入被包装的错误
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		list = j.Unwrap()
	} else {
		list = []error{err}
	}
	for _, e := range list {
		x.errf(pos, "%s%v", prefix, e)
	}
}

// warnf 添加一条警告（相同的警告只保留一条，例如矩阵的多个变体）。
func (x *expander) warnf(pos Pos, format string, args ...any) {
	d := Diag{File: x.w.File, Line: pos.Line, Col: pos.Col, Msg: fmt.Sprintf(format, args...), Warning: true}
	if !slices.Contains(x.p.Diags, d) {
		x.p.Diags = append(x.p.Diags, d)
	}
}

// baseCtx 返回某个 environment 下的基础上下文：github、inputs、vars。
func (x *expander) baseCtx(env string) expr.Context {
	vars := expr.Lenient{}
	for k, v := range x.pc.Vars(env) {
		vars[k] = v
	}
	return expr.Context{ctxGitHub: x.github, ctxInputs: x.inputs, ctxVars: vars}
}

// expandConcurrency 对 run 级 concurrency 求值。
func (x *expander) expandConcurrency() {
	c := x.w.Concurrency
	if c == nil {
		return
	}
	ctx := x.baseCtx("")
	g, err := expr.Interpolate(c.Group, ctx)
	if err != nil {
		x.errf(c.Pos, "concurrency.group：%v", err)
	}
	x.p.ConcurrencyGroup = g
	cancel, err := expr.EvalCondition(c.CancelInProgress, ctx)
	if err != nil {
		x.errf(c.Pos, "concurrency.cancel-in-progress：%v", err)
	}
	x.p.CancelInProgress = cancel
}

// expandJobs 展开每个 job 的矩阵，返回 job ID → 变体。
func (x *expander) expandJobs() map[string][]*PlannedJob {
	variants := map[string][]*PlannedJob{}
	for _, j := range x.w.Jobs {
		combos, order, err := expandMatrix(j, x.baseCtx(staticEnvironment(j)))
		if err != nil {
			x.errf(j.Pos, "job %s 的矩阵：%v", j.ID, err)
			continue
		}
		// 合计超过上限时立即停止：剩下的变体不必再展开（展开每个变体都要对全部 step 求值）。
		if len(x.p.Jobs)+len(combos) > MaxJobs {
			x.errf(Pos{}, "矩阵展开后共有 %d 个 job，超过上限 %d", len(x.p.Jobs)+len(combos), MaxJobs)
			return variants
		}
		for _, combo := range combos {
			pj, err := x.expandJob(j, combo, order)
			if err != nil {
				x.errs(j.Pos, "job "+j.ID+"：", err)
				continue
			}
			variants[j.ID] = append(variants[j.ID], pj)
			x.p.Jobs = append(x.p.Jobs, pj)
		}
		if len(combos) == 0 {
			x.errf(j.Pos, "job %s 的矩阵展开后为空", j.ID)
		}
	}
	return variants
}

// checkUnique 检查显示名、job_key 不重复。
func (x *expander) checkUnique() {
	names := map[string]string{}
	keys := map[string]bool{}
	for _, pj := range x.p.Jobs {
		pos := x.w.Job(pj.JobID).Pos
		if other, ok := names[pj.DisplayName]; ok {
			x.errf(pos, "job %s 与 %s 的显示名都是 %q，Check Run 会重名", pj.Key, other, pj.DisplayName)
		}
		names[pj.DisplayName] = pj.Key
		if keys[pj.Key] {
			x.errf(pos, "job %s 的矩阵中有重复的变体", pj.Key)
		}
		keys[pj.Key] = true
	}
}

// resolveDeps 把 needs 和 fetch 解析为上游变体的 job_key。
func (x *expander) resolveDeps(variants map[string][]*PlannedJob) {
	for _, pj := range x.p.Jobs {
		j := x.w.Job(pj.JobID)
		for _, n := range j.Needs {
			for _, up := range variants[n] {
				pj.NeedsKeys = append(pj.NeedsKeys, up.Key)
			}
		}
		for i, f := range j.Fetch {
			upID, out := f.Split()
			chosen := x.selectVariants(pj, f, variants[upID])
			if len(chosen) != 1 {
				x.errf(f.Pos, "job %s 的 fetch %s 选出了 %d 个变体，应恰好一个", pj.Key, f.From, len(chosen))
				continue
			}
			if pj.Spec != nil && i < len(pj.Spec.Fetch) {
				pj.Spec.Fetch[i].JobKey = chosen[0].Key
				pj.Spec.Fetch[i].Output = out
			}
		}
	}
}

// selectVariants 返回上游变体中与 fetch.matrix 相符的那些；没有写 matrix 时返回全部。
func (x *expander) selectVariants(pj *PlannedJob, f Fetch, ups []*PlannedJob) []*PlannedJob {
	if len(f.Matrix) == 0 {
		return ups
	}
	// vars 按变体求值后的 environment 名称读取（名称中可能有表达式）
	environment := ""
	if pj.Spec != nil {
		environment = pj.Spec.Environment
	}
	ctx := x.baseCtx(environment)
	// 与 job 的其他字段一致：带矩阵时引用不存在的变量报错，而不是静默得到空字符串
	ctx[ctxMatrix] = matrixCtx(x.w.Job(pj.JobID), pj.Matrix)
	want := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(f.Matrix)) {
		v, err := expr.Interpolate(f.Matrix[k], ctx)
		if err != nil {
			x.errf(f.Pos, "fetch.matrix.%s：%v", k, err)
			return nil
		}
		want[k] = v
	}
	var chosen []*PlannedJob
	for _, up := range ups {
		if variantMatches(up.Matrix, want) {
			chosen = append(chosen, up)
		}
	}
	return chosen
}

// variantMatches 判断矩阵取值是否与 want 中的每一项（按字符串比较）都相同。
func variantMatches(matrix map[string]any, want map[string]string) bool {
	for k, v := range want {
		got, ok := matrix[k]
		if !ok || expr.ToString(got) != v {
			return false
		}
	}
	return true
}

// JobKey 返回 job ID 加按变量名排序的矩阵取值，例如 build{goarch=arm64,goos=linux}（PLAN #5.2）。
func JobKey(id string, combo map[string]any) string {
	if len(combo) == 0 {
		return id
	}
	keys := slices.Sorted(maps.Keys(combo))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + expr.ToString(combo[k])
	}
	return id + "{" + strings.Join(parts, ",") + "}"
}

// refersTo 判断表达式引用（expr.StringRefs 的结果，例如 "matrix.os"、"secrets.TOKEN"）中是否有属于上下文 name 的。
// 上下文名不区分大小写，与表达式求值一致。
func refersTo(refs []string, name string) bool {
	return slices.ContainsFunc(refs, func(ref string) bool {
		head, _, _ := strings.Cut(ref, ".")
		return strings.EqualFold(head, name)
	})
}
