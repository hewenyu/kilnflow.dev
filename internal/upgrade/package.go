package upgrade

import "strings"

// Scope 是 Runner 升级包的 npm scope（PLAN #27.11）：Kiln 自己的 release.yml 发布为 @yuebanlaosiji/kilnflow-<程序名去掉 kiln>-<os>-<arch>，
// 每种 Runner、每个平台一个包，包中只有这一个程序。
const Scope = "@yuebanlaosiji"

// PackagePrefix 是发布到 npm 的包名前缀：程序名开头的 programPrefix 换成它，包名和二进制名因此可以不同
// （程序 kiln-runner 发布为 kilnflow-runner-<os>-<arch>）。
const PackagePrefix = "kilnflow"

// programPrefix 是程序名的品牌前缀，换成 PackagePrefix 后就是包名。
const programPrefix = "kiln"

// DefaultRegistry 是没有设置 registry 时 Runner 使用的官方 npm registry。
const DefaultRegistry = "https://registry.npmjs.org/"

// PackageName 返回程序 program 在平台 goos/goarch（Go 的写法，如 darwin/arm64、linux/amd64）上的 npm 包名。
func PackageName(program, goos, goarch string) string {
	return Scope + "/" + PackagePrefix + strings.TrimPrefix(program, programPrefix) + "-" + goos + "-" + goarch
}

// PackageFile 返回升级包中程序的路径（npm tarball 的条目都在 package/ 下）。
func PackageFile(program string) string { return "package/" + program }

// Registry 返回实际使用的 registry：configured 为空时是官方 registry。
func Registry(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return DefaultRegistry
	}
	return configured
}
