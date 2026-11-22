package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 要与另一个进程（libcurl、git）打开同一个文件的路径按原始分量拼接，由文件系统解析符号链接和 ..（joinRaw）。布局：
// root/w 是传输的工作目录，root/w/link 指向 root/far/inner，所以 link/../x 实际是 root/far/x；按词法规整会变成
// root/w/x（诱饵）。每个文件写一个无敏感信息的标记，比较检查器选中的文件与按原始路径打开的文件是不是同一个。
type rawPathLayout struct {
	root, w, far string
}

func newRawPathLayout(t *testing.T) rawPathLayout {
	t.Helper()
	root := t.TempDir()
	l := rawPathLayout{root: root, w: filepath.Join(root, "w"), far: filepath.Join(root, "far")}
	for _, d := range []string{l.w, filepath.Join(l.far, "inner")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(l.far, "inner"), filepath.Join(l.w, "link")); err != nil {
		t.Fatal(err)
	}
	return l
}

// marker 读文件 p 的内容（p 原样交给文件系统解析）。
func marker(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s：%v", p, err)
	}
	return strings.TrimSpace(string(b))
}

// netrc 的定位（NETRC 的相对 / 绝对路径、HOME）：检查的就是 libcurl 按原始路径读的那个文件；只有真正被读的文件里有
// github.com 时报告冲突，诱饵里有时不报告。与 kiln-orca 的 gitops.test.ts 中同名用例相同的一组情形。
func TestNetrcFilesRawPath(t *testing.T) {
	t.Parallel()
	l := newRawPathLayout(t)
	mustWrite(t, filepath.Join(l.far, "n"), "machine github.com login real")
	mustWrite(t, filepath.Join(l.w, "n"), "machine example.com login decoy")
	mustWrite(t, filepath.Join(l.far, ".netrc"), "machine github.com login real-home")
	mustWrite(t, filepath.Join(l.w, ".netrc"), "machine example.com login decoy-home")
	cases := []struct {
		name, want string // want 是 libcurl 按原始路径读的文件（相对路径相对传输的工作目录 l.w）
		env        []string
	}{
		{"相对的 NETRC", l.w + "/link/../n", []string{"NETRC=link/../n", "HOME="}},
		{"绝对的 NETRC", l.w + "/link/../n", []string{"NETRC=" + l.w + "/link/../n", "HOME="}},
		{"HOME", l.w + "/link/../.netrc", []string{"HOME=" + l.w + "/link/.."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := marker(t, tc.want)
			var got []string
			for _, f := range netrcFiles(l.w, tc.env) {
				if strings.HasPrefix(f, l.root) {
					got = append(got, f)
				}
			}
			if len(got) != 1 {
				t.Fatalf("应只有一个这次布局中的 netrc：%v", got)
			}
			if m := marker(t, got[0]); m != want {
				t.Errorf("检查的是 %s（%q），libcurl 读的是 %q", got[0], m, want)
			}
			// 真正被读的文件里有 github.com：报告冲突（不会因为看了诱饵而漏报）
			c := netrcConflicts("github.com", l.w, tc.env, gitAuthQueryTimeout)
			if !slices.ContainsFunc(c, func(c gitAuthConflict) bool { return strings.HasPrefix(c.source, l.root) }) {
				t.Errorf("真正被读的 netrc 中有 github.com，应报告冲突：%+v", c)
			}
		})
	}
}

// git exec 镜像中的符号链接、传输包装找真实的远程助手：都指向 git --exec-path 原样给出的目录里的文件。
func TestGitExecRawPath(t *testing.T) {
	l := newRawPathLayout(t)
	for _, d := range []string{filepath.Join(l.far, "core"), filepath.Join(l.w, "core")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(l.far, "core", "git-remote-https"), "real")
	mustWrite(t, filepath.Join(l.far, "core", "git-upload-pack"), "real-pack")
	mustWrite(t, filepath.Join(l.w, "core", "git-remote-https"), "decoy")
	mustWrite(t, filepath.Join(l.w, "core", "git-upload-pack"), "decoy-pack")
	src := l.w + "/link/../core" // git --exec-path 的输出
	mirror := filepath.Join(l.root, "mirror")
	if err := buildGitExecMirror(mirror, src, "/bin/false"); err != nil {
		t.Fatal(err)
	}
	if m := marker(t, filepath.Join(mirror, "git-upload-pack")); m != "real-pack" {
		t.Errorf("镜像中的 git-upload-pack 指向 %q，应为 git --exec-path 给出的目录中的那个", m)
	}
	t.Setenv(gitRealExecEnv, src)
	helper, err := realRemoteHelper("git-remote-https")
	if err != nil {
		t.Fatal(err)
	}
	if m := marker(t, helper); m != "real" {
		t.Errorf("传输包装找到的真实远程助手是 %s（%q），应为 git --exec-path 给出的目录中的那个", helper, m)
	}
}
