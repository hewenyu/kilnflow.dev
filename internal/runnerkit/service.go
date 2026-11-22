package runnerkit

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/logging"
)

// 服务安装（v0：以当前用户运行。macOS 为 LaunchAgent，Linux 为 systemd 用户服务；
// 不创建 kiln-job 用户，见 PLAN #25）。

// serviceCmdTimeout 是 launchctl / systemctl 命令的超时。
const serviceCmdTimeout = time.Minute

// serviceCmd 执行一个服务管理命令，返回合并的输出。
func serviceCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceCmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// launchdPlistPath 返回 LaunchAgent 的 plist 路径。
func launchdPlistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// systemdUnitPath 返回 systemd 用户服务的 unit 文件路径。
func systemdUnitPath(unit string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unit+".service"), nil
}

// InstallService 安装并启动服务，返回服务文件的路径（及需要用户执行的后续操作）。
// Runner 的日志写到 h.LogDir() 中按天切分的文件（保留 7 天）。stop 是服务管理器停止 Runner 时等待的时间（launchd 的
// ExitTimeOut、systemd 的 TimeoutStopSec）：Runner 收到信号后会等正在执行的任务结束，再留出提交结果和清理的时间。
// 服务定义先生成：路径写不进服务定义时在改动任何东西之前报错。
//
// 重启策略：launchd 的 KeepAlive 只写 SuccessfulExit=false，systemd 用 Restart=on-failure，两者都只在非 0 退出或被信号
// 杀死时重启。Runner 收到停止信号（SIGTERM、SIGINT）后等任务结束以 0 退出、被吊销时以 0 退出，都不再被拉起；出错（含
// 试用的新版本没有通过确认，由启动器计次回滚，PLAN #27.7）以非 0 退出、崩溃、被 SIGKILL 时被拉起。自升级切换是 exec
// 启动器（同一个进程，不退出）。
func InstallService(ctx context.Context, h *Home, stop time.Duration) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	// 服务入口是启动器（PLAN #27.7）：复制当前程序为启动器和当前版本，服务以 bin/launcher/<程序名> launch 运行
	launcher := LauncherPath(h.Dir, h.Prog.Name)
	var install func(context.Context) (string, error)
	switch runtime.GOOS {
	case "darwin":
		content, err := launchdPlist(h, launcher, os.Getenv("PATH"), stop)
		if err != nil {
			return "", err
		}
		install = func(ctx context.Context) (string, error) { return installLaunchd(ctx, h.Prog.LaunchdLabel, content) }
	case "linux":
		content, err := systemdUnitFile(h, launcher, os.Getenv("PATH"), stop)
		if err != nil {
			return "", err
		}
		install = func(ctx context.Context) (string, error) { return installSystemd(ctx, h.Prog.SystemdUnit, content) }
	default:
		return "", fmt.Errorf("不支持的系统 %s", runtime.GOOS)
	}
	if err := os.MkdirAll(h.LogDir(), 0o755); err != nil {
		return "", err
	}
	if _, err := installLayout(h.Dir, h.Prog.Name, self, buildinfo.Version()); err != nil {
		return "", fmt.Errorf("建立自升级目录：%w", err)
	}
	return install(ctx)
}

