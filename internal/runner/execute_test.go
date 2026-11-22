package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// stepEnv 从日志中取出 step 用 `env | sed 's/^/ENV:/'` 打印的环境变量。
func stepEnv(log string) map[string]string {
	env := map[string]string{}
	for l := range strings.SplitSeq(log, "\n") {
		if kv, ok := strings.CutPrefix(l, "ENV:"); ok {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
	}
	return env
}

const printEnv = "env | sed 's/^/ENV:/'"

// host executor 的 step 只看到 Kiln 设置的环境变量（PLAN #8.5、#17.1）：Runner 自己的环境不会泄漏进去。
func TestExecuteHostEnv(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs, func(c *Config) { c.ExtraPath = []string{"/opt/extra/bin"} })
	spec := hostJob(1, &runnerv1.Step{Name: "env", Run: printEnv, Env: map[string]string{"OVERRIDE": "step"}})
	spec.Env = map[string]string{"WS_FILE": workflow.WorkspacePlaceholder + "/x", "JOB_VAR": "job", "OVERRIDE": "job"}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")

	root := r.workDir(1)
	ws := filepath.Join(root, attemptWorkspaceDir)
	lang := "C.UTF-8"
	if runtime.GOOS == "darwin" {
		lang = "en_US.UTF-8"
	}
	want := map[string]string{
		"HOME":              filepath.Join(root, attemptHomeDir),
		"TMPDIR":            filepath.Join(root, attemptTmpDir),
		"KILN_WORKSPACE":    ws,
		"CI":                "true",
		"KILN":              "true",
		"TZ":                "UTC",
		"SOURCE_DATE_EPOCH": "315532800",
		"LANG":              lang,
		"SHELL":             "/bin/bash",
		"PATH":              "/opt/extra/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		// network: false 时代理指向不可用的地址，NO_PROXY 只有本机地址
		"HTTP_PROXY": noNetworkProxy, "HTTPS_PROXY": noNetworkProxy, "ALL_PROXY": noNetworkProxy,
		"http_proxy": noNetworkProxy, "https_proxy": noNetworkProxy, "all_proxy": noNetworkProxy,
		"NO_PROXY": noNetworkNoProxy, "no_proxy": noNetworkNoProxy,
		"WS_FILE":  ws + "/x", // 工作区占位符在执行时替换
		"JOB_VAR":  "job",
		"OVERRIDE": "step", // step 的 env 覆盖 job 的 env
	}
	env := stepEnv(log)
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q，期望 %q", k, env[k], v)
		}
	}
	if env["USER"] == "" || env["USER"] != env["LOGNAME"] {
		t.Errorf("USER = %q，LOGNAME = %q", env["USER"], env["LOGNAME"])
	}
	// bash 自己设置的变量之外，不能有其他变量
	allowed := map[string]bool{"USER": true, "LOGNAME": true, "PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
	// git 凭据通道的设置（值见 TestGitAccessApply、TestStepGitFetchPerTransport）
	for _, k := range []string{"GIT_EXEC_PATH", "GIT_TERMINAL_PROMPT", gitSocketEnv, gitRealExecEnv} {
		allowed[k] = true
	}
	for k := range env {
		if _, ok := want[k]; !ok && !allowed[k] && !strings.HasPrefix(k, "GIT_CONFIG_") {
			t.Errorf("step 中出现了不该有的环境变量 %s=%s", k, env[k])
		}
	}
}

// Runner 不信任收到的 toolcache 配置（纵深防御）：name 中的 .. 会逃出缓存根目录，env 会覆写关键变量，
// 两者都必须在执行前以用户错误失败（workflow 解析层已在 Server 侧拦截）。
func TestExecuteRejectsUnsafeToolcache(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tc   *runnerv1.ToolCache
	}{
		{"name 逃逸", &runnerv1.ToolCache{Name: "../../etc", Env: "GOCACHE"}},
		{"name 分隔符", &runnerv1.ToolCache{Name: "a/b", Env: "GOCACHE"}},
		{"env PATH", &runnerv1.ToolCache{Name: "x", Env: "PATH"}},
		{"env GIT", &runnerv1.ToolCache{Name: "x", Env: "GIT_CONFIG_GLOBAL"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			spec := hostJob(1, bash("x", "true"))
			spec.Toolcache = []*runnerv1.ToolCache{c.tc}
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_FAILED, classUser)
			if !strings.Contains(got.Message, "toolcache") {
				t.Errorf("错误信息应说明 toolcache：%q", got.Message)
			}
		})
	}
}

// network: true 且没有配置 [proxy.host] 时不注入代理变量（也不继承 Runner 进程的代理）。
func TestExecuteHostNetwork(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("env", printEnv))
	spec.Network = true
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, pair := range proxyEnvPairs {
		for _, k := range pair {
			if v, ok := stepEnv(log)[k]; ok {
				t.Errorf("network: true 时不应设置 %s（%s）", k, v)
			}
		}
	}
}

