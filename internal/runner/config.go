// Package runner 实现 kiln-runner：注册、领取任务、获取源码和 runtime、执行 step、上传输出（PLAN #8）。
package runner

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 数据目录中 kiln-runner 独有的布局（PLAN #8.3），路径都相对于 Config.Dir；共用的部分（config.toml、runner.json、
// keys/、drain、logs/）见 runnerkit。
const (
	workDirName      = "work"      // work/<attempt>/：每个 attempt 的工作区（见 attemptDirs）
	casDirName       = "cas"       // 本地 CAS 缓存
	rtDirName        = "rt"        // rt/<digest hex>/：解包后的 runtime
	gitDirName       = "git"       // git/<repo id>.git：每个仓库一个 bare 镜像
	toolcacheDirName = "toolcache" // toolcache/<执行身份>/<repo id>/<平台>/<名称>/
	buildkitDirName  = "buildkit"  // 从 BuildKit 镜像中取出的 buildctl
	logsDirName      = runnerkit.LogsDirName
)

// Program 是 kiln-runner：注册令牌的类型为 ci，数据目录默认 ~/kiln（$KILN_RUNNER_DIR）。
var Program = &runnerkit.Program{
	Name: "kiln-runner", Kind: "ci",
	WrongKind: func(kind string) string {
		return fmt.Sprintf("这是 %s 类型 Runner 的注册令牌，请用 AI 执行节点的连接器（kiln-orca）注册；kiln-runner 需要 CI Runner 的令牌（kiln runner token）", kind)
	},
	DirEnv: "KILN_RUNNER_DIR", DefaultDir: "kiln", LogName: "runner",
	LaunchdLabel: "dev.kiln.runner", SystemdUnit: "kiln-runner", Description: "Kiln runner",
}

// LogName 是服务日志文件名的前缀：logs/runner-<日期>.log。
var LogName = Program.LogName

// dockerDisabled 是 Config.Docker 中表示关闭 oci executor 的值。
const dockerDisabled = "-"

// 配置的默认值。
const (
	defaultMaxJobs       = 4
	defaultReserveCPU    = 2
	defaultReserveMemory = "4Gi"
	// defaultReserveMemoryBytes 与 defaultReserveMemory 相同，用于 ReserveMemory 无法解析时。
	defaultReserveMemoryBytes = 4 << 30
	defaultDocker             = "docker"
	defaultToolcacheMaxSize   = 10 << 30
	defaultDiskHigh           = 80
	defaultDiskLow            = 65
)

