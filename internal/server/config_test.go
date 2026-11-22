package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// configEnvs 是 LoadConfig 读取的全部环境变量。
var configEnvs = []string{
	envPublicURL, envListen, envDatabaseURL, envDataDir, envGitHubWebhookSecret, envGitHubAPIURL, envGitHubUploadURL,
	envGitHubClientID, envGitHubClientSecret, envAllowedAccounts, envAdmins, envUsers, envAdminToken, envAdminTokenFile,
	envGitHubAppID, envGitHubPrivateKey, envGitHubPrivateKeyFile, envMasterKey, envMasterKeyFile, envLeaseTTL,
	envReconcileInterval, envReconcileRepos, envRetention,
}

// testMasterKey 是 32 字节的主密钥。
var testMasterKey = bytes.Repeat([]byte{7}, 32)

// minimalEnv 是 LoadConfig 成功所需的最少环境变量（不配置 GitHub App）。
func minimalEnv() map[string]string {
	return map[string]string{
		envDatabaseURL: "postgres://db/kiln",
		envAdminToken:  "admin-token-0123456789",
		envMasterKey:   hex.EncodeToString(testMasterKey),
	}
}

// setConfigEnv 清空全部配置环境变量（空字符串与未设置等价），再设置 env 中的值；测试结束后恢复。
func setConfigEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range configEnvs {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// writeTemp 把 content 写入临时目录中的文件，返回路径。
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadConfigDefaults：只设置必需的环境变量时，其余取 PLAN #20.1 的默认值。
func TestLoadConfigDefaults(t *testing.T) {
	setConfigEnv(t, minimalEnv())
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Listen", c.Listen, ":8080"},
		{"DataDir", c.DataDir, "./kiln-data"},
		{"PublicURL", c.PublicURL, "http://localhost:8080"},
		{"DatabaseURL", c.DatabaseURL, "postgres://db/kiln"},
		{"GitHubAppID", c.GitHubAppID, int64(0)},
		{"LeaseTTL", c.LeaseTTL, 60 * time.Second},
		{"AcquireLongPoll", c.AcquireLongPoll, 25 * time.Second},
		{"InfraRetryMax", c.InfraRetryMax, 2},
		{"CancelGrace", c.CancelGrace, 30 * time.Second},
		{"SweepInterval", c.SweepInterval, 5 * time.Second},
		{"ReconcileInterval", c.ReconcileInterval, time.Duration(0)},
		{"ReconcileDelay", c.ReconcileDelay, 30 * time.Second},
		{"Retention", c.Retention, 12 * time.Hour},
		{"RetentionInterval", c.RetentionInterval, time.Hour},
		{"AllowedAccounts", len(c.AllowedAccounts), 0},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v，期望 %v", ch.name, ch.got, ch.want)
		}
	}
	if !bytes.Equal(c.MasterKey, testMasterKey) {
		t.Errorf("MasterKey = %x", c.MasterKey)
	}
	// DefaultConfig 与 LoadConfig 的默认值一致
	d := DefaultConfig()
	if d.Listen != c.Listen || d.DataDir != c.DataDir || d.LeaseTTL != c.LeaseTTL || d.ReconcileDelay != c.ReconcileDelay || d.Retention != c.Retention {
		t.Errorf("DefaultConfig = %+v 与 LoadConfig 的默认值不一致", d)
	}
}