// [proxy.host]：network: true 的 host job 中注入两种大小写的代理变量；workflow 的 env
// 覆盖 Runner 的配置（只写一种大小写也覆盖另一种）；代理 URL 中的密码在日志中脱敏。
func TestExecuteHostProxy(t *testing.T) {
	t.Parallel()
	// 密码中有 userinfo 需要转义的字符：注入环境变量的是编码形式 proxy%3Asecret&pw，也要脱敏
	const pw = "proxy:secret&pw"
	proxy := "http://kiln:proxy%3Asecret&pw@127.0.0.1:3128"
	fs := newFakeServer(t)
	r := newTestRunner(t, fs, func(c *Config) {
		c.Proxy.Host = ProxySettings{HTTPProxy: proxy, HTTPSProxy: proxy, NoProxy: "localhost,.internal"}
	})
	spec := hostJob(1,
		bash("env", printEnv),
		&runnerv1.Step{Name: "step env", Run: "echo STEP:$HTTP_PROXY:$http_proxy", Env: map[string]string{"http_proxy": "http://step:1"}})
	spec.Network = true
	spec.Env = map[string]string{"https_proxy": "http://workflow:8080"}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, v := range []string{pw, "proxy%3Asecret&pw", "secret"} {
		if strings.Contains(log, v) {
			t.Errorf("日志中出现了代理密码 %q：\n%s", v, log)
		}
	}
	env := stepEnv(log)
	masked := "http://kiln:***@127.0.0.1:3128"
	for k, v := range map[string]string{
		"HTTP_PROXY": masked, "http_proxy": masked,
		"HTTPS_PROXY": "http://workflow:8080", "https_proxy": "http://workflow:8080",
		"NO_PROXY": "localhost,.internal", "no_proxy": "localhost,.internal",
	} {
		if env[k] != v {
			t.Errorf("%s = %q，期望 %q", k, env[k], v)
		}
	}
	if _, ok := env["ALL_PROXY"]; ok {
		t.Errorf("没有配置时不设置 ALL_PROXY：%q", env["ALL_PROXY"])
	}
	if !strings.Contains(log, "STEP:http://step:1:http://step:1") {
		t.Errorf("step 的 env 应覆盖两种大小写：\n%s", log)
	}
}

// 经带认证的代理执行 curl -v：调试输出中的 Proxy-Authorization: Basic base64(用户名:密码) 也要脱敏。
func TestExecuteHostProxyBasicAuthMasked(t *testing.T) {
	t.Parallel()
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("没有 curl")
	}
	const user, pw = "kiln-user", "kiln-proxy-pw"
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + pw))
	var mu sync.Mutex
	var gotAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Proxy-Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer proxy.Close()
	proxyURL := "http://" + user + ":" + pw + "@" + strings.TrimPrefix(proxy.URL, "http://")
	fs := newFakeServer(t)
	r := newTestRunner(t, fs, func(c *Config) { c.Proxy.Host = ProxySettings{HTTPProxy: proxyURL} })
	spec := hostJob(1, bash("curl", curl+" -sS -v --max-time 10 http://kiln-proxy-test.invalid/"))
	spec.Network = true
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Basic "+basic {
		t.Fatalf("代理收到的 Proxy-Authorization = %q", gotAuth)
	}
	if !strings.Contains(log, "Proxy-Authorization: Basic ***") {
		t.Errorf("日志中应有脱敏后的 Proxy-Authorization：\n%s", log)
	}
	for _, v := range []string{basic, pw} {
		if strings.Contains(log, v) {
			t.Errorf("日志中出现了代理凭证 %q：\n%s", v, log)
		}
	}
}

// network: false 的 host job：配置了 [proxy.host]，workflow 或 step 的 env 写了代理或 NO_PROXY（大小写混用）
// 也不能恢复外网访问。
func TestExecuteHostNoNetworkLocked(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs, func(c *Config) {
		c.Proxy.Host = ProxySettings{HTTPProxy: "http://127.0.0.1:3128", HTTPSProxy: "http://127.0.0.1:3128"}
	})
	spec := hostJob(1, &runnerv1.Step{Name: "env", Run: printEnv, Env: map[string]string{"HTTP_PROXY": "http://step:1", "all_proxy": "socks5://step:1"}})
	spec.Env = map[string]string{"https_proxy": "http://workflow:8080", "NO_PROXY": "*", "no_proxy": "*"}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	env := stepEnv(log)
	for _, pair := range proxyEnvPairs {
		want := noNetworkProxy
		if pair[0] == "NO_PROXY" {
			want = noNetworkNoProxy
		}
		for _, k := range pair {
			if env[k] != want {
				t.Errorf("%s = %q，期望 %q", k, env[k], want)
			}
		}
	}
}

