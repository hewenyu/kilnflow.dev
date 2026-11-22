package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// lint 构造的 github 上下文与 Server 一样包含全部字段：push 时引用 head_ref、PR 时引用 ref_type 都不能误报。
func TestLocalPlansFullGitHubContext(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".kiln", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wf := `on: [push, pull_request]
jobs:
  a:
    runs-on: linux/arm64
    steps:
      - run: echo "${{ github.head_ref }} ${{ github.base_ref }} ${{ github.ref_type }} ${{ github.ref_name }}"
`
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	plans, diags := localPlans(root, workflowFiles(root, nil), nil, false)
	if diags.HasErrors() {
		t.Fatalf("不应报错：%v", diags)
	}
	if len(plans) != 3 {
		t.Fatalf("应按 3 种事件展开，实际 %d", len(plans))
	}
}

const (
	okWorkflow = `on: [push, pull_request]
jobs:
  build:
    runs-on: linux/arm64
    steps:
      - run: go build ./...
`
	goWorkflow = `on: push
jobs:
  build:
    runs-on: linux/arm64
    runtimes: [go]
    steps:
      - run: go build ./...
`
	goLock = `[go]
version = "1.24.4"
"linux/arm64" = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`
)

// localRepo 在临时目录中创建仓库（files 的键是相对路径），切换到该目录并返回它。
// 设置 GIT_CEILING_DIRECTORIES，临时目录位于其他 git 仓库中时也不会向上找到那个仓库。
func localRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	t.Chdir(root)
	return root
}

