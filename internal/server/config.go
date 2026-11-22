// Package server 是 kiln-server 的组装层（composition root）：读取配置、连接数据库，
// 创建各子包的组件（store、engine、planner、ingest、runnerapi、publish、outbox、admin、web 等）并注入依赖，
// 注册 HTTP 路由（webhook、Runner/Admin RPC、CAS、Dashboard、healthz），启动后台循环。
package server

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/retention"
)

// Server 读取的环境变量（PLAN #20.1）。新增时同步更新 cmd/kiln-server 的 usage 和 deploy/server.env.example
// （TestUsageCoversConfig 检查）。
//
//nolint:gosec // G101 误报：这些是环境变量名，不是凭据
const (
	envPublicURL            = "KILN_PUBLIC_URL"
	envListen               = "KILN_LISTEN"
	envDatabaseURL          = "KILN_DATABASE_URL"
	envDataDir              = "KILN_DATA_DIR"
	envGitHubWebhookSecret  = "KILN_GITHUB_WEBHOOK_SECRET"
	envGitHubAPIURL         = "KILN_GITHUB_API_URL"
	envGitHubUploadURL      = "KILN_GITHUB_UPLOAD_URL"
	envGitHubClientID       = "KILN_GITHUB_CLIENT_ID"
	envGitHubClientSecret   = "KILN_GITHUB_CLIENT_SECRET"
	envAllowedAccounts      = "KILN_ALLOWED_ACCOUNTS"
	envAdmins               = "KILN_ADMINS"
	envUsers                = "KILN_USERS"
	envAdminToken           = "KILN_ADMIN_TOKEN"
	envAdminTokenFile       = "KILN_ADMIN_TOKEN_FILE"
	envGitHubAppID          = "KILN_GITHUB_APP_ID"
	envGitHubPrivateKey     = "KILN_GITHUB_PRIVATE_KEY"
	envGitHubPrivateKeyFile = "KILN_GITHUB_PRIVATE_KEY_FILE"
	envMasterKey            = "KILN_MASTER_KEY"
	envMasterKeyFile        = "KILN_MASTER_KEY_FILE"
	envLeaseTTL             = "KILN_LEASE_TTL"
	envReconcileInterval    = "KILN_RECONCILE_INTERVAL"
	envReconcileRepos       = "KILN_RECONCILE_REPOS"
	envRetention            = "KILN_RETENTION"
)

const (
	// defaultListen 是默认的监听地址。
	defaultListen = ":8080"
	// defaultDataDir 是默认的数据目录（CAS、日志）。
	defaultDataDir = "./kiln-data"
	// minAdminTokenLen 是管理 token 的最短长度。
	minAdminTokenLen = 16
	// minReconcileInterval 是周期对账的最小间隔。
	minReconcileInterval = time.Minute
	// minRetention 是保留时间的下限，避免写错单位（例如 7m）时清掉刚结束的 run 的日志。
	minRetention = time.Hour
)

// Config 是 Server 的配置（PLAN #20.1），全部来自环境变量。
type Config struct {
	PublicURL   string
	Listen      string
	DatabaseURL string
	DataDir     string

	GitHubAppID         int64
	GitHubPrivateKey    []byte
	GitHubWebhookSecret string
	GitHubAPIURL        string
	GitHubUploadURL     string
	GitHubClientID      string
	GitHubClientSecret  string

	// AllowedAccounts 是允许安装的 GitHub 账号（ID 或登录名，小写）。
	AllowedAccounts map[string]bool
	// Admins / Users 是可以登录 Dashboard 的 GitHub 登录名或 ID（小写）。
	Admins map[string]bool
	Users  map[string]bool

	AdminToken string
	MasterKey  []byte

	LeaseTTL        time.Duration
	AcquireLongPoll time.Duration
	InfraRetryMax   int
	CancelGrace     time.Duration
	SweepInterval   time.Duration
	// ReconcileInterval 是可选的周期引用对账的间隔（KILN_RECONCILE_INTERVAL），默认 0 表示关闭。
	ReconcileInterval time.Duration
	// ReconcileDelay 是启动后第一次周期对账前的等待时间。
	ReconcileDelay time.Duration
	// ReconcileRepos 是周期对账的范围（KILN_RECONCILE_REPOS）：仓库全名（小写），"*" 表示全部启用中的仓库。
	ReconcileRepos []string
	// Retention 是 run 结束后日志和产物等过程数据的保留时间（KILN_RETENTION，默认 12 小时，PLAN #10.3）。
	Retention time.Duration
	// RetentionInterval 是两次清理的间隔。
	RetentionInterval time.Duration
}