// step 的执行：退出码、continue-on-error、shell、working-directory。
func TestExecuteSteps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		steps    []*runnerv1.Step
		outcome  runnerv1.Outcome
		class    string
		exitCode int32
		logHas   []string
		logLacks []string
	}{
		{
			name:    "全部成功，按顺序执行",
			steps:   []*runnerv1.Step{bash("one", "echo first-step"), bash("", "echo second-step")},
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
			// 没有名称的 step 用脚本的第一行作为名称
			logHas: []string{"[1/2] one", "first-step", "[2/2] echo second-step", "second-step", "结果：成功"},
		},
		{
			name:     "非零退出码使 job 失败，之后的 step 不执行",
			steps:    []*runnerv1.Step{bash("fail", "echo before\nexit 3\necho after"), bash("next", "echo not-run")},
			outcome:  runnerv1.Outcome_OUTCOME_FAILED,
			class:    classUser,
			exitCode: 3,
			logHas:   []string{"before", `step "fail" 失败，退出码 3`},
			logLacks: []string{"after", "not-run"},
		},
		{
			name:     "bash 带 -eo pipefail：管道中的失败也会让 step 失败",
			steps:    []*runnerv1.Step{bash("pipe", "false | true\necho unreachable")},
			outcome:  runnerv1.Outcome_OUTCOME_FAILED,
			class:    classUser,
			exitCode: 1,
			logLacks: []string{"unreachable"},
		},
		{
			name: "continue-on-error 的 step 失败后继续",
			steps: []*runnerv1.Step{
				{Name: "flaky", Run: "exit 5", ContinueOnError: true},
				bash("next", "echo still-runs"),
			},
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
			logHas:  []string{"退出码 5（continue-on-error，继续）", "still-runs"},
		},
		{
			name:    "shell: sh",
			steps:   []*runnerv1.Step{{Name: "sh", Run: "echo from-sh", Shell: "sh"}},
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
			logHas:  []string{"from-sh"},
		},
		{
			name:    "自定义 shell 中的 {0} 替换为脚本路径",
			steps:   []*runnerv1.Step{{Name: "custom", Run: "echo custom-shell", Shell: "bash --noprofile {0}"}},
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
			logHas:  []string{"custom-shell"},
		},
		{
			name:    "不支持的 shell 是用户错误",
			steps:   []*runnerv1.Step{{Name: "fish", Run: "echo x", Shell: "fish"}},
			outcome: runnerv1.Outcome_OUTCOME_FAILED,
			class:   classUser,
			logHas:  []string{`不支持的 shell "fish"`},
		},
		{
			name: "working-directory 相对工作区",
			steps: []*runnerv1.Step{
				bash("mk", "mkdir -p sub/dir && echo inner > sub/dir/f"),
				{Name: "cd", Run: "cat f", WorkingDirectory: "sub/dir"},
			},
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
			logHas:  []string{"inner"},
		},
		{
			name:    "working-directory 不存在是用户错误",
			steps:   []*runnerv1.Step{{Name: "cd", Run: "true", WorkingDirectory: "missing"}},
			outcome: runnerv1.Outcome_OUTCOME_FAILED,
			class:   classUser,
			logHas:  []string{`working-directory "missing" 不存在`},
		},
		{
			name:    "命令不在 PATH 中",
			steps:   []*runnerv1.Step{{Name: "py", Run: "x", Shell: "no-such-interpreter-kiln {0}"}},
			outcome: runnerv1.Outcome_OUTCOME_FAILED,
			class:   classUser,
			logHas:  []string{"启动 no-such-interpreter-kiln 失败"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			got, log := runJob(t, r, fs, hostJob(1, c.steps...))
			wantOutcome(t, got, log, c.outcome, c.class)
			if got.ExitCode != c.exitCode {
				t.Errorf("exit_code = %d，期望 %d", got.ExitCode, c.exitCode)
			}
			for _, s := range c.logHas {
				if !strings.Contains(log, s) {
					t.Errorf("日志中没有 %q：\n%s", s, log)
				}
			}
			for _, s := range c.logLacks {
				if strings.Contains(log, s) {
					t.Errorf("日志中不应有 %q：\n%s", s, log)
				}
			}
		})
	}
}

// 超时后结束 step 的整个进程组，结果为 timed_out。
func TestExecuteTimeout(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("slow", "sleep 30"), bash("next", "echo not-run"))
	spec.TimeoutSeconds = 1
	start := time.Now()
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_TIMED_OUT, classTimeout)
	if !strings.Contains(got.Message, "超过 1s") {
		t.Errorf("message = %q", got.Message)
	}
	if strings.Contains(log, "not-run") {
		t.Error("超时后不应继续执行之后的 step")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("超时后用了 %s 才结束", el)
	}
}

// heartbeatAtStep 返回一个心跳钩子：Runner 上报已经开始执行 step 后调用 f 一次。
func heartbeatAtStep(f func() (*runnerv1.HeartbeatResponse, error)) func(*runnerv1.HeartbeatRequest) (*runnerv1.HeartbeatResponse, error) {
	return func(req *runnerv1.HeartbeatRequest) (*runnerv1.HeartbeatResponse, error) {
		if req.CurrentStep < 0 {
			return nil, nil
		}
		return f()
	}
}

// 心跳响应中带取消指令时，结束正在执行的 step，结果为 cancelled；step 收到的是 SIGTERM。
func TestExecuteCancelByHeartbeat(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	// 租约 1 秒：心跳间隔为 250ms
	spec := hostJob(1, bash("wait", "trap 'echo got-sigterm; exit 1' TERM\necho started\nsleep 30 &\nwait"), bash("next", "echo not-run"))
	spec.LeaseTtlSeconds = 1
	fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
		return &runnerv1.HeartbeatResponse{CancelRequested: true}, nil
	})
	start := time.Now()
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_CANCELLED, "")
	for _, s := range []string{"收到取消指令", "结果：已取消"} {
		if !strings.Contains(log, s) {
			t.Errorf("日志中没有 %q：\n%s", s, log)
		}
	}
	if strings.Contains(log, "not-run") {
		t.Error("取消后不应继续执行之后的 step")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("取消后用了 %s 才结束", el)
	}
	if got.Ref.Epoch != testEpoch {
		t.Errorf("提交的 epoch = %d，期望 %d", got.Ref.Epoch, testEpoch)
	}
}

// 心跳返回 STALE_EPOCH：attempt 已不属于本 Runner，立即停止并且不提交结果。
func TestExecuteStaleEpoch(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("wait", "sleep 30"))
	spec.LeaseTtlSeconds = 1
	fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
		return nil, rpcerr.StaleEpoch()
	})
	start := time.Now()
	got, log := runJob(t, r, fs, spec)
	if got != nil {
		t.Fatalf("epoch 过期后不应提交结果，实际提交了 %s；日志：\n%s", got.Outcome, log)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("epoch 过期后用了 %s 才结束", el)
	}
}

