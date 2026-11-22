package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var jobIDRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// 不支持的字段的说明。
const (
	usesHint          = "Kiln 不运行 marketplace action：actions/checkout 用 inputs 代替，setup-go/setup-node 用 runtimes 代替，actions/cache 不需要（自动缓存），upload/download-artifact 用 outputs/fetch 代替，导入签名证书（import-codesign-certs 等）用 job 的 signing 代替（PLAN #4.5）"
	permissionsHint   = "job 不直接访问 GitHub，发布由 Server 完成（PLAN #4.5）"
	withHint          = "不支持（没有 uses）"
	registryLoginHint = "v1 只支持不需要登录的镜像"
	unsupportedHint   = "不支持"
)

// Parse 解析一个 workflow 文件。返回的 Diags 中有错误时，Workflow 可能不完整。
func Parse(file string, data []byte) (*Workflow, Diags) {
	d := &decoder{file: file}
	wf := &Workflow{File: file, Name: BaseName(file), Env: NewOrderedMap(), jobIndex: map[string]*Job{}}
	if BaseName(filepath.Base(file)) == ReservedName {
		d.diags = append(d.diags, Diag{File: file, Msg: "文件名 " + filepath.Base(file) + " 保留给 Kiln 的 AI 审计，请改用其他文件名"})
	}
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		d.diags = append(d.diags, Diag{File: file, Msg: "YAML 无法解析：" + err.Error(), Line: yamlErrLine(err)})
		return wf, d.diags
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			d.diags = append(d.diags, Diag{File: file, Msg: "YAML 无法解析：" + err.Error(), Line: yamlErrLine(err)})
		} else {
			d.errAt(&extra, "workflow 只能包含一个 YAML 文档")
		}
		return wf, d.diags
	}
	if !d.validateNodes(&root) {
		return wf, d.diags
	}
	top := resolve(&root)
	// 空文件（或只有注释）时 yaml.v3 不生成文档节点，root 是零值节点
	if top == nil || top.Kind == 0 {
		d.diags = append(d.diags, Diag{File: file, Msg: "文件为空"})
		return wf, d.diags
	}
	hasOn, hasJobs := false, false
	d.fields(top, "workflow", handlers{
		"name":        func(_, v *yaml.Node) { wf.Name = d.str(v) },
		"on":          func(_, v *yaml.Node) { hasOn = true; d.parseOn(v, &wf.On) },
		"env":         func(_, v *yaml.Node) { wf.Env = d.strMap(v, "env") },
		"defaults":    func(_, v *yaml.Node) { wf.Defaults = d.parseDefaults(v) },
		"concurrency": func(_, v *yaml.Node) { wf.Concurrency = d.parseConcurrency(v) },
		"jobs":        func(_, v *yaml.Node) { hasJobs = true; d.parseJobs(v, wf) },
	}, map[string]string{
		"permissions": permissionsHint,
		"run-name":    "Kiln 不支持 run-name",
	})
	// YAML 1.1 中 on 可能被解析为 true；yaml.v3 不会，但仍然兼容 "true" 键。
	if !hasOn {
		d.errAt(top, "缺少 on")
	}
	if !hasJobs {
		d.errAt(top, "缺少 jobs")
	}
	d.validate(wf)
	return wf, d.diags
}

func (d *decoder) parseDefaults(v *yaml.Node) Defaults {
	var out Defaults
	d.fields(v, "defaults", handlers{
		"run": func(_, v *yaml.Node) {
			d.fields(v, "defaults.run", handlers{
				"shell":             func(_, v *yaml.Node) { out.Shell = d.str(v) },
				"working-directory": func(_, v *yaml.Node) { out.WorkingDirectory = d.str(v) },
			}, nil)
		},
	}, nil)
	return out
}

func (d *decoder) parseConcurrency(v *yaml.Node) *Concurrency {
	v = resolve(v)
	if isNull(v) {
		return nil
	}
	c := &Concurrency{CancelInProgress: "false", Pos: pos(v)}
	if v.Kind == yaml.ScalarNode {
		c.Group = v.Value
	} else {
		d.fields(v, "concurrency", handlers{
			"group":              func(_, v *yaml.Node) { c.Group = d.str(v) },
			"cancel-in-progress": func(_, v *yaml.Node) { c.CancelInProgress = d.str(v) },
		}, nil)
	}
	if c.Group == "" {
		d.errAt(v, "concurrency.group 不能为空")
	}
	return c
}

