package toolchain

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// 查找版本声明的规则（PLAN #12.3）：显式指定 > 根目录 > 子目录中的所有声明。
func TestDeclFinder(t *testing.T) {
	t.Parallel()
	monorepo := map[string]string{
		"app/go.mod":              "module app\n\ngo 1.24.0\n\ntoolchain go1.24.4\n",
		"server/go.mod":           "module server\n\ngo 1.24.4\n",
		"app/ui/package.json":     `{"name":"ui"}`,
		"server/web/package.json": `{"name":"web","engines":{"node":">=22"}}`,
		"app/ui/.node-version":    "22.11.0\n",
	}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		out := maps.Clone(base)
		maps.Copy(out, extra)
		return out
	}
	cases := []struct {
		name  string
		files map[string]string
		rt    string
		want  []string // 候选的 Source
		err   string
	}{
		{
			name:  "单模块仓库：只用根目录的声明，不看子目录",
			files: map[string]string{"go.mod": "module x\ngo 1.24\n", "tools/go.mod": "module tools\ngo 1.23\n"},
			rt:    "go",
			want:  []string{"go.mod go"},
		},
		{
			name:  "monorepo：子目录中的所有 go 声明",
			files: monorepo,
			rt:    "go",
			want:  []string{"app/go.mod toolchain", "server/go.mod go"},
		},
		{
			name:  "monorepo：每个目录按自己的优先级取一个 node 声明",
			files: monorepo,
			rt:    "node",
			want:  []string{"app/ui/.node-version", "server/web/package.json engines.node"},
		},
		{
			name: "跳过隐藏目录、node_modules、vendor、testdata",
			files: with(monorepo, map[string]string{
				".claude/skills/x/go.mod":            "module x\ngo 1.20\n",
				"app/ui/node_modules/a/package.json": `{"engines":{"node":"18"}}`,
				"server/vendor/b/go.mod":             "module b\ngo 1.19\n",
				"server/testdata/go.mod":             "module t\ngo 1.18\n",
			}),
			rt:   "go",
			want: []string{"app/go.mod toolchain", "server/go.mod go"},
		},
		{
			name:  "显式指定文件",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\ngo = \"server/go.mod\"\n"}),
			rt:    "go",
			want:  []string{"server/go.mod go"},
		},
		{
			name:  "显式指定目录：按同一目录中的优先级读取",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\nnode = \"app/ui\"\n"}),
			rt:    "node",
			want:  []string{"app/ui/.node-version"},
		},
		{
			name:  "显式指定优先于根目录",
			files: map[string]string{"go.mod": "module x\ngo 1.24\n", "tools/go.mod": "module t\ngo 1.23\n", ConfigPath: "[declarations]\ngo = \"tools/go.mod\"\n"},
			rt:    "go",
			want:  []string{"tools/go.mod go"},
		},
		{
			name:  "显式指定根目录的文件",
			files: map[string]string{"go.mod": "module x\ngo 1.24\n", ConfigPath: "[declarations]\ngo = \"go.mod\"\n"},
			rt:    "go",
			want:  []string{"go.mod go"},
		},
		{
			name:  "显式指定的文件中没有该 runtime 的声明",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\nnode = \"app/ui/package.json\"\n"}),
			rt:    "node",
			err:   `.kiln/runtimes.toml 的 [declarations] node = "app/ui/package.json"：其中没有 node 的版本声明`,
		},
		{
			name:  "显式指定的路径不存在",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\ngo = \"cmd/go.mod\"\n"}),
			rt:    "go",
			err:   `[declarations] go = "cmd/go.mod"：`,
		},
		{
			name:  "显式指定的路径在仓库外",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\ngo = \"../go.mod\"\n"}),
			rt:    "go",
			err:   "应为仓库中的相对路径",
		},
		{
			name:  "不是内置 runtime",
			files: with(monorepo, map[string]string{ConfigPath: "[declarations]\nruby = \"app\"\n"}),
			rt:    "go",
			err:   "[declarations] 中的 ruby 不是内置 runtime",
		},
		{
			name:  "不支持的配置",
			files: with(monorepo, map[string]string{ConfigPath: "tla2tools = \"1.8.0\"\n"}),
			rt:    "go",
			err:   `不支持 "tla2tools"`,
		},
		{
			name:  "哪里都没有声明",
			files: map[string]string{"app/main.go": "package main\n"},
			rt:    "go",
			err:   "找不到 runtime go 的版本声明",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := findSources(t, writeRepo(t, tc.files), tc.rt)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("find = %v, %v，期望错误包含 %q", got, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("find = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 显式指定的不是版本声明文件时报错。
func TestDeclFinderExplicitNotDeclFile(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{"app/go.mod": "module x\ngo 1.24\n", "app/README.md": "x", ConfigPath: "[declarations]\ngo = \"app/README.md\"\n"})
	if _, err := findSources(t, repo, "go"); err == nil || !strings.Contains(err.Error(), "不是版本声明文件") {
		t.Fatalf("find = %v，期望不是版本声明文件", err)
	}
}

// git 仓库中只看跟踪的和未被忽略的文件：构建产物、第三方下载中的声明不参与。
func TestDeclFinderGitIgnored(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{
		".gitignore":                    "target/\n",
		"app/go.mod":                    "module app\ntoolchain go1.24.4\n",
		"app/new/.node-version":         "22\n", // 未跟踪但没有被忽略：参与
		"app/target/third/package.json": `{"engines":{"node":"18"}}`,
		"app/target/third/go.mod":       "module third\ngo 1.20\n",
	})
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init：%v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "add", "app/go.mod").CombinedOutput(); err != nil {
		t.Fatalf("git add：%v %s", err, out)
	}
	for rt, want := range map[string][]string{"go": {"app/go.mod toolchain"}, "node": {"app/new/.node-version"}} {
		got, err := findSources(t, repo, rt)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s：find = %q，期望 %q", rt, got, want)
		}
	}
}