// 连续连不上 Server 超过上限（PLAN #6.3）：Runner 自行停止 job，结果为 infra.disconnected。
// 普通 job 的上限是 runner_max_disconnected，有副作用的 job 是租约时长（#6.7）。
func TestExecuteDisconnected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		effects bool
		limit   time.Duration // Runner 的 runner_max_disconnected
	}{
		{"普通 job", false, time.Second},
		{"有副作用的 job 按租约时长", true, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			r.maxDisconnected = tc.limit
			spec := hostJob(1, bash("wait", "sleep 30"), bash("next", "echo not-run"))
			spec.LeaseTtlSeconds, spec.Effects = 1, tc.effects
			fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("网络不通"))
			})
			start := time.Now()
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classDisconnected)
			if el := time.Since(start); el > 15*time.Second {
				t.Errorf("连不上 Server 后用了 %s 才停止", el)
			}
			if !strings.Contains(log, "连不上 Kiln Server（上限 1s），停止执行") || strings.Contains(log, "not-run") {
				t.Errorf("日志：\n%s", log)
			}
		})
	}
}

// Server 不响应（心跳一直等不到结果）时同样按上限停止，不会因为等待单次心跳（heartbeatTimeout）而超过上限。
func TestExecuteDisconnectedHang(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("wait", "sleep 30"))
	spec.LeaseTtlSeconds, spec.Effects = 1, true
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 先于 fake server 关闭执行，让挂起的请求返回
	fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
		<-release
		return nil, nil
	})
	start := time.Now()
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classDisconnected)
	if el := time.Since(start); el > heartbeatTimeout/2 {
		t.Errorf("Server 不响应时用了 %s 才停止（上限 1s）", el)
	}
}

// 短暂连不上 Server（没有超过上限）后恢复：job 照常执行完。
func TestExecuteDisconnectedRecovers(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	r.maxDisconnected = 2 * time.Second
	spec := hostJob(1, bash("wait", "sleep 3"))
	spec.LeaseTtlSeconds = 1 // 心跳间隔 250ms
	var mu sync.Mutex
	n := 0
	fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		// 每 4 次心跳（约 1 秒）中失败 3 次：断续连不上约 0.75 秒，每次都没有超过上限；总时长超过上限
		if n++; n%4 != 0 {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("网络不通"))
		}
		return nil, nil
	})
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
}

// Runner 退出（parent ctx 结束）时中断 step，仍然提交 infra.runner_shutdown 的结果。
func TestExecuteRunnerShutdown(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("wait", "sleep 30"))
	spec.LeaseTtlSeconds = 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fs.heartbeatHook = heartbeatAtStep(func() (*runnerv1.HeartbeatResponse, error) {
		cancel()
		return nil, nil
	})
	fs.register(spec)
	r.newJob(spec).run(ctx)
	got, log := completedJob(fs, 1)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classRunnerShutdown)
}

// 提交结果遇到临时错误时重试；结果只提交一次，带领取时的 epoch。
func TestExecuteCompleteRetry(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.completeErrs = []error{connect.NewError(connect.CodeUnavailable, errors.New("Server 重启中"))}
	got, log := runJob(t, r, fs, hostJob(1, bash("ok", "true")))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if n := len(fs.completeCalls()); n != 1 {
		t.Errorf("提交了 %d 次结果，期望 1 次", n)
	}
	if got.Ref.AttemptId != 1 || got.Ref.Epoch != testEpoch {
		t.Errorf("ref = %v", got.Ref)
	}
}

// 提交结果时 epoch 已过期（例如 sweeper 已经回收了 attempt）：不再重试。
func TestExecuteCompleteStale(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.completeErrs = []error{rpcerr.StaleEpoch(), rpcerr.StaleEpoch()}
	got, _ := runJob(t, r, fs, hostJob(1, bash("ok", "true")))
	if got != nil {
		t.Fatalf("不应提交成功：%v", got)
	}
	fs.mu.Lock()
	rest := len(fs.completeErrs)
	fs.mu.Unlock()
	if rest != 1 {
		t.Errorf("STALE_EPOCH 后又重试了（剩余注入的错误 %d 个，期望 1 个）", rest)
	}
}