// ---------- on ----------

var supportedPRTypes = map[string]bool{"opened": true, "synchronize": true, "reopened": true, "ready_for_review": true}

// parseOn 解析 on：可以是单个事件名、事件名列表，或事件名到配置的映射。
func (d *decoder) parseOn(v *yaml.Node, on *On) {
	v = resolve(v)
	switch v.Kind {
	case yaml.ScalarNode:
		d.parseEvent(on, v.Value, v, nullNode())
	case yaml.SequenceNode:
		for _, c := range v.Content {
			d.parseEvent(on, d.str(c), c, nullNode())
		}
	case yaml.MappingNode:
		d.pairs(v, "on", func(k, body *yaml.Node) { d.parseEvent(on, k.Value, k, body) })
	default:
		d.errAt(v, "on 的格式不正确")
	}
}

func (d *decoder) parseEvent(on *On, name string, k, body *yaml.Node) {
	switch name {
	case EventPush:
		on.Push = d.parsePush(body)
	case EventPullRequest:
		on.PullRequest = d.parsePR(body)
	case EventWorkflowDispatch:
		on.WorkflowDispatch = d.parseDispatch(body)
	case EventSchedule:
		on.Schedule = append(on.Schedule, d.parseSchedule(body)...)
	default:
		d.errAt(k, "不支持的触发事件 %s（支持 push、pull_request、workflow_dispatch、schedule）", name)
	}
}

// exclusive 检查成对的过滤条件（例如 branches 和 branches-ignore）没有同时使用。
func (d *decoder) exclusive(n *yaml.Node, what, field string, a, b []string) {
	if a != nil && b != nil {
		d.errAt(n, "%s 不能同时使用 %s 和 %s-ignore", what, field, field)
	}
}

// patterns 解析一组过滤模式，并检查 glob 语法。
func (d *decoder) patterns(v *yaml.Node) []string {
	l := d.strList(v)
	for _, p := range l {
		if !validFilterPattern(p) {
			d.errAt(v, "无效的匹配模式 %q", p)
		}
	}
	return l
}

func (d *decoder) parsePush(body *yaml.Node) *PushTrigger {
	p := &PushTrigger{}
	d.fields(body, "on.push", handlers{
		"branches":        func(_, v *yaml.Node) { p.Branches = d.patterns(v) },
		"branches-ignore": func(_, v *yaml.Node) { p.BranchesIgnore = d.patterns(v) },
		"tags":            func(_, v *yaml.Node) { p.Tags = d.patterns(v) },
		"tags-ignore":     func(_, v *yaml.Node) { p.TagsIgnore = d.patterns(v) },
		"paths":           func(_, v *yaml.Node) { p.Paths = d.patterns(v) },
		"paths-ignore":    func(_, v *yaml.Node) { p.PathsIgnore = d.patterns(v) },
	}, nil)
	d.exclusive(body, "on.push", "branches", p.Branches, p.BranchesIgnore)
	d.exclusive(body, "on.push", "tags", p.Tags, p.TagsIgnore)
	d.exclusive(body, "on.push", "paths", p.Paths, p.PathsIgnore)
	return p
}

func (d *decoder) parsePR(body *yaml.Node) *PRTrigger {
	p := &PRTrigger{}
	d.fields(body, "on.pull_request", handlers{
		"branches":        func(_, v *yaml.Node) { p.Branches = d.patterns(v) },
		"branches-ignore": func(_, v *yaml.Node) { p.BranchesIgnore = d.patterns(v) },
		"paths":           func(_, v *yaml.Node) { p.Paths = d.patterns(v) },
		"paths-ignore":    func(_, v *yaml.Node) { p.PathsIgnore = d.patterns(v) },
		"types": func(_, v *yaml.Node) {
			p.Types = d.strList(v)
			for _, t := range p.Types {
				if !supportedPRTypes[t] {
					d.errAt(v, "on.pull_request.types 只支持 opened、synchronize、reopened、ready_for_review，不支持 %s", t)
				}
			}
		},
	}, nil)
	d.exclusive(body, "on.pull_request", "branches", p.Branches, p.BranchesIgnore)
	d.exclusive(body, "on.pull_request", "paths", p.Paths, p.PathsIgnore)
	return p
}