// TestLoadConfigValues：各环境变量的读取和规范化（地址去掉末尾的 /，账号列表小写、多种分隔符）。
func TestLoadConfigValues(t *testing.T) {
	keyPEM := testKeyPEM(t)
	env := minimalEnv()
	maps.Copy(env, map[string]string{
		envPublicURL:            "https://kiln.example.com/",
		envListen:               "127.0.0.1:9090",
		envDataDir:              "/var/lib/kiln",
		envGitHubAppID:          "4242",
		envGitHubPrivateKeyFile: writeTemp(t, "app.pem", keyPEM),
		envGitHubWebhookSecret:  "whsecret",
		envGitHubAPIURL:         "https://ghe.example/api/v3",
		envGitHubUploadURL:      "https://ghe.example/api/uploads",
		envGitHubClientID:       "cid",
		envGitHubClientSecret:   "csecret",
		envAllowedAccounts:      "Alice, 42;bob",
		envAdmins:               "Root",
		envUsers:                "dev1 dev2",
		envLeaseTTL:             "90s",
		envReconcileInterval:    "15m",
		envReconcileRepos:       "alice/app",
		envRetention:            "3d",
	})
	setConfigEnv(t, env)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://kiln.example.com" || c.Listen != "127.0.0.1:9090" || c.DataDir != "/var/lib/kiln" {
		t.Errorf("地址 = %q %q %q", c.PublicURL, c.Listen, c.DataDir)
	}
	if c.GitHubAppID != 4242 || string(c.GitHubPrivateKey) != keyPEM || c.GitHubWebhookSecret != "whsecret" {
		t.Errorf("GitHub App = %d %q", c.GitHubAppID, c.GitHubWebhookSecret)
	}
	if c.GitHubAPIURL != "https://ghe.example/api/v3" || c.GitHubUploadURL != "https://ghe.example/api/uploads" ||
		c.GitHubClientID != "cid" || c.GitHubClientSecret != "csecret" {
		t.Errorf("GitHub 地址和 OAuth = %+v", c)
	}
	for name, got := range map[string]map[string]bool{"AllowedAccounts": c.AllowedAccounts, "Admins": c.Admins, "Users": c.Users} {
		want := map[string]map[string]bool{
			"AllowedAccounts": {"alice": true, "42": true, "bob": true},
			"Admins":          {"root": true},
			"Users":           {"dev1": true, "dev2": true},
		}[name]
		if !maps.Equal(got, want) {
			t.Errorf("%s = %v，期望 %v", name, got, want)
		}
	}
	if c.LeaseTTL != 90*time.Second || c.ReconcileInterval != 15*time.Minute || len(c.ReconcileRepos) != 1 {
		t.Errorf("LeaseTTL %s、周期对账 %s %v", c.LeaseTTL, c.ReconcileInterval, c.ReconcileRepos)
	}
	if c.Retention != 72*time.Hour {
		t.Errorf("Retention = %s", c.Retention)
	}
}

