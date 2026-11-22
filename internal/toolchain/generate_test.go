package toolchain

import (
	"context"
	"strings"
	"testing"
)

// 同一目录中版本声明的优先级（PLAN #12.3）：表驱动覆盖每条规则。
func TestDetectDirPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files map[string]string
		want  map[string]declared // 只比较列出的 runtime；值为零值表示不应出现
	}{
		{
			name:  "go.mod 只有 go 行",
			files: map[string]string{"go.mod": "module x\n\ngo 1.24\n"},
			want:  map[string]declared{"go": {Spec: "1.24", Source: "go.mod go"}},
		},
		{
			name:  "toolchain 优先于 go 行",
			files: map[string]string{"go.mod": "module x\ntoolchain go1.24.4\ngo 1.24\n"},
			want:  map[string]declared{"go": {Spec: "1.24.4", Source: "go.mod toolchain"}},
		},
		{
			name:  "go.work 优先于 go.mod",
			files: map[string]string{"go.work": "go 1.24\n\ntoolchain go1.24.4\n\nuse ./a\n", "go.mod": "module x\ngo 1.23\n"},
			want:  map[string]declared{"go": {Spec: "1.24.4", Source: "go.work toolchain"}},
		},
		{
			name:  "go.work 中没有版本时用 go.mod",
			files: map[string]string{"go.work": "use ./a\n", "go.mod": "module x\ngo 1.23\n"},
			want:  map[string]declared{"go": {Spec: "1.23", Source: "go.mod go"}},
		},
		{
			name:  "go.mod 中没有版本",
			files: map[string]string{"go.mod": "module x\n\nrequire example.com/a v1.0.0\n"},
			want:  map[string]declared{"go": {}},
		},
		{
			name:  "devEngines.runtime 为对象时优先于 .node-version",
			files: map[string]string{"package.json": `{"devEngines":{"runtime":{"name":"node","version":">=22"}}}`, ".node-version": "20.1.0"},
			want:  map[string]declared{"node": {Spec: ">=22", Source: "package.json devEngines.runtime"}},
		},
		{
			name:  ".node-version 优先于 .nvmrc",
			files: map[string]string{".node-version": "v22.1.0\n", ".nvmrc": "20\n"},
			want:  map[string]declared{"node": {Spec: "22.1.0", Source: ".node-version"}},
		},
		{
			name:  "空的 .node-version 跳过，使用 .nvmrc",
			files: map[string]string{".node-version": "\n", ".nvmrc": "lts/*\n"},
			want:  map[string]declared{"node": {Spec: "lts/*", Source: ".nvmrc"}},
		},
		{
			name:  "engines.node 最后",
			files: map[string]string{"package.json": `{"engines":{"node":"^20"}}`},
			want:  map[string]declared{"node": {Spec: "^20", Source: "package.json engines.node"}},
		},
		{
			name:  "packageManager 为 bun",
			files: map[string]string{"package.json": `{"packageManager":"bun@1.1.30"}`},
			want:  map[string]declared{"bun": {Spec: "1.1.30", Source: "package.json packageManager"}, "pnpm": {}},
		},
		{
			name:  "packageManager 为 yarn 时忽略",
			files: map[string]string{"package.json": `{"packageManager":"yarn@4.0.0"}`},
			want:  map[string]declared{"pnpm": {}, "bun": {}},
		},
		{
			name:  "devEngines 中的 bun 在没有 packageManager 时生效",
			files: map[string]string{"package.json": `{"devEngines":{"runtime":[{"name":"bun","version":"1.1.0"},{"name":"deno","version":"2"}]}}`},
			want:  map[string]declared{"bun": {Spec: "1.1.0", Source: "package.json devEngines.runtime"}},
		},
		{
			name:  "devEngines.runtime 格式不认识时忽略",
			files: map[string]string{"package.json": `{"devEngines":{"runtime":"node"},"engines":{"node":"20"}}`},
			want:  map[string]declared{"node": {Spec: "20", Source: "package.json engines.node"}},
		},
		{
			name:  "package.json 不是合法 JSON 时忽略",
			files: map[string]string{"package.json": `{`, ".nvmrc": "22"},
			want:  map[string]declared{"node": {Spec: "22", Source: ".nvmrc"}},
		},
		{
			name:  "没有任何文件",
			files: map[string]string{},
			want:  map[string]declared{"go": {}, "node": {}, "pnpm": {}, "bun": {}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := detectDir(writeRepo(t, tc.files), ".", "")
			for name, want := range tc.want {
				if g := got[name]; g.Spec != want.Spec || g.Source != want.Source {
					t.Errorf("%s = %+v，期望 %+v", name, g, want)
				}
			}
		})
	}
}

