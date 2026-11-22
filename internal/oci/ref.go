// Package oci 实现容器镜像发布所需的最小 OCI 支持（PLAN #13.8）：
// 镜像名和 tag 的校验、OCI image layout 的读取与校验（layout.go），
// 以及按 OCI Distribution 协议推送的 registry 客户端（registry.go）。
package oci

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
)

// Docker Hub 的名字和实际地址。
const (
	dockerHub     = "docker.io"
	dockerHubHost = "registry-1.docker.io"
)

var (
	// repoRe 是仓库路径（OCI Distribution 规范中 <name> 的写法）。
	repoRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	// tagRe 是 tag 的写法（OCI Distribution 规范）。
	tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// maxRepoLen 是仓库路径的长度上限（OCI Distribution 规范建议的上限）。
const maxRepoLen = 255

// Name 是解析后的镜像名 <registry>/<repository>。
type Name struct {
	// Registry 是写在镜像名中的 registry，例如 docker.io、ghcr.io、127.0.0.1:5000。
	Registry string
	// Host 是实际访问的地址：docker.io 对应 registry-1.docker.io，其他与 Registry 相同。
	Host string
	// Repository 是仓库路径；Docker Hub 上没有命名空间时补上 library/。
	Repository string
}

// String 返回规范化的镜像名，例如 docker.io/example/kiln-server。
func (n Name) String() string { return n.Registry + "/" + n.Repository }

// Insecure 判断是否用 http 访问：只有本机地址（与 docker 的默认行为一致），用于本地测试 registry。
func (n Name) Insecure() bool {
	host := n.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return endpoint.IsLoopbackHost(strings.Trim(host, "[]"))
}

// ParseName 解析 <registry>/<repository>。registry 必须写出来（例如 docker.io/example/app），
// 不能带 tag 或 digest：tag 由 tags / floating-tags 指定。
func ParseName(s string) (Name, error) {
	reg, repo, ok := strings.Cut(s, "/")
	if !ok || !validRegistry(reg) {
		return Name{}, fmt.Errorf("镜像名 %q 必须以 registry 开头，例如 docker.io/<namespace>/<name>", s)
	}
	if strings.ContainsAny(repo, "@:") {
		return Name{}, fmt.Errorf("镜像名 %q 不能带 tag 或 digest（tag 写在 tags / floating-tags 中）", s)
	}
	reg = strings.ToLower(reg)
	host := reg
	if reg == dockerHub || reg == "index.docker.io" || reg == dockerHubHost {
		reg, host = dockerHub, dockerHubHost
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	if repo == "" || len(repo) > maxRepoLen || !repoRe.MatchString(repo) {
		return Name{}, fmt.Errorf("镜像名 %q 中的仓库路径 %q 无效（只能用小写字母、数字和 . _ - /）", s, repo)
	}
	return Name{Registry: reg, Host: host, Repository: repo}, nil
}

// validRegistry 严格校验 registry：主机名是 RFC 1123 域名、IPv4 或用方括号写的 IPv6，可带数字端口（1..65535）。
// 拒绝空值、用户信息、查询参数、片段、空白和控制字符。
func validRegistry(reg string) bool {
	if reg == "" || strings.ContainsAny(reg, "/@?#\\") || strings.ContainsFunc(reg, unicode.IsSpace) ||
		strings.ContainsFunc(reg, unicode.IsControl) {
		return false
	}
	host := reg
	hasPort := false
	switch {
	case strings.HasPrefix(reg, "["): // IPv6，可能带端口
		h, port, err := net.SplitHostPort(reg)
		switch {
		case err != nil && !strings.HasSuffix(reg, "]"):
			return false
		case err != nil:
			host = reg[1 : len(reg)-1]
		case !validPort(port):
			return false
		default:
			host = h
		}
		// 方括号只用于 IPv6：拒绝 [example.com]、[127.0.0.1] 这类写法。
		ip := net.ParseIP(host)
		return ip != nil && ip.To4() == nil
	case strings.Contains(reg, ":"):
		h, port, err := net.SplitHostPort(reg)
		if err != nil || !validPort(port) {
			return false
		}
		host, hasPort = h, true
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	// registry 必须看起来像主机名（有域名后缀或是 localhost）：否则 "example/kiln" 会把命名空间当成 registry。
	// 带了端口时允许单标签主机名（registry:5000/org/app），端口已经表明这是 registry。
	if !hasPort && !strings.Contains(host, ".") && !strings.EqualFold(host, "localhost") {
		return false
	}
	return validDNSName(host)
}

// validPort 判断端口是 1..65535 的纯 ASCII 数字（拒绝 +443、-1、空白等 strconv 能接受的写法）。
func validPort(port string) bool {
	if port == "" {
		return false
	}
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// maxDNSName 是完整域名的长度上限（RFC 1035）。
const maxDNSName = 253

// maxDNSLabel 是单个标签的长度上限。
const maxDNSLabel = 63

// validDNSName 判断 host 是 RFC 1123 域名：标签由字母、数字和连字符组成，不以连字符开头或结尾，最长 63 个字符。
func validDNSName(host string) bool {
	if host == "" || len(host) > maxDNSName {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > maxDNSLabel {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
			if c == '-' && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
}

// ErrInvalidTag 表示 tag 不符合 OCI Distribution 的写法。
var ErrInvalidTag = errors.New("tag 只能包含字母、数字和 _ . -，不能以 . 或 - 开头，最长 128 个字符")

// CheckTag 校验 tag 的写法。
func CheckTag(tag string) error {
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("tag %q 无效：%w", tag, ErrInvalidTag)
	}
	return nil
}
