package runner

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// job 的出站代理（config.toml 的 [proxy.host]、[proxy.oci]，PLAN #8.5）。
//
// step 的环境变量由 Runner 构造，不继承 Runner 进程的环境（避免带入云平台密钥、DOCKER_HOST 等），
// 所以只能经代理上网的机器要在这里显式写出 job 使用的代理。Runner 自己的请求（Server、git、runtime 下载）
// 仍然使用进程环境中的 HTTP_PROXY 等变量（例如 systemd 单元的 Environment=）。
//
//   - 只在 network: true 的 job 中注入；workflow 的 env 可以覆盖（写一种大小写即可，另一种跟随）。
//   - network: false 的 host job 在合并 workflow env 之后强制把代理指向不可达地址（见 hostExec.lockEnv），
//     NO_PROXY 只保留本机地址，workflow env 无法恢复外网访问；这只是让常见工具失败，不是系统级隔离。
//   - [proxy.host] 给 host executor：127.0.0.1 就是宿主机。
//   - [proxy.oci] 给 oci executor 的 job 容器和 BuildKit daemon：容器有自己的网络命名空间，
//     127.0.0.1 指向容器自己，所以不接受本机地址，要写容器可达的地址（例如 docker 网桥网关上的代理）。
//   - 代理 URL 中的用户名密码按 secret 脱敏，doctor 只显示脱敏后的地址。

// ProxySettings 是一种 executor 的 job 使用的代理；字段为空表示不设置对应的变量。
type ProxySettings struct {
	HTTPProxy  string `toml:"http_proxy,omitempty"`
	HTTPSProxy string `toml:"https_proxy,omitempty"`
	NoProxy    string `toml:"no_proxy,omitempty"`
}

// ProxyConfig 是 [proxy] 段。
type ProxyConfig struct {
	Host ProxySettings `toml:"host,omitempty"`
	OCI  ProxySettings `toml:"oci,omitempty"`
}

// 代理相关的环境变量：大写和小写两种写法（Go 先看大写，curl 的 http_proxy 只认小写）。
var proxyEnvPairs = [][2]string{
	{"HTTP_PROXY", "http_proxy"},
	{"HTTPS_PROXY", "https_proxy"},
	{"ALL_PROXY", "all_proxy"},
	{"NO_PROXY", "no_proxy"},
}

// noNetworkNoProxy 是关闭网络时的 NO_PROXY：只有本机地址不经过（不可达的）代理，本地测试服务仍可访问。
const noNetworkNoProxy = "localhost,127.0.0.1,::1"

// empty 报告是否没有配置代理。
func (p ProxySettings) empty() bool {
	return p.HTTPProxy == "" && p.HTTPSProxy == "" && p.NoProxy == ""
}

// apply 把配置的代理写入 env（两种大小写）。
func (p ProxySettings) apply(env map[string]string) {
	for _, kv := range [][3]string{
		{p.HTTPProxy, "HTTP_PROXY", "http_proxy"},
		{p.HTTPSProxy, "HTTPS_PROXY", "https_proxy"},
		{p.NoProxy, "NO_PROXY", "no_proxy"},
	} {
		if kv[0] != "" {
			env[kv[1]], env[kv[2]] = kv[0], kv[0]
		}
	}
}