func (d *decoder) parseDispatch(body *yaml.Node) *DispatchTrigger {
	wd := &DispatchTrigger{}
	d.fields(body, "on.workflow_dispatch", handlers{
		"inputs": func(_, v *yaml.Node) {
			d.pairs(v, "on.workflow_dispatch.inputs", func(k, v *yaml.Node) {
				wd.Inputs = append(wd.Inputs, d.parseDispatchInput(k, v))
			})
		},
	}, nil)
	return wd
}

func (d *decoder) parseDispatchInput(k, v *yaml.Node) DispatchInput {
	in := DispatchInput{Name: k.Value, Type: InputString}
	d.fields(v, "input "+in.Name, handlers{
		"description": func(_, v *yaml.Node) { in.Description = d.str(v) },
		"type":        func(_, v *yaml.Node) { in.Type = d.str(v) },
		"default":     func(_, v *yaml.Node) { in.Default = d.str(v) },
		"required":    func(_, v *yaml.Node) { in.Required = d.boolv(v) },
		"options":     func(_, v *yaml.Node) { in.Options = d.strList(v) },
	}, nil)
	switch in.Type {
	case InputString, InputBoolean:
	case InputChoice:
		// 没有 options 时 CheckDispatchInputs 无从检查，任何值都会被接受（Actions 对此报错）
		if len(in.Options) == 0 {
			d.errAt(k, "input %s 的类型是 choice，需要 options", in.Name)
		}
	default:
		d.errAt(k, "input %s 的类型只能是 string、boolean、choice", in.Name)
	}
	return in
}

// parseSchedule 返回 on.schedule 中的 cron 表达式。
func (d *decoder) parseSchedule(body *yaml.Node) []string {
	crons := parseSeq(d, body, "on.schedule", func(c *yaml.Node) []string {
		var out []string
		d.fields(c, "on.schedule[]", handlers{
			"cron": func(_, v *yaml.Node) { out = append(out, d.str(v)) },
		}, nil)
		return out
	})
	return slices.Concat(crons...)
}

// ---------- jobs ----------

func (d *decoder) parseJobs(v *yaml.Node, wf *Workflow) {
	v = resolve(v)
	if v.Kind != yaml.MappingNode {
		d.errAt(v, "jobs 应为映射")
		return
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, body := v.Content[i], resolve(v.Content[i+1])
		id := k.Value
		if !jobIDRe.MatchString(id) {
			d.errAt(k, "job ID %q 只能包含字母、数字、_ 和 -，且以字母或 _ 开头", id)
		}
		j := d.parseJob(id, k, body)
		if _, dup := wf.jobIndex[id]; dup {
			d.errAt(k, "job %s 重复", id)
			continue
		}
		wf.Jobs = append(wf.Jobs, j)
		wf.jobIndex[id] = j
	}
	if len(wf.Jobs) == 0 {
		d.errAt(v, "jobs 不能为空")
	}
}