// installLaunchd 写入 plist 并启动 macOS LaunchAgent label，返回 plist 路径。
func installLaunchd(ctx context.Context, label, content string) (string, error) {
	plist, err := launchdPlistPath(label)
	if err != nil {
		return "", err
	}
	if err := writeServiceFile(plist, content); err != nil {
		return "", err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// 之前没有安装过时 bootout 会失败，忽略
	_, _ = serviceCmd(ctx, "launchctl", "bootout", domain+"/"+label)
	if out, err := serviceCmd(ctx, "launchctl", "bootstrap", domain, plist); err != nil {
		return "", fmt.Errorf("launchctl bootstrap：%w：%s", err, out)
	}
	return plist, nil
}

// installSystemd 写入 unit 文件并启动 systemd 用户服务 name，返回 unit 文件路径和 enable-linger 的提示。
func installSystemd(ctx context.Context, name, content string) (string, error) {
	unit, err := systemdUnitPath(name)
	if err != nil {
		return "", err
	}
	if err := writeServiceFile(unit, content); err != nil {
		return "", err
	}
	for _, args := range systemdInstallCmds(name) {
		if out, err := serviceCmd(ctx, "systemctl", args...); err != nil {
			return "", fmt.Errorf("systemctl %s：%w：%s", strings.Join(args, " "), err, out)
		}
	}
	return unit + "（无人登录时也运行需要执行一次：sudo loginctl enable-linger " + os.Getenv("USER") + "）", nil
}

// systemdInstallCmds 是安装 systemd 用户服务时依次执行的 systemctl 参数：重新加载 unit、开机启动，并重启服务。
// 已经在运行的旧服务（迁移到启动器，PLAN #27.7"首次迁移"）只 enable --now 不会重启，原来的进程继续运行、不经过启动器，
// 自动升级不生效（独立审查发现）。restart 用 --no-block：停止时 Runner 会等正在执行的 job 结束（TimeoutStopSec），
// 不在这里等。没有运行时 restart 等同于 start。
func systemdInstallCmds(name string) [][]string {
	return [][]string{{"--user", "daemon-reload"}, {"--user", "enable", name}, {"--user", "restart", "--no-block", name}}
}

// launchdPlist 返回 LaunchAgent 的 plist 内容：以 exe launch 运行（exe 是启动器），数据目录、日志目录和 PATH 通过环境变量传入；
// stdout / stderr 只剩启动失败、崩溃等输出，写到 consolePath。路径中有 XML 不能表示的控制字符时返回错误。
func launchdPlist(h *Home, exe, path string, stop time.Duration) (string, error) {
	label, env := h.Prog.LaunchdLabel, h.Prog.DirEnv
	var bad error
	x := func(what, s string) string {
		v, err := xmlText(what, s)
		if bad == nil {
			bad = err
		}
		return v
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>launch</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>%s</key><string>%s</string>
    <key>%s</key><string>%s</string>
    <key>PATH</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>ExitTimeOut</key><integer>%d</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, x("程序路径", exe), env, x("数据目录", h.Dir), logging.EnvDir, x("日志目录", h.LogDir()), x("PATH", path),
		int(stop/time.Second), x("日志路径", h.ConsolePath()), x("日志路径", h.ConsolePath()))
	return plist, bad
}

// xmlText 把 s 写成 plist 的 <string> 文本：& < > " ' 写成实体，制表、换行、回车写成字符引用（解析时原样还原）。
// 不是有效的 UTF-8、有 XML 1.0 不能表示的字符（制表、换行、回车以外的控制字符等）时返回错误（what 写进错误），不写出被替换过的路径。
func xmlText(what, s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s 不是有效的 UTF-8：%q", what, s)
	}
	for _, r := range s {
		if !xmlChar(r) {
			return "", fmt.Errorf("%s 中有 plist（XML）不能表示的字符 %U：%q", what, r, s)
		}
	}
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // 写入 strings.Builder 不会失败
	return b.String(), nil
}

// xmlChar 判断 r 是否是 XML 1.0 的字符（规范的 Char 产生式）。
func xmlChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
}

