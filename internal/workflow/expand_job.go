package workflow

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// interpolator 依次对多个字段插值，收集所有错误而不是只保留一个。
type interpolator struct{ errs []error }

func (in *interpolator) str(what, s string, ctx expr.Context) string {
	out, err := expr.Interpolate(s, ctx)
	if err != nil {
		in.fail(fmt.Errorf("%s：%w", what, err))
	}
	return out
}

// strs 对列表中的每一项插值。
func (in *interpolator) strs(what string, l []string, ctx expr.Context) []string {
	if l == nil {
		return nil
	}
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = in.str(what, s, ctx)
	}
	return out
}

func (in *interpolator) fail(err error) { in.errs = append(in.errs, err) }

func (in *interpolator) err() error { return errors.Join(in.errs...) }

// jobContexts 是展开一个 job 变体时使用的上下文。
type jobContexts struct {
	// base 用于 if、name、environment.url 等：github、inputs、vars、matrix。
	base expr.Context
	// run 在 base 的基础上加上 kiln，用于 runs-on、steps 等。
	run expr.Context
	// env 在 run 的基础上加上 secrets（占位符），用于 env 和发布凭证。
	env     expr.Context
	secrets *expr.Placeholder
}

// jobContexts 返回展开 job 时使用的上下文；vars 按 environment（已求值的名称）→ 仓库 → 账号的优先级合并。
func (x *expander) jobContexts(j *Job, combo map[string]any, environment string) jobContexts {
	base := x.baseCtx(environment)
	base[ctxMatrix] = matrixCtx(j, combo)
	run := maps.Clone(base)
	run[ctxKiln] = x.kiln
	secrets := &expr.Placeholder{Prefix: ctxSecrets, Used: map[string]bool{}}
	env := maps.Clone(run)
	env[ctxSecrets] = secrets
	return jobContexts{base: base, run: run, env: env, secrets: secrets}
}

// matrixCtx 返回 job 变体的 matrix 上下文（没有矩阵时引用 matrix 得到空值）。
func matrixCtx(j *Job, combo map[string]any) any {
	if j.Strategy.Matrix != nil {
		return expr.Strict{Name: ctxMatrix, Fields: combo}
	}
	return expr.Lenient{}
}

// environmentName 求出 job 变体实际使用的 environment 名称。名称中的表达式可以引用
// github、inputs、matrix 和账号级、仓库级的 vars；environment 级的 vars 要等名称确定后才知道读哪个，不能引用。
func (x *expander) environmentName(j *Job, combo map[string]any) (string, error) {
	if !expr.HasExpr(j.Environment) {
		return j.Environment, nil
	}
	ctx := x.baseCtx("")
	ctx[ctxMatrix] = matrixCtx(j, combo)
	return expr.Interpolate(j.Environment, ctx)
}

// staticEnvironment 返回不含表达式的 environment 名称（矩阵在变体确定之前展开，只能用这种名称读取 environment 级的 vars）；
// 名称中有表达式时返回空，矩阵只能引用账号级、仓库级的 vars。
func staticEnvironment(j *Job) string {
	if expr.HasExpr(j.Environment) {
		return ""
	}
	return j.Environment
}

