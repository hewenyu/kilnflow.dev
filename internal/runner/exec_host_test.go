package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// lookPathIn 在 step 的 PATH 中查找命令：跳过目录和不可执行的文件。
func TestLookPathIn(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(a, "tool"), "not executable")
	if err := os.Mkdir(filepath.Join(a, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(b, "tool"), filepath.Join(b, "dir")} {
		mustWrite(t, p, "#!/bin/sh\n")
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := a + ":" + b
	cases := map[string]string{
		"tool":          filepath.Join(b, "tool"), // a 中的不可执行
		"dir":           filepath.Join(b, "dir"),  // a 中的是目录
		"missing":       "missing",                // 找不到时原样返回
		"./rel/command": "./rel/command",          // 带 / 的不查找
	}
	for name, want := range cases {
		if got := lookPathIn(name, path, ""); got != want {
			t.Errorf("lookPathIn(%q) = %q，期望 %q", name, got, want)
		}
	}
}

// host 的 PATH：extra_path 在系统目录之前（PLAN #25.3）。
func TestHostExecBasePath(t *testing.T) {
	t.Parallel()
	if p := (&hostExec{}).basePath(); p != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("basePath = %q", p)
	}
	h := &hostExec{extraPath: []string{"/opt/homebrew/bin", "/usr/local/bin"}}
	if p := h.basePath(); p != "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("basePath = %q", p)
	}
	// 不修改配置中的切片
	if len(h.extraPath) != 2 {
		t.Errorf("extraPath 被修改：%v", h.extraPath)
	}
}

func TestHostExecJobEnv(t *testing.T) {
	t.Parallel()
	proxy := ProxySettings{HTTPProxy: "http://127.0.0.1:3128", HTTPSProxy: "http://127.0.0.1:3129", NoProxy: "localhost"}
	for _, network := range []bool{false, true} {
		env := map[string]string{"LANG": "C.UTF-8"}
		h := &hostExec{network: network, proxy: proxy}
		h.jobEnv(env)
		if env["SHELL"] != "/bin/bash" || env["USER"] == "" {
			t.Errorf("network=%v：env = %v", network, env)
		}
		if runtime.GOOS == "darwin" && env["LANG"] != "en_US.UTF-8" {
			t.Errorf("macOS 上 LANG = %q", env["LANG"])
		}
		// [proxy.host] 只在 network: true 时注入
		if got := env["https_proxy"]; (got == proxy.HTTPSProxy) != network {
			t.Errorf("network=%v：https_proxy = %q", network, got)
		}
		h.lockEnv(env)
		if got := env["HTTPS_PROXY"]; (got == noNetworkProxy) == network {
			t.Errorf("network=%v：lockEnv 后 HTTPS_PROXY = %q", network, got)
		}
	}
	// 没有配置代理时 network: true 不设置任何代理变量
	env := map[string]string{}
	h := &hostExec{network: true}
	h.jobEnv(env)
	h.lockEnv(env)
	for _, pair := range proxyEnvPairs {
		for _, k := range pair {
			if v, ok := env[k]; ok {
				t.Errorf("%s = %q", k, v)
			}
		}
	}
}

// step 脚本放在 TMPDIR 中，执行后删除；输出写到 out。
func TestHostExecRunScriptFile(t *testing.T) {
	t.Parallel()
	d := newTestDirs(t)
	h := &hostExec{d: d}
	var out bytes.Buffer
	code, err := h.run(t.Context(), stepSpec{index: 2, script: `echo "$0"; echo out; echo err >&2`, env: map[string]string{"PATH": "/usr/bin:/bin"}}, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, out.String())
	}
	script := filepath.Join(d.tmp, stepScriptName(2))
	if !strings.Contains(out.String(), script+"\nout\nerr\n") {
		t.Errorf("输出 = %q", out.String())
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("执行后 step 脚本应删除：%v", err)
	}
}