// 基础设施错误按阶段分类（PLAN #6.4）。
func TestExecuteInfraErrors(t *testing.T) {
	t.Parallel()
	rtDigest := cas.Digest([]byte("runtime"))
	cases := []struct {
		name  string
		setup func(t *testing.T, fs *fakeServer, r *Runner, spec *runnerv1.JobSpec)
		class string
	}{
		{
			name: "获取凭证失败",
			setup: func(_ *testing.T, fs *fakeServer, _ *Runner, _ *runnerv1.JobSpec) {
				fs.secretsErr = connect.NewError(connect.CodeNotFound, errors.New("attempt 不存在"))
			},
			class: classCredentials,
		},
		{
			name: "源码拉取失败",
			setup: func(t *testing.T, _ *fakeServer, _ *Runner, spec *runnerv1.JobSpec) {
				t.Helper()
				spec.Source = &runnerv1.Source{RepoId: 1, Repository: "o/r", CloneUrl: "file://" + filepath.Join(t.TempDir(), "missing"), Sha: strings.Repeat("a", 40)}
			},
			class: classFetch,
		},
		{
			name: "不支持的 executor",
			setup: func(_ *testing.T, _ *fakeServer, _ *Runner, spec *runnerv1.JobSpec) {
				spec.Executor.Kind = "tart"
			},
			class: classPrepare,
		},
		{
			name: "不支持的 runtime",
			setup: func(_ *testing.T, _ *fakeServer, _ *Runner, spec *runnerv1.JobSpec) {
				spec.Runtimes = []*runnerv1.RuntimeRef{{Name: "ruby", Version: "3.3.0", Platform: runnerkit.NativePlatform(), Digest: rtDigest}}
			},
			class: classRuntime,
		},
		{
			name: "runtime 的环境变量冲突",
			setup: func(t *testing.T, _ *fakeServer, r *Runner, spec *runnerv1.JobSpec) {
				t.Helper()
				a, b := cas.Digest([]byte("a")), cas.Digest([]byte("b"))
				installRuntime(t, r, "go", a, map[string]toolchain.EnvSpec{"GOROOT": {Path: "."}})
				installRuntime(t, r, "node", b, map[string]toolchain.EnvSpec{"GOROOT": {Path: "x"}})
				spec.Runtimes = []*runnerv1.RuntimeRef{
					{Name: "go", Version: "1", Platform: runnerkit.NativePlatform(), Digest: a},
					{Name: "node", Version: "1", Platform: runnerkit.NativePlatform(), Digest: b},
				}
			},
			class: classRuntime,
		},
		{
			name: "上游输出不存在",
			setup: func(_ *testing.T, _ *fakeServer, _ *Runner, spec *runnerv1.JobSpec) {
				spec.Fetch = []*runnerv1.FetchSpec{{From: "build.out", To: "deps", ManifestDigest: cas.Digest([]byte("missing"))}}
			},
			class: classFetch,
		},
		{
			name: "job 需要 buildkit 但 Runner 没有开启",
			setup: func(t *testing.T, _ *fakeServer, r *Runner, spec *runnerv1.JobSpec) {
				t.Helper()
				r.cfg.Docker, _ = fakeDocker(t)
				spec.Executor = &runnerv1.ExecutorSpec{Kind: "oci", Image: "alpine@sha256:abc", Platform: "linux/arm64", BuildkitImage: "moby/buildkit@sha256:" + strings.Repeat("b", 64)}
			},
			class: classPrepare,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			spec := hostJob(1, bash("never", "echo step-ran"))
			c.setup(t, fs, r, spec)
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, c.class)
			if strings.Contains(log, "step-ran") {
				t.Errorf("准备阶段失败后不应执行 step：\n%s", log)
			}
			if _, err := os.Stat(r.workDir(1)); err == nil {
				t.Error("失败后工作区应删除")
			}
		})
	}
}

// installRuntime 在 Runner 的 runtime 目录中放一个"已安装"的 runtime（带元数据），不访问网络。
func installRuntime(t *testing.T, r *Runner, name, digest string, env map[string]toolchain.EnvSpec) string {
	t.Helper()
	dir := r.cfg.path(rtDirName, strings.TrimPrefix(digest, cas.DigestPrefix))
	m := toolchain.Meta{Name: name, Version: "1", Platform: runnerkit.NativePlatform(), Digest: digest, Env: env}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, ".kiln-runtime.json"), string(b))
	return dir
}

// runtime 的环境变量按挂载点展开：PATH 前置、path、value。
func TestExecuteRuntimes(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	d := cas.Digest([]byte("go-runtime"))
	dir := installRuntime(t, r, "go", d, map[string]toolchain.EnvSpec{
		"PATH":        {PrependPaths: []string{"bin"}},
		"GOROOT":      {Path: "."},
		"GOTOOLCHAIN": {Value: "local", IsValue: true},
	})
	mustWrite(t, filepath.Join(dir, "bin", "kiln-test-tool"), "#!/bin/sh\necho tool-ran\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "kiln-test-tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := hostJob(1, bash("env", printEnv+"\nkiln-test-tool"))
	spec.Runtimes = []*runnerv1.RuntimeRef{{Name: "go", Version: "1", Platform: runnerkit.NativePlatform(), Digest: d}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	env := stepEnv(log)
	if want := dir + "/bin:/usr/bin:/bin:/usr/sbin:/sbin"; env["PATH"] != want {
		t.Errorf("PATH = %q，期望 %q", env["PATH"], want)
	}
	if env["GOROOT"] != dir || env["GOTOOLCHAIN"] != "local" {
		t.Errorf("GOROOT = %q，GOTOOLCHAIN = %q", env["GOROOT"], env["GOTOOLCHAIN"])
	}
	if !strings.Contains(log, "tool-ran") {
		t.Errorf("runtime 中的命令没有执行：\n%s", log)
	}
	// 已经装好的 runtime：日志写明直接使用
	if want := "runtime go 1（" + runnerkit.NativePlatform() + "）：已安装，直接使用"; !strings.Contains(log, want) {
		t.Errorf("日志中没有 %q：\n%s", want, log)
	}
}

// toolcache 目录按 执行身份/仓库/平台/名称 持久保存，不随工作区删除（PLAN #4.6）。
func TestExecuteToolcache(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1, bash("tc", `echo "TC=$GOMODCACHE"; echo cached > "$GOMODCACHE/marker"`))
	spec.Source = &runnerv1.Source{RepoId: 42}
	spec.Toolcache = []*runnerv1.ToolCache{{Name: "gomod", Env: "GOMODCACHE"}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	dir := r.cfg.path(toolcacheDirName, "host", "42", strings.ReplaceAll(runnerkit.NativePlatform(), "/", "-"), "gomod")
	if !strings.Contains(log, "TC="+dir) {
		t.Errorf("GOMODCACHE 应为 %s：\n%s", dir, log)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "marker")); err != nil || string(b) != "cached\n" {
		t.Errorf("toolcache 中的文件：%q，%v", b, err)
	}
}

// secret 替换进脚本和 env，日志中的原文、base64 都被脱敏（PLAN #16.3）。
func TestExecuteSecrets(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	const secret = "s3cr3t-value-123"
	fs.secrets = map[string]string{"TOKEN": secret}
	step := &runnerv1.Step{
		Name: "use",
		Run:  `echo "env=$FROM_ENV"; echo "inline=${{ secrets.TOKEN }}"; printf %s "$FROM_ENV" | base64; test "$FROM_ENV" = "` + secret + `"`,
		Env:  map[string]string{"FROM_ENV": "${{ secrets.TOKEN }}"},
	}
	got, log := runJob(t, r, fs, hostJob(1, step))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, leak := range []string{secret, "czNjcjN0LXZhbHVlLTEyMw"} {
		if strings.Contains(log, leak) {
			t.Errorf("日志中出现了 secret（%s）：\n%s", leak, log)
		}
	}
	for _, s := range []string{"env=***", "inline=***"} {
		if !strings.Contains(log, s) {
			t.Errorf("日志中没有 %q：\n%s", s, log)
		}
	}
}

// 提交给 Server 的结果说明（会显示在 Dashboard 和 Check Run 中）与日志一样脱敏：错误信息中可能带有命令输出。
func TestExecuteResultMessageMasked(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	const secret = "s3cr3t-in-error-456"
	fs.secrets = map[string]string{"TOKEN": secret}
	spec := hostJob(1, bash("never", "true"))
	// git 的错误信息中带有 clone 地址
	spec.Source = &runnerv1.Source{RepoId: 1, Repository: "o/r", CloneUrl: "file://" + filepath.Join(t.TempDir(), secret), Sha: strings.Repeat("a", 40)}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classFetch)
	if strings.Contains(got.Message, secret) || !strings.Contains(got.Message, "***") {
		t.Errorf("结果说明没有脱敏：%s", got.Message)
	}
}

