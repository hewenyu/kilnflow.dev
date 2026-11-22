package toolchain

import (
	"context"
	"strings"
	"testing"
)

func TestShasum(t *testing.T) {
	t.Parallel()
	text := "aaa  node-v22.11.0-linux-x64.tar.gz\nbbb *bun-linux-x64.zip\nmalformed line here\nccc  node-v22.11.0-linux-x64.tar.xz\n"
	cases := map[string]string{
		"node-v22.11.0-linux-x64.tar.gz": "aaa",
		"bun-linux-x64.zip":              "bbb", // 二进制模式的 * 前缀
		"node-v22.11.0-linux-x64.tar.xz": "ccc",
		"missing.zip":                    "",
		"here":                           "",
	}
	for file, want := range cases {
		if got := shasum(text, file); got != want {
			t.Errorf("shasum(%q) = %q，期望 %q", file, got, want)
		}
	}
}

func TestResolveNodeVersion(t *testing.T) {
	t.Parallel()
	rels := []nodeRelease{
		{Version: "v23.1.0", LTS: false},
		{Version: "v22.11.0", LTS: "Jod"},
		{Version: "v22.10.0", LTS: false},
		{Version: "v20.18.0", LTS: "Iron"},
		{Version: "v20.17.0", LTS: "Iron"},
		{Version: "v18.20.4", LTS: "Hydrogen"},
	}
	cases := []struct {
		spec, want string
		wantErr    bool
	}{
		// nvm 的 LTS 代号：该系列的最新版本（以前一律取最新的 LTS 版本）
		{spec: "lts/iron", want: "20.18.0"},
		{spec: "lts/Hydrogen", want: "18.20.4"},
		{spec: "lts/argon", wantErr: true},
		// lts/-N：往前数第 N 个 LTS 系列
		{spec: "lts/-0", want: "22.11.0"},
		{spec: "lts/-1", want: "20.18.0"},
		{spec: "lts/-2", want: "18.20.4"},
		{spec: "lts/-3", wantErr: true},
		{spec: "lts/-x", wantErr: true},
		// 表示最新正式版本的 nvm 别名
		{spec: "node", want: "23.1.0"},
		{spec: "stable", want: "23.1.0"},
		{spec: "ltsx", wantErr: true},
		{spec: "22", want: "22.11.0"},
		{spec: "^22.10", want: "22.11.0"},
		{spec: ">=20 <22", want: "20.18.0"},
		{spec: "22.10.0", want: "22.10.0"},
		{spec: "*", want: "23.1.0"},
		{spec: "lts/*", want: "22.11.0"},
		{spec: "lts", want: "22.11.0"},
		{spec: "16", wantErr: true},
		// 通配范围带构建元数据：npm 忽略它（22.x.x+build 即 >=22.0.0 <23.0.0-0）
		{spec: "22.x.x+build", want: "22.11.0"},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			t.Parallel()
			got, err := resolveNodeVersion(rels, c.spec)
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("resolveNodeVersion(%q) = %q, %v", c.spec, got, err)
			}
		})
	}
	if _, err := resolveNodeVersion([]nodeRelease{{Version: "v23.0.0", LTS: false}}, "lts/*"); err == nil {
		t.Error("没有 LTS 版本时应返回错误")
	}
}