// systemdUnitFile 返回 systemd 用户服务的 unit 内容，参数同 launchdPlist。exe 是 systemd 不接受的路径、路径不是有效的
// UTF-8（单元文件按 UTF-8 解析，写不出原样的字节）时返回错误。
func systemdUnitFile(h *Home, exe, path string, stop time.Duration) (string, error) {
	for _, v := range []string{exe, h.Dir, path} {
		if !utf8.ValidString(v) {
			return "", fmt.Errorf("systemd 单元文件中的路径不是有效的 UTF-8：%q", v)
		}
	}
	execStart, err := sdExecStart(exe, "launch")
	if err != nil {
		return "", err
	}
	env, desc := h.Prog.DirEnv, h.Prog.Description
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
ExecStart=%s
Environment=%s
Environment=%s
Environment=%s
Restart=on-failure
RestartSec=5
KillMode=mixed
TimeoutStopSec=%d

[Install]
WantedBy=default.target
`, sdText(desc), execStart, sdEnvironment(env, h.Dir), sdEnvironment(logging.EnvDir, h.LogDir()), sdEnvironment("PATH", path),
		int(stop/time.Second)), nil
}

// systemd 单元文件（systemd.unit(5)、systemd.service(5)、systemd.exec(5)）的转义规则，与 kiln-orca 的 src/host/escape.ts
// 相同（那边已在真实的 systemd 252 上实测）。

// sdSpecifiers 让 s 中的 % 不被当成 specifier（%% 是字面的 %）。
func sdSpecifiers(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// sdQuoted 把 s 写成单元文件中用双引号引起来的一项（ExecStart= 的参数、Environment= 的赋值共用的规则）：反斜杠、双引号
// 用 C 风格转义，控制字符写成 \xNN（引号中不能有换行），% 写成 %%。引号内的空白、单引号都是字面的。
func sdQuoted(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case sdControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return `"` + sdSpecifiers(b.String()) + `"`
}

// sdControl 判断 r 是否是 C0 控制字符或 DEL。
func sdControl(r rune) bool { return r < 0x20 || r == 0x7f }

// sdExecStart 把命令行写成 ExecStart= 的值（systemd.service(5) "Command lines"）：每一项按 sdQuoted 引起来；参数中的 $ 写成
// $$（参数中的 $VAR、${VAR} 会被替换成环境变量）。可执行文件的路径不做变量替换，$ 原样写（写成 $$ 时 systemd 去找字面的
// $$）；systemd 不接受其中有引号、反斜杠、控制字符（实测 systemd 252：Executable name contains special characters），
// 这时返回错误。
func sdExecStart(program string, args ...string) (string, error) {
	if strings.ContainsAny(program, `"'\`) || strings.ContainsFunc(program, sdControl) {
		return "", fmt.Errorf("systemd 不接受可执行文件的路径中有引号、反斜杠或控制字符：%q（把数据目录换到没有这些字符的路径后再安装）", program)
	}
	words := []string{sdQuoted(program)}
	for _, a := range args {
		words = append(words, strings.ReplaceAll(sdQuoted(a), "$", "$$"))
	}
	return strings.Join(words, " "), nil
}

// sdEnvironment 是一条 Environment= 的值（systemd.exec(5)：赋值可以整个用引号引起来；$ 在这里没有特殊含义，不转义）。
func sdEnvironment(name, value string) string { return sdQuoted(name + "=" + value) }

// sdText 是 Description= 这类自由文本（不去引号、不做 C 转义，只展开 specifier）：% 写成 %%，控制字符换成空格。
func sdText(s string) string {
	return sdSpecifiers(strings.Map(func(r rune) rune {
		if sdControl(r) {
			return ' '
		}
		return r
	}, s))
}

// writeServiceFile 写入 plist / unit 文件（路径由用户主目录拼出）。
func writeServiceFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// UninstallService 停止并删除服务（h 提供程序名、数据目录和服务名称）。
func UninstallService(ctx context.Context, h *Home) error {
	label, name := h.Prog.LaunchdLabel, h.Prog.SystemdUnit
	// 服务可能没有在运行，停止失败时仍然删除文件
	switch runtime.GOOS {
	case "darwin":
		plist, err := launchdPlistPath(label)
		if err != nil {
			return err
		}
		_, _ = serviceCmd(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
		return os.Remove(plist)
	case "linux":
		unit, err := systemdUnitPath(name)
		if err != nil {
			return err
		}
		_, _ = serviceCmd(ctx, "systemctl", "--user", "disable", "--now", name)
		return os.Remove(unit)
	}
	return nil
}