// 输出收集：目录、glob、单个文件，上传到 Server 的 CAS；Server 已有的对象不重复上传。
func TestExecuteOutputs(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.putBlob([]byte("a")) // Server 已有 a.txt 的内容
	spec := hostJob(1, bash("build", `mkdir -p dist/sub
printf a > dist/a.txt
printf b > dist/sub/b.bin && chmod +x dist/sub/b.bin
ln -s a.txt dist/link
printf r > report.txt`))
	spec.Outputs = []*runnerv1.OutputSpec{
		{Name: "dist", Path: "dist/"},
		{Name: "bins", Path: "dist/**/*.bin"},
		{Name: "report", Path: "report.txt"},
	}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if !cas.ValidDigest(got.ResultManifestDigest) {
		t.Fatalf("result_manifest_digest = %q", got.ResultManifestDigest)
	}
	outs := readResult(t, fs, got.ResultManifestDigest)
	want := map[string][]cas.FileEntry{
		"dist": {
			{Path: "a.txt", Digest: cas.Digest([]byte("a")), Size: 1},
			{Path: "link", Symlink: "a.txt"},
			{Path: "sub/b.bin", Digest: cas.Digest([]byte("b")), Size: 1, Exec: true},
		},
		"bins":   {{Path: "sub/b.bin", Digest: cas.Digest([]byte("b")), Size: 1, Exec: true}},
		"report": {{Path: "report.txt", Digest: cas.Digest([]byte("r")), Size: 1}},
	}
	if len(outs) != len(want) {
		t.Fatalf("输出 = %v", outs)
	}
	for name, files := range want {
		om := outs[name]
		if om.Schema != cas.OutputSchema || !slices.Equal(om.Files, files) {
			t.Errorf("输出 %s = %+v，期望 %+v", name, om, files)
		}
	}
	for _, c := range []string{"b", "r"} {
		if b, ok := fs.blob(cas.Digest([]byte(c))); !ok || string(b) != c {
			t.Errorf("Server 的 CAS 中没有内容 %q", c)
		}
	}
	// b.bin 在两个输出中只上传一次，a.txt 不上传：b、r、3 个输出 manifest、1 个结果 manifest
	fs.mu.Lock()
	puts := fs.putCalls
	fs.mu.Unlock()
	if puts != 6 {
		t.Errorf("上传了 %d 个对象，期望 6 个", puts)
	}
	if !strings.Contains(log, "输出 dist：3 个文件") {
		t.Errorf("日志中没有输出的统计：\n%s", log)
	}
}

// 输出路径的错误是用户错误：不存在、没有匹配、符号链接指向工作区或输出目录之外。
func TestExecuteOutputErrors(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"), "x")
	cases := []struct {
		name   string
		script string
		path   string
	}{
		{name: "不存在", script: "true", path: "dist/"},
		{name: "glob 没有匹配", script: "mkdir dist", path: "dist/*.zip"},
		{name: "符号链接指向输出目录之外", script: "mkdir dist && echo x > top && ln -s ../top dist/escape", path: "dist"},
		{name: "目录经由符号链接指向工作区之外", script: "ln -s " + outside + " leak", path: "leak/secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			spec := hostJob(1, bash("build", c.script))
			spec.Outputs = []*runnerv1.OutputSpec{{Name: "out", Path: c.path}}
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_FAILED, classUser)
			if !strings.HasPrefix(got.Message, "输出 out：") {
				t.Errorf("message = %q", got.Message)
			}
		})
	}
}