// secrets 返回代理 URL 中需要脱敏的凭证：有密码时是密码，只有用户名时（token 写在用户名中）是用户名。
// 解码后的值之外，还包括 URL 中写的编码形式（注入环境变量的就是它）和 Go 的规范编码：userinfo 的转义规则
// 与 query、path 不同，logShipper 自己生成的编码形式覆盖不到（例如密码 pass:word&x 写作 pass%3Aword&x）；
// 以及 用户名:密码（HTTP Basic 认证的内容，logShipper 会登记它的 base64）。
func (p ProxySettings) secrets() []string {
	var out []string
	add := func(vs ...string) {
		for _, v := range vs {
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	for _, s := range []string{p.HTTPProxy, p.HTTPSProxy} {
		u, err := url.Parse(s)
		if err != nil || u.User == nil {
			continue
		}
		rawName, rawPw, _ := strings.Cut(rawUserinfo(s), ":")
		canonName, canonPw, _ := strings.Cut(u.User.String(), ":")
		// 还有完整的 用户名:密码：AddSecret 会加上它的 base64，即工具调试输出中的 Proxy-Authorization: Basic …
		if pw, ok := u.User.Password(); ok {
			add(pw, rawPw, canonPw, u.User.Username()+":"+pw)
		} else {
			add(u.User.Username(), rawName, canonName, u.User.Username()+":")
		}
	}
	return out
}

// rawUserinfo 返回 URL 中原样写出的 userinfo（user:password，未解码）；没有时为空。
func rawUserinfo(s string) string {
	_, rest, ok := strings.Cut(s, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		return rest[:i]
	}
	return ""
}

// String 返回脱敏后的配置，用于 doctor。
func (p ProxySettings) String() string {
	if p.empty() {
		return "未配置"
	}
	var parts []string
	for _, kv := range [][2]string{{"http_proxy", p.HTTPProxy}, {"https_proxy", p.HTTPSProxy}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+redactProxy(kv[1]))
		}
	}
	if p.NoProxy != "" {
		parts = append(parts, "no_proxy="+p.NoProxy)
	}
	return strings.Join(parts, "，")
}

// redactProxy 隐藏代理 URL 中的用户名和密码。
func redactProxy(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "（无法解析）"
	}
	if u.User == nil {
		return u.String()
	}
	u.User = nil
	return u.Scheme + "://***@" + strings.TrimPrefix(u.String(), u.Scheme+"://")
}

// validate 检查代理地址；container 为 true 时（oci）不接受本机地址。
func (p ProxySettings) validate(section string, container bool) error {
	for _, kv := range [][2]string{{"http_proxy", p.HTTPProxy}, {"https_proxy", p.HTTPSProxy}} {
		if kv[1] == "" {
			continue
		}
		if err := validateProxyURL(kv[1], container); err != nil {
			// 不带原值：其中可能有密码
			return fmt.Errorf("[%s] 的 %s：%w", section, kv[0], err)
		}
	}
	return nil
}

func validateProxyURL(s string, container bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("不是合法的 URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("scheme 要是 http、https、socks5 或 socks5h，不是 %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("缺少主机名")
	}
	if container && isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%s 在容器中指向容器自己，要写容器可达的地址（例如 docker 网桥网关上的代理）", u.Hostname())
	}
	return nil
}

// isLoopbackHost 报告 host 是否是本机地址（localhost、127.0.0.0/8、::1）。
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// syncProxyCase 让 workflow env 只写了一种大小写的代理变量也覆盖另一种：
// 否则 Runner 注入的 HTTPS_PROXY 会压过 workflow 写的 https_proxy（Go 先看大写）。
func syncProxyCase(env, workflowEnv map[string]string) {
	for _, pair := range proxyEnvPairs {
		_, upOK := workflowEnv[pair[0]]
		_, lowOK := workflowEnv[pair[1]]
		switch {
		case upOK && !lowOK:
			env[pair[1]] = env[pair[0]]
		case lowOK && !upOK:
			env[pair[0]] = env[pair[1]]
		}
	}
}

// blockNetworkEnv 让常见工具的代理指向不可达地址，NO_PROXY 只保留本机地址（network: false 的 host job）。
func blockNetworkEnv(env map[string]string) {
	for _, pair := range proxyEnvPairs {
		v := noNetworkProxy
		if pair[0] == "NO_PROXY" {
			v = noNetworkNoProxy
		}
		env[pair[0]], env[pair[1]] = v, v
	}
}

// proxyFor 返回 p 对 target 的请求使用的代理，nil 表示直连。规则与 step 中的 Go 程序（http.ProxyFromEnvironment）
// 相同：用的就是它的实现 golang.org/x/net/http/httpproxy（https 只看 https_proxy；NO_PROXY 按主机和有效端口匹配，
// .example.com 只匹配子域名；本机地址总是直连）。用于 doctor 的联网检查。
func (p ProxySettings) proxyFor(target *url.URL) (*url.URL, error) {
	cfg := httpproxy.Config{HTTPProxy: p.HTTPProxy, HTTPSProxy: p.HTTPSProxy, NoProxy: p.NoProxy}
	u, err := cfg.ProxyFunc()(target)
	if err != nil {
		return nil, errors.New("代理地址不是合法的 URL")
	}
	return u, nil
}