// setDefaults 填写时间间隔等默认值（PLAN #20.1）。
func (c *Config) setDefaults() {
	c.LeaseTTL = 60 * time.Second
	c.AcquireLongPoll = 25 * time.Second
	c.InfraRetryMax = 2
	c.CancelGrace = 30 * time.Second
	c.SweepInterval = 5 * time.Second
	// 默认按 webhook 事件工作，不做周期对账
	c.ReconcileInterval = 0
	c.ReconcileDelay = 30 * time.Second
	c.Retention = retention.DefaultRetention
	c.RetentionInterval = retention.DefaultInterval
}

// DefaultConfig 返回只填好默认值的配置（测试和嵌入使用）。
func DefaultConfig() *Config {
	c := &Config{Listen: defaultListen, DataDir: defaultDataDir}
	c.setDefaults()
	return c
}

// LoadConfig 从环境变量读取配置。
func LoadConfig() (*Config, error) {
	c := &Config{
		PublicURL:           strings.TrimRight(os.Getenv(envPublicURL), "/"),
		Listen:              envOr(envListen, defaultListen),
		DatabaseURL:         os.Getenv(envDatabaseURL),
		DataDir:             envOr(envDataDir, defaultDataDir),
		GitHubWebhookSecret: os.Getenv(envGitHubWebhookSecret),
		GitHubAPIURL:        os.Getenv(envGitHubAPIURL),
		GitHubUploadURL:     os.Getenv(envGitHubUploadURL),
		GitHubClientID:      os.Getenv(envGitHubClientID),
		GitHubClientSecret:  os.Getenv(envGitHubClientSecret),
		AllowedAccounts:     splitSet(os.Getenv(envAllowedAccounts)),
		Admins:              splitSet(os.Getenv(envAdmins)),
		Users:               splitSet(os.Getenv(envUsers)),
		AdminToken:          os.Getenv(envAdminToken),
	}
	c.setDefaults()
	if c.PublicURL == "" {
		c.PublicURL = defaultPublicURL(c.Listen)
	}
	if err := checkPublicURL(c.PublicURL); err != nil {
		return nil, err
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("需要设置 " + envDatabaseURL)
	}
	if c.AdminToken == "" {
		if f := os.Getenv(envAdminTokenFile); f != "" {
			b, err := readConfigFile(f)
			if err != nil {
				return nil, err
			}
			c.AdminToken = strings.TrimSpace(string(b))
		}
	}
	if len(c.AdminToken) < minAdminTokenLen {
		return nil, fmt.Errorf("需要设置 %s（至少 %d 个字符），kiln CLI 和 Dashboard 登录使用它", envAdminToken, minAdminTokenLen)
	}
	if s := os.Getenv(envGitHubAppID); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s 无效：%w", envGitHubAppID, err)
		}
		c.GitHubAppID = id
	}
	if f := os.Getenv(envGitHubPrivateKeyFile); f != "" {
		b, err := readConfigFile(f)
		if err != nil {
			return nil, fmt.Errorf("读取 %s：%w", envGitHubPrivateKeyFile, err)
		}
		c.GitHubPrivateKey = b
	} else if s := os.Getenv(envGitHubPrivateKey); s != "" {
		c.GitHubPrivateKey = []byte(s)
	}
	if c.GitHubAppID != 0 && len(c.GitHubPrivateKey) == 0 {
		return nil, fmt.Errorf("设置了 %s，还需要 %s", envGitHubAppID, envGitHubPrivateKeyFile)
	}
	if c.GitHubAppID != 0 && c.GitHubWebhookSecret == "" {
		return nil, errors.New("需要设置 " + envGitHubWebhookSecret)
	}
	if (c.GitHubClientID == "") != (c.GitHubClientSecret == "") {
		return nil, fmt.Errorf("需要同时设置 %s 和 %s（Dashboard 的 GitHub 登录）", envGitHubClientID, envGitHubClientSecret)
	}
	if c.GitHubAppID != 0 && len(c.AllowedAccounts) == 0 {
		return nil, fmt.Errorf("需要设置 %s（允许安装 App 的 GitHub 账号，逗号分隔，PLAN #16.1）", envAllowedAccounts)
	}
	key, err := loadMasterKey()
	if err != nil {
		return nil, err
	}
	c.MasterKey = key
	if err := c.loadReconcile(); err != nil {
		return nil, err
	}
	if s := os.Getenv(envLeaseTTL); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("%s 无效：%w", envLeaseTTL, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s 无效：%s，应大于 0", envLeaseTTL, s)
		}
		c.LeaseTTL = d
	}
	if s := os.Getenv(envRetention); s != "" {
		d, err := parseRetention(s)
		if err != nil {
			return nil, fmt.Errorf("%s 无效：%w", envRetention, err)
		}
		c.Retention = d
	}
	return c, nil
}