func findSources(t *testing.T, repo, rt string) ([]string, error) {
	t.Helper()
	f, err := newDeclFinder(context.Background(), repo)
	if err != nil {
		return nil, err
	}
	cands, err := f.find(rt)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cands))
	for i, d := range cands {
		out[i] = d.Source
	}
	return out, nil
}

// monorepo：多个声明解析出同一个版本时直接使用；不一致时报歧义并提示怎么显式选择；显式选择后可以生成。
func TestGenerateMonorepo(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	needs := map[string][]string{"go": {"linux/amd64"}, "node": {"linux/amd64"}}

	t.Run("一致", func(t *testing.T) {
		// 1.24 和 1.24.4 都解析为 1.24.4；node 只有一个声明
		repo := writeRepo(t, map[string]string{
			"app/go.mod":           "module app\n\ngo 1.24\n",
			"server/go.mod":        "module server\n\ngo 1.24.0\n\ntoolchain go1.24.4\n",
			"app/ui/package.json":  `{"name":"ui"}`,
			"app/ui/.node-version": "22.11.0\n",
		})
		var logs []string
		lock, err := Generate(context.Background(), repo, needs, nil, func(format string, args ...any) {
			logs = append(logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
		})
		if err != nil {
			t.Fatal(err)
		}
		assertLock(t, lock, map[string]*LockEntry{
			"go":   {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/amd64": rel.goLinux.digest}},
			"node": {Name: "node", Version: "22.11.0", Platforms: map[string]string{"linux/amd64": rel.nodeLinux.digest}},
		})
		want := `go 1.24.4（来自 app/go.mod go "1.24"、server/go.mod toolchain "1.24.4"）`
		if !slices.Contains(logs, want) {
			t.Errorf("进度输出 %q，期望包含 %q", logs, want)
		}
	})

	conflict := map[string]string{
		"app/go.mod":           "module app\ntoolchain go1.24.4\n",
		"server/go.mod":        "module server\ngo 1.24.3\n",
		"app/ui/.node-version": "22.11.0\n",
	}
	t.Run("不一致时报歧义", func(t *testing.T) {
		_, err := Generate(context.Background(), writeRepo(t, conflict), needs, nil, discardf)
		if err == nil {
			t.Fatal("期望报歧义")
		}
		for _, want := range []string{
			"runtime go 的版本声明不一致",
			`app/go.mod toolchain = "1.24.4" → 1.24.4`,
			`server/go.mod go = "1.24.3" → 1.24.3`,
			"[declarations]\n  go = \"app/go.mod\"",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误 %q 中没有 %q", err, want)
			}
		}
	})

	t.Run("显式选择", func(t *testing.T) {
		files := map[string]string{ConfigPath: "[declarations]\ngo = \"app/go.mod\"\n"}
		maps.Copy(files, conflict)
		lock, err := Generate(context.Background(), writeRepo(t, files), map[string][]string{"go": {"linux/amd64"}}, nil, discardf)
		if err != nil {
			t.Fatal(err)
		}
		if got := lock.Entries["go"].Version; got != "1.24.4" {
			t.Errorf("go = %s，期望 1.24.4（app/go.mod）", got)
		}
	})

	t.Run("其中一个声明解析失败", func(t *testing.T) {
		repo := writeRepo(t, map[string]string{"app/go.mod": "module app\ngo 1.24\n", "server/go.mod": "module s\ngo 1.21\n"})
		_, err := Generate(context.Background(), repo, map[string][]string{"go": {"linux/amd64"}}, nil, discardf)
		if err == nil || !strings.Contains(err.Error(), `server/go.mod go = "1.21"`) {
			t.Fatalf("Generate = %v，期望指出 server/go.mod", err)
		}
	})
}
