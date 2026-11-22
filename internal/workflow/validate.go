package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// validate 做跨字段的结构校验：needs、输出引用、执行方式、digest 和 secrets 的使用位置。
func (d *decoder) validate(wf *Workflow) {
	d.checkNeeds(wf)
	for _, j := range wf.Jobs {
		d.checkRefs(wf, j)
		d.checkExecutor(j)
		d.checkDigests(j)
		d.checkServerJob(j)
		d.checkSecrets(j)
		d.checkSigning(j)
	}
}

// errJob 报告 job 的错误；p 为零值时使用 job 的位置。
func (d *decoder) errJob(j *Job, p Pos, format string, args ...any) {
	if p.Line == 0 {
		p = j.Pos
	}
	d.diags = append(d.diags, Diag{File: d.file, Line: p.Line, Col: p.Col, Msg: fmt.Sprintf(format, args...)})
}

// checkNeeds 检查 needs 引用的 job 存在且没有环。
func (d *decoder) checkNeeds(wf *Workflow) {
	for _, j := range wf.Jobs {
		for _, n := range j.Needs {
			if wf.jobIndex[n] == nil {
				d.errJob(j, Pos{}, "job %s 的 needs 引用了不存在的 job %s", j.ID, n)
			}
		}
	}
	if cyc := findCycle(wf); cyc != "" {
		d.diags = append(d.diags, Diag{File: d.file, Msg: "needs 构成了环：" + cyc})
	}
}

// checkRefs 检查 fetch、release、image 中的 <job>.<output> 引用。
func (d *decoder) checkRefs(wf *Workflow, j *Job) {
	for _, f := range j.Fetch {
		d.checkRef(wf, j, f.Pos, f.From, "fetch")
		jid, _ := splitRef(f.From)
		if u := wf.jobIndex[jid]; u != nil && u.Strategy.Matrix != nil && len(f.Matrix) == 0 {
			d.errJob(j, f.Pos, "job %s 带矩阵，fetch 必须用 matrix: 选出恰好一个变体", jid)
		}
	}
	if r := j.Release; r != nil {
		// release.github.assets 在解析时已经移到 release.assets
		for _, a := range r.Assets {
			d.checkRef(wf, j, a.Pos, a.From, "release.assets")
		}
		for _, t := range r.Npm {
			d.checkRef(wf, j, t.Pos, t.From, "release.npm")
		}
		for _, t := range r.Images {
			d.checkRef(wf, j, j.Pos, t.From, "release.images")
		}
		for _, t := range r.Runtimes {
			d.checkRef(wf, j, j.Pos, t.From, "release.runtimes")
		}
	}
	if j.Image != nil {
		for _, f := range j.Image.Files {
			d.checkRef(wf, j, j.Pos, f.From, "image.files")
		}
	}
}

// checkRef 检查 ref 指向 needs 中某个 job 的已声明输出。镜像组装 job 只有名为 image 的输出。
func (d *decoder) checkRef(wf *Workflow, j *Job, p Pos, ref, what string) {
	jid, out := splitRef(ref)
	up := wf.jobIndex[jid]
	if up == nil || out == "" {
		d.errJob(j, p, "job %s 的 %s 引用 %q 不存在（应写成 <job>.<output>）", j.ID, what, ref)
		return
	}
	if !slices.Contains(j.Needs, jid) {
		d.errJob(j, p, "job %s 的 %s 引用了 %s，但它不在 needs 中", j.ID, what, jid)
	}
	if up.Image != nil {
		if out != "image" {
			d.errJob(j, p, "镜像组装 job %s 只有名为 image 的输出", jid)
		}
	} else if _, ok := up.Outputs.Values[out]; !ok {
		d.errJob(j, p, "job %s 没有名为 %s 的输出", jid, out)
	}
}

// checkExecutor 检查 runs-on、container、vm、services、buildkit 的组合。
func (d *decoder) checkExecutor(j *Job) {
	if j.Release == nil && j.Image == nil && j.RunsOn == "" {
		d.errJob(j, Pos{}, "job %s 缺少 runs-on", j.ID)
	}
	if j.Container != "" && j.VM != "" {
		d.errJob(j, Pos{}, "job %s 不能同时写 container 和 vm", j.ID)
	}
	if len(j.Services) > 0 && j.Container == "" {
		d.errJob(j, Pos{}, "job %s：services 只能用于带 container 的 job", j.ID)
	}
	if j.Buildkit && j.Container == "" {
		d.errJob(j, Pos{}, "job %s：buildkit 只能用于带 container 的 job", j.ID)
	}
}