// 平台到上游文件名的映射：Go 平台名 → Node（x64）、Bun（aarch64、musl）和 pnpm 原生包（x64，只有 glibc）的写法。
func TestPlatformNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		platform, node, bun, pnpm string // 空字符串表示不支持
	}{
		{platform: "linux/amd64", node: "linux-x64", bun: "bun-linux-x64", pnpm: "@pnpm/exe.linux-x64"},
		{platform: "linux/arm64", node: "linux-arm64", bun: "bun-linux-aarch64", pnpm: "@pnpm/exe.linux-arm64"},
		{platform: "darwin/arm64", node: "darwin-arm64", bun: "bun-darwin-aarch64", pnpm: "@pnpm/exe.darwin-arm64"},
		{platform: "darwin/amd64", node: "darwin-x64", bun: "bun-darwin-x64", pnpm: "@pnpm/exe.darwin-x64"},
		{platform: "linux/amd64/musl", node: "linux-x64", bun: "bun-linux-x64-musl", pnpm: "@pnpm/exe.linux-x64"},
		{platform: "windows/amd64"},
		{platform: "linux/riscv64"},
		{platform: "linux"},
	}
	for _, c := range cases {
		t.Run(c.platform, func(t *testing.T) {
			t.Parallel()
			if got, err := nodePlatform(c.platform); got != c.node || (err != nil) != (c.node == "") {
				t.Errorf("nodePlatform(%q) = %q, %v，期望 %q", c.platform, got, err, c.node)
			}
			if got, err := bunTarget(c.platform); got != c.bun || (err != nil) != (c.bun == "") {
				t.Errorf("bunTarget(%q) = %q, %v，期望 %q", c.platform, got, err, c.bun)
			}
			if got, err := pnpmExePackage(c.platform); got != c.pnpm || (err != nil) != (c.pnpm == "") {
				t.Errorf("pnpmExePackage(%q) = %q, %v，期望 %q", c.platform, got, err, c.pnpm)
			}
		})
	}
}

// 各 importer 通过假上游解析版本和发行包。
func TestImporterResolve(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	ctx := context.Background()

	versions := []struct {
		name, spec, want string
	}{
		{name: "go", spec: "1.24", want: "1.24.4"},
		{name: "go", spec: "go1.24.3", want: "1.24.3"},
		{name: "node", spec: " 22 ", want: "22.11.0"},
		{name: "node", spec: "lts/*", want: "22.11.0"},
		{name: "pnpm", spec: "9.12.0", want: "9.12.0"}, // 完整版本不访问 registry
		{name: "pnpm", spec: "~9.11", want: "9.11.0"},
		{name: "pnpm", spec: "9", want: "9.12.0"},
		{name: "bun", spec: "v1.1.30", want: "1.1.30"},
		{name: "bun", spec: "1.1", want: "1.1.30"},
		// 完整的预发布版本直接使用，不当成范围（以前解析成正式版本或者找不到）
		{name: "pnpm", spec: "10.0.0-rc.1", want: "10.0.0-rc.1"},
		{name: "bun", spec: "v1.2.0-canary.1", want: "1.2.0-canary.1"},
	}
	for _, v := range versions {
		got, err := importers[v.name].ResolveVersion(ctx, v.spec)
		if err != nil || got != v.want {
			t.Errorf("%s.ResolveVersion(%q) = %q, %v，期望 %q", v.name, v.spec, got, err, v.want)
		}
	}

	arts := []struct {
		name, version, platform string
		want                    artifact
	}{
		{name: "go", version: "1.24.4", platform: "linux/amd64", want: artifact{URL: rel.goLinux.url, SHA256: sha256Hex(rel.goLinux.body), Format: formatTarGz, Strip: 1}},
		{name: "node", version: "22.11.0", platform: "linux/amd64", want: artifact{URL: rel.nodeLinux.url, SHA256: sha256Hex(rel.nodeLinux.body), Format: formatTarGz, Strip: 1}},
		{name: "pnpm", version: "9.12.0", platform: anyPlatform, want: artifact{URL: rel.pnpm.url, Integrity: sha512Integrity(rel.pnpm.body), Format: formatTarGz, Strip: 1}},
		// 按平台发行的 pnpm：取这个平台的原生包，不是包装
		{name: "pnpm", version: "12.14.0", platform: "linux/arm64", want: artifact{URL: rel.pnpmLinuxArm.url, Integrity: sha512Integrity(rel.pnpmLinuxArm.body), Format: formatTarGz, Strip: 1}},
		{name: "bun", version: "1.1.30", platform: "linux/amd64", want: artifact{URL: rel.bunLinux.url, SHA256: sha256Hex(rel.bunLinux.body), Format: formatZip, Strip: 1}},
	}
	for _, a := range arts {
		got, err := importers[a.name].Artifact(ctx, a.version, a.platform)
		if err != nil || got != a.want {
			t.Errorf("%s.Artifact(%s, %s) = %+v, %v，期望 %+v", a.name, a.version, a.platform, got, err, a.want)
		}
	}
	// 只有 JavaScript 发行的 pnpm 平台无关；按平台发行原生可执行文件的 pnpm 版本不是
	independent := []struct {
		name, version string
		want          bool
	}{
		{"go", "1.24.4", false},
		{"node", "22.11.0", false},
		{"bun", "1.1.30", false},
		{"pnpm", "9.12.0", true},
		{"pnpm", "12.14.0", false},
	}
	for _, c := range independent {
		if got, err := importers[c.name].PlatformIndependent(ctx, c.version); err != nil || got != c.want {
			t.Errorf("%s.PlatformIndependent(%s) = %v, %v，期望 %v", c.name, c.version, got, err, c.want)
		}
	}
}

