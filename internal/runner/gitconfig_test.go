package runner

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// gitConfigCountCases 是 GIT_CONFIG_COUNT 的各种写法（unset 表示没有设置）。
var gitConfigCountCases = []string{
	"unset", "", " ", "0", "1", "2", "3", " 2", "\t2", "\n2", "\v2", "\f2", "\r2", "+2", "-0", "-2", "2 ", "02", "008",
	"00000000000002", "abc", "+", "-", "0x2", "2147483647", "2147483648", "99999999999", "00000000002147483648",
}

// imageConfig 是镜像（或 workflow）已有的三项环境变量配置。
func imageConfig(count string) []string {
	env := []string{
		"GIT_CONFIG_KEY_0=img.a", "GIT_CONFIG_VALUE_0=1", "GIT_CONFIG_KEY_1=img.b", "GIT_CONFIG_VALUE_1=2",
		"GIT_CONFIG_KEY_2=img.c", "GIT_CONFIG_VALUE_2=3",
	}
	if count != "unset" {
		env = append(env, "GIT_CONFIG_COUNT="+count)
	}
	return env
}

// gitEnvBase 是只读环境变量配置的 git 环境（不读全局、系统配置）。
func gitEnvBase(t *testing.T) []string {
	t.Helper()
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
}

// gitRejectsCount 用真实的 git 判断它是否拒绝这个 GIT_CONFIG_COUNT（bogus count / too many entries，而不是缺少某一项）。
func gitRejectsCount(t *testing.T, count string) bool {
	t.Helper()
	cmd := exec.Command("git", "config", "--get-regexp", `^img\.`)
	cmd.Env, cmd.Dir = append(gitEnvBase(t), imageConfig(count)...), t.TempDir()
	out, _ := cmd.CombinedOutput()
	return strings.Contains(string(out), "bogus count") || strings.Contains(string(out), "too many entries")
}

// gitConfigCount、step 启动脚本中的 kiln_git_config 与真实的 git 对同一组 GIT_CONFIG_COUNT 的判定一致：git 接受时从它的值
// 开始追加（镜像原有的项仍可读，Kiln 的项可读），git 拒绝时什么都不改（git 照样失败）；用完的 KILN_GIT_CONFIG_* 都 unset。
func TestGitConfigCountMatchesGit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	script := ociGitConfigScript() +
		`printf 'count=%s\n' "${GIT_CONFIG_COUNT-unset}"; env | grep '^KILN_GIT_CONFIG' || true; git config --get kiln.k || true; git config --get-regexp '^img\.' || true`
	for _, c := range gitConfigCountCases {
		rejects := gitRejectsCount(t, c)
		n, ok := gitConfigCount(strings.TrimPrefix(c, "unset"))
		if c == "unset" {
			n, ok = gitConfigCount("")
		}
		if ok == rejects {
			t.Errorf("GIT_CONFIG_COUNT=%q：git 拒绝=%v，gitConfigCount ok=%v", c, rejects, ok)
			continue
		}
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(append(gitEnvBase(t), imageConfig(c)...), ociGitConfigN+"=1", ociGitConfigKey+"0=kiln.k", ociGitConfigValue+"0=v")
		cmd.Dir = t.TempDir()
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("GIT_CONFIG_COUNT=%q：%v", c, err)
		}
		out := string(b)
		if strings.Contains(out, "KILN_GIT_CONFIG") {
			t.Errorf("GIT_CONFIG_COUNT=%q：KILN_GIT_CONFIG_* 没有 unset：\n%s", c, out)
		}
		if !ok {
			if want := "count=" + c + "\n"; !strings.HasPrefix(out, want) {
				t.Errorf("GIT_CONFIG_COUNT=%q（git 拒绝）：不应改动它，输出：\n%s", c, out)
			}
			continue
		}
		if want := "count=" + strconv.Itoa(n+1) + "\n"; !strings.HasPrefix(out, want) {
			t.Errorf("GIT_CONFIG_COUNT=%q：应从 %d 开始追加，输出：\n%s", c, n, out)
		}
		if n > 3 {
			continue // 镜像只有三项，git 会因缺少某一项而失败，与 Kiln 无关
		}
		if !strings.Contains(out, "\nv\n") {
			t.Errorf("GIT_CONFIG_COUNT=%q：Kiln 的项读不到，输出：\n%s", c, out)
		}
		if got := strings.Count(out, "img."); got != n {
			t.Errorf("GIT_CONFIG_COUNT=%q：镜像原有的 %d 项应仍可读，读到 %d 项：\n%s", c, n, got, out)
		}
	}
}

