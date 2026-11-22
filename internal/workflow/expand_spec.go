package workflow

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
)

// job 没有写对应字段时的默认值。
const (
	defaultCPU         = 1
	defaultMemoryBytes = 1 << 30
	// DefaultTimeoutMinutes 是 job 没有写 timeout-minutes 时的超时（分钟）；Server 的超时检查也按它兜底。
	DefaultTimeoutMinutes = 60
	defaultShell          = "bash"
	// maxStepNameLen 是用 run 的第一行作为 step 名字时保留的最大字符数。
	maxStepNameLen = 80
)

// newSpec 返回填好默认值和不需要求值的字段的 Spec。
func newSpec(w *Workflow, j *Job) *Spec {
	spec := &Spec{
		Workflow:       w.File,
		JobID:          j.ID,
		Network:        j.Network,
		Reproducible:   j.Reproducible,
		Cache:          true,
		Effects:        j.Effects,
		CPU:            defaultCPU,
		MemoryBytes:    defaultMemoryBytes,
		Toolcache:      j.Toolcache,
		Repositories:   sortedCopy(j.Repositories),
		TimeoutMinutes: DefaultTimeoutMinutes,
		Environment:    j.Environment,
		Pool:           j.Pool,
		Host:           j.Host,
		FailFast:       true,
		MaxParallel:    j.Strategy.MaxParallel,
		Env:            map[string]string{},
	}
	if j.Strategy.FailFast != nil {
		spec.FailFast = *j.Strategy.FailFast
	}
	if j.Cache != nil {
		spec.Cache = *j.Cache
	}
	if j.TimeoutMinutes > 0 {
		spec.TimeoutMinutes = j.TimeoutMinutes
	}
	if j.Resources.CPU > 0 {
		spec.CPU = j.Resources.CPU
	}
	if j.Resources.Memory != "" {
		// 格式错误已在解析时报告
		if m, err := ParseMemory(j.Resources.Memory); err == nil {
			spec.MemoryBytes = m
		}
	}
	if j.Effects {
		spec.Cache = false
	}
	return spec
}

// expandRelease 展开发布 job：由 Server 执行，不缓存。
// release.images 的 name、tags、floating-tags 在规划时求值（可以引用 vars、github、matrix）；
// username、password 还可以引用 secrets（PLAN #4.7），规划时保留占位符，由 Server 发布时解密替换。
func expandRelease(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	spec.Executor = ExecutorServer
	spec.Platform = PlatformServer
	spec.Cache = false
	r := *j.Release
	// release.npm 的 name、package、dist-tag 由 Server 发布时按上游变体求值（与 asset 的 name 相同）；
	// 凭据来自账号级 npm 配置，不引用 secret
	r.Images = make([]ImageTarget, len(j.Release.Images))
	for i, t := range j.Release.Images {
		t.Name = in.str("release.images.name", t.Name, cx.run)
		t.Tags = in.strs("release.images.tags", t.Tags, cx.run)
		t.FloatingTags = in.strs("release.images.floating-tags", t.FloatingTags, cx.run)
		t.Username = in.str("release.images.username", t.Username, cx.env)
		t.Password = in.str("release.images.password", t.Password, cx.env)
		r.Images[i] = t
	}
	r.Uploads = make([]UploadTarget, len(j.Release.Uploads))
	for i, t := range j.Release.Uploads {
		t.URL = in.str("release.uploads.url", t.URL, cx.run)
		if err := upload.CheckURL(t.URL); err != nil && expr.HasExpr(j.Release.Uploads[i].URL) {
			in.fail(fmt.Errorf("release.uploads.url：%w", err))
		}
		t.Name = in.str("release.uploads.name", t.Name, cx.run)
		if t.Name == "" {
			if u, err := url.Parse(t.URL); err == nil {
				t.Name = u.Hostname()
			}
		}
		t.Token = in.str("release.uploads.token", t.Token, cx.env)
		r.Uploads[i] = t
	}
	// 展开后复检重复的 url：表达式可能把静态写法绕过 parse 阶段的检查。两边都是字面量时 parse 已报告过。
	seenURL := map[string]int{}
	for i, t := range r.Uploads {
		if t.URL == "" {
			continue
		}
		if prev, ok := seenURL[t.URL]; ok {
			if expr.HasExpr(j.Release.Uploads[i].URL) || expr.HasExpr(j.Release.Uploads[prev].URL) {
				in.fail(fmt.Errorf("release.uploads 中 url %s 重复（展开后）", t.URL))
			}
			continue
		}
		seenURL[t.URL] = i
	}
	if len(r.Uploads) == 0 {
		r.Uploads = nil
	}
	spec.Release = &r
}