func (d *decoder) parseJob(id string, k, v *yaml.Node) *Job {
	j := &Job{ID: id, Pos: pos(k), Env: NewOrderedMap(), Outputs: NewOrderedMap()}
	hasSteps := false
	d.fields(v, "job "+id, handlers{
		"name":         func(_, v *yaml.Node) { j.Name = d.str(v) },
		"needs":        func(_, v *yaml.Node) { j.Needs = d.strList(v) },
		"if":           func(_, v *yaml.Node) { j.If = d.str(v) },
		"runs-on":      func(_, v *yaml.Node) { j.RunsOn = d.parseRunsOn(v) },
		"pool":         func(_, v *yaml.Node) { j.Pool = d.str(v) },
		"host":         func(_, v *yaml.Node) { j.Host = d.str(v) },
		"container":    func(_, v *yaml.Node) { j.Container = d.parseContainer(v) },
		"vm":           func(_, v *yaml.Node) { j.VM = d.str(v) },
		"runtimes":     func(_, v *yaml.Node) { j.Runtimes = d.strList(v) },
		"inputs":       func(_, v *yaml.Node) { j.Inputs = d.parseInputs(v) },
		"outputs":      func(_, v *yaml.Node) { j.Outputs = d.strMap(v, "outputs") },
		"fetch":        func(_, v *yaml.Node) { j.Fetch = parseSeq(d, v, "fetch", d.parseFetch) },
		"network":      func(_, v *yaml.Node) { j.Network = d.boolv(v) },
		"reproducible": func(_, v *yaml.Node) { j.Reproducible = d.boolv(v) },
		"cache":        func(_, v *yaml.Node) { j.Cache = d.boolp(v) },
		"resources":    func(_, v *yaml.Node) { j.Resources = d.parseResources(v) },
		"toolcache":    func(_, v *yaml.Node) { j.Toolcache = parseSeq(d, v, "toolcache", d.parseToolCache) },
		"services": func(_, v *yaml.Node) {
			d.pairs(v, "services", func(k, v *yaml.Node) { j.Services = append(j.Services, d.parseService(k, v)) })
		},
		"repositories":    func(_, v *yaml.Node) { j.Repositories = d.strList(v) },
		"effects":         func(_, v *yaml.Node) { j.Effects = d.boolv(v) },
		"release":         func(_, v *yaml.Node) { j.Release = d.parseRelease(v) },
		"image":           func(_, v *yaml.Node) { j.Image = d.parseImage(v) },
		"buildkit":        func(_, v *yaml.Node) { j.Buildkit = d.boolv(v) },
		"signing":         func(_, v *yaml.Node) { j.Signing = d.parseSigning(v) },
		"timeout-minutes": func(_, v *yaml.Node) { j.TimeoutMinutes = d.parseBoundedInt(v, "timeout-minutes", maxTimeoutMinutes) },
		"environment":     func(_, v *yaml.Node) { j.Environment, j.EnvironmentURL = d.parseEnvironment(v) },
		"concurrency":     func(_, v *yaml.Node) { j.Concurrency = d.parseConcurrency(v) },
		"strategy":        func(_, v *yaml.Node) { d.parseStrategy(v, &j.Strategy) },
		"env":             func(_, v *yaml.Node) { j.Env = d.strMap(v, "env") },
		"defaults":        func(_, v *yaml.Node) { j.Defaults = d.parseDefaults(v) },
		"steps": func(_, v *yaml.Node) {
			hasSteps = true
			j.Steps = parseSeq(d, v, "steps", d.parseStep)
		},
	}, map[string]string{
		"uses":              usesHint,
		"permissions":       permissionsHint,
		"continue-on-error": "job 级 continue-on-error 不支持",
		"secrets":           unsupportedHint,
		"with":              withHint,
	})
	d.checkJobKind(j, k, hasSteps)
	return j
}

// checkJobKind 检查 job 恰好是 steps、release、image 三种之一。
func (d *decoder) checkJobKind(j *Job, k *yaml.Node, hasSteps bool) {
	kinds := 0
	for _, has := range []bool{hasSteps, j.Release != nil, j.Image != nil} {
		if has {
			kinds++
		}
	}
	switch {
	case kinds == 0:
		d.errAt(k, "job %s 需要 steps、release 或 image 之一", j.ID)
	case kinds > 1:
		d.errAt(k, "job %s 的 steps、release、image 只能有一个", j.ID)
	}
	if hasSteps && len(j.Steps) == 0 {
		d.errAt(k, "job %s 的 steps 不能为空", j.ID)
	}
}

func (d *decoder) parseRunsOn(v *yaml.Node) string {
	l := d.strList(v)
	if len(l) != 1 {
		d.errAt(v, "runs-on 应为一个平台，例如 linux/arm64、darwin/arm64")
		return ""
	}
	return l[0]
}

// parseContainer 解析 container：可以直接写镜像，也可以写成 {image: …}。
func (d *decoder) parseContainer(v *yaml.Node) string {
	if v.Kind != yaml.MappingNode {
		return d.str(v)
	}
	var image string
	d.fields(v, "container", handlers{
		"image": func(_, v *yaml.Node) { image = d.str(v) },
	}, map[string]string{
		"env":         "请把 env 写在 job 级",
		"options":     unsupportedHint,
		"volumes":     unsupportedHint,
		"credentials": registryLoginHint,
		"ports":       "不需要",
	})
	return image
}

// parseInputs 解析 inputs；写了空列表时返回非 nil 的空切片（与"没有写"区分）。
func (d *decoder) parseInputs(v *yaml.Node) []string {
	l := d.strList(v)
	if l == nil {
		l = []string{}
	}
	for _, p := range l {
		if !validInputPattern(p) {
			d.errAt(v, "inputs 中有无效的匹配模式 %q", p)
		}
	}
	return l
}