// 从 go.mod 和 package.json 生成 lockfile：版本解析、各平台的 digest、平台无关的 runtime 只有 any。
func TestGenerate(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	repo := writeRepo(t, map[string]string{
		"go.mod":       "module x\n\ngo 1.24\n",
		"package.json": `{"packageManager":"pnpm@^9","engines":{"node":">=22 <23"}}`,
	})
	needs := map[string][]string{
		"go":   {"linux/amd64", "darwin/arm64/gnu"},
		"node": {"linux/amd64"},
		"pnpm": {"linux/amd64", "darwin/arm64"},
	}
	var logs []string
	lock, err := Generate(context.Background(), repo, needs, nil, func(format string, args ...any) {
		logs = append(logs, format)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*LockEntry{
		"go":   {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/amd64": rel.goLinux.digest, "darwin/arm64": rel.goDarwin.digest}},
		"node": {Name: "node", Version: "22.11.0", Platforms: map[string]string{"linux/amd64": rel.nodeLinux.digest}},
		"pnpm": {Name: "pnpm", Version: "9.12.0", Platforms: map[string]string{anyPlatform: rel.pnpm.digest}},
	}
	assertLock(t, lock, want)
	if len(logs) == 0 {
		t.Error("期望输出进度")
	}
	// lockfile 写出后能原样读回
	back, err := ParseLock(lock.Format())
	if err != nil {
		t.Fatal(err)
	}
	assertLock(t, back, want)
	// pnpm 的 digest 需要下载 tarball 计算；Go 和 Node 的 sha256 来自索引，不下载发行包
	if n := f.hitCount(t, rel.pnpm.url); n != 1 {
		t.Errorf("pnpm tarball 被下载 %d 次，期望 1 次", n)
	}
	for _, u := range []string{rel.goLinux.url, rel.goDarwin.url, rel.nodeLinux.url} {
		if n := f.hitCount(t, u); n != 0 {
			t.Errorf("%s 被下载 %d 次，期望不下载", u, n)
		}
	}
}

func TestGenerateBun(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	repo := writeRepo(t, map[string]string{"package.json": `{"packageManager":"bun@1.1.30"}`})
	lock, err := Generate(context.Background(), repo, map[string][]string{"bun": {"linux/amd64"}}, nil, discardf)
	if err != nil {
		t.Fatal(err)
	}
	assertLock(t, lock, map[string]*LockEntry{
		"bun": {Name: "bun", Version: "1.1.30", Platforms: map[string]string{"linux/amd64": rel.bunLinux.digest}},
	})
}

// 版本没有变化时沿用旧 lockfile 中已有平台的 digest，不再请求上游；新增的平台照常计算。
func TestGenerateReusesOldEntries(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	repo := writeRepo(t, map[string]string{
		"go.mod":       "module x\ntoolchain go1.24.4\n",
		"package.json": `{"packageManager":"pnpm@9.12.0"}`,
	})
	kept := "sha256:" + strings.Repeat("ab", 32)
	old := &Lock{Entries: map[string]*LockEntry{
		"go":   {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/amd64": kept}},
		"pnpm": {Name: "pnpm", Version: "9.11.0", Platforms: map[string]string{anyPlatform: kept}},
	}}
	lock, err := Generate(context.Background(), repo, map[string][]string{
		"go":   {"linux/amd64", "darwin/arm64"},
		"pnpm": {"linux/amd64"},
	}, old, discardf)
	if err != nil {
		t.Fatal(err)
	}
	assertLock(t, lock, map[string]*LockEntry{
		// go 版本相同：linux/amd64 沿用旧值，darwin/arm64 新算
		"go": {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/amd64": kept, "darwin/arm64": rel.goDarwin.digest}},
		// pnpm 版本变化：重新计算
		"pnpm": {Name: "pnpm", Version: "9.12.0", Platforms: map[string]string{anyPlatform: rel.pnpm.digest}},
	})
}

// 按平台发行原生可执行文件的 pnpm 版本按平台锁定；旧 lockfile 中同版本的 any 条目（旧的生成方式）不沿用。
func TestGeneratePnpmNative(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	repo := writeRepo(t, map[string]string{"package.json": `{"packageManager":"pnpm@12.14.0"}`})
	want := map[string]*LockEntry{
		"pnpm": {Name: "pnpm", Version: "12.14.0", Platforms: map[string]string{"linux/arm64": rel.pnpmLinuxArm.digest}},
	}
	needs := map[string][]string{"pnpm": {"linux/arm64/gnu"}}
	lock, err := Generate(context.Background(), repo, needs, nil, discardf)
	if err != nil {
		t.Fatal(err)
	}
	assertLock(t, lock, want)

	old := &Lock{Entries: map[string]*LockEntry{
		"pnpm": {Name: "pnpm", Version: "12.14.0", Platforms: map[string]string{anyPlatform: rel.pnpmWrapper.digest}},
	}}
	if lock, err = Generate(context.Background(), repo, needs, old, discardf); err != nil {
		t.Fatal(err)
	}
	assertLock(t, lock, want)
	// 包装不参与：它的 tarball 从不下载
	if n := f.hitCount(t, rel.pnpmWrapper.url); n != 0 {
		t.Errorf("pnpm 包装被下载 %d 次，期望 0 次", n)
	}
}

func TestGenerateErrors(t *testing.T) {
	f := newFakeUpstream(t)
	registerAll(t, f)
	cases := []struct {
		name  string
		files map[string]string
		needs map[string][]string
		want  string
	}{
		{name: "不是内置 runtime", files: nil, needs: map[string][]string{"deno": {"linux/amd64"}}, want: "不是内置 runtime"},
		{name: "没有版本声明", files: nil, needs: map[string][]string{"go": {"linux/amd64"}}, want: "找不到 runtime go 的版本声明"},
		{name: "版本不存在", files: map[string]string{"go.mod": "module x\ngo 1.21\n"}, needs: map[string][]string{"go": {"linux/amd64"}}, want: `go.mod go = "1.21"`},
		{name: "平台没有发行包", files: map[string]string{"go.mod": "module x\ngo 1.24\n"}, needs: map[string][]string{"go": {"windows/amd64"}}, want: "没有 windows/amd64 平台的 Go 1.24.4 发行包"},
		{name: "Node 不支持的平台", files: map[string]string{".nvmrc": "22"}, needs: map[string][]string{"node": {"windows/arm64"}}, want: "没有 windows/arm64 平台的 Node 发行包"},
		{name: "SHASUMS 中没有该平台", files: map[string]string{".nvmrc": "22"}, needs: map[string][]string{"node": {"linux/arm64"}}, want: "SHASUMS256.txt 中没有 node-v22.11.0-linux-arm64.tar.gz"},
		// 假上游中没有 pnpm 9.11.0 的版本元数据
		{name: "上游 404", files: map[string]string{"package.json": `{"packageManager":"pnpm@9.11.0"}`}, needs: map[string][]string{"pnpm": {"linux/amd64"}}, want: "404"},
		{name: "pnpm 原生发行中没有该平台", files: map[string]string{"package.json": `{"packageManager":"pnpm@12.14.0"}`}, needs: map[string][]string{"pnpm": {"windows/amd64"}}, want: "没有 windows/amd64 平台的 pnpm 发行包"},
		{name: "npm 中没有满足范围的版本", files: map[string]string{"package.json": `{"packageManager":"pnpm@^11"}`}, needs: map[string][]string{"pnpm": {"linux/amd64"}}, want: `找不到满足 "^11" 的 pnpm 版本`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Generate(context.Background(), writeRepo(t, tc.files), tc.needs, nil, discardf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Generate = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

// 上游给出的 sha256 不是 64 位十六进制时，lock 失败，不把无效的 digest 写进 lockfile。
func TestGenerateInvalidUpstreamSHA256(t *testing.T) {
	f := newFakeUpstream(t)
	registerAll(t, f)
	f.set(t, "https://nodejs.org/dist/v22.11.0/SHASUMS256.txt", []byte("abc123  node-v22.11.0-linux-x64.tar.gz\n"))
	repo := writeRepo(t, map[string]string{".nvmrc": "22"})
	_, err := Generate(context.Background(), repo, map[string][]string{"node": {"linux/amd64"}}, nil, discardf)
	if err == nil || !strings.Contains(err.Error(), `sha256 "abc123" 无效`) {
		t.Fatalf("Generate = %v，期望 sha256 无效", err)
	}
}

// pnpm 的 integrity 与下载内容不一致时，lock 失败。
func TestGenerateIntegrityMismatch(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	f.set(t, rel.pnpm.url, []byte("tampered"))
	repo := writeRepo(t, map[string]string{"package.json": `{"packageManager":"pnpm@9.12.0"}`})
	_, err := Generate(context.Background(), repo, map[string][]string{"pnpm": {"linux/amd64"}}, nil, discardf)
	if err == nil || !strings.Contains(err.Error(), "integrity 不一致") {
		t.Fatalf("Generate = %v，期望 integrity 不一致", err)
	}
}

// assertLock 比较 lockfile 的条目。
func assertLock(t *testing.T, got *Lock, want map[string]*LockEntry) {
	t.Helper()
	if len(got.Entries) != len(want) {
		t.Fatalf("lockfile 有 %d 个条目，期望 %d 个：%s", len(got.Entries), len(want), got.Format())
	}
	for name, w := range want {
		g := got.Entries[name]
		if g == nil {
			t.Errorf("缺少 %s", name)
			continue
		}
		if g.Name != w.Name || g.Version != w.Version || len(g.Platforms) != len(w.Platforms) {
			t.Errorf("%s = %+v，期望 %+v", name, g, w)
			continue
		}
		for p, d := range w.Platforms {
			if g.Platforms[p] != d {
				t.Errorf("%s 的 %s = %s，期望 %s", name, p, g.Platforms[p], d)
			}
		}
	}
}