func TestLint(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		args       []string
		wantCode   int
		wantStdout []string // 期望 stdout 中逐行（按顺序）出现的内容
		wantStderr string
	}{
		{
			name:       "没有 workflow",
			files:      map[string]string{"README.md": "x"},
			wantCode:   exitOK,
			wantStdout: []string{"没有找到 .kiln/workflows/*.yml"},
		},
		{
			name:       "yml 和 yaml 都检查",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/workflows/other.yaml": okWorkflow, ".kiln/workflows/notes.txt": "x"},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 2 个 workflow 没有问题"},
		},
		{
			name:  "解析错误带位置，uses 被拒绝",
			files: map[string]string{".kiln/workflows/ci.yml": "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps:\n      - uses: actions/checkout@v4\n"},
			// 第 6 行的 uses
			wantCode:   exitError,
			wantStdout: []string{".kiln/workflows/ci.yml:6:", "uses"},
			wantStderr: "错误：workflow 检查未通过",
		},
		{
			name: "展开错误在三种事件中只报一次",
			files: map[string]string{".kiln/workflows/ci.yml": `on: [push, pull_request]
jobs:
  a:
    runs-on: linux/arm64
    steps:
      - run: echo ${{ foo.bar }}
  b:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [linux/arm64, plan9/mips]
    steps:
      - run: echo
`},
			wantCode: exitError,
			wantStdout: []string{
				`.kiln/workflows/ci.yml:3:3: 错误：job a：steps.run：表达式 "foo.bar"：上下文 foo 在此处不可用`,
				`.kiln/workflows/ci.yml:7:3: 错误：job b：runs-on "plan9/mips" 不是已知平台`,
			},
			wantStderr: "workflow 检查未通过",
		},
		{
			name:       "使用 runtime 但没有 lockfile",
			files:      map[string]string{".kiln/workflows/ci.yml": goWorkflow},
			wantCode:   exitError,
			wantStdout: []string{"job build：job 使用了 runtime go，但仓库中没有 .kiln/runtimes.lock（运行 kiln runtime lock 生成）"},
		},
		{
			name:       "lockfile 中有 runtime 和平台",
			files:      map[string]string{".kiln/workflows/ci.yml": goWorkflow, toolchain.LockPath: goLock},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name:       "lockfile 中没有这个 runtime",
			files:      map[string]string{".kiln/workflows/ci.yml": strings.Replace(goWorkflow, "[go]", "[go, node]", 1), toolchain.LockPath: goLock},
			wantCode:   exitError,
			wantStdout: []string{"runtime node 不在 .kiln/runtimes.lock 中"},
		},
		{
			name:       "lockfile 中没有这个平台",
			files:      map[string]string{".kiln/workflows/ci.yml": strings.Replace(goWorkflow, "linux/arm64", "darwin/arm64", 1), toolchain.LockPath: goLock},
			wantCode:   exitError,
			wantStdout: []string{"runtime go 在 .kiln/runtimes.lock 中没有平台 darwin/arm64 的条目"},
		},
		{
			name: "AI 任务的工具链在 lockfile 中缺少平台",
			files: map[string]string{
				".kiln/workflows/ci.yml": okWorkflow, toolchain.LockPath: goLock,
				".kiln/agent/config.yml": "version: 1\nrequirements: {tools: [go]}\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n",
			},
			wantCode:   exitError,
			wantStdout: []string{"错误：.kiln/agent/config.yml：requirements.tools 的 go（darwin/arm64）：runtime go 在 .kiln/runtimes.lock 中没有平台 darwin/arm64 的条目"},
			wantStderr: ".kiln/agent/config.yml 的工具链检查未通过（运行 kiln runtime lock）",
		},
		{
			name: "没有 workflow 时也检查 AI 任务的工具链",
			files: map[string]string{
				".kiln/agent/config.yml": "version: 1\nrequirements: {tools: [go]}\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n",
			},
			wantCode:   exitError,
			wantStdout: []string{"仓库中没有 .kiln/runtimes.lock", "没有找到 .kiln/workflows/*.yml"},
			wantStderr: "工具链检查未通过",
		},
		{
			name: "AI 任务的工具链在 lockfile 中都有",
			files: map[string]string{
				".kiln/workflows/ci.yml": okWorkflow, toolchain.LockPath: strings.Replace(goLock, "\"linux/arm64\"", "\"darwin/arm64\"", 1),
				".kiln/agent/config.yml": "version: 1\nrequirements: {tools: [go]}\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n",
			},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name:       "lockfile 损坏",
			files:      map[string]string{".kiln/workflows/ci.yml": goWorkflow, toolchain.LockPath: "[go\n"},
			wantCode:   exitError,
			wantStderr: "错误：.kiln/runtimes.lock：",
		},
		{
			name: "只有警告时通过",
			files: map[string]string{".kiln/workflows/ci.yml": `on: push
jobs:
  a:
    runs-on: linux/arm64
    env:
      TOKEN: ${{ secrets.TOKEN }}
    steps:
      - run: echo "$TOKEN"
`},
			wantCode:   exitOK,
			wantStdout: []string{".kiln/workflows/ci.yml:3:3: 警告：job a 引用了 secrets 却没有设置 cache: false", "✓ 1 个 workflow 没有问题"},
		},
		{
			name:       "合并配置有错误",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/merge.yml": "method: fast\n"},
			wantCode:   exitError,
			wantStdout: []string{"错误：.kiln/merge.yml：method 只能是"},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			name:       "只有合并配置，有错误",
			files:      map[string]string{".kiln/merge.yml": "bogus: 1\n"},
			wantCode:   exitError,
			wantStdout: []string{"错误：.kiln/merge.yml：", "没有找到 .kiln/workflows/*.yml"},
		},
		{
			name:       "合并配置正确",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/merge.yml": "method: rebase\n"},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name:       "合并配置的 branches 不能是空列表",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/merge.yml": "branches: []\n"},
			wantCode:   exitError,
			wantStdout: []string{"错误：.kiln/merge.yml：branches 不能是空列表或 null"},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			// 必需 workflow 的分支过滤把 branches 中的 dev 排除了：这些分支上的 PR 会被合并队列移出
			name: "必需的 workflow 没有覆盖 branches 中的分支",
			files: map[string]string{
				".kiln/workflows/ci.yml": "on:\n  pull_request:\n    branches: [main]\njobs:\n  build:\n    runs-on: linux/arm64\n    steps:\n      - run: go build ./...\n",
				".kiln/merge.yml":        "branches: [main, dev]\nrequired: [ci]\n",
			},
			wantCode:   exitError,
			wantStdout: []string{"错误：.kiln/merge.yml：branches 中的 dev 不会触发必需 workflow ci.yml 的 pull_request"},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			name: "必需的 workflow 覆盖 branches 中的每个分支",
			files: map[string]string{
				".kiln/workflows/ci.yml": "on:\n  pull_request:\n    branches: [main, dev, 'release/**']\njobs:\n  build:\n    runs-on: linux/arm64\n    steps:\n      - run: go build ./...\n",
				".kiln/merge.yml":        "branches: [main, dev]\nrequired: [ci]\n",
			},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name: "required 引用的验证都存在",
			files: map[string]string{
				".kiln/workflows/ci.yml": okWorkflow, ".kiln/agent/config.yml": "version: 1\nagent: codex\nreview:\n  required_platforms: [{ os: macos, arch: arm64 }]\n",
				".kiln/merge.yml": "required: [ci, ci/build, ai-review]\n",
			},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name: "required 引用的验证不存在",
			files: map[string]string{
				".kiln/workflows/ci.yml":      okWorkflow,
				".kiln/workflows/nightly.yml": "on: push\njobs:\n  t:\n    runs-on: linux/arm64\n    steps:\n      - run: make\n",
				".kiln/merge.yml":             "required: [ci/test, release, nightly, ai-review, ai-review/x]\n",
			},
			wantCode: exitError,
			wantStdout: []string{
				"错误：.kiln/merge.yml：required 的 ci/test：ci.yml 中没有 job test",
				"required 的 release：.kiln/workflows/release.yml（或 .yaml）不存在",
				"required 的 nightly：nightly.yml 不在 pull_request 时触发",
				"required 的 ai-review：仓库没有 .kiln/agent/config.yml",
				"required 的 ai-review/x：ai-review 没有 job",
			},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			// 以前没有 workflow 时提前返回，不检查 required
			name:     "没有 workflow 时也检查 required",
			files:    map[string]string{".kiln/merge.yml": "required: [ci/test, ai-review]\n"},
			wantCode: exitError,
			wantStdout: []string{
				"没有找到 .kiln/workflows/*.yml",
				"required 的 ci/test：.kiln/workflows/ci.yml（或 .yaml）不存在",
				"required 的 ai-review：仓库没有 .kiln/agent/config.yml",
			},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			name: "只有 AI 审计时 required 的 ai-review 通过",
			files: map[string]string{
				".kiln/agent/config.yml": "version: 1\nagent: codex\nreview:\n  required_platforms: [{ os: macos, arch: arm64 }]\n",
				".kiln/merge.yml":        "required: [ai-review]\n",
			},
			wantCode:   exitOK,
			wantStdout: []string{"没有找到 .kiln/workflows/*.yml"},
		},
		{
			name:       "ai-review.yml 是保留的文件名",
			files:      map[string]string{".kiln/workflows/ai-review.yml": okWorkflow},
			wantCode:   exitError,
			wantStdout: []string{"保留给 Kiln 的 AI 审计"},
		},
		{
			name: "required 的 ai-review 没有开启 PR 审计",
			files: map[string]string{
				".kiln/workflows/ci.yml": okWorkflow,
				".kiln/agent/config.yml": "version: 1\nagent: codex\nreview:\n  required_platforms: [{ os: macos, arch: arm64 }]\n  triggers: { pull_request: false }\n", ".kiln/merge.yml": "required: [ai-review]\n",
			},
			wantCode:   exitError,
			wantStdout: []string{"required 的 ai-review：.kiln/agent/config.yml 没有开启 PR 审计"},
			wantStderr: ".kiln/merge.yml 检查未通过",
		},
		{
			name:       "指定文件时不检查合并配置",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/merge.yml": "method: fast\n"},
			args:       []string{".kiln/workflows/ci.yml"},
			wantCode:   exitOK,
			wantStdout: []string{"✓ 1 个 workflow 没有问题"},
		},
		{
			name:       "命令行给出的相对路径显示在诊断中",
			files:      map[string]string{"wf/bad.yml": "on: push\njobs: {}\nbogus: 1\n"},
			args:       []string{"wf/bad.yml"},
			wantCode:   exitError,
			wantStdout: []string{"wf/bad.yml:3:1: 错误："},
		},
		{
			name:       "命令行给出的文件不存在",
			files:      map[string]string{},
			args:       []string{"missing.yml"},
			wantCode:   exitError,
			wantStdout: []string{"missing.yml: 错误："},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localRepo(t, tt.files)
			got := runApp(t, "", "", append([]string{"lint"}, tt.args...)...)
			if got.code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stdout = %q，stderr = %q", got.code, tt.wantCode, got.stdout, got.stderr)
			}
			rest := got.stdout
			for _, want := range tt.wantStdout {
				i := strings.Index(rest, want)
				if i < 0 {
					t.Errorf("stdout = %q，应（按顺序）包含 %q", got.stdout, want)
					break
				}
				rest = rest[i+len(want):]
			}
			if !strings.Contains(got.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.wantStderr)
			}
		})
	}
}