func TestImporterArtifactErrors(t *testing.T) {
	f := newFakeUpstream(t)
	registerAll(t, f)
	ctx := context.Background()
	cases := []struct {
		name, imp, version, platform, want string
	}{
		{name: "Go 版本不在索引中", imp: "go", version: "1.99.0", platform: "linux/amd64", want: "没有 linux/amd64 平台的 Go 1.99.0 发行包"},
		{name: "Go 平台格式错误", imp: "go", version: "1.24.4", platform: "linux", want: "无效的平台"},
		{name: "Go 版本存在但没有该平台的归档", imp: "go", version: "1.24.4", platform: "darwin/amd64", want: "没有 darwin/amd64 平台"},
		{name: "Node 的 SHASUMS 不存在", imp: "node", version: "21.0.0", platform: "linux/amd64", want: "404"},
		{name: "Bun 的 SHASUMS 不存在", imp: "bun", version: "0.1.0", platform: "linux/amd64", want: "404"},
		{name: "Bun 的 SHASUMS 中没有该平台", imp: "bun", version: "1.1.30", platform: "linux/arm64", want: "SHASUMS256.txt 中没有 bun-linux-aarch64.zip"},
		{name: "Bun 不支持的平台", imp: "bun", version: "1.1.30", platform: "windows/amd64", want: "没有 windows/amd64 平台的 Bun 发行包"},
		{name: "pnpm 不支持的系统", imp: "pnpm", version: "12.14.0", platform: "windows/amd64", want: "没有 windows/amd64 平台的 pnpm 发行包"},
		{name: "pnpm 不支持的架构", imp: "pnpm", version: "12.14.0", platform: "linux/386", want: "没有 linux/386 平台的 pnpm 发行包"},
		{name: "pnpm 原生发行中没有该平台", imp: "pnpm", version: "12.14.0", platform: "linux/amd64", want: "没有 linux/amd64 平台的 pnpm 12.14.0 发行包"},
		{name: "与平台无关的 pnpm 版本给了具体平台", imp: "pnpm", version: "9.12.0", platform: "linux/amd64", want: "pnpm 9.12.0 的发行包与平台无关，lockfile 中应为 any 条目"},
		{name: "pnpm 版本元数据不存在", imp: "pnpm", version: "9.11.0", platform: "linux/arm64", want: "404"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importers[c.imp].Artifact(ctx, c.version, c.platform)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Artifact = %v，期望包含 %q", err, c.want)
			}
		})
	}
}

// 上游索引不可用（非 200、不是合法 JSON）时返回错误，不缓存失败的结果。
func TestIndexUnavailable(t *testing.T) {
	f := newFakeUpstream(t)
	ctx := context.Background()
	if _, err := importers["go"].ResolveVersion(ctx, "1.24"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("索引 404 时 ResolveVersion = %v", err)
	}
	f.set(t, "https://nodejs.org/dist/index.json", []byte("<html>"))
	if _, err := importers["node"].ResolveVersion(ctx, "22"); err == nil {
		t.Fatal("索引不是 JSON 时应返回错误")
	}
	// 上游恢复后可以正常解析
	registerAll(t, f)
	if v, err := importers["go"].ResolveVersion(ctx, "1.24"); err != nil || v != "1.24.4" {
		t.Fatalf("上游恢复后 ResolveVersion = %q, %v", v, err)
	}
	if v, err := importers["node"].ResolveVersion(ctx, "22"); err != nil || v != "22.11.0" {
		t.Fatalf("上游恢复后 ResolveVersion = %q, %v", v, err)
	}
}