// expandJob 展开一个矩阵变体。
func (x *expander) expandJob(j *Job, combo map[string]any, order []string) (*PlannedJob, error) {
	environment, err := x.environmentName(j, combo)
	if err != nil {
		return nil, fmt.Errorf("environment：%w", err)
	}
	cx := x.jobContexts(j, combo, environment)
	name, err := displayName(j, combo, order, cx.base)
	if err != nil {
		return nil, err
	}
	pj := &PlannedJob{JobID: j.ID, Key: JobKey(j.ID, combo), DisplayName: name, Matrix: combo, MatrixKeys: order, Kind: JobKindNormal}
	spec := newSpec(x.w, j)
	pj.Spec = spec

	if j.If != "" {
		ok, err := expr.EvalCondition(j.If, cx.base)
		if err != nil {
			return nil, fmt.Errorf("if：%w", err)
		}
		if !ok {
			pj.SkipReason = "if 条件为 false"
		}
	}
	if j.Environment != "" {
		spec.Environment = environment
		u, err := expr.Interpolate(j.EnvironmentURL, cx.base)
		if err != nil {
			return nil, fmt.Errorf("environment.url：%w", err)
		}
		spec.EnvironmentURL = u
	}

	var in interpolator
	x.w.Env.Each(func(k, v string) { spec.Env[k] = in.str("env."+k, v, cx.env) })
	j.Env.Each(func(k, v string) { spec.Env[k] = in.str("env."+k, v, cx.env) })
	switch {
	case j.Release != nil:
		pj.Kind = JobKindPublish
		expandRelease(spec, j, cx, &in)
	case j.Image != nil:
		pj.Kind = JobKindImage
		spec.Executor = ExecutorImage
		spec.Platform = PlatformLinuxAny
		im := *j.Image
		spec.Image = &im
	default:
		x.expandExecutor(spec, j, cx, &in)
		expandSigning(spec, j, cx, &in)
		if j.Buildkit {
			spec.BuildkitImage = BuildkitImage
		}
		expandIO(spec, j, cx, &in)
		expandServices(spec, j, cx, &in)
		x.expandSteps(spec, j, cx, &in)
	}
	if err := in.err(); err != nil {
		return nil, err
	}
	spec.Secrets = cx.secrets.Names()
	x.finishJob(pj, j)
	return pj, nil
}

// finishJob 根据展开结果设置 FailReason、SkipReason 和警告。
func (x *expander) finishJob(pj *PlannedJob, j *Job) {
	spec := pj.Spec
	// v0 尚未实现的功能：job 判为 failed（PLAN #22）
	if pj.SkipReason == "" {
		pj.FailReason = notImplemented(j)
	}
	if len(spec.Secrets) > 0 && spec.Cache && pj.Kind == JobKindNormal {
		x.warnf(j.Pos, "job %s 引用了 secrets 却没有设置 cache: false；secret 不计入 action key（PLAN #11.3）", j.ID)
	}
	// Bot 规则（PLAN #16.1）：可信的自动 tag push 保留 SenderIsBot，但允许执行 secret 和发布 job（#29.4）
	if x.pc.SenderIsBot && !x.pc.TrustedAutoTag && pj.SkipReason == "" {
		switch {
		case len(spec.Secrets) > 0:
			pj.SkipReason = "Bot 触发的 run 不下发 secret（PLAN #16.1）"
		case pj.Kind == JobKindPublish:
			pj.SkipReason = "Bot 触发的 run 不执行发布（PLAN #16.1）"
		}
	}
}

// displayName 返回 job 的显示名（PLAN #13.3）：name 没有引用 matrix 时在后面加上矩阵取值。
func displayName(j *Job, combo map[string]any, order []string, ctx expr.Context) (string, error) {
	name := j.ID
	nameRefsMatrix := false
	if j.Name != "" {
		n, err := expr.Interpolate(j.Name, ctx)
		if err != nil {
			return "", fmt.Errorf("name：%w", err)
		}
		name = n
		refs, err := expr.StringRefs(j.Name)
		if err != nil {
			return "", fmt.Errorf("name：%w", err)
		}
		nameRefsMatrix = refersTo(refs, ctxMatrix)
	}
	if len(combo) == 0 || nameRefsMatrix {
		return name, nil
	}
	var vals []string
	for _, k := range order {
		if v, ok := combo[k]; ok {
			vals = append(vals, expr.ToString(v))
		}
	}
	return name + " (" + strings.Join(vals, ", ") + ")", nil
}

func sortedCopy(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(l))
}

// firstLine 返回 s 的第一个非空行，超过 maxStepNameLen 个字符时截断（按字符，不会截断多字节字符）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	s, _, _ = strings.Cut(s, "\n")
	if utf8.RuneCountInString(s) <= maxStepNameLen {
		return s
	}
	return string([]rune(s)[:maxStepNameLen]) + "…"
}