// 同一个 workflow 的多个 job（ci/build、ci/test）只按 workflow 报一次分支覆盖问题，不重复刷屏。
func TestLintBranchCoveragePerWorkflow(t *testing.T) {
	localRepo(t, map[string]string{
		".kiln/workflows/ci.yml": "on:\n  pull_request:\n    branches: [main]\njobs:\n  build:\n    runs-on: linux/arm64\n    steps:\n" +
			"      - run: go build ./...\n  test:\n    runs-on: linux/arm64\n    steps:\n      - run: go test ./...\n",
		".kiln/merge.yml": "branches: [main, dev]\nrequired: [ci/build, ci/test]\n",
	})
	got := runApp(t, "", "", "lint")
	if got.code != exitError {
		t.Fatalf("退出码 = %d，期望 %d；stdout = %q", got.code, exitError, got.stdout)
	}
	if n := strings.Count(got.stdout, "branches 中的 dev 不会触发必需 workflow ci.yml"); n != 1 {
		t.Errorf("同一个 workflow 的分支覆盖问题应只报一次，实际 %d 次：\n%s", n, got.stdout)
	}
}

// 同一条诊断不会因为按多种事件展开而重复输出。
func TestLintDedupesDiagnostics(t *testing.T) {
	localRepo(t, map[string]string{".kiln/workflows/ci.yml": `on: [push, pull_request]
jobs:
  a:
    runs-on: linux/arm64
    runtimes: [go]
    steps:
      - run: echo ${{ foo.bar }}
`})
	got := runApp(t, "", "", "lint")
	if got.code != exitError {
		t.Fatalf("退出码 = %d，期望 1", got.code)
	}
	seen := map[string]int{}
	for l := range strings.SplitSeq(strings.TrimSpace(got.stdout), "\n") {
		seen[l]++
	}
	if len(seen) != 2 {
		t.Errorf("应有 2 条不同的诊断（runtime 和表达式），实际：\n%s", got.stdout)
	}
	for l, n := range seen {
		if n != 1 {
			t.Errorf("诊断重复了 %d 次：%s", n, l)
		}
	}
}