// host：Kiln 的项接续在已有的环境变量配置之后，对已有的 GIT_CONFIG_COUNT 的判定与 git 相同。
func TestAppendGitConfigMatchesGit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	for _, c := range gitConfigCountCases {
		env := map[string]string{}
		for _, kv := range imageConfig(c) {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		before := len(env)
		n, _ := gitConfigCount(env["GIT_CONFIG_COUNT"]) // 追加之前的值
		appendGitConfig(env, [][2]string{{"kiln.k", "v"}})
		if gitRejectsCount(t, c) {
			if len(env) != before || (c != "unset" && env["GIT_CONFIG_COUNT"] != c) {
				t.Errorf("GIT_CONFIG_COUNT=%q（git 拒绝）：不应改动：%v", c, env)
			}
			continue
		}
		if env["GIT_CONFIG_KEY_"+strconv.Itoa(n)] != "kiln.k" || env["GIT_CONFIG_COUNT"] != strconv.Itoa(n+1) {
			t.Errorf("GIT_CONFIG_COUNT=%q：Kiln 的项应在第 %d 项：%v", c, n, env)
		}
	}
}

// 镜像（或 workflow）已有的 credential helper：Kiln 先用空值清空、再加自己的，最后生效的是 Kiln 的；镜像的其他配置
// 仍可读。用真实的 git 读启动脚本之后的环境。
func TestOCIGitConfigAfterImageHelper(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	env := map[string]string{}
	(&gitAccess{scope: "https://github.com", helper: "!kiln-helper", configInScript: true}).apply(env)
	if _, ok := env["GIT_CONFIG_COUNT"]; ok {
		t.Fatalf("oci 的 git 配置应交给启动脚本，不直接写 GIT_CONFIG_COUNT：%v", env)
	}
	script := ociGitConfigScript() +
		`git config --get-all credential.https://github.com.helper; echo ---; git config --get init.defaultBranch; git config --get credential.https://github.com.useHttpPath`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(gitEnvBase(t), envList(env)...)
	cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.https://github.com.helper", "GIT_CONFIG_VALUE_0=image-helper",
		"GIT_CONFIG_KEY_1=init.defaultBranch", "GIT_CONFIG_VALUE_1=trunk")
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "image-helper\n\n!kiln-helper\n---\ntrunk\ntrue\n"; string(out) != want {
		t.Errorf("输出：\n%q\n期望：\n%q", out, want)
	}
}

// git --exec-path 的输出只去掉末尾的那一个换行：以空格结尾的 exec 目录原样返回，能据此建出镜像。
func TestGitExecPathKeepsTrailingSpace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	core := root + "/core " // 以空格结尾
	if err := os.Mkdir(core, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, core+"/git-upload-pack", "real")
	git := root + "/git"
	mustWrite(t, git, "#!/bin/sh\nprintf '%s\\n' '"+core+"'\n")
	if err := os.Chmod(git, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := gitExecPath(t.Context(), git, "", os.Environ())
	if err != nil || got != core {
		t.Fatalf("gitExecPath = %q, %v；期望 %q", got, err, core)
	}
	mirror := root + "/mirror"
	if err := buildGitExecMirror(mirror, got, "/bin/false"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(mirror + "/git-upload-pack"); err != nil || string(b) != "real" {
		t.Errorf("镜像中的 git-upload-pack：%q %v", b, err)
	}
}