func (d *decoder) parseResources(v *yaml.Node) Resources {
	var r Resources
	d.fields(v, "resources", handlers{
		"cpu": func(_, v *yaml.Node) {
			f, err := strconv.ParseFloat(d.str(v), 64)
			if err != nil || f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
				d.errAt(v, "resources.cpu 应为正数")
			}
			r.CPU = f
		},
		"memory": func(_, v *yaml.Node) {
			r.Memory = d.str(v)
			if _, err := ParseMemory(r.Memory); err != nil {
				d.errAt(v, "resources.memory：%v", err)
			}
		},
	}, nil)
	return r
}

// maxTimeoutMinutes 是 job 超时的上限：24 小时。超过这个值几乎肯定是写错了（把秒当成分钟之类）。
const maxTimeoutMinutes = 24 * 60

// parseBoundedInt 解析 1 到 limit 之间的整数（0、负数和过大都报错）。
func (d *decoder) parseBoundedInt(v *yaml.Node, what string, limit int) int {
	n := resolve(v)
	if n == nil || isNull(n) {
		d.errAt(v, "%s 应在 1 到 %d 之间", what, limit)
		return 0
	}
	x, err := strconv.Atoi(n.Value)
	if err != nil {
		d.errAt(n, "应为整数")
		return 0
	}
	if x <= 0 || x > limit {
		d.errAt(n, "%s 应在 1 到 %d 之间", what, limit)
	}
	return x
}

func (d *decoder) parseToolCache(c *yaml.Node) ToolCache {
	var tc ToolCache
	d.fields(c, "toolcache[]", handlers{
		"name": func(_, v *yaml.Node) { tc.Name = d.str(v) },
		"env":  func(_, v *yaml.Node) { tc.Env = d.str(v) },
	}, nil)
	if tc.Name == "" || tc.Env == "" {
		d.errAt(c, "toolcache 的每一项都需要 name 和 env")
		return tc
	}
	if !ValidToolCacheName(tc.Name) {
		d.errAt(c, "toolcache 的 name %q 无效（只能包含字母、数字、点、下划线和连字符，不能以点开头或包含 ..）", tc.Name)
	}
	if !ValidToolCacheEnv(tc.Env) {
		d.errAt(c, "toolcache 的 env %q 无效或会覆盖 Runner 保留的环境变量（如 PATH、HOME、LD_*、GIT_*、KILN_*）", tc.Env)
	}
	return tc
}

func (d *decoder) parseService(k, v *yaml.Node) Service {
	s := Service{Name: k.Value, Env: NewOrderedMap()}
	if !ValidServiceName(s.Name) {
		d.errAt(k, "services 的服务名 %q 无效（只能包含字母、数字、点、下划线和连字符，不能包含 ..）", s.Name)
	}
	d.fields(v, "services."+s.Name, handlers{
		"image":   func(_, v *yaml.Node) { s.Image = d.str(v) },
		"env":     func(_, v *yaml.Node) { s.Env = d.strMap(v, "env") },
		"command": func(_, v *yaml.Node) { s.Command = d.strList(v) },
	}, map[string]string{
		"ports":       "不需要，job 通过服务名访问",
		"options":     unsupportedHint,
		"volumes":     unsupportedHint,
		"credentials": registryLoginHint,
	})
	if s.Image == "" {
		d.errAt(k, "services.%s 缺少 image", s.Name)
	}
	return s
}

// parseEnvironment 解析 environment：可以直接写名字，也可以写成 {name: …, url: …}。
func (d *decoder) parseEnvironment(v *yaml.Node) (name, url string) {
	if v.Kind != yaml.MappingNode {
		return d.str(v), ""
	}
	d.fields(v, "environment", handlers{
		"name": func(_, v *yaml.Node) { name = d.str(v) },
		"url":  func(_, v *yaml.Node) { url = d.str(v) },
	}, nil)
	return name, url
}

