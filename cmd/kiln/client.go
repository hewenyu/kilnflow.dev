package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
)

// 请求超时：日志等接口可能返回较多数据，留足时间；登录只验证 token。
const (
	apiTimeout   = 5 * time.Minute
	loginTimeout = 30 * time.Second
)

// cliConfig 是 kiln login 保存的配置（cli.json）。
type cliConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// errNoConfigPath 表示无法确定 cli.json 的位置。
var errNoConfigPath = errors.New("无法确定配置文件的位置（没有设置 HOME）：设置 HOME，或者用环境变量 KILN_SERVER、KILN_ADMIN_TOKEN")

// defaultConfigPath 返回 cli.json 的默认位置；无法确定用户配置目录时返回空字符串（loadConfig 只读环境变量，saveConfig 报错）。
// 不能退回到相对路径：否则 admin token 会写进当前目录，而当前目录可能是某个仓库的工作区。
func defaultConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	// UserConfigDir 在 macOS 上直接拼接 HOME，HOME 是相对路径时结果也是相对路径
	if !filepath.IsAbs(d) {
		return ""
	}
	return filepath.Join(d, "kiln", "cli.json")
}

// loadConfig 读取 path（不存在或 path 为空时为空配置），再用 KILN_SERVER、KILN_ADMIN_TOKEN 覆盖。
func loadConfig(path string) (cliConfig, error) {
	var c cliConfig
	var b []byte
	err := fs.ErrNotExist
	if path != "" {
		b, err = os.ReadFile(path)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return c, fmt.Errorf("读取 %s：%w", path, err)
	default:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s 已损坏，请修复或删除后重新 kiln login：%w", path, err)
		}
	}
	if v := os.Getenv("KILN_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("KILN_ADMIN_TOKEN"); v != "" {
		c.Token = v
	}
	c.Server = strings.TrimRight(c.Server, "/")
	return c, nil
}

// configPerm 是 cli.json 的权限：其中有 admin token，只允许本人访问。
const configPerm = 0o600

func saveConfig(path string, c cliConfig) error {
	if path == "" {
		return errNoConfigPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录：%w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, configPerm)
	if err != nil {
		return fmt.Errorf("保存配置：%w", err)
	}
	return writeConfigFile(f, b)
}

// configFile 是 writeConfigFile 用到的文件操作（*os.File）。
type configFile interface {
	Chmod(mode os.FileMode) error
	Write(b []byte) (int, error)
	Close() error
}

// writeConfigFile 把配置 b 写入已打开的 f 并关闭它。打开时的权限只在新建时生效：已有的文件（例如手工创建、权限更宽）
// 先收紧再写入 token，不能先写入再收紧，否则中间其他用户可以读到 token；收紧失败时不写入。
func writeConfigFile(f configFile, b []byte) error {
	if err := f.Chmod(configPerm); err != nil {
		_ = f.Close()
		return fmt.Errorf("设置配置文件的权限：%w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("保存配置：%w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("保存配置：%w", err)
	}
	return nil
}

// authed 返回给每个 unary 请求加上 admin token 的拦截器（streaming 请求原样通过）。
func authed(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

func newClient(c cliConfig, timeout time.Duration) apiv1connect.AdminServiceClient {
	hc := &http.Client{Timeout: timeout, CheckRedirect: safeRedirect}
	return apiv1connect.NewAdminServiceClient(hc, c.Server, connect.WithInterceptors(authed(c.Token)))
}

// maxRedirects 是允许的最大重定向次数（与 net/http 的默认值一致）。
const maxRedirects = 10

// safeRedirect 是 CLI 的重定向策略：admin token 只发给配置的 Server。拒绝 https 降级到 http（任何一跳是 https
// 都不允许后续变回 http）、限制重定向次数，跨 origin（协议 + 主机 + 有效端口）时去掉 Authorization。
func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("重定向次数超过 %d 次", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		for _, v := range via {
			if v.URL.Scheme == "https" {
				return errors.New("拒绝从 https 重定向到 http（admin token 会以明文传输）")
			}
		}
	}
	if !sameOrigin(via[0].URL, req.URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

// sameOrigin 判断两个地址的协议、主机名和有效端口是否相同（省略端口时按协议补默认端口）。
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && portOf(a) == portOf(b)
}

// portOf 返回地址的有效端口：显式端口，或协议对应的默认端口（https 443、http 80）。
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// client 按保存的配置创建 AdminService 客户端，并记下 Server 地址。
func (a *app) client() (apiv1connect.AdminServiceClient, error) {
	c, err := loadConfig(a.configPath)
	if err != nil {
		return nil, err
	}
	if c.Server == "" || c.Token == "" {
		return nil, errors.New("还没有登录：kiln login --server https://kiln.example.com")
	}
	a.server = c.Server
	return newClient(c, apiTimeout), nil
}

func req[T any](m *T) *connect.Request[T] { return connect.NewRequest(m) }
