package workflow

import (
	"strings"
	"testing"
)

// needs 引用不存在的 job 和环检测。
func TestCheckNeeds(t *testing.T) {
	cases := []struct {
		name string
		jobs string
		want string
	}{
		{"引用不存在的 job", "  a: { needs: [zz], runs-on: linux/arm64, steps: [{run: x}] }", "x.yml:3:3: 错误：job a 的 needs 引用了不存在的 job zz"},
		{"依赖自己", "  a: { needs: a, runs-on: linux/arm64, steps: [{run: x}] }", "x.yml: 错误：needs 构成了环：a → a"},
		{"三个 job 的环", `  c: { needs: [a], runs-on: linux/arm64, steps: [{run: x}] }
  b: { needs: [c], runs-on: linux/arm64, steps: [{run: x}] }
  a: { needs: [b], runs-on: linux/arm64, steps: [{run: x}] }`, "needs 构成了环：a → b → c → a"},
		{"环不经过起点", `  a: { needs: [b], runs-on: linux/arm64, steps: [{run: x}] }
  b: { needs: [c], runs-on: linux/arm64, steps: [{run: x}] }
  c: { needs: [b], runs-on: linux/arm64, steps: [{run: x}] }`, "needs 构成了环：b → c → b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n"+c.jobs+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("菱形依赖不是环", func(t *testing.T) {
		mustParse(t, `on: push
jobs:
  a: { runs-on: linux/arm64, steps: [{run: x}] }
  b: { needs: [a], runs-on: linux/arm64, steps: [{run: x}] }
  c: { needs: [a], runs-on: linux/arm64, steps: [{run: x}] }
  d: { needs: [b, c], runs-on: linux/arm64, steps: [{run: x}] }
`)
	})
	t.Run("只报告一个环", func(t *testing.T) {
		_, diags := Parse("x.yml", []byte(`on: push
jobs:
  a: { needs: [a], runs-on: linux/arm64, steps: [{run: x}] }
  b: { needs: [b], runs-on: linux/arm64, steps: [{run: x}] }
`))
		if n := strings.Count(diags.Error(), "构成了环"); n != 1 {
			t.Errorf("应只报告一个环，实际 %d 个：\n%s", n, diags.Error())
		}
	})
}

// fetch、release、image 中的 <job>.<output> 引用。
func TestCheckRefs(t *testing.T) {
	const up = `  build: { runs-on: linux/arm64, steps: [{run: x}], outputs: { bin: dist/ } }
  mx: { runs-on: linux/arm64, strategy: { matrix: { os: [a, b] } }, steps: [{run: x}], outputs: { bin: dist/ } }
  img: { needs: [build], image: { base: "a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", files: [{ from: build.bin, to: / }] } }
`
	cases := []struct {
		name string
		job  string
		want string
	}{
		{"没有输出名", "{ needs: [build], runs-on: linux/arm64, fetch: [{ from: build, to: x }], steps: [{run: x}] }", `job j 的 fetch 引用 "build" 不存在（应写成 <job>.<output>）`},
		{"job 不存在", "{ runs-on: linux/arm64, fetch: [{ from: nope.bin, to: x }], steps: [{run: x}] }", `job j 的 fetch 引用 "nope.bin" 不存在`},
		{"输出不存在", "{ needs: [build], runs-on: linux/arm64, fetch: [{ from: build.lib, to: x }], steps: [{run: x}] }", "job build 没有名为 lib 的输出"},
		{"上游带矩阵但没有选变体", "{ needs: [mx], runs-on: linux/arm64, fetch: [{ from: mx.bin, to: x }], steps: [{run: x}] }", "job mx 带矩阵，fetch 必须用 matrix: 选出恰好一个变体"},
		{"镜像组装 job 只有 image 输出", "{ needs: [img], runs-on: linux/arm64, fetch: [{ from: img.bin, to: x }], steps: [{run: x}] }", "镜像组装 job img 只有名为 image 的输出"},
		{"release.github.assets 不在 needs 中", "{ release: { github: { assets: [{ from: build.bin }] } } }", "job j 的 release.assets 引用了 build，但它不在 needs 中"},
		{"release.npm", "{ needs: [build], release: { npm: [{ from: build.pkg, name: a, access: public }] } }", "job build 没有名为 pkg 的输出"},
		{"release.runtimes", "{ release: { runtimes: [{ from: build.bin }] } }", "job j 的 release.runtimes 引用了 build，但它不在 needs 中"},
		{"release.images", `{ needs: [img], release: { images: [{ from: img.out, name: docker.io/o/a, tags: [v1], username: u, password: "${{ secrets.P }}" }] } }`, "镜像组装 job img 只有名为 image 的输出"},
		{"image.files", "{ image: { base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, files: [{ from: build.bin, to: / }] } }", "job j 的 image.files 引用了 build，但它不在 needs 中"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n"+up+"  j: "+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("合法的引用", func(t *testing.T) {
		mustParse(t, "on: push\njobs:\n"+up+`  j:
    needs: [mx, img]
    runs-on: linux/arm64
    fetch:
      - { from: mx.bin, to: a/, matrix: { os: a } }
      - { from: img.image, to: b/ }
    steps: [{run: x}]
`)
	})
}

// 执行方式的组合、发布 job 的字段、digest 固定。
func TestCheckExecutorAndDigests(t *testing.T) {
	cases := []struct {
		name string
		job  string
		want string
	}{
		{"container 和 vm 同时写", "{ runs-on: darwin/arm64, container: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, vm: b@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, steps: [{run: x}] }", "job a 不能同时写 container 和 vm"},
		{"services 没有 container", "{ runs-on: linux/arm64, services: { pg: { image: p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa } }, steps: [{run: x}] }", "job a：services 只能用于带 container 的 job"},
		{"发布 job 写 runs-on", "{ runs-on: linux/arm64, release: { github: {} } }", "job a：发布 job 和镜像组装 job 不能写 runs-on、container、runtimes"},
		{"镜像组装 job 写 runtimes", "{ runtimes: [go], image: { base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa } }", "job a：发布 job 和镜像组装 job 不能写 runs-on、container、runtimes"},
		{"发布 job 写 container", "{ container: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, release: { github: {} } }", "发布 job 和镜像组装 job 不能写"},
		{"vm 没有 digest", "{ runs-on: darwin/arm64, vm: macos:15, steps: [{run: x}] }", "job a 的 vm 必须用 digest 固定（例如 image@sha256:…）：macos:15"},
		{"service 镜像没有 digest", "{ runs-on: linux/arm64, container: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: { pg: { image: postgres:17 } }, steps: [{run: x}] }", "job a 的 services.pg.image 必须用 digest 固定"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n  a: "+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("含表达式的镜像在展开时检查", func(t *testing.T) {
		mustParse(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, container: '${{ vars.IMAGE }}', steps: [{run: x}] }\n")
	})
	t.Run("发布 job 不需要 runs-on", func(t *testing.T) {
		mustParse(t, "on: push\njobs:\n  a: { release: { github: {} } }\n")
	})
}

// secrets 只能用在 env 中（发布 job 的凭证字段除外）。
func TestCheckSecrets(t *testing.T) {
	cases := []struct {
		name string
		job  string
		want string
	}{
		{"if", `{ if: "${{ secrets.X == 'a' }}", runs-on: linux/arm64, steps: [{run: x}] }`, "job a 的 if 不能引用 secrets"},
		{"runs-on", `{ runs-on: "${{ secrets.P }}", steps: [{run: x}] }`, "job a 的 runs-on 不能引用 secrets"},
		{"name", `{ name: "${{ secrets.N }}", runs-on: linux/arm64, steps: [{run: x}] }`, "job a 的 name 不能引用 secrets"},
		{"container", `{ container: "${{ secrets.C }}", runs-on: linux/arm64, steps: [{run: x}] }`, "job a 的 container 不能引用 secrets"},
		{"steps.if", `{ runs-on: linux/arm64, steps: [{ run: x, if: "${{ secrets.X }}" }] }`, "job a 的 steps.if 不能引用 secrets"},
		{"steps.name", `{ runs-on: linux/arm64, steps: [{ run: x, name: "${{ secrets.X }}" }] }`, "job a 的 steps.name 不能引用 secrets"},
		{"steps.working-directory", `{ runs-on: linux/arm64, steps: [{ run: x, working-directory: "${{ secrets.X }}" }] }`, "job a 的 steps.working-directory 不能引用 secrets"},
		{"outputs", `{ runs-on: linux/arm64, outputs: { o: "${{ secrets.X }}" }, steps: [{run: x}] }`, "job a 的 outputs.o 不能引用 secrets"},
		{"services.env", `{ runs-on: linux/arm64, container: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: { pg: { image: p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, env: { P: "${{ secrets.X }}" } } }, steps: [{run: x}] }`, "job a 的 services.pg.env 不能引用 secrets"},
		{"通过下标引用", `{ runs-on: linux/arm64, steps: [{ run: "${{ secrets['X'] }}" }] }`, "job a 的 steps.run 不能引用 secrets"},
		{"在函数参数中引用", `{ runs-on: linux/arm64, steps: [{ run: "${{ format('{0}', secrets.X) }}" }] }`, "job a 的 steps.run 不能引用 secrets"},
		{"表达式语法错误", `{ runs-on: linux/arm64, steps: [{ run: "${{ (1 }}" }] }`, "job a 的 steps.run：表达式"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n  a: "+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("env 中可以引用 secrets", func(t *testing.T) {
		mustParse(t, `on: push
env: { W: "${{ secrets.W }}" }
jobs:
  a:
    runs-on: linux/arm64
    env: { J: "${{ secrets.J }}" }
    steps: [{ run: x, env: { S: "${{ secrets.S }}" } }]
`)
	})
}