func (d *decoder) parseStep(v *yaml.Node) Step {
	s := Step{Pos: pos(v), Env: NewOrderedMap()}
	d.fields(v, "step", handlers{
		"id":                func(_, v *yaml.Node) { s.ID = d.str(v) },
		"name":              func(_, v *yaml.Node) { s.Name = d.str(v) },
		"if":                func(_, v *yaml.Node) { s.If = d.str(v) },
		"run":               func(_, v *yaml.Node) { s.Run = d.str(v) },
		"shell":             func(_, v *yaml.Node) { s.Shell = d.str(v) },
		"working-directory": func(_, v *yaml.Node) { s.WorkingDirectory = d.str(v) },
		"env":               func(_, v *yaml.Node) { s.Env = d.strMap(v, "env") },
		"continue-on-error": func(_, v *yaml.Node) { s.ContinueOnError = d.boolv(v) },
	}, map[string]string{
		"uses":            usesHint,
		"with":            withHint,
		"timeout-minutes": "step 级超时不支持，请使用 job 级 timeout-minutes",
	})
	if s.Run == "" {
		d.errAt(v, "step 缺少 run")
	}
	return s
}

func (d *decoder) parseFetch(c *yaml.Node) Fetch {
	f := Fetch{Pos: pos(c)}
	d.fields(c, "fetch[]", handlers{
		"from":   func(_, v *yaml.Node) { f.From = d.str(v) },
		"to":     func(_, v *yaml.Node) { f.To = d.str(v) },
		"matrix": func(_, v *yaml.Node) { f.Matrix = d.strMap(v, "fetch.matrix").Values },
	}, nil)
	if f.From == "" || f.To == "" {
		d.errAt(c, "fetch 的每一项都需要 from 和 to")
	}
	return f
}

func (d *decoder) parseStrategy(v *yaml.Node, s *Strategy) {
	d.fields(v, "strategy", handlers{
		"matrix":       func(_, v *yaml.Node) { d.parseMatrix(v, s) },
		"fail-fast":    func(_, v *yaml.Node) { s.FailFast = d.boolp(v) },
		"max-parallel": func(_, v *yaml.Node) { s.MaxParallel = d.parseBoundedInt(v, "max-parallel", MaxJobs) },
	}, nil)
}

// parseMatrix 解析 strategy.matrix。整个矩阵或某个变量的取值可以是表达式（字符串），在展开时求值。
func (d *decoder) parseMatrix(v *yaml.Node, s *Strategy) {
	if v.Kind == yaml.ScalarNode {
		s.Matrix = v.Value
		return
	}
	if v.Kind != yaml.MappingNode {
		d.errAt(v, "strategy.matrix 应为映射")
		return
	}
	m := map[string]any{}
	seen := map[string]bool{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		key := v.Content[i].Value
		val := resolve(v.Content[i+1])
		if !d.uniqueKey(seen, v.Content[i], "strategy.matrix") {
			continue
		}
		if key != matrixInclude && key != matrixExclude {
			s.MatrixOrder = append(s.MatrixOrder, key)
		}
		switch {
		case val.Kind == yaml.ScalarNode:
			m[key] = val.Value // 表达式，展开时求值
		case val.Kind != yaml.SequenceNode:
			d.errAt(val, "matrix.%s 应为列表", key)
		default:
			if key == matrixInclude {
				s.IncludeOrder = appendIncludeKeys(s.IncludeOrder, val)
			}
			m[key] = toValue(val)
		}
	}
	s.Matrix = m
}

// appendIncludeKeys 把 include 各项中出现的变量名按首次出现的顺序追加到 order。
func appendIncludeKeys(order []string, include *yaml.Node) []string {
	for _, item := range include.Content {
		item = resolve(item)
		if item.Kind != yaml.MappingNode {
			continue
		}
		for k := 0; k+1 < len(item.Content); k += 2 {
			if name := item.Content[k].Value; !slices.Contains(order, name) {
				order = append(order, name)
			}
		}
	}
	return order
}

// memoryUnits 是内存大小的单位，二进制单位（Ki、Mi…）在前，避免被十进制单位的后缀先匹配。
var memoryUnits = []struct {
	suffix string
	mul    int64
}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}

// ParseMemory 解析 4Gi、512Mi、2G 这样的内存大小。
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, u := range memoryUnits {
		num, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil || f <= 0 || math.IsNaN(f) {
			return 0, fmt.Errorf("无效的大小 %q", s)
		}
		v := f * float64(u.mul)
		// float64(math.MaxInt64) 等于 2^63，>= 时转换成 int64 会溢出；不到 1 字节时截断为 0
		if v >= float64(math.MaxInt64) || v < 1 {
			return 0, fmt.Errorf("大小 %q 超出范围", s)
		}
		return int64(v), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("无效的大小 %q（例如 4Gi、512Mi）", s)
	}
	return n, nil
}