// Config 是 ~/kiln/config.toml。
type Config struct {
	runnerkit.Common
	// MaxJobs 是同时执行的 job 数上限，默认 4。
	MaxJobs int `toml:"max_jobs"`
	// ReserveCPU / ReserveMemory 为宿主机保留的资源（PLAN #8.6），默认 2 核、4Gi。
	ReserveCPU    float64 `toml:"reserve_cpu"`
	ReserveMemory string  `toml:"reserve_memory"`
	// ExtraPath 追加到 host executor 的 PATH（在 runtime 之后、系统目录之前），例如 /opt/homebrew/bin。
	// 这些目录中的工具不计入 action key，只放自己确信不影响产物的东西。
	ExtraPath []string `toml:"extra_path"`
	// Docker 是 docker 命令的路径，默认 "docker"。设为 "-"（dockerDisabled）关闭 oci executor。
	Docker string `toml:"docker"`
	// Buildkit 为 true 时接受 buildkit: true 的 job（PLAN #13.8）：为每个这样的 job 启动一个
	// 独立的特权 BuildKit 容器（不挂载宿主机目录和 docker.sock，job 结束即删除）。需要 oci executor，默认关闭。
	Buildkit bool `toml:"buildkit"`
	// OCIPlatforms 是 oci executor 提供的平台，默认 linux/<本机架构>。
	OCIPlatforms []string `toml:"oci_platforms"`
	// KeepWorkspaces 为 true 时不删除工作区（调试用）。
	KeepWorkspaces bool `toml:"keep_workspaces"`
	// CleanupImage 是删除容器中 root 写入的文件时使用的清理容器镜像，默认 DefaultCleanupImage（PLAN #8.3）。
	CleanupImage string `toml:"cleanup_image,omitempty"`
	// ToolcacheMaxSize 是每个 toolcache 目录的上限，默认 10Gi；超过后在空闲时整个删除，下次使用时重建（PLAN #8.3）。
	ToolcacheMaxSize string `toml:"toolcache_max_size,omitempty"`
	// RuntimeMaxSize 是装好的 runtime 的总量上限，默认 toolchain.DefaultMaxSize（5Gi）；超过后在空闲时从最久没用的删起
	// （PLAN #12.8）。
	RuntimeMaxSize string `toml:"runtime_max_size,omitempty"`
	// CacheMaxSize 是本地缓存（toolcache、runtime、git 镜像、CAS、buildctl、保留的工作区）的总量上限，
	// 超过后在空闲时按最后使用时间从旧到新删除（先删缓存，最后才删 runtime）。为空表示不限制总量（仍受磁盘水位约束）。
	CacheMaxSize string `toml:"cache_max_size,omitempty"`
	// DiskHighWatermark / DiskLowWatermark 是数据目录所在磁盘的使用率水位（百分比），默认 80、65：
	// 超过高水位时在空闲时按最后使用时间从旧到新删除本地缓存，直到低于低水位。设为 100 关闭。
	DiskHighWatermark int `toml:"disk_high_watermark,omitempty"`
	DiskLowWatermark  int `toml:"disk_low_watermark,omitempty"`
	// Proxy 是 network: true 的 job 使用的出站代理（[proxy.host]、[proxy.oci]，见 proxy.go）。
	// 不配置时 step 中没有代理变量（不继承 Runner 进程的环境）。
	Proxy ProxyConfig `toml:"proxy,omitempty"`

	// Dir 是 Runner 的数据目录（config.toml 所在目录），不来自文件。
	Dir string `toml:"-"`
	// probeTimeout 是探测本机 docker 等命令的超时，0 表示 runnerkit.ProbeTimeout；不来自文件，由构造方注入（测试中放宽：
	// 新写入的可执行文件在 macOS 上第一次执行要经过系统检查，负载高时会超过默认值）。
	probeTimeout time.Duration
}

// State 是注册后保存的身份信息（~/kiln/runner.json）。
type State = runnerkit.State

// home 返回数据目录（共用的身份、锁、drain、日志和自升级布局）。
func (c *Config) home() *runnerkit.Home {
	return &runnerkit.Home{Dir: c.Dir, Prog: Program}
}

// DefaultDir 返回默认数据目录：$KILN_RUNNER_DIR 或 ~/kiln。
func DefaultDir() string { return Program.DataDir() }