// expandExecutor 确定平台和执行方式，并检查 runtime。
func (x *expander) expandExecutor(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	before := len(in.errs)
	spec.Platform = in.str("runs-on", j.RunsOn, cx.run)
	platformOK := len(in.errs) == before
	if platformOK && !ValidPlatform(spec.Platform) {
		in.fail(fmt.Errorf("runs-on %q 不是已知平台（例如 darwin/arm64、linux/arm64、linux/amd64）", spec.Platform))
		platformOK = false
	}
	spec.Container = in.str("container", j.Container, cx.run)
	spec.VM = in.str("vm", j.VM, cx.run)
	executor := ExecutorHost
	switch {
	case spec.Container != "":
		executor = ExecutorOCI
		if !hasPinnedDigest(spec.Container) {
			in.fail(fmt.Errorf("container 必须用 digest 固定：%s", spec.Container))
		}
	case spec.VM != "":
		executor = ExecutorTart
	}
	spec.Executor = executor
	spec.Runtimes = j.Runtimes
	if platformOK && x.pc.CheckRuntime != nil {
		for _, rt := range j.Runtimes {
			if err := x.pc.CheckRuntime(rt, spec.Platform); err != nil {
				in.fail(err)
			}
		}
	}
}

// expandIO 展开 inputs、outputs 和 fetch 的目标路径（fetch 的上游在 resolveDeps 中填写）。
func expandIO(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	if j.Inputs == nil {
		spec.InputsDefault = true
	} else {
		for _, p := range j.Inputs {
			spec.Inputs = append(spec.Inputs, in.str("inputs", p, cx.run))
		}
	}
	j.Outputs.Each(func(k, v string) {
		spec.Outputs = append(spec.Outputs, NamedPath{Name: k, Path: in.str("outputs."+k, v, cx.run)})
	})
	for _, f := range j.Fetch {
		spec.Fetch = append(spec.Fetch, FetchRef{To: in.str("fetch.to", f.To, cx.run)})
	}
}

// expandServices 按名字排序展开 services。
func expandServices(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	svcs := slices.SortedFunc(slices.Values(j.Services), func(a, b Service) int { return cmp.Compare(a.Name, b.Name) })
	for _, s := range svcs {
		ss := ServiceSpec{Name: s.Name, Image: in.str("services.image", s.Image, cx.run), Env: map[string]string{}, Command: s.Command}
		// 展开后的镜像同样必须用 digest 固定：表达式可以把静态写法绕过 parse 阶段的 checkDigests。
		if ss.Image != "" && !hasPinnedDigest(ss.Image) {
			in.fail(fmt.Errorf("services.%s.image 必须用 digest 固定（例如 image@sha256:…）：%s", s.Name, ss.Image))
		}
		s.Env.Each(func(k, v string) { ss.Env[k] = in.str("services.env", v, cx.run) })
		spec.Services = append(spec.Services, ss)
	}
}

// expandSteps 展开 steps：if 为 false 的 step 直接去掉。
func (x *expander) expandSteps(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	shell := cmp.Or(j.Defaults.Shell, x.w.Defaults.Shell, defaultShell)
	wd := cmp.Or(j.Defaults.WorkingDirectory, x.w.Defaults.WorkingDirectory)
	for i, s := range j.Steps {
		if s.If != "" {
			ok, err := expr.EvalCondition(s.If, cx.base)
			if err != nil {
				in.fail(fmt.Errorf("steps[%d].if：%w", i, err))
				continue
			}
			if !ok {
				continue
			}
		}
		st := StepSpec{
			Name:             in.str("steps.name", s.Name, cx.run),
			Run:              in.str("steps.run", s.Run, cx.run),
			Shell:            cmp.Or(s.Shell, shell),
			WorkingDirectory: in.str("steps.working-directory", cmp.Or(s.WorkingDirectory, wd), cx.run),
			ContinueOnError:  s.ContinueOnError,
		}
		if s.Env.Len() > 0 {
			st.Env = map[string]string{}
			s.Env.Each(func(k, v string) { st.Env[k] = in.str("steps.env."+k, v, cx.env) })
		}
		if st.Name == "" {
			st.Name = firstLine(st.Run)
		}
		spec.Steps = append(spec.Steps, st)
	}
}

// notImplemented 返回 v0 尚未实现的功能说明；都已实现时返回空字符串。
func notImplemented(j *Job) string {
	ni := func(what string) string { return what + " 尚未实现（Kiln v0）" }
	switch {
	case j.VM != "":
		return ni("vm（tart executor）")
	case j.Image != nil:
		return ni("镜像组装 job（image）")
	case j.Concurrency != nil:
		return ni("job 级 concurrency")
	}
	r := j.Release
	if r == nil {
		return ""
	}
	switch {
	case r.VerifyReproducible:
		return ni("release.verify-reproducible")
	case len(r.Runtimes) > 0:
		return ni("release.runtimes")
	case !r.HasTarget():
		return "release 中没有任何发布目标"
	}
	for _, t := range r.Images {
		// 与 release.github 一致：v0 中不写 sign 视为 none（PLAN #25.2）
		if t.Sign != "" && t.Sign != SignNone {
			return ni("release.images.sign: " + t.Sign)
		}
	}
	g := r.GitHub
	if g == nil {
		return ""
	}
	switch {
	case g.Sign != "" && g.Sign != SignNone:
		return ni("release.github.sign: " + g.Sign)
	case g.Provenance != "" && g.Provenance != ProvenanceNone:
		return ni("release.github.provenance: " + g.Provenance)
	case g.SBOM != nil && *g.SBOM:
		return ni("release.github.sbom")
	case g.NotesFrom == "commits":
		return ni("release.github.notes.from: commits")
	}
	return ""
}
