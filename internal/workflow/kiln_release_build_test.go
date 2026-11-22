package workflow

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// release.yml 的 build 使用的 .kiln/scripts/release-build.sh：在临时目录中搭一个
// 模块路径与本仓库相同的最小模块，用真实的 go build 执行脚本，检查传入的每个程序都编译到 dist/<程序名>、
// 版本按 VERSION 注入，以及平台不对、缺前端、版本输出不对、缺参数时失败。

const releaseBuildScript = ".kiln/scripts/release-build.sh"

// releaseBuildMain 是假程序：version 子命令输出 "<程序名> <版本>"（与真实的 kiln-* 相同）。
const releaseBuildMain = `package main

import (
	"fmt"
	"os"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(%q, buildinfo.Version())
	}
}
`

// newReleaseBuildRepo 建一个可以运行脚本的最小仓库：脚本、buildinfo 和 programs 中的每个命令。
// programs 的值是 version 输出中的程序名（通常就是键，用于构造版本输出不对的程序）。
func newReleaseBuildRepo(t *testing.T, programs map[string]string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(releaseBuildScript)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{
		releaseBuildScript:           string(script),
		"go.mod":                     "module github.com/hewenyu/kilnflow.dev\n\ngo 1.22\n",
		"internal/buildinfo/info.go": "package buildinfo\n\nvar version string\n\nfunc Version() string { return version }\n",
	}
	for cmd, name := range programs {
		files["cmd/"+cmd+"/main.go"] = fmt.Sprintf(releaseBuildMain, name)
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runReleaseBuild 在 dir 中执行脚本，env 覆盖 TARGETOS、TARGETARCH、VERSION 等（值为空表示不设置）。
func runReleaseBuild(t *testing.T, dir string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{releaseBuildScript}, args...)...)
	cmd.Dir = dir
	vars := map[string]string{
		"TARGETOS":    runtime.GOOS,
		"TARGETARCH":  runtime.GOARCH,
		"VERSION":     "v1.2.3",
		"CGO_ENABLED": "0",
	}
	maps.Copy(vars, env)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !hasKey(vars, k) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range vars {
		if v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

func requireReleaseBuildTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("没有 %s：%v", tool, err)
		}
	}
}

func TestReleaseBuildScriptBuildsEachProgram(t *testing.T) {
	t.Parallel()
	requireReleaseBuildTools(t)
	programs := map[string]string{"kiln-server": "kiln-server", "kiln-runner": "kiln-runner", "kiln": "kiln"}
	dir := newReleaseBuildRepo(t, programs)
	// kiln-server 内嵌 Dashboard 前端，脚本要求先取到 web.dist
	index := filepath.Join(dir, "internal/server/web/ui/dist/index.html")
	if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReleaseBuild(t, dir, nil, "kiln-server", "kiln-runner", "kiln")
	if err != nil {
		t.Fatalf("脚本失败：%v\n%s", err, out)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "dist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(programs) {
		t.Errorf("dist 中有 %d 个文件，应只有传入的 %d 个程序：%v", len(entries), len(programs), entries)
	}
	for b := range programs {
		bin := filepath.Join(dir, "dist", b)
		st, err := os.Stat(bin)
		if err != nil {
			t.Fatalf("没有编译 %s：%v\n%s", b, err, out)
		}
		if st.Mode()&0o111 == 0 {
			t.Errorf("dist/%s 不可执行：%v", b, st.Mode())
		}
		got, err := exec.Command(bin, "version").Output()
		if err != nil {
			t.Fatal(err)
		}
		if want := b + " v1.2.3\n"; string(got) != want {
			t.Errorf("dist/%s version 输出 %q，应为 %q（VERSION 注入到 buildinfo）", b, got, want)
		}
	}
}

// 不内嵌前端的程序（kiln CLI）不需要 web.dist，只编译传入的程序。
func TestReleaseBuildScriptWithoutWeb(t *testing.T) {
	t.Parallel()
	requireReleaseBuildTools(t)
	dir := newReleaseBuildRepo(t, map[string]string{"kiln": "kiln", "kiln-runner": "kiln-runner"})
	out, err := runReleaseBuild(t, dir, nil, "kiln")
	if err != nil {
		t.Fatalf("脚本失败：%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "kiln")); err != nil {
		t.Fatalf("没有编译 kiln：%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "dist", "kiln-runner")); !os.IsNotExist(err) {
		t.Errorf("没有传入的 kiln-runner 不应编译：%v", err)
	}
}

func TestReleaseBuildScriptFailures(t *testing.T) {
	t.Parallel()
	requireReleaseBuildTools(t)
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "darwin"
	}
	tests := []struct {
		name     string
		programs map[string]string // 仓库中的命令 → version 输出中的程序名
		env      map[string]string
		args     []string
		want     string // 输出中应包含的原因
	}{
		{"缺前端", map[string]string{"kiln-server": "kiln-server"}, nil, []string{"kiln-server"}, "没有取到 Dashboard 前端"},
		{"目标系统不是本机", map[string]string{"kiln-runner": "kiln-runner"}, map[string]string{"TARGETOS": otherOS}, []string{"kiln-runner"}, "必须在 " + otherOS + "/"},
		{"目标架构不是本机", map[string]string{"kiln-runner": "kiln-runner"}, map[string]string{"TARGETARCH": "riscv64"}, []string{"kiln-runner"}, "必须在 " + runtime.GOOS + "/riscv64"},
		{"version 输出的程序名不对", map[string]string{"kiln-runner": "kiln"}, nil, []string{"kiln-runner"}, "version 输出 kiln v1.2.3，应为 kiln-runner v1.2.3"},
		{"程序不存在", map[string]string{"kiln-runner": "kiln-runner"}, nil, []string{"kiln-nope"}, "kiln-nope"},
		{"没有参数", nil, nil, nil, "用法"},
		{"没有 VERSION", map[string]string{"kiln-runner": "kiln-runner"}, map[string]string{"VERSION": ""}, []string{"kiln-runner"}, "VERSION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newReleaseBuildRepo(t, tt.programs)
			out, err := runReleaseBuild(t, dir, tt.env, tt.args...)
			if err == nil {
				t.Fatalf("应失败，实际成功：\n%s", out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("输出应包含 %q：\n%s", tt.want, out)
			}
		})
	}
}