// LoadConfig 读取 <dir>/config.toml（不存在时使用默认值）。
func LoadConfig(dir string) (*Config, error) {
	c := &Config{Dir: dir}
	p := filepath.Join(dir, runnerkit.ConfigFileName)
	var md toml.MetaData
	switch _, err := os.Stat(p); {
	case err == nil:
		if md, err = toml.DecodeFile(p, c); err != nil {
			return nil, fmt.Errorf("读取 %s：%w", p, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		// 无法访问（例如没有权限）时不能悄悄退回默认配置
		return nil, fmt.Errorf("读取 %s：%w", p, err)
	}
	c.Dir = dir
	if c.MaxJobs <= 0 {
		c.MaxJobs = defaultMaxJobs
	}
	// 显式写 reserve_cpu = 0 表示不预留（小机器上把全部资源交给 job）
	if c.ReserveCPU < 0 || (c.ReserveCPU == 0 && !md.IsDefined("reserve_cpu")) {
		c.ReserveCPU = defaultReserveCPU
	}
	if c.ReserveMemory == "" {
		c.ReserveMemory = defaultReserveMemory
	}
	for _, v := range []struct {
		name string
		val  string
	}{{"toolcache_max_size", c.ToolcacheMaxSize}, {"runtime_max_size", c.RuntimeMaxSize}, {"cache_max_size", c.CacheMaxSize}} {
		if v.val != "" {
			if _, err := workflow.ParseMemory(v.val); err != nil {
				return nil, fmt.Errorf("%s 的 %s：%w", p, v.name, err)
			}
		}
	}
	c.DiskHighWatermark, c.DiskLowWatermark = c.diskWatermarks()
	if err := c.Proxy.Host.validate("proxy.host", false); err != nil {
		return nil, fmt.Errorf("%s：%w", p, err)
	}
	if err := c.Proxy.OCI.validate("proxy.oci", true); err != nil {
		return nil, fmt.Errorf("%s：%w", p, err)
	}
	if c.Docker == "" {
		c.Docker = defaultDocker
	}
	c.Server = strings.TrimRight(c.Server, "/")
	return c, nil
}

// Save 写回 config.toml（原子写：崩溃或磁盘满时不会留下半截配置，下次启动仍能读到旧配置）。
func (c *Config) Save() error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("# " + Program.Name + " 配置（PLAN #8.3）\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return runnerkit.WriteFileAtomic(c.path(runnerkit.ConfigFileName), b.Bytes(), 0o600)
}

func (c *Config) reserveMemoryBytes() int64 {
	if strings.TrimSpace(c.ReserveMemory) == "0" {
		return 0 // 显式不预留
	}
	n, err := workflow.ParseMemory(c.ReserveMemory)
	if err != nil {
		return defaultReserveMemoryBytes
	}
	return n
}

// diskWatermarks 返回磁盘的高、低水位（百分比）；高水位为 100 表示不按磁盘使用率回收。
func (c *Config) diskWatermarks() (high, low int) {
	high = c.DiskHighWatermark
	if high <= 0 || high > 100 {
		high = defaultDiskHigh
	}
	low = cmp.Or(c.DiskLowWatermark, defaultDiskLow)
	if low <= 0 || low >= high {
		low = high * 4 / 5
	}
	return high, low
}

// cleanupImage 返回清理容器的镜像。
func (c *Config) cleanupImage() string {
	return cmp.Or(c.CleanupImage, DefaultCleanupImage)
}

// toolcacheMaxBytes 返回每个 toolcache 目录的上限。
func (c *Config) toolcacheMaxBytes() int64 {
	if n, err := workflow.ParseMemory(c.ToolcacheMaxSize); err == nil && n > 0 {
		return n
	}
	return defaultToolcacheMaxSize
}

// runtimeMaxBytes 返回装好的 runtime 的总量上限。
func (c *Config) runtimeMaxBytes() int64 {
	if n, err := workflow.ParseMemory(c.RuntimeMaxSize); err == nil && n > 0 {
		return n
	}
	return toolchain.DefaultMaxSize
}

// probeTimeoutOrDefault 返回探测命令的超时。
func (c *Config) probeTimeoutOrDefault() time.Duration {
	if c.probeTimeout > 0 {
		return c.probeTimeout
	}
	return runnerkit.ProbeTimeout
}

// cacheMaxBytes 返回本地缓存的总量上限，0 表示不限制。
func (c *Config) cacheMaxBytes() int64 {
	if n, err := workflow.ParseMemory(c.CacheMaxSize); err == nil && n > 0 {
		return n
	}
	return 0
}

func (c *Config) path(parts ...string) string {
	return filepath.Join(append([]string{c.Dir}, parts...)...)
}

// LogDir 返回服务的日志目录：service install 让 Runner 把日志写到其中按天切分的 runner-<日期>.log。
func (c *Config) LogDir() string { return c.home().LogDir() }

// LoadState 读取注册信息。
func (c *Config) LoadState() (*State, error) { return c.home().LoadState() }

// TryLockDataDir 供手动执行的 kiln-runner gc 使用：数据目录正被 kiln-runner run 使用（服务正在运行）时
// 返回 running 为 true；否则锁住数据目录并返回释放函数（清理期间一直持有，见 runnerkit.Home.TryLock）。
func TryLockDataDir(cfg *Config) (unlock func(), running bool, err error) {
	return cfg.home().TryLock()
}
