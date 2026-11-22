package toolchainctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// preinstall 在 root 下放一个已经装好的 go（只有元数据和 bin/go）：ensure 应直接复用，不访问下载源（测试中没有网络）。
func preinstall(t *testing.T, root, hex string) string {
	t.Helper()
	dir := filepath.Join(root, hex)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"name": "go", "version": "1.27.3", "platform": "darwin/arm64", "digest": "sha256:" + hex,
		"env": map[string]any{
			"PATH":        map[string]any{"prepend_paths": []string{"bin"}},
			"GOROOT":      map[string]any{"path": "."},
			"GOTOOLCHAIN": map[string]any{"value": "local", "is_value": true},
		},
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kiln-runtime.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, args []string, stdin string, o Options) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = Run(context.Background(), args, strings.NewReader(stdin), &out, &errb, o)
	return out.String(), errb.String(), err
}

func TestInfo(t *testing.T) {
	t.Parallel()
	out, _, err := run(t, []string{"info"}, "", Options{})
	var info Info
	if err != nil || json.Unmarshal([]byte(out), &info) != nil || info.Format != Format || info.Version == "" {
		t.Fatalf("info = %q, %v", out, err)
	}
}

// ensure 读 stdin 的工具列表，装到 --root（这里已经装好：复用），stdout 输出目录、复用和环境变量，stderr 是进度。
func TestEnsure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hex := strings.Repeat("a", 64)
	dir := preinstall(t, root, hex)
	in := `{"tools":[{"name":"go","version":"1.27.3","platform":"darwin/arm64","digest":"sha256:` + hex + `"}]}`
	out, stderr, err := run(t, []string{"ensure", "--root", root}, in, Options{})
	if err != nil {
		t.Fatalf("ensure = %v；stderr：%s", err, stderr)
	}
	var got EnsureOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Format != Format || len(got.Tools) != 1 {
		t.Fatalf("输出 = %s", out)
	}
	want := EnsuredTool{Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: "sha256:" + hex, Dir: dir, Reused: true, Millis: got.Tools[0].Millis}
	if *got.Tools[0] != want {
		t.Errorf("工具 = %+v，期望 %+v", got.Tools[0], want)
	}
	wantEnv := &toolchain.Env{Set: map[string]string{"GOROOT": dir, "GOTOOLCHAIN": "local"}, Prepend: map[string][]string{"PATH": {filepath.Join(dir, "bin")}}}
	if !reflect.DeepEqual(got.Env, wantEnv) {
		t.Errorf("环境变量 = %+v，期望 %+v", got.Env, wantEnv)
	}
	if stderr != "go 1.27.3（darwin/arm64）：已安装，直接使用\n" {
		t.Errorf("stderr = %q", stderr)
	}
	// 复用时刷新最后使用时间
	if st, err := os.Stat(dir); err != nil || time.Since(st.ModTime()) > 10*time.Minute {
		t.Errorf("mtime 没有刷新：%v", err)
	}
}

func TestEnsureErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		usage bool
		want  string
	}{
		{name: "缺少 --root", args: []string{"ensure"}, usage: true, want: "需要 --root"},
		{name: "多余的参数", args: []string{"ensure", "--root", root, "x"}, usage: true, want: "多余的参数"},
		{name: "stdin 不是 JSON", args: []string{"ensure", "--root", root}, stdin: "go", want: "stdin 不是工具列表"},
		{name: "不认识的字段", args: []string{"ensure", "--root", root}, stdin: `{"tools":[],"url":"x"}`, want: "stdin 不是工具列表"},
		{name: "不是内置 runtime", args: []string{"ensure", "--root", root}, stdin: `{"tools":[{"name":"python","version":"3","platform":"any","digest":"sha256:` + strings.Repeat("b", 64) + `"}]}`, want: "只支持内置 runtime"},
		{name: "未知的子命令", args: []string{"bogus"}, usage: true, want: "未知的子命令"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, tc.args, tc.stdin, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.want)
			}
			if _, ok := errors.AsType[*UsageError](err); ok != tc.usage {
				t.Errorf("用法错误 = %v，期望 %v", ok, tc.usage)
			}
		})
	}
}

// gc：--keep 的不删；删除失败时输出结果并返回错误。
func TestGC(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	keep := preinstall(t, root, strings.Repeat("a", 64))
	drop := preinstall(t, root, strings.Repeat("b", 64))
	out, _, err := run(t, []string{"gc", "--root", root, "--keep", "sha256:" + strings.Repeat("a", 64), "--max-bytes", "1"}, "", Options{TempRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Format  int              `json:"format"`
		Removed []map[string]any `json:"removed"`
		Kept    int              `json:"kept"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Format != Format || len(res.Removed) != 1 || res.Removed[0]["path"] != drop || res.Kept != 1 {
		t.Fatalf("gc = %s, %v", out, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("--keep 的被删除了：%v", err)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Errorf("超过上限的没有删除：%v", err)
	}

	preinstall(t, root, strings.Repeat("c", 64))
	out, _, err = run(t, []string{"gc", "--root", root, "--max-bytes", "1"}, "", Options{Remove: func(string) error { return errors.New("busy") }})
	if err == nil || !strings.Contains(out, `"failures":[{`) {
		t.Errorf("删除失败时 = %s, %v", out, err)
	}
	if _, _, err := run(t, []string{"gc", "--root", root, "--max-bytes", "-1"}, "", Options{}); err == nil {
		t.Error("--max-bytes 为负数时应报错")
	}
}