// checkServerJob 检查发布 job 和镜像组装 job 没有写只适用于 Runner 的字段。
func (d *decoder) checkServerJob(j *Job) {
	if (j.Release != nil || j.Image != nil) && (j.RunsOn != "" || j.Container != "" || len(j.Runtimes) > 0) {
		d.errJob(j, Pos{}, "job %s：发布 job 和镜像组装 job 不能写 runs-on、container、runtimes", j.ID)
	}
}

// checkDigests 检查镜像都用 digest 固定（含表达式的在展开时检查）。
func (d *decoder) checkDigests(j *Job) {
	check := func(what, ref string) {
		if ref != "" && !expr.HasExpr(ref) && !hasPinnedDigest(ref) {
			d.errJob(j, Pos{}, "job %s 的 %s 必须用 digest 固定（例如 image@sha256:…）：%s", j.ID, what, ref)
		}
	}
	check("container", j.Container)
	check("vm", j.VM)
	for _, s := range j.Services {
		check("services."+s.Name+".image", s.Image)
	}
	if j.Image != nil {
		check("image.base", j.Image.Base)
	}
}

// checkSecrets 检查 secrets 只出现在 env 中（发布 job 的凭证字段除外，PLAN #4.5）。
func (d *decoder) checkSecrets(j *Job) {
	check := func(what, s string) {
		refs, err := expr.StringRefs(s)
		if err != nil {
			d.errJob(j, Pos{}, "job %s 的 %s：%v", j.ID, what, err)
			return
		}
		if refersTo(refs, ctxSecrets) {
			d.errJob(j, Pos{}, "job %s 的 %s 不能引用 secrets；secrets 只能用在 env 中（PLAN #4.5）", j.ID, what)
		}
	}
	check("if", j.If)
	check("runs-on", j.RunsOn)
	check("name", j.Name)
	check("container", j.Container)
	for _, s := range j.Steps {
		check("steps.run", s.Run)
		check("steps.if", s.If)
		check("steps.name", s.Name)
		check("steps.working-directory", s.WorkingDirectory)
	}
	j.Outputs.Each(func(k, v string) { check("outputs."+k, v) })
	if j.Release != nil {
		for _, t := range j.Release.Images {
			check("release.images.name", t.Name)
			for _, tag := range append(slices.Clone(t.Tags), t.FloatingTags...) {
				check("release.images.tags", tag)
			}
			// 密码只能来自 secret，不能明文写在 workflow 中（PLAN #13.8）
			refs, err := expr.StringRefs(t.Password)
			if err != nil {
				d.errJob(j, t.Pos, "job %s 的 release.images.password：%v", j.ID, err)
			} else if t.Password != "" && !refersTo(refs, ctxSecrets) {
				d.errJob(j, t.Pos, "job %s 的 release.images.password 必须引用 secret（例如 ${{ secrets.DOCKERHUB_TOKEN }}），不能直接写密码", j.ID)
			}
		}
		for _, t := range j.Release.Uploads {
			check("release.uploads.name", t.Name)
			check("release.uploads.url", t.URL)
			// token 只能来自 secret（PLAN #13.9）
			refs, err := expr.StringRefs(t.Token)
			if err != nil {
				d.errJob(j, t.Pos, "job %s 的 release.uploads.token：%v", j.ID, err)
			} else if t.Token != "" && !refersTo(refs, ctxSecrets) {
				d.errJob(j, t.Pos, "job %s 的 release.uploads.token 必须引用 secret（例如 ${{ secrets.UPLOAD_TOKEN }}），不能直接写 token", j.ID)
			}
		}
	}
	for _, s := range j.Services {
		s.Env.Each(func(_, v string) { check("services."+s.Name+".env", v) })
	}
}

// findCycle 返回 needs 中的一个环（例如 "a → b → a"），没有环时返回空字符串。
func findCycle(wf *Workflow) string {
	const (
		unvisited = iota
		visiting  // 在当前 DFS 路径上
		done      // 已确认从它出发没有环
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) string
	visit = func(id string) string {
		switch state[id] {
		case visiting:
			i := slices.Index(stack, id)
			return strings.Join(append(slices.Clone(stack[i:]), id), " → ")
		case done:
			return ""
		}
		state[id] = visiting
		stack = append(stack, id)
		if j := wf.jobIndex[id]; j != nil {
			for _, n := range j.Needs {
				if c := visit(n); c != "" {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return ""
	}
	for _, id := range slices.Sorted(maps.Keys(wf.jobIndex)) {
		if c := visit(id); c != "" {
			return c
		}
	}
	return ""
}
