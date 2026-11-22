package runner

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// [proxy.host] / [proxy.oci] 的读取和校验：host 可以用本机地址；oci 不行（容器中的 127.0.0.1
// 是容器自己）；错误中不带代理的原值（可能有密码）。
func TestLoadConfigProxy(t *testing.T) {
	t.Parallel()
	load := func(t *testing.T, body string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "config.toml"), body)
		return LoadConfig(dir)
	}
	c, err := load(t, `
[proxy.host]
http_proxy = "http://127.0.0.1:3128"
https_proxy = "http://u:pw@127.0.0.1:3128"
no_proxy = "localhost,.internal"

[proxy.oci]
https_proxy = "http://172.17.0.1:3128"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := ProxyConfig{
		Host: ProxySettings{HTTPProxy: "http://127.0.0.1:3128", HTTPSProxy: "http://u:pw@127.0.0.1:3128", NoProxy: "localhost,.internal"},
		OCI:  ProxySettings{HTTPSProxy: "http://172.17.0.1:3128"},
	}
	if c.Proxy != want {
		t.Errorf("Proxy = %+v", c.Proxy)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"oci 用 127.0.0.1", "[proxy.oci]\nhttps_proxy = \"http://u:secretpw@127.0.0.1:3128\"", "容器中指向容器自己"},
		{"oci 用 localhost", "[proxy.oci]\nhttp_proxy = \"http://localhost:3128\"", "容器中指向容器自己"},
		{"oci 用 ::1", "[proxy.oci]\nhttp_proxy = \"http://[::1]:3128\"", "容器中指向容器自己"},
		{"不支持的 scheme", "[proxy.host]\nhttp_proxy = \"ftp://secretpw@proxy:21\"", "scheme"},
		{"没有 scheme", "[proxy.host]\nhttps_proxy = \"127.0.0.1:3128\"", "scheme"},
		{"缺少主机名", "[proxy.host]\nhttps_proxy = \"http://:3128\"", "主机名"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig = %v，期望包含 %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secretpw") {
				t.Errorf("错误中出现了密码：%v", err)
			}
		})
	}
}

// Save 写回时保留代理配置；没有配置时不写 [proxy]。
func TestConfigSaveProxy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &Config{Dir: dir}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "proxy") {
		t.Errorf("没有配置代理时不应写 [proxy]：\n%s", b)
	}
	c.Proxy.Host = ProxySettings{HTTPSProxy: "http://127.0.0.1:3128"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxy != c.Proxy {
		t.Errorf("读回 Proxy = %+v", got.Proxy)
	}
}

func TestProxySettingsApply(t *testing.T) {
	t.Parallel()
	env := map[string]string{"HTTP_PROXY": "keep"}
	ProxySettings{HTTPSProxy: "http://p:1", NoProxy: "localhost"}.apply(env)
	want := map[string]string{"HTTP_PROXY": "keep", "HTTPS_PROXY": "http://p:1", "https_proxy": "http://p:1", "NO_PROXY": "localhost", "no_proxy": "localhost"}
	if len(env) != len(want) {
		t.Errorf("env = %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q，期望 %q", k, env[k], v)
		}
	}
}

// workflow 只写了一种大小写时另一种跟随；两种都写了就各用各的；没写的不动。
func TestSyncProxyCase(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"HTTP_PROXY": "runner", "http_proxy": "wf-low",
		"HTTPS_PROXY": "wf-up", "https_proxy": "runner",
		"NO_PROXY": "a", "no_proxy": "b",
		"ALL_PROXY": "runner", "all_proxy": "runner",
	}
	syncProxyCase(env, map[string]string{"http_proxy": "wf-low", "HTTPS_PROXY": "wf-up", "NO_PROXY": "a", "no_proxy": "b"})
	for k, v := range map[string]string{
		"HTTP_PROXY": "wf-low", "http_proxy": "wf-low", "HTTPS_PROXY": "wf-up", "https_proxy": "wf-up",
		"NO_PROXY": "a", "no_proxy": "b", "ALL_PROXY": "runner", "all_proxy": "runner",
	} {
		if env[k] != v {
			t.Errorf("%s = %q，期望 %q", k, env[k], v)
		}
	}
}

// 代理凭证：有密码时脱敏密码，只有用户名时（token）脱敏用户名；String 不含凭证。
func TestProxySecretsAndString(t *testing.T) {
	t.Parallel()
	p := ProxySettings{HTTPProxy: "http://user:pass-word@proxy:3128", HTTPSProxy: "https://tok-en-123@proxy:3129", NoProxy: "localhost"}
	if got := p.secrets(); !slices.Equal(got, []string{"pass-word", "user:pass-word", "tok-en-123", "tok-en-123:"}) {
		t.Errorf("secrets = %v", got)
	}
	// 编码过的密码：解码值、URL 中写的形式、Go 的规范编码都要脱敏
	enc := ProxySettings{HTTPProxy: "http://kiln:pass%3Aword%26x@proxy:3128", HTTPSProxy: "http://kiln:pass%3Aword&x@proxy:3128"}
	if got := enc.secrets(); !slices.Equal(got, []string{"pass:word&x", "pass%3Aword%26x", "pass%3Aword&x", "kiln:pass:word&x"}) {
		t.Errorf("编码的密码：secrets = %v", got)
	}
	s := p.String()
	if strings.Contains(s, "pass-word") || strings.Contains(s, "tok-en-123") || strings.Contains(s, "user") {
		t.Errorf("String 中有凭证：%s", s)
	}
	if !strings.Contains(s, "http_proxy=http://***@proxy:3128") {
		t.Errorf("String = %s", s)
	}
	if (ProxySettings{}).String() != "未配置" {
		t.Errorf("空配置 String = %q", ProxySettings{}.String())
	}
}

// doctor 的代理选择与 step 中的 Go 程序（http.ProxyFromEnvironment 的规则）一致：https 只看 https_proxy；
// NO_PROXY 按有效端口匹配，.example.com 只匹配子域名，example.com 也匹配子域名；本机地址总是直连。
func TestProxyFor(t *testing.T) {
	t.Parallel()
	const hp, sp = "http://h.invalid:1", "http://s.invalid:2"
	for _, tc := range []struct {
		noProxy, target, want string
	}{
		{"", "https://github.com/", sp},
		{"", "http://github.com/", hp},
		{"*", "https://github.com/", ""},
		{"github.com", "https://api.github.com/", ""},
		{"github.com", "https://github.com/", ""},
		{".github.com", "https://api.github.com/", ""},
		{".example.com", "https://example.com/", sp},
		{"github.com", "https://notgithub.com/", sp},
		{"proxy.golang.org:80", "https://proxy.golang.org/", sp},
		{"proxy.golang.org:443", "https://proxy.golang.org/", ""},
		{"10.0.0.0/8", "https://10.1.2.3/", ""},
		{"10.0.0.0/8", "https://11.1.2.3/", sp},
		{"", "https://127.0.0.1/", ""},
	} {
		u, _ := url.Parse(tc.target)
		got, err := ProxySettings{HTTPProxy: hp, HTTPSProxy: sp, NoProxy: tc.noProxy}.proxyFor(u)
		gotS := ""
		if got != nil {
			gotS = got.String()
		}
		if err != nil || gotS != tc.want {
			t.Errorf("no_proxy=%q %s：proxyFor = %q, %v，想要 %q", tc.noProxy, tc.target, gotS, err, tc.want)
		}
	}
}

// oci：[proxy.oci] 只在 network: true 时注入 job 容器的环境（PLAN #8.5）。
func TestOCIExecProxyEnv(t *testing.T) {
	t.Parallel()
	proxy := ProxySettings{HTTPSProxy: "http://172.17.0.1:3128"}
	for _, network := range []bool{false, true} {
		sp := ociSpec()
		sp.Network = network
		o := newOCIExec("docker", newTestDirs(t), sp, testRunnerID)
		o.proxy = proxy
		env := map[string]string{"HTTPS_PROXY": "workflow"}
		o.jobEnv(env)
		o.lockEnv(env)
		want := "workflow"
		if network {
			want = proxy.HTTPSProxy
		}
		if env["HTTPS_PROXY"] != want {
			t.Errorf("network=%v：HTTPS_PROXY = %q，期望 %q", network, env["HTTPS_PROXY"], want)
		}
	}
}

// buildkit: true 且 network: true：daemon 用 bridge 网络，代理经 env 文件（不在命令行中）传给 daemon，启动后删除。
func TestOCIExecBuildkitProxy(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	d := newTestDirs(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sp := ociSpec()
	sp.Network = true
	o := newOCIExec(docker, d, sp, testRunnerID)
	o.proxy = ProxySettings{HTTPSProxy: "http://u:secretpw@172.17.0.1:3128", NoProxy: "registry.internal"}
	image := "moby/buildkit@sha256:" + strings.Repeat("c", 64)
	if err := o.prepareBuildkitClient(t.Context(), image, t.TempDir(), log); err != nil {
		t.Fatal(err)
	}
	if err := o.prepare(t.Context(), log); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(d.root, "buildkitd.env")
	var run string
	for _, c := range dockerCalls(t, calls) {
		if strings.HasPrefix(c, "run -d --name kiln-5-buildkitd ") {
			run = c
		}
	}
	if !strings.Contains(run, "--network bridge") || !strings.Contains(run, "--env-file "+envFile) || strings.Contains(run, "secretpw") {
		t.Errorf("daemon 的 docker run：%s", run)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Errorf("启动后应删除 env 文件：%v", err)
	}

	// env 文件的内容
	p, err := o.writeBuildkitEnv()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "HTTPS_PROXY=http://u:secretpw@172.17.0.1:3128\nNO_PROXY=registry.internal\nhttps_proxy=http://u:secretpw@172.17.0.1:3128\nno_proxy=registry.internal\n"
	if string(b) != want {
		t.Errorf("env 文件：\n%s", b)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("env 文件权限：%v %v", st, err)
	}
	// 没有配置代理或 network: false 时不写
	for _, o2 := range []*ociExec{{d: d, network: true}, {d: d, proxy: o.proxy}} {
		if p, err := o2.writeBuildkitEnv(); p != "" || err != nil {
			t.Errorf("writeBuildkitEnv = %q, %v", p, err)
		}
	}
}

// doctor --network：按 [proxy.host] 访问，区分代理不可达、代理认证失败、DNS 失败，错误中不带密码。
func TestDoctorHostNetwork(t *testing.T) {
	t.Parallel()
	// 正常的 HTTP 代理：转发请求的绝对 URL，直接回 200
	var mu sync.Mutex
	var proxied []string
	okProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		proxied = append(proxied, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	defer okProxy.Close()
	// 要求认证的代理：对 CONNECT 回 407
	authProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer authProxy.Close()
	// 没有监听的端口
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := l.Addr().String()
	_ = l.Close()

	type item struct {
		ok     bool
		detail string
	}
	run := func(p ProxySettings, targets ...string) []item {
		var out []item
		doctorHostNetwork(t.Context(), p, targets, func(ok bool, _, detail string) { out = append(out, item{ok, detail}) })
		return out
	}

	got := run(ProxySettings{HTTPProxy: okProxy.URL}, "http://proxy.golang.org/")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !got[0].ok || !slices.Equal(proxied, []string{"http://proxy.golang.org/"}) {
		t.Errorf("经代理：%+v，代理收到 %v", got, proxied)
	}

	got = run(ProxySettings{HTTPProxy: "http://kiln:secretpw@" + refused}, "http://proxy.golang.org/")
	if len(got) != 1 || got[0].ok || !strings.Contains(got[0].detail, "代理") || strings.Contains(got[0].detail, "secretpw") {
		t.Errorf("代理不可达：%+v", got)
	}

	got = run(ProxySettings{HTTPSProxy: authProxy.URL}, "https://proxy.golang.org/")
	if len(got) != 1 || got[0].ok || !strings.Contains(got[0].detail, "407") {
		t.Errorf("代理认证：%+v", got)
	}

	got = run(ProxySettings{}, "http://kiln-doctor.invalid/")
	if len(got) != 1 || got[0].ok || !strings.Contains(got[0].detail, "DNS") || !strings.Contains(got[0].detail, "[proxy.host]") {
		t.Errorf("直连 DNS 失败：%+v", got)
	}
}

// classifyNetErr 能识别本地构造的几类错误。
func TestClassifyNetErr(t *testing.T) {
	t.Parallel()
	// 本地 TLS 服务器用自签证书：默认的客户端验证失败
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	err := probeURL(t.Context(), srv.URL, nil)
	if err == nil || classifyNetErr(err, false) != "TLS 错误" {
		t.Errorf("自签证书：%v → %s", err, classifyNetErr(err, false))
	}
	// 代理返回非 HTTP 的内容
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write(bytes.Repeat([]byte{0}, 16))
		_ = c.Close()
	}()
	u, _ := ProxySettings{HTTPProxy: "http://" + l.Addr().String()}.proxyFor(&url.URL{Scheme: "http", Host: "example.com"})
	if err := probeURL(t.Context(), "http://example.com/", u); err == nil {
		t.Error("代理返回垃圾内容时应失败")
	}
}
