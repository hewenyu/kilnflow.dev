// Package runnerkit 是 Runner 的共用机制（PLAN #30.3）：数据目录与身份、注册与认证、调用与重试、日志上传、自升级与启动器、
// 服务安装、数据目录锁和 drain。现在只有 kiln-runner（CI）使用；AI 执行节点是独立的 npm 连接器（kiln-orca，PLAN #33），
// 不复用这里。只放机制，不含领域判断；不依赖 internal/runner 及其领域包（依赖测试检查）。
package runnerkit

import (
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 数据目录中两种 Runner 共用的部分（PLAN #8.3），路径都相对于数据目录。
const (
	// ConfigFileName 是配置文件。
	ConfigFileName = "config.toml"
	stateFileName  = "runner.json"    // 注册后保存的身份信息
	keysDirName    = "keys"           // 私钥所在目录（0700）
	keyFileName    = "runner.ed25519" // keys/ 下的 ed25519 私钥（0600）
	drainFileName  = "drain"          // 存在时不再领取新任务（drain 命令）
	// LogsDirName 是服务的日志目录：<日志名>-<日期>.log（按天切分，保留 7 天）。
	LogsDirName     = "logs"
	consoleFileName = "runner.out.log" // logs/ 下服务管理器保存的 stdout / stderr（只有启动失败、崩溃等输出）
	// lockFileName 是数据目录中的锁文件：run 运行期间持有（flock）。
	lockFileName = "runner.lock"
)

const (
	// Retention 是过程数据（本地缓存、镜像、工作区）没有用到时的保留时间（C10）。
	Retention = 12 * time.Hour
	// GCInterval 是两次按保留策略清理的最小间隔（Runner 空闲时才清理）。
	GCInterval = time.Hour
)

// Program 描述一种 Runner 程序（kiln-runner）：命令名、注册令牌的类型、数据目录和服务的名称。
type Program struct {
	// Name 是命令名，也是自升级布局和 npm 包中的程序名。
	Name string
	// Kind 是这个程序接受的注册令牌的 Runner 类型（ci | ai）。
	Kind string
	// WrongKind 返回注册令牌的类型（kind）不是 Kind 时的错误说明。
	WrongKind func(kind string) string
	// DirEnv 是指定数据目录的环境变量；DefaultDir 是主目录下的默认数据目录名。
	DirEnv, DefaultDir string
	// LogName 是服务日志文件名的前缀：logs/<LogName>-<日期>.log。
	LogName string
	// LaunchdLabel、SystemdUnit、Description 是服务的 LaunchAgent 标签、systemd unit 名称和说明。
	LaunchdLabel, SystemdUnit, Description string
}

// DataDir 返回程序的数据目录：$DirEnv 或 ~/<DefaultDir>。
func (p *Program) DataDir() string {
	if d := os.Getenv(p.DirEnv); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, p.DefaultDir)
}

// Home 是一个 Runner 的数据目录。
type Home struct {
	Dir  string
	Prog *Program
}

// Path 返回数据目录中的路径。
func (h *Home) Path(parts ...string) string {
	return filepath.Join(append([]string{h.Dir}, parts...)...)
}

// LogDir 返回服务的日志目录：service install 让 Runner 把日志写到其中按天切分的 <日志名>-<日期>.log。
func (h *Home) LogDir() string { return h.Path(LogsDirName) }

// ConsolePath 返回服务管理器保存 stdout / stderr 的文件（日志已经写到 LogDir，这里只有启动失败、崩溃等输出）。
func (h *Home) ConsolePath() string { return h.Path(LogsDirName, consoleFileName) }

// Common 是两种 Runner 的 config.toml 共有的设置（嵌入各自的配置）。
type Common struct {
	Server string `toml:"server"`
	Name   string `toml:"name"`
	// Upgrade 为 false 时本地关闭自升级（PLAN #27.7"本地关闭"）：不下载、不切换，上报 manual 和 disabled，Server 不接受对它的升级请求。默认允许。
	Upgrade *bool `toml:"upgrade,omitempty"`
}

// UpgradeEnabled 判断是否允许自升级（config.toml 的 upgrade，默认允许）。
func (c *Common) UpgradeEnabled() bool { return c.Upgrade == nil || *c.Upgrade }

// ServerURL 返回 Server 地址：config.toml 中没有设置时用注册时保存的地址。
func (c *Common) ServerURL(st *State) string {
	return strings.TrimRight(cmp.Or(c.Server, st.Server), "/")
}

// State 是注册后保存的身份信息（<数据目录>/runner.json）。
type State struct {
	Server   string `json:"server"`
	RunnerID int64  `json:"runner_id"`
	Name     string `json:"name"`
	Pool     string `json:"pool"`
}

// LoadState 读取注册信息。
func (h *Home) LoadState() (*State, error) {
	b, err := os.ReadFile(h.Path(stateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("还没有注册，先执行 %s enroll", h.Prog.Name)
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("解析 %s：%w", h.Path(stateFileName), err)
	}
	return &s, nil
}

// SaveState 保存注册信息（0600，原子写：崩溃时不会留下半截文件）。
func (h *Home) SaveState(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(h.Path(stateFileName), append(b, '\n'), 0o600)
}

// keyReadAttempts / keyReadDelay 是读到另一个进程正在写的私钥前的重试次数和间隔。
const (
	keyReadAttempts = 50
	keyReadDelay    = 20 * time.Millisecond
)

// Key 读取 ed25519 私钥（keys/runner.ed25519，权限 600）；create 为 true 时不存在就生成。
// 生成时用 O_EXCL 创建：两个进程同时首次启动时只有一个写入，另一个等待后读它写好的私钥。
func (h *Home) Key(create bool) (ed25519.PrivateKey, error) {
	p := h.Path(keysDirName, keyFileName)
	b, err := os.ReadFile(p)
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s 格式不正确", p)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !create {
		return nil, fmt.Errorf("读取私钥 %s：%w", p, err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	seed := []byte(hex.EncodeToString(priv.Seed()) + "\n")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// 另一个进程同时生成了私钥：等它写完再用它的，不覆盖（覆盖会让先写入的一方失去身份）
		var lastErr error
		for range keyReadAttempts {
			key, rerr := h.Key(false)
			if rerr == nil {
				return key, nil
			}
			lastErr = rerr
			time.Sleep(keyReadDelay)
		}
		return nil, lastErr
	}
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(seed)
	if werr == nil {
		werr = f.Sync()
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(p) // 没写完整的私钥不能留下
		return nil, err
	}
	return priv, nil
}

// Draining 判断是否设置了本地 drain 标记（drain 命令）。
func (h *Home) Draining() bool {
	_, err := os.Stat(h.Path(drainFileName))
	return err == nil
}

// SetDrain 设置或清除本地 drain 标记（drain / undrain 命令）。
func (h *Home) SetDrain(on bool) error {
	if on {
		return os.WriteFile(h.Path(drainFileName), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	}
	err := os.Remove(h.Path(drainFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
