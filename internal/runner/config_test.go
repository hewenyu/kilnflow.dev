package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 没有 config.toml 时使用默认值。
func TestLoadConfigDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != dir || c.MaxJobs != 4 || c.ReserveCPU != 2 || c.ReserveMemory != "4Gi" || c.Docker != "docker" || c.Buildkit || c.KeepWorkspaces {
		t.Errorf("默认配置 = %+v", c)
	}
	if c.reserveMemoryBytes() != 4<<30 {
		t.Errorf("reserveMemoryBytes = %d", c.reserveMemoryBytes())
	}
	if c.LogDir() != filepath.Join(dir, "logs") || c.home().ConsolePath() != filepath.Join(dir, "logs", "runner.out.log") {
		t.Errorf("LogDir = %s，consolePath = %s", c.LogDir(), c.home().ConsolePath())
	}
}

func TestLoadConfigFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "config.toml"), `
server = "https://kiln.example.com/"
name = "mini-01"
max_jobs = 0
reserve_cpu = 1.5
reserve_memory = "512Mi"
extra_path = ["/opt/homebrew/bin"]
docker = "-"
buildkit = true
oci_platforms = ["linux/amd64"]
keep_workspaces = true
`)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "https://kiln.example.com" {
		t.Errorf("server 应去掉末尾的 /：%q", c.Server)
	}
	if c.MaxJobs != defaultMaxJobs {
		t.Errorf("max_jobs = 0 时取默认值，实际 %d", c.MaxJobs)
	}
	if c.Name != "mini-01" || c.ReserveCPU != 1.5 || c.reserveMemoryBytes() != 512<<20 || c.Docker != dockerDisabled ||
		!c.Buildkit || !c.KeepWorkspaces || !slices.Equal(c.ExtraPath, []string{"/opt/homebrew/bin"}) || !slices.Equal(c.OCIPlatforms, []string{"linux/amd64"}) {
		t.Errorf("配置 = %+v", c)
	}
	if c.Dir != dir {
		t.Errorf("Dir = %q，不来自文件", c.Dir)
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "config.toml"), "max_jobs = = 1")
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("LoadConfig = %v，期望带文件名的错误", err)
	}
}

// config.toml 无法访问（不是"不存在"）时报错，而不是悄悄使用默认配置。
func TestLoadConfigUnreadable(t *testing.T) {
	t.Parallel()
	notDir := filepath.Join(t.TempDir(), "file")
	mustWrite(t, notDir, "")
	if _, err := LoadConfig(notDir); err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("LoadConfig = %v，期望带文件名的错误", err)
	}
}

// reserve_memory 无法解析时按默认的 4Gi 计算。
func TestReserveMemoryInvalid(t *testing.T) {
	t.Parallel()
	c := &Config{ReserveMemory: "lots"}
	if c.reserveMemoryBytes() != defaultReserveMemoryBytes {
		t.Errorf("reserveMemoryBytes = %d", c.reserveMemoryBytes())
	}
}

// Save 写出的文件能被 LoadConfig 读回。
func TestConfigSaveRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "new")
	c := &Config{Dir: dir, Server: "https://kiln.example.com", Name: "n", MaxJobs: 3, ReserveCPU: 1, ReserveMemory: "2Gi", ExtraPath: []string{"/x"}, Docker: "podman"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "# kiln-runner 配置") || strings.Contains(string(b), dir) {
		t.Errorf("config.toml：\n%s", b)
	}
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != c.Server || got.Name != c.Name || got.MaxJobs != 3 || got.ReserveMemory != "2Gi" || got.Docker != "podman" || !slices.Equal(got.ExtraPath, c.ExtraPath) {
		t.Errorf("读回 = %+v", got)
	}
}