// fetch：上游输出从 Server 的 CAS 下载到工作区，step 可以直接使用；本地缓存的对象不再下载。
func TestExecuteFetch(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	toolContent, readmeContent := "#!/bin/sh\necho tool-from-upstream\n", "upstream readme\n"
	tool := fs.putBlob([]byte(toolContent))
	readme := fs.putBlob([]byte(readmeContent))
	mb, err := actionkey.Canonical(cas.OutputManifest{Schema: cas.OutputSchema, Files: []cas.FileEntry{
		{Path: "README", Digest: readme, Size: int64(len(readmeContent))},
		{Path: "bin/tool", Digest: tool, Size: int64(len(toolContent)), Exec: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := fs.putBlob(mb)
	spec := hostJob(1, bash("use", "cat deps/README\n./deps/bin/tool"))
	spec.Fetch = []*runnerv1.FetchSpec{{From: "build.out", To: "deps", ManifestDigest: m}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, s := range []string{"fetch build.out → deps（2 个文件）", "upstream readme", "tool-from-upstream"} {
		if !strings.Contains(log, s) {
			t.Errorf("日志中没有 %q：\n%s", s, log)
		}
	}
	for _, d := range []string{m, tool, readme} {
		if has, err := r.casStore().Has(t.Context(), d); err != nil || !has {
			t.Errorf("本地 CAS 缓存中没有 %s：%v", d, err)
		}
	}
	// 第二个 job：对象都在本地缓存中，Server 上删掉也不影响
	fs.mu.Lock()
	clear(fs.blobs)
	fs.mu.Unlock()
	got, log = runJob(t, r, fs, func() *runnerv1.JobSpec {
		s := hostJob(2, bash("use", "./deps/bin/tool"))
		s.Fetch = spec.Fetch
		return s
	}())
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
}

// 源码检出：用本地 git 仓库作为 clone 地址，只检出 inputs 选中的文件，没有 .git；输入摘要必须一致。
func TestExecuteSource(t *testing.T) {
	t.Parallel()
	repo, _ := newTestRepo(t)
	sha := commitFiles(t, repo, map[string]string{
		"src/main.go":           "package main\n",
		"docs/guide.md":         "# guide\n",
		".kiln/workflows/c.yml": "on: push\n",
	})
	// 与 Planner 相同的算法计算期望的输入摘要
	digestOf := func(inputs []string) string {
		m := workflow.NewInputMatcher(inputs)
		var sel []actionkey.TreeEntry
		for _, e := range parseLsTree([]byte(runGit(t, repo, "", "ls-tree", "-r", "-z", "--full-tree", sha))) {
			if m.Match(e.Path) {
				sel = append(sel, e)
			}
		}
		return actionkey.InputDigest(sel)
	}
	cases := []struct {
		name        string
		inputs      []string
		digest      string
		check       string
		outcome     runnerv1.Outcome
		class       string
		logContains string
	}{
		{
			name:    "只检出 inputs 选中的文件",
			inputs:  []string{"src/**"},
			digest:  digestOf([]string{"src/**"}),
			check:   "test -f src/main.go && test ! -e docs && test ! -e a.txt && test ! -e .git",
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
		},
		{
			name:    "默认 inputs：整个仓库，不含 .kiln/workflows",
			digest:  digestOf(nil),
			check:   "test -f src/main.go && test -f docs/guide.md && test -x bin/run.sh && test -L link && test ! -e .kiln/workflows/c.yml",
			outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED,
		},
		{
			name:        "输入摘要不一致",
			inputs:      []string{"src/**"},
			digest:      digestOf(nil),
			check:       "true",
			outcome:     runnerv1.Outcome_OUTCOME_INFRA_ERROR,
			class:       classFetch,
			logContains: "输入摘要不一致",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			spec := hostJob(1, bash("check", c.check))
			spec.Source = &runnerv1.Source{RepoId: 9, Repository: "o/r", CloneUrl: "file://" + repo, Sha: sha, Inputs: c.inputs, InputDigest: c.digest}
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, c.outcome, c.class)
			if c.logContains != "" && !strings.Contains(got.Message, c.logContains) {
				t.Errorf("message = %q，应包含 %q", got.Message, c.logContains)
			}
		})
	}
}

// 同一个仓库的 commit 已在 bare 镜像中时不再拉取。
func TestExecuteSourceReusesMirror(t *testing.T) {
	t.Parallel()
	repo, _ := newTestRepo(t)
	sha := runGit(t, repo, "", "rev-parse", "HEAD")
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	src := &runnerv1.Source{RepoId: 9, Repository: "o/r", CloneUrl: "file://" + repo, Sha: sha}
	for i, wantFetch := range []bool{true, false} {
		spec := hostJob(int64(i+1), bash("check", "test -f a.txt"))
		spec.Source = src
		got, log := runJob(t, r, fs, spec)
		wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
		if fetched := strings.Contains(log, "拉取 o/r@"); fetched != wantFetch {
			t.Errorf("第 %d 个 job：拉取 = %v，期望 %v", i+1, fetched, wantFetch)
		}
	}
	if _, err := os.Stat(r.cfg.path(gitDirName, "9.git", "HEAD")); err != nil {
		t.Errorf("没有 bare 镜像：%v", err)
	}
}

// 工作区清理：成功时删除；失败且 keep_on_failure 或配置了 keep_workspaces 时保留（PLAN #8.9）。
func TestExecuteWorkspaceCleanup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		fail           bool
		keepOnFailure  bool
		keepWorkspaces bool
		kept           bool
	}{
		{name: "成功时删除"},
		{name: "失败时删除", fail: true},
		{name: "失败且 keep_on_failure 时保留", fail: true, keepOnFailure: true, kept: true},
		{name: "成功时 keep_on_failure 不生效", keepOnFailure: true},
		{name: "keep_workspaces 时保留", keepWorkspaces: true, kept: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs, func(cfg *Config) { cfg.KeepWorkspaces = c.keepWorkspaces })
			// 只读目录（例如 Go 模块缓存）也要能删除
			script := "mkdir -p ro/sub && touch ro/sub/f && chmod 555 ro/sub ro"
			if c.fail {
				script += "\nexit 1"
			}
			spec := hostJob(1, bash("work", script))
			spec.KeepOnFailure = c.keepOnFailure
			got, log := runJob(t, r, fs, spec)
			if got == nil {
				t.Fatalf("没有提交结果：\n%s", log)
			}
			_, err := os.Stat(filepath.Join(r.workDir(1), attemptWorkspaceDir, "ro", "sub", "f"))
			if kept := err == nil; kept != c.kept {
				t.Errorf("工作区保留 = %v，期望 %v", kept, c.kept)
			}
			if c.kept && !strings.Contains(log, "保留工作区："+r.workDir(1)) {
				t.Errorf("日志中没有保留工作区的说明：\n%s", log)
			}
			if c.kept {
				_ = runnerkit.RemoveTree(r.workDir(1)) // 让 t.TempDir 能清理
			}
		})
	}
}

