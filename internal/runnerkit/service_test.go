package runnerkit

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// plistValues 解析 plist（同时检查它是合法的 XML），按出现顺序返回 key、string、integer 元素的文本。
func plistValues(t *testing.T, plist string) []string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(plist))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	var out []string
	var cur string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("plist 不是合法的 XML：%v\n%s", err, plist)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			cur = tk.Name.Local
			if cur == "true" || cur == "false" {
				out = append(out, cur)
			}
		case xml.EndElement:
			cur = ""
		case xml.CharData:
			if cur == "key" || cur == "string" || cur == "integer" {
				out = append(out, string(tk))
			}
		}
	}
}

// LaunchAgent（PLAN #8.1、#27.7）：以启动器 launch 运行，开机自启、只在非 0 退出或被信号杀死时重启（被吊销、正常停止以 0
// 退出后不再拉起），输出写到日志文件；停止时等待的时间足够让正在执行的 job 结束。路径中的特殊字符要转义，解析后与原来相同。
func TestLaunchdPlist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dir  string
		exe  string
		path string
	}{
		{name: "普通路径", dir: "/Users/kiln/kiln", exe: "/usr/local/bin/kiln-runner", path: "/usr/bin:/bin"},
		{name: "特殊字符", dir: "/Users/a&b/<kiln>\"q\"'s", exe: "/Applications/K & R/kiln-runner", path: "/opt/a&b:/usr/bin"},
		{name: "制表换行回车", dir: "/Users/a\tb\nc\rd\r\n", exe: "/opt/${VERSION}/kiln-runner", path: "/opt/100%/bin:$HOME"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := &Home{Dir: c.dir, Prog: testProgram}
			plist, err := launchdPlist(h, c.exe, c.path, testStopTimeout)
			if err != nil {
				t.Fatal(err)
			}
			got := plistValues(t, plist)
			console := h.ConsolePath()
			want := []string{
				"Label", testProgram.LaunchdLabel,
				"ProgramArguments", c.exe, "launch",
				"EnvironmentVariables", testProgram.DirEnv, c.dir, "KILN_LOG_DIR", h.LogDir(), "PATH", c.path,
				"RunAtLoad", "true",
				"KeepAlive", "SuccessfulExit", "false",
				"ProcessType", "Interactive",
				"ExitTimeOut", "3700",
				"StandardOutPath", console,
				"StandardErrorPath", console,
			}
			if !slices.Equal(got, want) {
				t.Errorf("plist 的值：\n%q\n期望：\n%q", got, want)
			}
		})
	}
}

// TestLaunchdPlistInvalid：路径中有 XML 1.0 不能表示的字符或不是有效的 UTF-8 时报错，不写出被替换过的路径。
func TestLaunchdPlistInvalid(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/Users/a\x01b", "/Users/a\x1bb", "/Users/a\xffb", "/Users/a\uFFFEb"} {
		if _, err := launchdPlist(&Home{Dir: dir, Prog: testProgram}, "/bin/x", "/usr/bin", testStopTimeout); err == nil {
			t.Errorf("数据目录 %q：没有报错", dir)
		}
	}
	if _, err := launchdPlist(&Home{Dir: "/d", Prog: testProgram}, "/bin/x\x00", "/usr/bin", testStopTimeout); err == nil {
		t.Error("程序路径中有 NUL：没有报错")
	}
}

// TestLaunchdPlistPlutil（macOS）：plutil 能解析，读回来的参数、环境变量和日志路径与原来逐字相同。
func TestLaunchdPlistPlutil(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("plutil 只在 macOS 上有")
	}
	dir := t.TempDir()
	h := &Home{Dir: filepath.Join(dir, "a&b <c> \"q\" it's\tx\ny\r"), Prog: testProgram}
	exe, path := "/opt/${VERSION}/K & R/kiln-runner", "/opt/100%/bin:$HOME"
	plist, err := launchdPlist(h, exe, path, testStopTimeout)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "x.plist")
	if err := os.WriteFile(file, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", file).Output()
	if err != nil {
		t.Fatalf("plutil：%v", err)
	}
	var v struct {
		ProgramArguments     []string
		EnvironmentVariables map[string]string
		StandardOutPath      string
		KeepAlive            map[string]bool
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.ProgramArguments, []string{exe, "launch"}) || v.EnvironmentVariables[testProgram.DirEnv] != h.Dir ||
		v.EnvironmentVariables["PATH"] != path || v.StandardOutPath != h.ConsolePath() || len(v.KeepAlive) != 1 || v.KeepAlive["SuccessfulExit"] {
		t.Fatalf("plutil 读回来的值：%+v", v)
	}
}

