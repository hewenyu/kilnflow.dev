package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/redact"
)

// doctorNetworkTimeout 是 doctor --network 中每个地址的超时。
const doctorNetworkTimeout = 15 * time.Second

// doctorNetworkTargets 是 doctor --network 检查的常见下载入口。
var doctorNetworkTargets = []string{
	"https://proxy.golang.org/",
	"https://registry.npmjs.org/",
	"https://github.com/",
}

// DoctorOptions 是 doctor 的可选检查。
type DoctorOptions struct {
	// Network 为 true 时以 network: true 的 host job 的代理设置（[proxy.host]，不是 Runner 进程的环境）
	// 访问常见下载入口，并区分 DNS、代理、认证、TLS、超时等失败原因。
	Network bool
}

// doctorHostNetwork 以 host job 等效的代理访问 targets，每个地址报告一项。
func doctorHostNetwork(ctx context.Context, p ProxySettings, targets []string, check func(ok bool, item, detail string)) {
	via := "直连（没有配置 [proxy.host]）"
	if !p.empty() {
		via = "经 [proxy.host]"
	}
	for _, t := range targets {
		u, err := url.Parse(t)
		if err != nil {
			check(false, "job 网络（host）", fmt.Sprintf("%s：%v", t, err))
			continue
		}
		proxy, err := p.proxyFor(u)
		if err != nil {
			check(false, "job 网络（host）", fmt.Sprintf("%s：%v", u.Host, err))
			continue
		}
		route := "直连"
		if proxy != nil {
			route = "代理 " + redactProxy(proxy.String())
		}
		if err := probeURL(ctx, t, proxy); err != nil {
			detail := fmt.Sprintf("%s：%s（%s）：%s", u.Host, classifyNetErr(err, proxy != nil), route, redact.String(err.Error(), p.secrets()...))
			if proxy == nil && p.empty() {
				detail += "；只能经代理上网时在 config.toml 中配置 [proxy.host]"
			}
			check(false, "job 网络（host）", detail)
			continue
		}
		check(true, "job 网络（host）", fmt.Sprintf("%s 可访问（%s，%s）", u.Host, route, via))
	}
}

// probeURL 经 proxy（nil 为直连）请求 target：收到任何 HTTP 响应即可（不跟随跳转）。
func probeURL(ctx context.Context, target string, proxy *url.URL) error {
	ctx, cancel := context.WithTimeout(ctx, doctorNetworkTimeout)
	defer cancel()
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return proxy, nil }}
	defer tr.CloseIdleConnections()
	c := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return errors.New(resp.Status)
	}
	return nil
}

// classifyNetErr 给出失败的层次：DNS、网络不可达、代理不可达、连接被拒绝、代理认证、TLS、超时。
func classifyNetErr(err error, viaProxy bool) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var recErr tls.RecordHeaderError
	switch {
	case strings.Contains(err.Error(), "Proxy Authentication Required"):
		return "代理认证失败（407）"
	case errors.As(err, &dnsErr):
		return "DNS 解析失败"
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH):
		return "网络不可达"
	case errors.As(err, &opErr) && opErr.Op == "proxyconnect":
		return "代理不可达"
	case errors.Is(err, syscall.ECONNREFUSED):
		if viaProxy {
			return "代理拒绝连接"
		}
		return "连接被拒绝"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &recErr):
		return "TLS 错误"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "超时"
	}
	return "请求失败"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
