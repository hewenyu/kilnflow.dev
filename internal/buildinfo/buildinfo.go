// Package buildinfo 保存构建时注入的版本号，kiln-server、kiln-runner 和 kiln 共用。
package buildinfo

import (
	"runtime/debug"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// version 由构建时注入：-ldflags "-X github.com/hewenyu/kilnflow.dev/internal/buildinfo.version=..."。
var version string

// Version 返回构建版本。没有注入时退回到 Go 记录的 VCS 修订号（go build 在 git 仓库中会记录），都没有时为 "dev"。
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	rev = shorten.Hash(rev)
	return "dev-" + rev + dirty
}