// TestParseRetention：保留时间写成天数或 Go 的时长，至少 1 小时。
func TestParseRetention(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour, "168h": 168 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseRetention(in); err != nil || got != want {
			t.Errorf("parseRetention(%q) = %s %v，期望 %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7", "xd", "-1d", "0d", "30m", "7 days"} {
		if _, err := parseRetention(in); err == nil {
			t.Errorf("parseRetention(%q) 应返回错误", in)
		}
	}
}

// TestLoadConfigPublicURLFromListen：没有设置 KILN_PUBLIC_URL 时由监听地址推出。
func TestLoadConfigPublicURLFromListen(t *testing.T) {
	env := minimalEnv()
	env[envListen] = ":9999"
	setConfigEnv(t, env)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "http://localhost:9999" {
		t.Fatalf("PublicURL = %q", c.PublicURL)
	}
}

// TestDefaultPublicURL：监听所有地址时用 localhost，监听具体主机时用这个主机。
func TestDefaultPublicURL(t *testing.T) {
	for listen, want := range map[string]string{
		":8080":          "http://localhost:8080",
		"0.0.0.0:8080":   "http://localhost:8080",
		"[::]:8080":      "http://localhost:8080",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"[::1]:9000":     "http://[::1]:9000",
		"kiln.lan:80":    "http://kiln.lan:80",
	} {
		if got := defaultPublicURL(listen); got != want {
			t.Errorf("defaultPublicURL(%q) = %q，期望 %q", listen, got, want)
		}
	}
}

// TestLoadConfigSecretsFromFiles：admin token、私钥和主密钥可以从文件读取（去掉首尾空白），私钥也可以直接写在环境变量中。
func TestLoadConfigSecretsFromFiles(t *testing.T) {
	keyPEM := testKeyPEM(t)
	setConfigEnv(t, map[string]string{
		envDatabaseURL:         "postgres://db/kiln",
		envAdminTokenFile:      writeTemp(t, "token", "  file-token-0123456789\n"),
		envMasterKeyFile:       writeTemp(t, "master", base64.StdEncoding.EncodeToString(testMasterKey)+"\n"),
		envGitHubAppID:         "1",
		envGitHubPrivateKey:    keyPEM,
		envGitHubWebhookSecret: "s",
		envAllowedAccounts:     "a",
	})
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken != "file-token-0123456789" {
		t.Errorf("AdminToken = %q", c.AdminToken)
	}
	if !bytes.Equal(c.MasterKey, testMasterKey) {
		t.Errorf("MasterKey = %x", c.MasterKey)
	}
	if string(c.GitHubPrivateKey) != keyPEM {
		t.Error("没有读取 KILN_GITHUB_PRIVATE_KEY")
	}
	// 环境变量中的 admin token 优先于文件
	t.Setenv(envAdminToken, "env-token-0123456789")
	if c, err := LoadConfig(); err != nil || c.AdminToken != "env-token-0123456789" {
		t.Errorf("AdminToken = %q, %v，期望使用环境变量", c.AdminToken, err)
	}
}

// TestLoadConfigErrors：缺少必需的配置或取值无效时返回指出变量名的错误。
func TestLoadConfigErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	app := map[string]string{envGitHubAppID: "1", envGitHubPrivateKey: "pem", envGitHubWebhookSecret: "s", envAllowedAccounts: "a"}
	tests := []struct {
		name    string
		set     map[string]string
		unset   []string
		wantErr string
	}{
		{name: "缺少数据库", unset: []string{envDatabaseURL}, wantErr: envDatabaseURL},
		{name: "公网地址没有协议", set: map[string]string{envPublicURL: "kiln.example.com"}, wantErr: envPublicURL},
		{name: "公网地址协议不对", set: map[string]string{envPublicURL: "ftp://kiln.example.com"}, wantErr: envPublicURL},
		{name: "公网地址没有主机", set: map[string]string{envPublicURL: "https://"}, wantErr: envPublicURL},
		{name: "公网地址带用户信息", set: map[string]string{envPublicURL: "https://u:p@kiln.example.com"}, wantErr: envPublicURL},
		{name: "公网地址带查询参数", set: map[string]string{envPublicURL: "https://kiln.example.com/?a=1"}, wantErr: envPublicURL},
		{name: "缺少 admin token", unset: []string{envAdminToken}, wantErr: envAdminToken},
		{name: "admin token 太短", set: map[string]string{envAdminToken: "short"}, wantErr: "至少 16 个字符"},
		{name: "admin token 文件不存在", unset: []string{envAdminToken}, set: map[string]string{envAdminTokenFile: missing}, wantErr: "missing"},
		{name: "App ID 无效", set: map[string]string{envGitHubAppID: "abc"}, wantErr: envGitHubAppID},
		{name: "私钥文件不存在", set: map[string]string{envGitHubPrivateKeyFile: missing}, wantErr: envGitHubPrivateKeyFile},
		{name: "App 缺少私钥", set: map[string]string{envGitHubAppID: "1"}, wantErr: envGitHubPrivateKeyFile},
		{name: "App 缺少 webhook 密钥", set: app, unset: []string{envGitHubWebhookSecret}, wantErr: envGitHubWebhookSecret},
		{name: "App 缺少白名单", set: app, unset: []string{envAllowedAccounts}, wantErr: envAllowedAccounts},
		{name: "缺少主密钥", unset: []string{envMasterKey}, wantErr: envMasterKeyFile},
		{name: "主密钥长度不对", set: map[string]string{envMasterKey: hex.EncodeToString([]byte("short"))}, wantErr: "32 字节"},
		{name: "主密钥不是 hex 或 base64", set: map[string]string{envMasterKey: "not a key!"}, wantErr: "32 字节"},
		{name: "主密钥文件不存在", unset: []string{envMasterKey}, set: map[string]string{envMasterKeyFile: missing}, wantErr: envMasterKeyFile},
		{name: "租约时间无效", set: map[string]string{envLeaseTTL: "soon"}, wantErr: envLeaseTTL},
		{name: "租约时间不是正数", set: map[string]string{envLeaseTTL: "0s"}, wantErr: envLeaseTTL},
		{name: "GitHub 登录只设 client ID", set: map[string]string{envGitHubClientID: "cid"}, wantErr: envGitHubClientSecret},
		{name: "GitHub 登录只设 client secret", set: map[string]string{envGitHubClientSecret: "cs"}, wantErr: envGitHubClientID},
		{name: "保留时间太短", set: map[string]string{envRetention: "7m"}, wantErr: envRetention},
		{name: "周期对账只设一项", set: map[string]string{envReconcileInterval: "15m"}, wantErr: envReconcileRepos},
		{name: "周期对账间隔无效", set: map[string]string{envReconcileInterval: "x", envReconcileRepos: "*"}, wantErr: envReconcileInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := minimalEnv()
			maps.Copy(env, tt.set)
			for _, k := range tt.unset {
				delete(env, k)
			}
			setConfigEnv(t, env)
			c, err := LoadConfig()
			if err == nil {
				t.Fatalf("LoadConfig() = %+v，期望出错", c)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误 %q 中没有 %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadEnvFileMissing：文件不存在时返回错误。
func TestLoadEnvFileMissing(t *testing.T) {
	t.Parallel()
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "none.env")); err == nil {
		t.Fatal("文件不存在时应返回错误")
	}
}

// TestLoadReconcile：周期对账默认关闭；开启时两项都要设置，范围写仓库全名或 *。
func TestLoadReconcile(t *testing.T) {
	tests := []struct {
		name, interval, repos string
		wantErr               bool
		wantRepos             []string
	}{
		{name: "默认关闭"},
		{name: "只设间隔", interval: "15m", wantErr: true},
		{name: "只设范围", repos: "a/b", wantErr: true},
		{name: "间隔太短", interval: "10s", repos: "a/b", wantErr: true},
		{name: "范围格式错误", interval: "15m", repos: "a", wantErr: true},
		{name: "仓库列表", interval: "15m", repos: "Owner/B, owner/a", wantRepos: []string{"owner/a", "owner/b"}},
		{name: "全部仓库", interval: "1h", repos: "*", wantRepos: []string{"*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KILN_RECONCILE_INTERVAL", tt.interval)
			t.Setenv("KILN_RECONCILE_REPOS", tt.repos)
			c := DefaultConfig()
			err := c.loadReconcile()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，期望出错 %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !slices.Equal(c.ReconcileRepos, tt.wantRepos) {
				t.Errorf("范围 %v，期望 %v", c.ReconcileRepos, tt.wantRepos)
			}
			if (c.ReconcileInterval > 0) != (tt.wantRepos != nil) {
				t.Errorf("间隔 %s 与范围 %v 不一致", c.ReconcileInterval, c.ReconcileRepos)
			}
		})
	}
}

func TestLoadEnvFile(t *testing.T) {
	const (
		keyPlain  = "KILN_TEST_ENVFILE_PLAIN"
		keyQuoted = "KILN_TEST_ENVFILE_QUOTED"
		keyExport = "KILN_TEST_ENVFILE_EXPORT"
		keySet    = "KILN_TEST_ENVFILE_SET"
	)
	// t.Setenv 让测试结束后恢复这些变量；先清空，再由 LoadEnvFile 设置。
	for _, k := range []string{keyPlain, keyQuoted, keyExport} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(keySet, "already")

	p := filepath.Join(t.TempDir(), "server.env")
	content := "# 注释\n\n" +
		keyPlain + "=a b\n" +
		keyQuoted + `="quoted"` + "\n" +
		"export " + keyExport + "='single'\n" +
		keySet + "=override\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(p); err != nil {
		t.Fatalf("LoadEnvFile() error = %v", err)
	}
	want := map[string]string{keyPlain: "a b", keyQuoted: "quoted", keyExport: "single", keySet: "already"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q，期望 %q", k, got, v)
		}
	}
}

func TestLoadEnvFileInvalidLine(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(p, []byte("NOT_A_PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := LoadEnvFile(p)
	if err == nil || !strings.Contains(err.Error(), "bad.env:1") {
		t.Fatalf("LoadEnvFile() error = %v，期望指出 bad.env:1", err)
	}
}
