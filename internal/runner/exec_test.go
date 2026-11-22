package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 后台进程拿着输出管道时，step 不能一直卡住（setsid 的进程不在进程组里，杀不掉）。
func TestHostExecBackgroundHoldsPipe(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("需要 perl")
	}
	h := &hostExec{d: newTestDirs(t)}
	var out bytes.Buffer
	start := time.Now()
	script := "perl -e 'use POSIX; setsid(); sleep 20' &\necho hi\n"
	code, err := h.run(context.Background(), stepSpec{script: script, env: map[string]string{"PATH": os.Getenv("PATH")}}, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, out.String())
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("step 被后台进程卡住 %s", el)
	}
	if !bytes.Contains(out.Bytes(), []byte("hi")) {
		t.Fatalf("输出：%s", out.String())
	}
}

func TestHostExecExitCodeAndCancel(t *testing.T) {
	t.Parallel()
	h := &hostExec{d: newTestDirs(t)}
	env := map[string]string{"PATH": os.Getenv("PATH")}
	var out bytes.Buffer
	code, err := h.run(context.Background(), stepSpec{script: "exit 3\n", env: env}, &out)
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = h.run(ctx, stepSpec{index: 1, script: "sleep 30\n", env: env}, &out)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 DeadlineExceeded，实际 %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("取消后没有及时结束：%s", el)
	}
}

func TestAttemptDirsPermissions(t *testing.T) {
	t.Parallel()
	d := newTestDirs(t)
	st, err := os.Stat(d.root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("attempt 根目录权限为 %o，期望 700", perm)
	}
	// 再次创建会清掉旧内容
	if err := os.WriteFile(filepath.Join(d.ws, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := newAttemptDirs(d.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d2.ws, "stale")); err == nil {
		t.Fatal("旧的工作区内容没有清掉")
	}
}

func TestExportScript(t *testing.T) {
	t.Parallel()
	s, err := exportScript(map[string]string{"B": "it's", "A_1": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "export A_1='x'\nexport B='it'\\''s'\n"; s != want {
		t.Fatalf("exportScript = %q，期望 %q", s, want)
	}
	for _, name := range []string{"1A", "A-B", "A B", "A;touch /tmp/x;B", ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := exportScript(map[string]string{name: "v"})
			if _, ok := errors.AsType[*userError](err); !ok {
				t.Errorf("变量名 %q：期望 userError，实际 %v", name, err)
			}
		})
	}
}

// shell 的写法与 GitHub Actions 一致。
func TestShellCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		shell     string
		container bool
		want      string
		userErr   bool
	}{
		{name: "默认 bash", want: "bash --noprofile --norc -eo pipefail /s"},
		{name: "容器中没有 bash 时退回 sh", container: true, want: `sh -c if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -eo pipefail "$0"; else exec sh -e "$0"; fi /s`},
		{name: "bash", shell: "bash", container: true, want: "bash --noprofile --norc -eo pipefail /s"},
		{name: "sh", shell: "sh", want: "sh -e /s"},
		{name: "python", shell: "python", want: "python3 /s"},
		{name: "自定义命令", shell: "node {0} --flag", want: "node /s --flag"},
		{name: "不支持的 shell", shell: "fish", userErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			argv, err := shellCommand(c.shell, "/s", c.container)
			if c.userErr {
				if _, ok := errors.AsType[*userError](err); !ok {
					t.Fatalf("期望 userError，实际 %v", err)
				}
				return
			}
			if err != nil || strings.Join(argv, " ") != c.want {
				t.Errorf("shellCommand = %q, %v，期望 %q", strings.Join(argv, " "), err, c.want)
			}
		})
	}
}

// envList 按名称排序，结果稳定。
func TestEnvList(t *testing.T) {
	t.Parallel()
	got := envList(map[string]string{"B": "2", "A": "1=x", "C": ""})
	if strings.Join(got, ";") != "A=1=x;B=2;C=" {
		t.Errorf("envList = %v", got)
	}
}

func TestExitStatus(t *testing.T) {
	t.Parallel()
	err := exec.Command("sh", "-c", "exit 4").Run()
	if code, e := exitStatus(err); code != 4 || e != nil {
		t.Errorf("非零退出：%d, %v", code, e)
	}
	if code, e := exitStatus(nil); code != 0 || e != nil {
		t.Errorf("成功：%d, %v", code, e)
	}
	other := errors.New("启动失败")
	if code, e := exitStatus(other); code != -1 || !errors.Is(e, other) {
		t.Errorf("其他错误：%d, %v", code, e)
	}
}

func TestPollUntil(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name  string
		ctx   context.Context
		check func(n int) (bool, error)
		want  error
		calls int
	}{
		{name: "第一次就完成", ctx: context.Background(), check: func(int) (bool, error) { return true, nil }, calls: 1},
		{name: "第三次完成", ctx: context.Background(), check: func(n int) (bool, error) { return n == 3, nil }, calls: 3},
		{name: "检查出错", ctx: context.Background(), check: func(int) (bool, error) { return false, boom }, want: boom, calls: 1},
		{name: "ctx 已结束", ctx: cancelled, check: func(int) (bool, error) { return false, nil }, want: context.Canceled, calls: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n := 0
			err := pollUntil(c.ctx, time.Minute, time.Millisecond, func() (bool, error) {
				n++
				return c.check(n)
			})
			if !errors.Is(err, c.want) || n != c.calls {
				t.Errorf("pollUntil = %v，调用 %d 次，期望 %v、%d 次", err, n, c.want, c.calls)
			}
		})
	}
	// 超过时间上限
	if err := pollUntil(context.Background(), 0, time.Millisecond, func() (bool, error) { return false, nil }); !errors.Is(err, errPollTimeout) {
		t.Errorf("超时时 pollUntil = %v", err)
	}
}

// cleanupContext 不随 job 取消，但有超时。
func TestCleanupContext(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, done := cleanupContext(parent)
	defer done()
	if ctx.Err() != nil {
		t.Error("清理用的 context 不应随 job 取消")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("清理用的 context 应有超时")
	}
}

func TestResultFromErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		err   error
		class string
		code  int
	}{
		{"用户错误", userErrorf("x"), classUser, 0},
		{"step 退出码", &userError{msg: "x", exitCode: 2}, classUser, 2},
		{"基础设施错误", infra(classFetch, errors.New("x")), classFetch, 0},
		{"infra 不包装用户错误", infra(classFetch, userErrorf("x")), classUser, 0},
		{"未分类", errors.New("x"), classExec, 0},
		{"取消", errCancelled, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := resultFromErr(c.err)
			if res.class != c.class || res.exitCode != c.code {
				t.Errorf("resultFromErr = %+v", res)
			}
		})
	}
}