// 日志按 offset 分段上传：超过单次上限的输出分多次追加，Server 上的内容完整、连续。
func TestExecuteLogChunks(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	const lines = 250000 // 每行 11 字节，约 2.6MB，超过 maxAppendChunk
	got, log := runJob(t, r, fs, hostJob(1, bash("big", "awk 'BEGIN { for (i = 0; i < "+strconv.Itoa(lines)+"; i++) print \"abcdefghij\" }'")))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if n := strings.Count(log, "abcdefghij\n"); n != lines {
		t.Errorf("日志中有 %d 行输出，期望 %d 行", n, lines)
	}
	fs.mu.Lock()
	calls := fs.appendCalls
	fs.mu.Unlock()
	if calls < 3 {
		t.Errorf("AppendLog 调用了 %d 次，期望分段上传（至少 3 次）", calls)
	}
	// 头部和结果行都在
	if !strings.HasPrefix(log, "\x1b[36m[kiln]\x1b[0m ci · test · run #1 · job #101 · attempt #1 · Runner test-runner\n") {
		t.Errorf("日志开头：%q", log[:min(len(log), 200)])
	}
	if !strings.Contains(log, "结果：成功") {
		t.Error("日志中没有结果行")
	}
}

// step 结束后，发送剩余日志和提交结果很慢时心跳仍要续约，否则 Server 可能在结果提交前判定租约过期、
// 把已经成功的 job 当作丢失重新调度。收尾期间收到的取消指令不再改变结果。
func TestHeartbeatDuringFinish(t *testing.T) {
	t.Parallel()
	const block = 1500 * time.Millisecond // 超过 1 秒的租约
	cases := []struct {
		name    string
		slowLog bool
		cancel  bool
		outcome runnerv1.Outcome
	}{
		{name: "发送剩余日志慢", slowLog: true, outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED},
		{name: "提交结果慢", outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED},
		{name: "取消后发送剩余日志慢", slowLog: true, cancel: true, outcome: runnerv1.Outcome_OUTCOME_CANCELLED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			script := "echo done"
			if tc.cancel {
				script = "sleep 30"
			}
			spec := hostJob(1, bash("build", script))
			spec.LeaseTtlSeconds = 1 // 心跳间隔 250ms

			var mu sync.Mutex
			var beats []time.Time
			var blockedFrom, blockedTo time.Time
			fs.heartbeatHook = func(req *runnerv1.HeartbeatRequest) (*runnerv1.HeartbeatResponse, error) {
				mu.Lock()
				defer mu.Unlock()
				beats = append(beats, time.Now())
				return &runnerv1.HeartbeatResponse{CancelRequested: tc.cancel && req.CurrentStep >= 0}, nil
			}
			slow := func() {
				mu.Lock()
				if !blockedFrom.IsZero() {
					mu.Unlock()
					return
				}
				blockedFrom = time.Now()
				mu.Unlock()
				time.Sleep(block)
				mu.Lock()
				blockedTo = time.Now()
				mu.Unlock()
			}
			if tc.slowLog {
				fs.appendHook = func(req *runnerv1.AppendLogRequest) {
					if strings.Contains(string(req.Data), "结果：") {
						slow()
					}
				}
			} else {
				fs.completeHook = func(*runnerv1.CompleteRequest) { slow() }
			}
			got, log := runJob(t, r, fs, spec)
			wantOutcome(t, got, log, tc.outcome, "")
			mu.Lock()
			defer mu.Unlock()
			if blockedFrom.IsZero() {
				t.Fatal("收尾时没有经过慢请求")
			}
			n := 0
			for _, b := range beats {
				if b.After(blockedFrom) && b.Before(blockedTo) {
					n++
				}
			}
			if n < 3 {
				t.Fatalf("收尾的 %s 内只收到 %d 次心跳，租约 1 秒会过期", block, n)
			}
			if strings.Count(log, "收到取消指令") > 1 {
				t.Errorf("取消指令只应记录一次：\n%s", log)
			}
		})
	}
}