// 在 git 仓库的子目录中运行时，检查仓库根目录下的 workflow。
func TestLintFromSubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	root := localRepo(t, map[string]string{".kiln/workflows/ci.yml": okWorkflow, "sub/dir/x.go": "package x"})
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init：%v：%s", err, out)
	}
	t.Chdir(filepath.Join(root, "sub", "dir"))
	got := runApp(t, "", "", "lint")
	if got.code != exitOK || !strings.Contains(got.stdout, "✓ 1 个 workflow 没有问题") {
		t.Errorf("退出码 = %d，stdout = %q，stderr = %q；期望找到仓库根目录的 workflow", got.code, got.stdout, got.stderr)
	}
}

func TestDisplayPath(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	outside := filepath.Join(filepath.Dir(root), "elsewhere.yml")
	tests := []struct {
		name, f, want string
	}{
		{name: "仓库内的绝对路径", f: filepath.Join(root, ".kiln", "workflows", "ci.yml"), want: ".kiln/workflows/ci.yml"},
		{name: "相对当前目录的路径", f: filepath.Join("a", "b.yml"), want: "a/b.yml"},
		{name: "仓库外的文件按原样显示", f: outside, want: filepath.ToSlash(outside)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayPath(root, tt.f); got != tt.want {
				t.Errorf("displayPath(%q) = %q，期望 %q", tt.f, got, tt.want)
			}
		})
	}
}