// defaultPublicURL 是没有设置 KILN_PUBLIC_URL 时由监听地址推出的地址：监听所有地址（:8080、0.0.0.0:8080）时用 localhost，
// 否则用监听的主机。监听地址无法解析时退回 http://localhost:8080 的形式。
func defaultPublicURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://localhost" + listen
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// checkPublicURL 检查 KILN_PUBLIC_URL：必须是带主机的 http(s) 地址，不带用户信息、查询参数和 fragment。
// 它拼成 OAuth 回调地址、CAS 预签名地址和 Runner 的注册命令，并决定会话 cookie 是否加 Secure：
// 例如漏写 https:// 时 GitHub 登录无法回调，通过 https 访问的 Dashboard 的 cookie 也不会加 Secure。
func checkPublicURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return fmt.Errorf("%s 无效：%w", envPublicURL, err)
	case u.Scheme != "http" && u.Scheme != "https", u.Host == "":
		return fmt.Errorf("%s 无效：%q，应写成 https://主机[:端口]（例如 https://kiln.example.com）", envPublicURL, s)
	case u.User != nil, u.RawQuery != "", u.Fragment != "", strings.ContainsAny(s, "?#"):
		return fmt.Errorf("%s 无效：%q，不能带用户信息、查询参数或 fragment", envPublicURL, s)
	}
	return nil
}

// parseRetention 解析保留时间：Go 的时长（例如 168h）或天数（例如 7d），不能少于 minRetention。
func parseRetention(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q 不是天数（例如 7d）", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("%q 不是时长（例如 7d 或 168h）", s)
		}
	}
	if d < minRetention {
		return 0, fmt.Errorf("%q 太短，至少 %s", s, minRetention)
	}
	return d, nil
}

// loadReconcile 读取可选的周期对账（PLAN #7.3）：KILN_RECONCILE_INTERVAL 和 KILN_RECONCILE_REPOS 必须同时设置，
// 范围是仓库全名（owner/name，逗号分隔）或 "*"（全部启用中的仓库）。默认都不设置，不做周期对账。
func (c *Config) loadReconcile() error {
	iv, repos := os.Getenv(envReconcileInterval), os.Getenv(envReconcileRepos)
	if iv == "" && repos == "" {
		return nil
	}
	if iv == "" || repos == "" {
		return fmt.Errorf("开启周期对账需要同时设置 %s（例如 15m）和 %s（owner/name 列表，或 *）", envReconcileInterval, envReconcileRepos)
	}
	d, err := time.ParseDuration(iv)
	if err != nil {
		return fmt.Errorf("%s 无效：%w", envReconcileInterval, err)
	}
	if d < minReconcileInterval {
		return fmt.Errorf("%s 太短：%s，至少 %s", envReconcileInterval, d, minReconcileInterval)
	}
	set := splitSet(repos)
	list := make([]string, 0, len(set))
	for r := range set {
		owner, name, ok := strings.Cut(r, "/")
		if r != "*" && (!ok || owner == "" || name == "" || strings.Contains(name, "/")) {
			return fmt.Errorf("%s 中的 %q 应写成 owner/name（或 * 表示全部仓库）", envReconcileRepos, r)
		}
		list = append(list, r)
	}
	slices.Sort(list)
	c.ReconcileInterval, c.ReconcileRepos = d, list
	return nil
}

func loadMasterKey() ([]byte, error) {
	var raw string
	if f := os.Getenv(envMasterKeyFile); f != "" {
		b, err := readConfigFile(f)
		if err != nil {
			return nil, fmt.Errorf("读取 %s：%w", envMasterKeyFile, err)
		}
		raw = strings.TrimSpace(string(b))
	} else {
		raw = os.Getenv(envMasterKey)
	}
	if raw == "" {
		return nil, fmt.Errorf("需要设置 %s（32 字节密钥，hex 或 base64；可用 `openssl rand -hex 32` 生成），用于加密 secret", envMasterKeyFile)
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == secretbox.KeySize {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == secretbox.KeySize {
		return b, nil
	}
	return nil, errors.New("master key 应为 32 字节（64 位 hex 或 base64）")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		m[strings.ToLower(strings.TrimSpace(p))] = true
	}
	return m
}

// readConfigFile 读取环境变量指定的文件（admin token、App 私钥、主密钥）。
func readConfigFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // 路径来自运维设置的环境变量，不是外部输入
}

// LoadEnvFile 读取 KEY=VALUE 格式的文件（# 开头为注释，可以带 export 前缀，值可以用引号括起）并设置为环境变量；
// 已经设置的环境变量优先，不会被文件覆盖。
func LoadEnvFile(path string) error {
	b, err := readConfigFile(path)
	if err != nil {
		return err
	}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return fmt.Errorf("%s:%d：应为 KEY=VALUE", path, n+1)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); set {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("%s:%d：设置 %s：%w", path, n+1, k, err)
		}
	}
	return nil
}