// systemd 用户服务：ExecStart、环境变量、只在失败时重启和停止超时；值按 systemd 的规则转义（与 kiln-orca 的 escape.ts 相同）。
func TestSystemdUnitFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		dir   string
		exe   string
		path  string
		lines []string
	}{
		{
			name: "普通路径",
			dir:  "/home/kiln/kiln", exe: "/usr/local/bin/kiln-runner", path: "/usr/bin:/bin",
			lines: []string{
				`ExecStart="/usr/local/bin/kiln-runner" "launch"`,
				`Environment="KILN_RUNNER_DIR=/home/kiln/kiln"`,
				`Environment="KILN_LOG_DIR=/home/kiln/kiln/logs"`,
				`Environment="PATH=/usr/bin:/bin"`,
			},
		},
		{
			name: "特殊字符",
			dir:  "/home/a b/50%\"kiln\"\\x\tit's", exe: "/opt/my tools/${VERSION}/100%/kiln-runner", path: "/opt/x y:/opt/100%/bin:$HOME/bin",
			lines: []string{
				// 可执行文件的路径中的 $ 原样（不做变量替换），% 写成 %%
				`ExecStart="/opt/my tools/${VERSION}/100%%/kiln-runner" "launch"`,
				// Environment= 中 $ 不转义，反斜杠、引号 C 转义，控制字符写成 \xNN
				`Environment="KILN_RUNNER_DIR=/home/a b/50%%\"kiln\"\\x\x09it's"`,
				`Environment="KILN_LOG_DIR=/home/a b/50%%\"kiln\"\\x\x09it's/logs"`,
				`Environment="PATH=/opt/x y:/opt/100%%/bin:$HOME/bin"`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			unit, err := systemdUnitFile(&Home{Dir: c.dir, Prog: testProgram}, c.exe, c.path, testStopTimeout)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(unit, "\n")
			for _, l := range append(c.lines, "Restart=on-failure", "KillMode=mixed", "TimeoutStopSec=3700", "WantedBy=default.target") {
				if !slices.Contains(lines, l) {
					t.Errorf("unit 中没有 %q：\n%s", l, unit)
				}
			}
			if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Restart=") && l != "Restart=on-failure" }) {
				t.Errorf("unit 中有别的 Restart=：\n%s", unit)
			}
		})
	}
}

// TestSystemdUnitFileNewline：值中的换行不会结束这一行、加出别的设置。
func TestSystemdUnitFileNewline(t *testing.T) {
	t.Parallel()
	unit, err := systemdUnitFile(&Home{Dir: "/home/a\nExecStartPre=/bin/evil", Prog: testProgram}, "/bin/x", "/usr/bin", testStopTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `Environment="KILN_RUNNER_DIR=/home/a\x0aExecStartPre=/bin/evil"`) ||
		slices.ContainsFunc(strings.Split(unit, "\n"), func(l string) bool { return strings.HasPrefix(l, "ExecStartPre") }) {
		t.Fatalf("换行没有转义：\n%s", unit)
	}
}

// TestSystemdUnitFileRejects：systemd 不接受可执行文件的路径中有引号、反斜杠、控制字符（实测 systemd 252），路径不是有效的
// UTF-8 时也写不出原样的字节：生成 unit 时报错（安装前），错误说明原因。
func TestSystemdUnitFileRejects(t *testing.T) {
	t.Parallel()
	for _, exe := range []string{`/opt/q"uote/kiln-runner`, "/opt/it's/kiln-runner", `/opt/back\slash/kiln-runner`, "/opt/tab\there/kiln-runner", "/opt/nl\nx/kiln-runner", "/opt/del\x7f/kiln-runner"} {
		_, err := systemdUnitFile(&Home{Dir: "/d", Prog: testProgram}, exe, "/usr/bin", testStopTimeout)
		if err == nil || !strings.Contains(err.Error(), "systemd 不接受可执行文件的路径中有引号、反斜杠或控制字符") {
			t.Errorf("可执行文件 %q：err = %v", exe, err)
		}
	}
	if _, err := systemdUnitFile(&Home{Dir: "/d\xff", Prog: testProgram}, "/bin/x", "/usr/bin", testStopTimeout); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("不是 UTF-8 的数据目录：err = %v", err)
	}
}

// TestSdEscape：单元文件各处的转义规则（用例对照 kiln-orca 的 test/service.test.ts）。
func TestSdEscape(t *testing.T) {
	t.Parallel()
	got, err := sdExecStart("/opt/${VERSION}/bin/node", "/opt/x %n/cli.js", "run", "/data/ai%n%%${VERSION}$HOMEa bq\"uoteit'sback\\slasha&b<x>tab\there")
	want := `"/opt/${VERSION}/bin/node" "/opt/x %%n/cli.js" "run" "/data/ai%%n%%%%$${VERSION}$$HOMEa bq\"uoteit'sback\\slasha&b<x>tab\x09here"`
	if err != nil || got != want {
		t.Errorf("sdExecStart = %s, %v\n期望 %s", got, err, want)
	}
	if got, _ := sdExecStart("/bin/x", "a\nb"); got != `"/bin/x" "a\x0ab"` {
		t.Errorf("引号内的换行：%s", got)
	}
	if got := sdEnvironment("PATH", "/usr/bin:/opt/100%/bin:$HOME/bin"); got != `"PATH=/usr/bin:/opt/100%%/bin:$HOME/bin"` {
		t.Errorf("sdEnvironment = %s", got)
	}
	if got := sdText("Kiln 100% 节点 ${X}\nExecStartPre=/bin/evil"); got != "Kiln 100%% 节点 ${X} ExecStartPre=/bin/evil" {
		t.Errorf("sdText = %s", got)
	}
}

// TestSystemdInstallCmds（PLAN #27.7"首次迁移"，独立审查发现）：安装时重启服务（--no-block，不等 job 结束）：已经在运行的
// 旧服务只 enable --now 不会重启，原来的进程不经过启动器，自动升级不生效。
func TestSystemdInstallCmds(t *testing.T) {
	t.Parallel()
	got := systemdInstallCmds("kiln-runner")
	want := [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "kiln-runner"}, {"--user", "restart", "--no-block", "kiln-runner"}}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("systemctl 命令 %v，想要 %v", got, want)
	}
}

// testStopTimeout 是测试中服务停止时等待的时间（与 kiln-runner 相同：默认 job 超时加 100 秒）。
const testStopTimeout = time.Hour + 100*time.Second