func TestRuntimeLock(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "workflow 有错误时先修复",
			files:      map[string]string{".kiln/workflows/ci.yml": "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps:\n      - uses: actions/setup-go@v5\n"},
			wantCode:   exitError,
			wantStderr: "错误：workflow 有错误，先修复（kiln lint）",
		},
		{
			name:       "没有 job 使用 runtime",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow},
			wantCode:   exitOK,
			wantStdout: "workflow 中没有 job 使用 runtimes，.kiln/agent/config.yml 也没有 requirements.tools，不需要 lockfile",
		},
		{
			name:       "AI 配置无效",
			files:      map[string]string{".kiln/workflows/ci.yml": okWorkflow, ".kiln/agent/config.yml": "version: 1\nrequirements: {tools: [ruby]}\n"},
			wantCode:   exitError,
			wantStderr: `requirements.tools 中的 "ruby" 不是内置 runtime`,
		},
		{
			name:       "没有 workflow",
			files:      map[string]string{},
			wantCode:   exitOK,
			wantStdout: "不需要 lockfile",
		},
		{
			name:       "找不到版本声明",
			files:      map[string]string{".kiln/workflows/ci.yml": goWorkflow},
			wantCode:   exitError,
			wantStderr: "找不到 runtime go 的版本声明",
		},
		{
			name: "monorepo 中显式指定的声明不存在",
			files: map[string]string{
				".kiln/workflows/ci.yml": goWorkflow, "app/go.mod": "module app\ngo 1.24\n",
				toolchain.ConfigPath: "[declarations]\ngo = \"server/go.mod\"\n",
			},
			wantCode:   exitError,
			wantStderr: `.kiln/runtimes.toml 的 [declarations] go = "server/go.mod"`,
		},
		{
			name:       "不是内置 runtime",
			files:      map[string]string{".kiln/workflows/ci.yml": strings.Replace(goWorkflow, "[go]", "[ruby]", 1)},
			wantCode:   exitError,
			wantStderr: "runtime ruby 不是内置 runtime",
		},
		{
			name:       "已有的 lockfile 损坏",
			files:      map[string]string{".kiln/workflows/ci.yml": goWorkflow, toolchain.LockPath: "[go]\nversion = 1\n"},
			wantCode:   exitError,
			wantStderr: "错误：.kiln/runtimes.lock：",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := localRepo(t, tt.files)
			before, beforeErr := os.ReadFile(filepath.Join(root, toolchain.LockPath))
			got := runApp(t, "", "", "runtime", "lock")
			if got.code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stdout = %q，stderr = %q", got.code, tt.wantCode, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stdout, tt.wantStdout) {
				t.Errorf("stdout = %q，应包含 %q", got.stdout, tt.wantStdout)
			}
			if !strings.Contains(got.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.wantStderr)
			}
			// 失败或不需要时不写入（也不改动已有的）lockfile
			after, afterErr := os.ReadFile(filepath.Join(root, toolchain.LockPath))
			if string(before) != string(after) || errors.Is(beforeErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
				t.Errorf("lockfile 被改动：之前 %q（%v），之后 %q（%v）", before, beforeErr, after, afterErr)
			}
		})
	}
}

func TestRuntimeUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"runtime"}, {"runtime", "update"}} {
		got := runApp(t, "", "", args...)
		if got.code != exitUsage || !strings.Contains(got.stderr, "用法：kiln runtime lock") {
			t.Errorf("%v：退出码 = %d，stderr = %q；期望用法错误", args, got.code, got.stderr)
		}
	}
}

// lockPlatforms：workflow 的 runtimes 与 AI 任务的工具链（requirements.tools，PLAN #33.12）合并为 lockfile 要锁定的平台；
// 只有 AI 配置的仓库同样需要 lockfile，同一个 runtime 的平台取并集、排序、去重。
func TestLockPlatforms(t *testing.T) {
	aiConfig := "version: 1\nrequirements: {tools: [go, node]}\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n"
	tests := []struct {
		name  string
		files map[string]string
		want  map[string][]string
	}{
		{name: "只有 AI 配置", files: map[string]string{".kiln/agent/config.yml": aiConfig}, want: map[string][]string{"go": {"darwin/arm64"}, "node": {"darwin/arm64"}}},
		{name: "只有 workflow", files: map[string]string{".kiln/workflows/ci.yml": goWorkflow}, want: map[string][]string{"go": {"linux/arm64"}}},
		{
			name:  "CI 与 AI 合并",
			files: map[string]string{".kiln/workflows/ci.yml": goWorkflow, ".kiln/agent/config.yml": aiConfig},
			want:  map[string][]string{"go": {"darwin/arm64", "linux/arm64"}, "node": {"darwin/arm64"}},
		},
		{
			name:  "同一平台去重",
			files: map[string]string{".kiln/workflows/ci.yml": strings.Replace(goWorkflow, "linux/arm64", "darwin/arm64", 1), ".kiln/agent/config.yml": aiConfig},
			want:  map[string][]string{"go": {"darwin/arm64"}, "node": {"darwin/arm64"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := localRepo(t, tt.files)
			plans, diags := localPlans(root, workflowFiles(root, nil), nil, false)
			if diags.HasErrors() {
				t.Fatalf("workflow 有错误：%v", diags)
			}
			got, err := lockPlatforms(root, plans)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lockPlatforms = %v, %v，期望 %v", got, err, tt.want)
			}
		})
	}
}
