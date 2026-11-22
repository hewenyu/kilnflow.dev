package runnerkit

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 本机信息的探测（PLAN #8.2）中两种 Runner 共用的部分：平台、内存和宿主机指纹。

const (
	// ProbeTimeout 是探测本机环境（docker、sw_vers 等）的命令超时。
	ProbeTimeout = 10 * time.Second
	// defaultMemory 是无法探测内存时假设的内存大小。
	defaultMemory = 8 << 30
	// maxResultMessage 是结果说明的长度上限（字节）。
	maxResultMessage = 4000
)

// NativePlatform 返回本机的平台（os/arch）。
func NativePlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// TotalMemory 返回本机的内存大小；探测不到时返回 8 GiB。
func TotalMemory(ctx context.Context) int64 {
	switch runtime.GOOS {
	case "darwin":
		if n, err := strconv.ParseInt(CmdOut(ctx, "sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			return n
		}
	case "linux":
		if n, ok := linuxMemTotal(); ok {
			return n
		}
	}
	return defaultMemory
}

func linuxMemTotal() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var kb int64
		if n, _ := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb); n == 1 {
			return kb << 10, true
		}
	}
	return 0, false
}

// CmdOut 执行一个探测命令，返回去掉首尾空白的 stdout，失败时返回空字符串；超时为 ProbeTimeout。
func CmdOut(ctx context.Context, name string, args ...string) string {
	return CmdOutTimeout(ctx, ProbeTimeout, name, args...)
}

// CmdOutTimeout 同 CmdOut，超时由调用方给出（探测的命令可能由配置指定，超时随配置注入）。
// 不取 stderr：docker 等命令会在 stderr 输出警告（例如 DOCKER_HOST 覆盖了 context），混进来会污染探测到的值。
func CmdOutTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// FirstLine 返回 s 的第一行（去掉首尾空白）。
func FirstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(l)
}

// HostFingerprint 返回宿主机指纹：系统版本和编译器等（PLAN #8.2）。
func HostFingerprint(ctx context.Context) map[string]string {
	fp := map[string]string{}
	switch runtime.GOOS {
	case "darwin":
		fp["macos"] = CmdOut(ctx, "sw_vers", "-productVersion")
		fp["macos_build"] = CmdOut(ctx, "sw_vers", "-buildVersion")
		fp["xcode"] = CmdOut(ctx, "xcode-select", "-p")
		fp["clang"] = FirstLine(CmdOut(ctx, "/usr/bin/clang", "--version"))
	case "linux":
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for l := range strings.SplitSeq(string(b), "\n") {
				if k, v, ok := strings.Cut(l, "="); ok && (k == "ID" || k == "VERSION_ID") {
					fp[strings.ToLower(k)] = strings.Trim(v, `"`)
				}
			}
		}
		fp["glibc"] = FirstLine(CmdOut(ctx, "ldd", "--version"))
		fp["cc"] = FirstLine(CmdOut(ctx, "cc", "--version"))
	}
	for k, v := range fp {
		if v == "" {
			delete(fp, k)
		}
	}
	return fp
}

// RemoveTree 删除目录树：先尽力让其中的目录可写（Go 模块缓存等会把目录设为只读）。
func RemoveTree(root string) error {
	makeWritable(root)
	return os.RemoveAll(root)
}

// makeWritable 尽力让目录树可删除；遇到的错误留给之后的删除报告。
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		// 尽力而为：跳过无法访问的条目
		if err == nil && d.IsDir() {
			if info, ierr := d.Info(); ierr == nil && info.Mode().Perm()&0o200 == 0 {
				_ = os.Chmod(p, info.Mode().Perm()|0o700) //nolint:gosec // 只是恢复属主的权限，以便删除
			}
		}
		return nil
	})
}

// Touch 把路径的修改时间设为现在，记录最后使用的时间；尽力而为（失败时最多提前被清理，之后重新下载）。
func Touch(p string) {
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

// ClampInt32 把 int 转换为 int32，超出范围时取边界值。
func ClampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

// TruncateMsg 把结果说明截断到上限以内（不截断多字节字符：proto 的 string 字段必须是合法的 UTF-8，
// 否则 Complete 请求无法序列化）。
func TruncateMsg(s string) string {
	return shorten.Text(s, maxResultMessage)
}
