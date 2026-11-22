package runnerkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// 自升级的 Runner 端测试（PLAN #27.7、#27.10）：启动器用真实子进程（本测试程序以 launch 模式运行，exec 测试中写的
// 脚本程序），下载核对用 httptest 和构造的 tarball（含恶意条目），切换和确认用替换的 exec。
// launchTestEnv 设置时测试程序作为启动器运行（TestMain）。
const launchTestEnv = "KILN_TEST_LAUNCH_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(launchTestEnv); dir != "" {
		err := Launch(dir, "kiln-runner", []string{"run"}, syscall.Exec)
		fmt.Fprintln(os.Stderr, "launch:", err)
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// fakeProgram 返回一个假的 kiln-runner：version 输出 "kiln-runner v<version>"，其他命令把版本和 KILN_LAUNCHER
// 追加到 $KILN_TEST_MARK，然后以 code 退出。
func fakeProgram(version string, code int) []byte {
	return fakeProgramFor("kiln-runner", version, code)
}

// fakeProgramFor 与 fakeProgram 相同，程序名为 prog。
func fakeProgramFor(prog, version string, code int) []byte {
	return fmt.Appendf(nil, `#!/bin/sh
if [ "$1" = version ]; then echo "%s v%s"; exit 0; fi
echo "%s $* launcher=$KILN_LAUNCHER" >> "$KILN_TEST_MARK"
exit %d
`, prog, version, version, code)
}

// installVersion 把假程序装到 versions/<v>/kiln-runner。
func installVersion(t *testing.T, l layout, v string, code int) {
	t.Helper()
	if err := writeFileAtomic(l.program(v), fakeProgram(v, code), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestChooseBoot(t *testing.T) {
	t.Parallel()
	usable := func(vs ...string) func(string) bool { return func(v string) bool { return slices.Contains(vs, v) } }
	cases := []struct {
		name     string
		st       BootState
		usable   func(string) bool
		want     string
		boots    int // 写回的 trial.boots（-1 表示 trial 已清掉）
		rollback string
		err      bool
	}{
		{name: "没有 trial：启动 current", st: BootState{Current: "1.0.0"}, usable: usable("1.0.0"), want: "1.0.0", boots: -1},
		{name: "trial 第 1 次", st: BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0"}}, usable: usable("1.0.0", "1.1.0"), want: "1.1.0", boots: 1},
		{name: "trial 第 3 次", st: BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: 2}}, usable: usable("1.0.0", "1.1.0"), want: "1.1.0", boots: 3},
		{name: "3 次后回滚", st: BootState{
			Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: 3, Generation: 5},
			Failed: []FailedVersion{{Version: "1.1.0", Generation: 1}, {Version: "0.8.0", Generation: 2}},
		}, usable: usable("1.0.0", "1.1.0"), want: "1.0.0", boots: -1, rollback: "1.1.0"},
		{name: "trial 文件不存在时回滚", st: BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0"}}, usable: usable("1.0.0"), want: "1.0.0", boots: -1, rollback: "1.1.0"},
		{name: "current 不存在时启动 previous", st: BootState{Current: "1.0.0", Previous: "0.9.0"}, usable: usable("0.9.0"), want: "0.9.0", boots: -1},
		{name: "都不能启动", st: BootState{Current: "1.0.0", Previous: "0.9.0"}, usable: usable(), err: true, boots: -1},
	}
	for _, tc := range cases {
		// 调用前的深拷贝：chooseBoot 是纯函数，不能改输入（trial 的计次、失败列表）
		before, err := json.Marshal(tc.st)
		if err != nil {
			t.Fatal(err)
		}
		d, err := chooseBoot(tc.st, tc.usable)
		if after, _ := json.Marshal(tc.st); !bytes.Equal(before, after) {
			t.Errorf("%s：不应修改输入：%s → %s", tc.name, before, after)
		}
		if (err != nil) != tc.err || d.Version != tc.want || d.RolledBack != tc.rollback {
			t.Errorf("%s：启动 %q 回滚 %q err=%v", tc.name, d.Version, d.RolledBack, err)
			continue
		}
		switch {
		case tc.boots < 0 && d.State.Trial != nil:
			t.Errorf("%s：trial 应已清掉", tc.name)
		case tc.boots >= 0 && (d.State.Trial == nil || d.State.Trial.Boots != tc.boots):
			t.Errorf("%s：trial = %+v，想要 boots %d", tc.name, d.State.Trial, tc.boots)
		}
		if tc.rollback != "" && (!d.State.failedAt(tc.rollback, tc.st.Trial.Generation) || d.State.RolledBack == nil) {
			t.Errorf("%s：回滚的版本应记入 failed：%+v", tc.name, d.State)
		}
	}
}

// launchOnce 以子进程运行一次启动器，返回假程序写下的记录（最后一行）和退出码。
func launchOnce(t *testing.T, dir, mark string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), launchTestEnv+"="+dir, "KILN_TEST_MARK="+mark)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(mark)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	_ = out
	return lines[len(lines)-1], code
}

// TestLaunchExecRejected（U2，真实子进程）：current 的文件看起来可以执行（普通文件、有执行位）但内核
// 拒绝（内容损坏、架构不对）时改为启动 previous，并把它记为 current；只读的 bin/ 下试用版本写不了计次、current 又不在
// 时也启动 previous。
func TestLaunchExecRejected(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	t.Run("current 损坏", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mark := filepath.Join(dir, "mark")
		l := newLayout(dir, "kiln-runner")
		installVersion(t, l, "0.9.0", 0)
		if err := writeFileAtomic(l.program("1.0.0"), []byte("not a program\x00\x01"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := l.save(&BootState{Current: "1.0.0", Previous: "0.9.0"}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if line, code := launchOnce(t, dir, mark); line != "0.9.0 run launcher=1" {
				t.Fatalf("current 不能执行时应启动 previous：%q（退出码 %d）", line, code)
			}
		}
		if st, err := l.load(); err != nil || st.Current != "0.9.0" {
			t.Fatalf("应把 previous 记为 current：%+v %v", st, err)
		}
	})
	t.Run("只读且 current 不在", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root 不受目录权限限制")
		}
		dir := t.TempDir()
		mark := filepath.Join(dir, "mark")
		l := newLayout(dir, "kiln-runner")
		installVersion(t, l, "0.9.0", 0)
		installVersion(t, l, "1.1.0", 0)
		if err := l.save(&BootState{Current: "1.0.0", Previous: "0.9.0", Trial: &BootTrial{Version: "1.1.0", Generation: 1}}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(l.dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(l.dir, 0o755) })
		if line, code := launchOnce(t, dir, mark); line != "0.9.0 run launcher=1" {
			t.Fatalf("写不了计次、current 不在时应启动 previous：%q（退出码 %d）", line, code)
		}
	})
}

// TestLaunchRollback（U2、U3，真实子进程）：新版本启动后立即崩溃，启动器计次，3 次后回滚到 current 并记下失败；
// 之后一直启动 current，不再试新版本。exec 的是同一个进程（服务管理器只看到一个进程），带 KILN_LAUNCHER=1。
func TestLaunchRollback(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	l := newLayout(dir, "kiln-runner")
	installVersion(t, l, "1.0.0", 0)
	installVersion(t, l, "1.1.0", 1) // 坏版本：立即退出
	if err := l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Generation: 7}}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxTrialBoots; i++ {
		line, code := launchOnce(t, dir, mark)
		if line != "1.1.0 run launcher=1" || code != 1 {
			t.Fatalf("第 %d 次启动：%q（退出码 %d），想要运行新版本", i, line, code)
		}
		st, err := l.load()
		if err != nil || st.Trial == nil || st.Trial.Boots != i {
			t.Fatalf("第 %d 次启动后 trial = %+v（%v）", i, st.Trial, err)
		}
	}
	for range 2 {
		line, code := launchOnce(t, dir, mark)
		if line != "1.0.0 run launcher=1" || code != 0 {
			t.Fatalf("回滚后：%q（退出码 %d），想要运行 1.0.0", line, code)
		}
	}
	st, err := l.load()
	if err != nil || st.Trial != nil || st.Current != "1.0.0" || !st.failedAt("1.1.0", 7) || st.RolledBack == nil {
		t.Fatalf("回滚后的状态：%+v（%v）", st, err)
	}
}

// TestLaunchStateReadOnly（U3）：写不了计次时不启动试用版本（否则坏版本会无限重启），退回 current。
func TestLaunchStateReadOnly(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	l := newLayout(dir, "kiln-runner")
	installVersion(t, l, "1.0.0", 0)
	installVersion(t, l, "1.1.0", 1)
	if err := l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.dir, 0o755) })
	if line, _ := launchOnce(t, dir, mark); line != "1.0.0 run launcher=1" {
		t.Fatalf("写不了计次时应启动 current：%q", line)
	}
	// 已经试了 3 次、要回滚时写不了：照常启动 current（不能什么都不启动，独立审查发现）
	_ = os.Chmod(l.dir, 0o755)
	if err := l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: maxTrialBoots, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if line, code := launchOnce(t, dir, mark); line != "1.0.0 run launcher=1" || code != 0 {
		t.Fatalf("回滚时写不了状态也应启动 current：%q（退出码 %d）", line, code)
	}
	// current 也不在了：回滚到 previous
	_ = os.Chmod(l.dir, 0o755)
	installVersion(t, l, "0.9.0", 0)
	if err := os.RemoveAll(l.versionDir("1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := l.save(&BootState{Current: "1.0.0", Previous: "0.9.0", Trial: &BootTrial{Version: "1.1.0", Boots: maxTrialBoots}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if line, code := launchOnce(t, dir, mark); line != "0.9.0 run launcher=1" || code != 0 {
		t.Fatalf("回滚时 current 不在、写不了状态也应启动 previous：%q（退出码 %d）", line, code)
	}
}

// TestLaunchMissing：current 被删掉时启动 previous；都没有时启动器退出并说明原因。
func TestLaunchMissing(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	l := newLayout(dir, "kiln-runner")
	installVersion(t, l, "0.9.0", 0)
	if err := l.save(&BootState{Current: "1.0.0", Previous: "0.9.0"}); err != nil {
		t.Fatal(err)
	}
	if line, _ := launchOnce(t, dir, mark); line != "0.9.0 run launcher=1" {
		t.Fatalf("应启动 previous：%q", line)
	}
	if err := os.RemoveAll(l.versionDir("0.9.0")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), launchTestEnv+"="+dir, "KILN_TEST_MARK="+mark)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), ErrNoBootable.Error()) {
		t.Fatalf("都不能启动时应说明原因：%s", out)
	}
}

// pkg 构造 npm 包（gzip 的 tar）。
type tarEntry struct {
	name     string
	typ      byte
	data     []byte
	linkname string
}

func makePackage(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.data)), Linkname: e.linkname}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func integrityOf(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// upEnv 是 Upgrader 的测试环境：数据目录、已安装的布局（current 1.0.0）和进程内的 registry 替身（packument 和 tarball）。
type upEnv struct {
	t   *testing.T
	dir string
	l   layout
	srv *httptest.Server
	// mu 保护下面的 registry 内容（后台下载、查询与测试并发访问）。
	mu    sync.Mutex
	pkgs  map[string][]byte // 路径 → 内容
	extra map[string][]byte // 路径 → 多返回的字节（integrity 不符）
	dists map[string]*npm.Dist
	// latest 是 dist-tags.latest；gone 为真时包不存在（404），status 非 0 时 packument 返回这个状态码。
	latest  string
	gone    bool
	status  int
	queries int
	execs   [][]string
}

func newUpEnv(t *testing.T) *upEnv {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	e := &upEnv{t: t, dir: t.TempDir(), pkgs: map[string][]byte{}, extra: map[string][]byte{}, dists: map[string]*npm.Dist{}}
	e.l = newLayout(e.dir, "kiln-runner")
	installVersion(t, e.l, "1.0.0", 0)
	if err := writeFileAtomic(e.l.launcher(), fakeProgram("1.0.0", 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.l.save(&BootState{Current: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if name, ok := strings.CutPrefix(r.URL.Path, "/@"); ok {
			e.packument(w, "@"+name)
			return
		}
		data, ok := e.pkgs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(append(slices.Clone(data), e.extra[r.URL.Path]...))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// packument 返回包 name 的元数据（调用方持有 mu）。
func (e *upEnv) packument(w http.ResponseWriter, name string) {
	e.queries++
	switch {
	case e.status != 0:
		w.WriteHeader(e.status)
		return
	case e.gone:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	versions := map[string]npm.Version{}
	for v, d := range e.dists {
		versions[v] = npm.Version{Name: name, Version: v, Dist: *d}
	}
	_ = json.NewEncoder(w).Encode(npm.Packument{Name: name, DistTags: map[string]string{"latest": e.latest}, Versions: versions})
}

// dist 修改 registry 中 version 的 dist。
func (e *upEnv) dist(version string, f func(d *npm.Dist)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(e.dists[version])
}

// home 返回测试环境的数据目录（kiln-runner）。
func (e *upEnv) home() *Home { return &Home{Dir: e.dir, Prog: testProgram} }

// upgraderAt 返回本进程版本为 version、经过启动器运行的 Upgrader（exec 被替换为记录）。
func (e *upEnv) upgraderAt(version string, opts ...func(*Common)) *Upgrader {
	var c Common
	for _, o := range opts {
		o(&c)
	}
	u := newUpgraderVersion(e.home(), c.UpgradeEnabled(), slog.New(slog.DiscardHandler), "v"+version, true, nil)
	e.useRegistry(u)
	u.exec = func(argv0 string, argv, _ []string) error {
		e.execs = append(e.execs, argv)
		return errors.New("exec 被测试替换")
	}
	return u
}

// foreground 返回本进程版本为 version、不经过启动器运行（前台 run）的 Upgrader。
func (e *upEnv) foreground(version string) *Upgrader {
	u := newUpgraderVersion(e.home(), true, slog.New(slog.DiscardHandler), "v"+version, false, nil)
	e.useRegistry(u)
	return u
}

// setPkg 设置下载地址 path 的内容（data 为 nil 时删除：下载返回 404）。
func (e *upEnv) setPkg(path string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if data == nil {
		delete(e.pkgs, path)
		return
	}
	e.pkgs[path] = data
}

// useRegistry 让 u 使用 registry 替身（不重试）。
func (e *upEnv) useRegistry(u *Upgrader) {
	u.registry, u.npmOpts = e.srv.URL+"/", []npm.Option{npm.WithRetry(1, 0)}
}

// serve 在 registry 中发布一个版本，返回指令。
func (e *upEnv) serve(version string, data []byte, gen int64) *runnerv1.UpgradeDirective {
	path := "/kiln-runner-" + version + ".tgz"
	e.mu.Lock()
	e.pkgs[path] = data
	e.mu.Unlock()
	return e.serveURL(version, e.srv.URL+path, data, gen)
}

// serveURL 在 registry 中登记一个版本，下载地址是 url（内容 data），返回指令。
func (e *upEnv) serveURL(version, url string, data []byte, gen int64) *runnerv1.UpgradeDirective {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dists[version] = &npm.Dist{Tarball: url, Integrity: integrityOf(data)}
	return &runnerv1.UpgradeDirective{Version: version, Generation: gen}
}

func goodPackage(t *testing.T, version string) []byte {
	t.Helper()
	return makePackage(t, tarEntry{name: "package/package.json", typ: tar.TypeReg, data: []byte(`{}`)},
		tarEntry{name: "package/kiln-runner", typ: tar.TypeReg, data: fakeProgram(version, 0)})
}

// waitState 处理指令直到下载结束（不再是 downloading），返回最后一次 handle 的结果。
func waitState(t *testing.T, u *Upgrader, d *runnerv1.UpgradeDirective) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		drain := u.handle(t.Context(), d)
		st := u.StatusMsg()
		if st.State != UpgradeDownloading {
			return drain
		}
		if time.Now().After(deadline) {
			t.Fatal("下载没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUpgradeSwitchAndConfirm（U2、U4）：下载核对 → 排空 → 写 trial 后 exec 启动器；新版本第一次 Acquire 成功后确认，
// current / previous 前移、清掉其他版本目录。
func TestUpgradeSwitchAndConfirm(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	installVersion(t, e.l, "0.5.0", 0) // 不在 current / previous 中的旧版本，确认后删除
	u := e.upgraderAt("1.0.0")
	d := e.serve("1.1.0", goodPackage(t, "1.1.0"), 4)
	if !waitState(t, u, d) || u.StatusMsg().State != UpgradeDraining {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	if err := u.SwitchTo(); err == nil {
		t.Fatal("exec 被替换为失败，switchTo 应返回错误")
	}
	if len(e.execs) != 1 || e.execs[0][0] != e.l.launcher() || e.execs[0][1] != "launch" {
		t.Fatalf("应 exec 启动器：%v", e.execs)
	}
	// exec 失败时撤销 trial
	if st, _ := e.l.load(); st.Trial != nil {
		t.Fatalf("exec 失败后应撤销 trial：%+v", st.Trial)
	}
	// exec 时 trial 已经落盘（exec 成功不会返回：在 exec 中取下状态，模拟进程被替换）
	var st *BootState
	u.exec = func(string, []string, []string) error {
		st, _ = e.l.load()
		return errors.New("exec 被测试替换")
	}
	_ = u.SwitchTo()
	if st == nil || st.Trial == nil || st.Trial.Version != "1.1.0" || st.Trial.Generation != 4 {
		t.Fatalf("exec 前应写 trial：%+v", st)
	}
	st.Trial.Boots = 1 // 启动器计了一次
	if err := e.l.save(st); err != nil {
		t.Fatal(err)
	}
	nu := e.upgraderAt("1.1.0")
	if !nu.Trial() || nu.handle(t.Context(), d) {
		t.Fatal("新版本确认前是试用状态，不处理指令")
	}
	if err := nu.Confirm(); err != nil {
		t.Fatal(err)
	}
	st, _ = e.l.load()
	if st.Current != "1.1.0" || st.Previous != "1.0.0" || st.Trial != nil || nu.Trial() {
		t.Fatalf("确认后：%+v", st)
	}
	if _, err := os.Stat(e.l.versionDir("0.5.0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("确认后应删除其他版本目录：%v", err)
	}
	if !e.l.usable("1.0.0") || !e.l.usable("1.1.0") {
		t.Error("current 和 previous 应保留")
	}
	if s := nu.StatusMsg(); s.State != UpgradeConfirmed || s.Current != "1.1.0" || s.Previous != "1.0.0" || !s.Launcher {
		t.Errorf("上报的状态：%+v", s)
	}
}

// TestUpgradeWithdrawn（独立审查发现）：下载好、排空中的版本的指令被撤回（没有指令，或者指令是当前版本，例如单台指定为
// 当前版本）时不再排空、改回 idle，Server 据此让出逐台推进的名额；再次下发时直接排空（已经下载好，不再下载）。
func TestUpgradeWithdrawn(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	d := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)
	if !waitState(t, u, d) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	for _, w := range []*runnerv1.UpgradeDirective{nil, {Version: "1.0.0", Generation: 2}} {
		if u.handle(t.Context(), w) {
			t.Fatal("指令撤回后不应排空")
		}
		if st := u.StatusMsg(); st.State != UpgradeIdle || st.Target != "" {
			t.Fatalf("指令撤回后应改回 idle：%+v", st)
		}
		if !u.handle(t.Context(), d) || u.StatusMsg().State != UpgradeDraining {
			t.Fatalf("再次下发时应直接排空：%+v", u.StatusMsg())
		}
	}
	e.setPkg("/kiln-runner-1.1.0.tgz", nil) // 不应再下载
	if !u.handle(t.Context(), d) {
		t.Fatal("已经下载好的版本应直接排空")
	}
}

// TestUpgradeReadyUsesLatestGeneration（U3）：已经下载好的版本被新一轮指令（generation 更大）复用时，
// trial 和回滚后的失败记录用新一轮的 generation，同一条指令不会再试一次回滚过的版本。
func TestUpgradeReadyUsesLatestGeneration(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	d1 := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)
	if !waitState(t, u, d1) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	u.handle(t.Context(), nil) // 撤回（例如暂停）
	d2 := e.serve("1.1.0", goodPackage(t, "1.1.0"), 2)
	if !u.handle(t.Context(), d2) {
		t.Fatalf("已经下载好的版本应直接排空：%+v", u.StatusMsg())
	}
	var written *BootState // exec 启动器时磁盘上的启动状态（exec 被替换为失败后 trial 会撤销）
	u.exec = func(string, []string, []string) error {
		st, err := e.l.load()
		if err != nil {
			t.Error(err)
		}
		written = st
		return errors.New("exec 被测试替换")
	}
	_ = u.SwitchTo()
	if written == nil || written.Trial == nil || written.Trial.Generation != 2 {
		t.Fatalf("切换时写的 trial 应是 generation 2：%+v", written)
	}
	st := *written
	// 启动器三次启动都没有确认，回滚
	for range maxTrialBoots + 1 {
		dec, err := chooseBoot(st, func(string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		st = *dec.State
	}
	if st.Trial != nil || !st.failedAt("1.1.0", 2) {
		t.Fatalf("回滚后应记下 generation 2 的失败：%+v", st)
	}
	if err := e.l.save(&st); err != nil {
		t.Fatal(err)
	}
	u2 := e.upgraderAt("1.0.0")
	if u2.handle(t.Context(), d2) {
		t.Fatalf("同一 generation 的指令不应再试回滚过的版本：%+v", u2.StatusMsg())
	}
}

// TestUpgradeStaleDownloadKeepsDrainTarget（U3）：已下载 1.1.0 后目标改为 1.2.0（后台慢慢下载），
// 下载完成前又改回 1.1.0（generation 3）并开始排空；旧的 1.2.0 下载完成时不能替换排空目标，切换写的是 1.1.0 / 3，
// 1.1.0 的目录也不会被清理。
func TestUpgradeStaleDownloadKeepsDrainTarget(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	if !waitState(t, u, e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	gate, entered := make(chan struct{}), make(chan struct{})
	slowPkg := goodPackage(t, "1.2.0")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-gate
		_, _ = w.Write(slowPkg)
	}))
	t.Cleanup(slow.Close)
	d12 := e.serveURL("1.2.0", slow.URL+"/p.tgz", slowPkg, 2)
	if u.handle(t.Context(), d12) {
		t.Fatal("还没下载好的版本不应排空")
	}
	<-entered
	if !u.handle(t.Context(), &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 3}) {
		t.Fatalf("改回已下载好的版本应直接排空：%+v", u.StatusMsg())
	}
	close(gate)
	deadline := time.Now().Add(20 * time.Second)
	for {
		u.mu.Lock()
		busy := u.busy
		u.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("下载没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := u.StatusMsg(); st.State != UpgradeDraining || st.Target != "1.1.0" {
		t.Fatalf("旧下载完成后排空目标应仍是 1.1.0：%+v", st)
	}
	var written *BootState
	u.exec = func(string, []string, []string) error {
		written, _ = e.l.load()
		return errors.New("exec 被测试替换")
	}
	_ = u.SwitchTo()
	if written == nil || written.Trial == nil || *written.Trial != (BootTrial{Version: "1.1.0", Generation: 3}) {
		t.Fatalf("切换写的 trial 应是 1.1.0 / generation 3：%+v", written)
	}
	if !e.l.usable("1.1.0") {
		t.Fatal("排空目标 1.1.0 的目录被删掉了")
	}
}

// TestServiceInstallDuringDownload（U2）：经过启动器运行的 1.0.0 正在后台下载 1.1.0 时执行
// service install 安装 1.2.0；下载完成后的清理按磁盘上的 state.json 保留刚安装的 current，旧进程之后也不在它上面切换。
func TestServiceInstallDuringDownload(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	gate, entered := make(chan struct{}), make(chan struct{})
	pkg := goodPackage(t, "1.1.0")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-gate
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(slow.Close)
	d := e.serveURL("1.1.0", slow.URL+"/p.tgz", pkg, 1)
	u.handle(t.Context(), d)
	<-entered
	exe := filepath.Join(t.TempDir(), "kiln-runner")
	if err := os.WriteFile(exe, fakeProgram("1.2.0", 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installLayout(e.dir, "kiln-runner", exe, "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if !waitState(t, u, d) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	st, err := e.l.load()
	if err != nil {
		t.Fatal(err)
	}
	if dec, err := chooseBoot(*st, e.l.usable); err != nil || dec.Version != "1.2.0" {
		t.Fatalf("服务应启动刚安装的 1.2.0：%+v %v（state %+v）", dec, err, st)
	}
	if err := u.SwitchTo(); err == nil || len(e.execs) != 0 {
		t.Fatalf("启动状态被 service install 改过后不应切换：%v %v", err, e.execs)
	}
	if st2, _ := e.l.load(); st2 == nil || st2.Trial != nil || st2.Current != "1.2.0" {
		t.Fatalf("不应在刚安装的版本上写 trial：%+v", st2)
	}
}

// TestServiceInstallThenFailureRecorded：service install 写好了布局但没能重启服务（服务文件写不了），
// 旧进程接着下载、integrity 不符记入失败（会把 state.json 重新读进来），retry 后下载成功：仍按本进程启动时的
// current / previous 判断布局被改过，不在刚安装的版本上切换到旧目标。
func TestServiceInstallThenFailureRecorded(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	exe := filepath.Join(t.TempDir(), "kiln-runner")
	if err := os.WriteFile(exe, fakeProgram("1.2.0", 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installLayout(e.dir, "kiln-runner", exe, "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	bad := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)
	e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = integrityOf([]byte("other")) })
	waitState(t, u, bad)
	if st := u.StatusMsg(); st.State != UpgradeFailed || len(st.Failed) != 1 {
		t.Fatalf("integrity 不符应记入失败：%+v", st)
	}
	if !waitState(t, u, e.serve("1.1.0", goodPackage(t, "1.1.0"), 2)) {
		t.Fatalf("retry 后下载核对应排空：%+v", u.StatusMsg())
	}
	if err := u.SwitchTo(); err == nil || len(e.execs) != 0 {
		t.Fatalf("布局被 service install 改过后不应切换：%v %v", err, e.execs)
	}
	if st, err := e.l.load(); err != nil || st.Current != "1.2.0" || st.Trial != nil {
		t.Fatalf("应保留 service install 写下的状态：%+v %v", st, err)
	}
}

// TestConfirmAfterServiceInstall：试用中的新版本确认之前执行了 service install（清掉 trial、换了
// current）：不确认、不覆盖安装写下的状态，本进程照常运行（不再按试用处理）。
func TestConfirmAfterServiceInstall(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	installVersion(t, e.l, "1.1.0", 0)
	if err := e.l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: 1, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	u := e.upgraderAt("1.1.0")
	if !u.Trial() {
		t.Fatal("应是试用版本")
	}
	exe := filepath.Join(t.TempDir(), "kiln-runner")
	if err := os.WriteFile(exe, fakeProgram("1.2.0", 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installLayout(e.dir, "kiln-runner", exe, "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := u.Confirm(); err != nil || u.Trial() {
		t.Fatalf("不应确认，也不应再按试用处理：%v trial=%v", err, u.Trial())
	}
	st, err := e.l.load()
	if err != nil || st.Current != "1.2.0" || st.Previous != "1.0.0" || st.Trial != nil {
		t.Fatalf("应保留 service install 写下的状态：%+v %v", st, err)
	}
	if !e.l.usable("1.1.0") {
		t.Fatal("不确认时不应清理版本")
	}
}

// TestForegroundIntegrityFailure：不经过启动器运行时 integrity 不符记不进失败列表，仍然一直上报这一轮的
// failed（Server 据此结束请求），同一 generation 不再尝试；管理员重新请求时再试。
func TestForegroundIntegrityFailure(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.foreground("1.0.0")
	d := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1000)
	e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = integrityOf([]byte("other")) })
	waitState(t, u, d)
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Generation != 1000 || len(st.Failed) != 0 {
		t.Fatalf("integrity 不符应上报这一轮的 failed：%+v", st)
	}
	if u.handle(t.Context(), d) || u.StatusMsg().State != UpgradeFailed {
		t.Fatalf("同一 generation 不应再尝试：%+v", u.StatusMsg())
	}
	waitState(t, u, &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 1001})
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Generation != 1001 {
		t.Fatalf("重新请求时应再试一次：%+v", st)
	}
}

// TestIntegrityFailureNotRecorded：经过启动器运行时 integrity 不符、但失败记不进 state.json
// （布局锁打不开）：同样一直上报这一轮的 failed，同一 generation 不再尝试。
func TestIntegrityFailureNotRecorded(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	lockPath := filepath.Join(e.l.dir, bootLockName)
	if err := os.Mkdir(lockPath, 0o755); err != nil { // 锁文件打不开
		t.Fatal(err)
	}
	d := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1000)
	e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = integrityOf([]byte("other")) })
	waitState(t, u, d)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Generation != 1000 || len(st.Failed) != 0 {
		t.Fatalf("应上报这一轮的 failed（没有记下失败）：%+v", st)
	}
	if u.handle(t.Context(), d) || u.StatusMsg().State != UpgradeFailed {
		t.Fatalf("同一 generation 不应再尝试：%+v", u.StatusMsg())
	}
}

// TestFallbackThenUpgradeKeepsPrevious（U2）：current 启动不了、启动器改为启动 previous（0.9.0）后
// 再升级到 1.1.0 并确认：previous 是实际运行过的 0.9.0，它的目录不会被清理。
func TestFallbackThenUpgradeKeepsPrevious(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	installVersion(t, e.l, "0.9.0", 0)
	if err := os.Remove(e.l.program("1.0.0")); err != nil {
		t.Fatal(err)
	}
	dec, err := chooseBoot(BootState{Current: "1.0.0", Previous: "0.9.0"}, e.l.usable)
	if err != nil || dec.Version != "0.9.0" || !dec.Changed || dec.State.Current != "0.9.0" || dec.State.Previous != "" {
		t.Fatalf("应启动 previous 并把它记为 current：%+v %+v %v", dec, dec.State, err)
	}
	if err := e.l.save(dec.State); err != nil {
		t.Fatal(err)
	}
	u := e.upgraderAt("0.9.0")
	if !waitState(t, u, e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	var written *BootState // exec 启动器时的启动状态（exec 被替换为失败后 trial 会撤销，这里写回，模拟切换成功）
	u.exec = func(string, []string, []string) error {
		written, _ = e.l.load()
		return errors.New("exec 被测试替换")
	}
	_ = u.SwitchTo()
	if written == nil || written.Trial == nil {
		t.Fatalf("切换时应写 trial：%+v", written)
	}
	if err := e.l.save(written); err != nil {
		t.Fatal(err)
	}
	u2 := e.upgraderAt("1.1.0")
	if err := u2.Confirm(); err != nil {
		t.Fatal(err)
	}
	st, err := e.l.load()
	if err != nil || st.Current != "1.1.0" || st.Previous != "0.9.0" {
		t.Fatalf("确认后 previous 应是实际运行过的 0.9.0：%+v %v", st, err)
	}
	if !e.l.usable("0.9.0") {
		t.Fatal("0.9.0 被清理了")
	}
}

// TestUpgradeFailureKeepsFailed（PLAN #27.6、#27.7）：一轮中任何一步失败（这里是下载 404）
// 都一直上报这一轮的 failed，不论 Server 多久之后才收到（例如一直在执行长任务、没有 Acquire），Server 据此结束请求；
// 同一 generation 不再尝试，即使包恢复了；管理员重新请求（更大的 generation）时立即执行。
func TestUpgradeFailureKeepsFailed(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	data := goodPackage(t, "1.1.0")
	d := e.serve("1.1.0", data, 1000)
	e.setPkg("/kiln-runner-1.1.0.tgz", nil) // 404
	waitState(t, u, d)
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Target != "1.1.0" || st.Generation != 1000 || len(st.Failed) != 0 || st.Error == "" {
		t.Fatalf("下载失败应上报这一轮的 failed（带原因）：%+v", st)
	}
	e.setPkg("/kiln-runner-1.1.0.tgz", data)
	if u.handle(t.Context(), d) {
		t.Fatal("同一 generation 不应再尝试")
	}
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Generation != 1000 {
		t.Fatalf("一直上报这一轮的 failed：%+v", st)
	}
	// 请求结束后 Server 不再下发：失败结果仍然保留（页面显示原因）
	u.handle(t.Context(), nil)
	if st := u.StatusMsg(); st.State != UpgradeFailed || st.Generation != 1000 {
		t.Fatalf("撤回指令后仍上报这一轮的 failed：%+v", st)
	}
	if !waitState(t, u, &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 1001}) || u.StatusMsg().State != UpgradeDraining {
		t.Fatalf("管理员重新请求应立即重新下载：%+v", u.StatusMsg())
	}
}

// TestUpgradeRecordedFailureKeepsFailed：上一轮（generation 1000）下载失败，管理员重新请求（1001）后 integrity 不符
// 记入失败列表：一直上报 1001 的 failed，同一 generation 不再尝试。
func TestUpgradeRecordedFailureKeepsFailed(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	data := goodPackage(t, "1.1.0")
	d := e.serve("1.1.0", data, 1000)
	e.setPkg("/kiln-runner-1.1.0.tgz", nil)
	waitState(t, u, d)
	e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = integrityOf([]byte("other")) })
	e.setPkg("/kiln-runner-1.1.0.tgz", data)
	retry := &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 1001}
	waitState(t, u, retry)
	st := u.StatusMsg()
	if st.State != UpgradeFailed || st.Generation != 1001 || !slices.ContainsFunc(st.Failed, func(f *runnerv1.FailedVersion) bool {
		return f.Version == "1.1.0" && f.Generation == 1001
	}) {
		t.Fatalf("记入失败列表的失败应一直上报 failed：%+v", st)
	}
	if u.handle(t.Context(), retry) || u.StatusMsg().State != UpgradeFailed {
		t.Fatalf("同一 generation 不应再尝试：%+v", u.StatusMsg())
	}
}

// TestUpgradeRejectsBadPackages（U1、威胁模型）：integrity、大小不符，路径穿越、符号链接、硬链接、目录、空文件、
// 版本不符的包都不会替换现有程序；路径穿越的条目不会写到任何地方。
func TestUpgradeRejectsBadPackages(t *testing.T) {
	t.Parallel()
	good := fakeProgram("1.1.0", 0)
	cases := []struct {
		name   string
		data   func(t *testing.T) []byte
		mutate func(e *upEnv)
		final  bool // 是否记入失败（同一 generation 不再重试）
	}{
		{
			name: "integrity 不符", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = integrityOf([]byte("other")) }) }, final: true,
		},
		{
			name: "多出字节", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.mu.Lock(); e.extra["/kiln-runner-1.1.0.tgz"] = []byte("x"); e.mu.Unlock() }, final: true,
		},
		{name: "路径穿越", data: func(t *testing.T) []byte {
			t.Helper()
			return makePackage(t, tarEntry{name: "package/../../evil", typ: tar.TypeReg, data: []byte("x")},
				tarEntry{name: "../evil2", typ: tar.TypeReg, data: []byte("x")})
		}, final: true},
		{name: "符号链接", data: func(t *testing.T) []byte {
			t.Helper()
			return makePackage(t, tarEntry{name: "package/kiln-runner", typ: tar.TypeSymlink, linkname: "/bin/sh"})
		}, final: true},
		{name: "硬链接", data: func(t *testing.T) []byte {
			t.Helper()
			return makePackage(t, tarEntry{name: "package/x", typ: tar.TypeReg, data: good},
				tarEntry{name: "package/kiln-runner", typ: tar.TypeLink, linkname: "package/x"})
		}, final: true},
		{name: "同名目录", data: func(t *testing.T) []byte {
			t.Helper()
			return makePackage(t, tarEntry{name: "package/kiln-runner", typ: tar.TypeDir})
		}, final: true},
		{name: "有内容的非普通文件（连续文件类型）", data: func(t *testing.T) []byte {
			t.Helper()
			return makePackage(t, tarEntry{name: "package/kiln-runner", typ: tar.TypeCont, data: good})
		}, final: true},
		{name: "版本不符", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "6.6.6") }, final: true},
		{
			name: "下载失败（404）", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.dist("1.1.0", func(d *npm.Dist) { d.Tarball += ".missing" }) },
		},
		{
			name: "registry 中没有这个版本", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.mu.Lock(); delete(e.dists, "1.1.0"); e.mu.Unlock() },
		},
		{
			name: "integrity 不是 sha512", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.dist("1.1.0", func(d *npm.Dist) { d.Integrity = "sha1-abc" }) },
		},
		{
			name: "registry 出错", data: func(t *testing.T) []byte { t.Helper(); return goodPackage(t, "1.1.0") },
			mutate: func(e *upEnv) { e.mu.Lock(); e.status = http.StatusInternalServerError; e.mu.Unlock() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newUpEnv(t)
			u := e.upgraderAt("1.0.0")
			d := e.serve("1.1.0", tc.data(t), 3)
			if tc.mutate != nil {
				tc.mutate(e)
			}
			if waitState(t, u, d) {
				t.Fatal("坏的包不应进入排空")
			}
			if s := u.StatusMsg(); s.State != UpgradeFailed || s.Error == "" {
				t.Fatalf("应上报失败：%+v", s)
			}
			if e.l.usable("1.1.0") {
				t.Fatal("坏的包不应装成版本")
			}
			for _, p := range []string{filepath.Join(e.dir, "evil"), filepath.Join(filepath.Dir(e.dir), "evil2"), filepath.Join(e.dir, binDirName, "evil")} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("路径穿越写出了 %s", p)
				}
			}
			st, _ := e.l.load()
			if got := st.failedAt("1.1.0", 3); got != tc.final {
				t.Fatalf("记入失败 = %v，想要 %v（%+v）", got, tc.final, st.Failed)
			}
			if st.Current != "1.0.0" || st.Trial != nil {
				t.Fatalf("现有程序不应受影响：%+v", st)
			}
			if tc.final {
				// 同一 generation 不再下载；generation 变大后重新下载
				if u.handle(t.Context(), d); u.StatusMsg().State != UpgradeFailed {
					t.Fatalf("同一 generation 不应重试：%+v", u.StatusMsg())
				}
				e.mu.Lock()
				delete(e.extra, "/kiln-runner-1.1.0.tgz")
				e.mu.Unlock()
				d = e.serve("1.1.0", goodPackage(t, "1.1.0"), 4)
				if !waitState(t, u, d) {
					t.Fatalf("generation 变大后应重新下载：%+v", u.StatusMsg())
				}
			}
		})
	}
}

// TestUpgradeManual：没有经过启动器（前台 run）或本地关闭了自动升级时不切换，上报需要手动升级。
func TestUpgradeManual(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	d := e.serve("1.1.0", goodPackage(t, "1.1.0"), 1)
	fg := newUpgraderVersion(e.home(), true, slog.New(slog.DiscardHandler), "v1.0.0", false, nil)
	e.useRegistry(fg)
	if waitState(t, fg, d) {
		t.Fatal("前台运行不应排空")
	}
	if !fg.handle(t.Context(), d) && fg.StatusMsg().State != UpgradeManual {
		t.Fatalf("前台运行应上报需要手动升级：%+v", fg.StatusMsg())
	}
	off := false
	dis := e.upgraderAt("1.0.0", func(c *Common) { c.Upgrade = &off })
	if dis.handle(t.Context(), d) || dis.StatusMsg().State != UpgradeManual {
		t.Fatalf("upgrade = false 时应上报需要手动升级：%+v", dis.StatusMsg())
	}
}

// TestUpgradeCrashLeftovers（崩溃点）：下载、解包中崩溃留下的 bin/tmp（包括其中的解包目录）在启动、持有数据目录锁之后清掉；
// rename 完成、写 state.json 之前崩溃时目录已存在且 version 正确，直接复用。
func TestUpgradeCrashLeftovers(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	for _, p := range []string{filepath.Join(e.l.tmpDir(), "partial"), filepath.Join(e.l.stagingDir("1.1.0"), "kiln-runner")} {
		if err := writeFileAtomic(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installVersion(t, e.l, "1.1.0", 0) // 上一次已经 rename 完成
	u := e.upgraderAt("1.0.0")
	// 构造时不清理（还没有持有数据目录锁，可能是因为锁冲突就要退出的第二个实例，独立审查发现）
	if _, err := os.Stat(e.l.stagingDir("1.1.0")); err != nil {
		t.Fatalf("没有持有锁之前不应清理：%v", err)
	}
	u.Cleanup()
	for _, p := range []string{e.l.tmpDir(), e.l.stagingDir("1.1.0")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 应已清掉", p)
		}
	}
	d := &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 1} // registry 中没有这个版本：不能下载
	if !waitState(t, u, d) {
		t.Fatalf("已经装好的版本应直接复用（不下载）：%+v", u.StatusMsg())
	}
}

// TestUpgradeTmpLikeVersion（独立审查发现）：版本名以 .tmp 结尾（合法的预发布版本）时，启动清理不会把它当成解包残留
// 删掉：解包目录在 bin/tmp 下，与 versions/ 中的版本目录分开。
func TestUpgradeTmpLikeVersion(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	const v = "1.1.0-rc.tmp"
	if !upgrade.Valid(v) {
		t.Fatalf("%s 应是合法版本", v)
	}
	u := e.upgraderAt("1.0.0")
	d := e.serve(v, goodPackage(t, v), 1)
	if !waitState(t, u, d) {
		t.Fatalf("下载核对后应排空：%+v", u.StatusMsg())
	}
	var st *BootState
	u.exec = func(string, []string, []string) error {
		st, _ = e.l.load()
		return errors.New("exec 被测试替换")
	}
	_ = u.SwitchTo()
	if st == nil || st.Trial == nil || st.Trial.Version != v {
		t.Fatalf("exec 前应写 trial：%+v", st)
	}
	if err := e.l.save(st); err != nil {
		t.Fatal(err)
	}
	nu := e.upgraderAt(v)
	nu.Cleanup() // 新版本启动（Run 持有数据目录锁之后）：清理残留，不能删掉名字像临时目录的版本
	if !e.l.usable(v) {
		t.Fatalf("启动清理删掉了版本 %s", v)
	}
	if err := nu.Confirm(); err != nil {
		t.Fatal(err)
	}
	st, _ = e.l.load()
	d2, err := chooseBoot(*st, e.l.usable)
	if err != nil || d2.Version != v {
		t.Fatalf("确认后下次启动 %q（%v），想要 %s", d2.Version, err, v)
	}
}

// TestUpgradeVersionLenMatchesLauncher（独立审查发现）：升级接受的版本号启动器都认（同一个长度上限），
// 超过上限的版本号在 Server 和下载之前就被拒绝，不会写进 trial。
func TestUpgradeVersionLenMatchesLauncher(t *testing.T) {
	t.Parallel()
	l := newLayout(t.TempDir(), "kiln-runner")
	ok := "1.1.0-alpha." + strings.Repeat("a", upgrade.MaxVersionLen-len("1.1.0-alpha."))
	tooLong := ok + "a"
	if !upgrade.Valid(ok) || !l.isLocalName(ok) {
		t.Fatalf("%d 个字符的版本号应合法且启动器认：%v %v", len(ok), upgrade.Valid(ok), l.isLocalName(ok))
	}
	if upgrade.Valid(tooLong) {
		t.Fatalf("%d 个字符的版本号应不合法", len(tooLong))
	}
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	d := e.serve(tooLong, goodPackage(t, tooLong), 1)
	if u.handle(t.Context(), d) || u.StatusMsg().State != UpgradeFailed {
		t.Fatalf("超长的版本号应拒绝：%+v", u.StatusMsg())
	}
}

// TestLaunchInvalidTrial（独立审查发现）：state.json 中的 trial 不是启动器认的目录名时（例如旧版本写入的超长版本号）
// 回滚到 current，而不是拒绝读取整个状态、什么也不启动。
func TestLaunchInvalidTrial(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	l := newLayout(dir, "kiln-runner")
	installVersion(t, l, "1.0.0", 0)
	bad := "1.1.0-alpha." + strings.Repeat("a", 60)
	if err := l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: bad, Generation: 3}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		line, code := launchOnce(t, dir, mark)
		if line != "1.0.0 run launcher=1" || code != 0 {
			t.Fatalf("应回滚并启动 1.0.0：%q（退出码 %d）", line, code)
		}
	}
	st, err := l.load()
	if err != nil || st.Trial != nil || st.Current != "1.0.0" || !st.failedAt(bad, 3) {
		t.Fatalf("回滚后的状态：%+v（%v）", st, err)
	}
	// 路径穿越的 trial 同样不会被当成程序运行
	if l.usable("../../x") {
		t.Fatal("不合法的名字不应可用")
	}
}

// TestInstallLayout：service install 建立布局（启动器、当前版本、state.json），再次安装时原来的 current 记为 previous。
func TestInstallLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "kiln-runner-src")
	if err := os.WriteFile(exe, fakeProgram("1.0.0", 0), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher, err := installLayout(dir, "kiln-runner", exe, "v1.0.0")
	l := newLayout(dir, "kiln-runner")
	if err != nil || launcher != l.launcher() || !l.installed() || !l.usable("1.0.0") {
		t.Fatalf("installLayout：%q %v", launcher, err)
	}
	if _, err := installLayout(dir, "kiln-runner", exe, "dev-abc123"); err != nil {
		t.Fatal(err)
	}
	st, err := l.load()
	if err != nil || st.Current != "dev-abc123" || st.Previous != "1.0.0" {
		t.Fatalf("再次安装：%+v（%v）", st, err)
	}
	if _, err := installLayout(dir, "kiln-runner", exe, "weird/../version"); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.load(); st.Current != "local" {
		t.Fatalf("不合法的版本名应记为 local：%+v", st)
	}
}

// TestTrialRetry（U3）：试用的新版本只在网络错误、并且没超过试用期限时重试；被拒绝或一直连不上超过期限就退出计次
// （新版本自己的网络处理坏了时不会永远不回滚，独立审查发现）。
func TestTrialRetry(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	if err := e.l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: 1}}); err != nil {
		t.Fatal(err)
	}
	installVersion(t, e.l, "1.1.0", 0)
	u := e.upgraderAt("1.1.0")
	if !u.Trial() {
		t.Fatal("应是试用状态")
	}
	network := connect.NewError(connect.CodeUnavailable, errors.New("连不上"))
	denied := connect.NewError(connect.CodePermissionDenied, errors.New("拒绝"))
	now := u.started
	if !u.TrialRetry(network, now.Add(time.Minute)) {
		t.Error("期限内的网络错误应重试")
	}
	if u.TrialRetry(network, now.Add(trialTimeout)) {
		t.Error("超过试用期限应退出")
	}
	if u.TrialRetry(denied, now) {
		t.Error("被拒绝应退出")
	}
	if err := u.Confirm(); err != nil {
		t.Fatal(err)
	}
	if !u.TrialRetry(denied, now.Add(time.Hour)) {
		t.Error("确认后不再按试用处理")
	}
}

// TestUpgradePrunesDownloads（独立审查发现）：目标换了几次、一直不切换（前台运行只下载、上报需要手动升级）时，之前下载的
// 版本在下一次下载完成时删掉，只保留本进程的版本和最新下载好的版本，磁盘占用有上限。
func TestUpgradePrunesDownloads(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.foreground("1.0.0")
	for i, v := range []string{"1.1.0", "1.2.0", "1.3.0", "1.4.0"} {
		waitState(t, u, e.serve(v, goodPackage(t, v), int64(i+1)))
	}
	entries, err := os.ReadDir(e.l.versionsRoot())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, en := range entries {
		got = append(got, en.Name())
	}
	if want := []string{"1.0.0", "1.4.0"}; !slices.Equal(got, want) {
		t.Fatalf("保留的版本 %v，想要 %v", got, want)
	}
}

// TestUpgradeForegroundKeepsBootVersions（U2）：不经过启动器运行（前台 run）时下载完成后的清理也保留
// 磁盘上 state.json 引用的 current、previous、trial，服务的启动器照常能启动；state.json 读不出时一个版本也不删。
func TestUpgradeForegroundKeepsBootVersions(t *testing.T) {
	t.Parallel()
	t.Run("保留启动状态中的版本", func(t *testing.T) {
		t.Parallel()
		e := newUpEnv(t)
		installVersion(t, e.l, "0.9.0", 0)
		installVersion(t, e.l, "0.8.0", 0)
		installVersion(t, e.l, "1.0.5", 0)
		boot := BootState{Current: "1.0.0", Previous: "0.9.0", Trial: &BootTrial{Version: "1.0.5", Generation: 1}}
		if err := e.l.save(&boot); err != nil {
			t.Fatal(err)
		}
		u := e.foreground("1.1.0")
		waitState(t, u, e.serve("1.2.0", goodPackage(t, "1.2.0"), 1))
		for _, v := range []string{"1.0.0", "0.9.0", "1.0.5", "1.2.0"} {
			if !e.l.usable(v) {
				t.Errorf("版本 %s 被删掉了", v)
			}
		}
		if e.l.usable("0.8.0") {
			t.Error("没有引用的旧版本应删掉")
		}
		boot.Trial = nil
		if dec, err := chooseBoot(boot, e.l.usable); err != nil || dec.Version != "1.0.0" {
			t.Fatalf("服务应照常启动 current：%+v %v", dec, err)
		}
	})
	t.Run("读不出启动状态时不删", func(t *testing.T) {
		t.Parallel()
		e := newUpEnv(t)
		installVersion(t, e.l, "0.9.0", 0)
		if err := os.WriteFile(e.l.statePath(), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		u := e.foreground("1.1.0")
		waitState(t, u, e.serve("1.2.0", goodPackage(t, "1.2.0"), 1))
		for _, v := range []string{"1.0.0", "0.9.0", "1.2.0"} {
			if !e.l.usable(v) {
				t.Errorf("版本 %s 被删掉了", v)
			}
		}
	})
}

// waitCheck 处理一次指令，等后台的查询结束，返回上报的状态。
func waitCheck(t *testing.T, u *Upgrader, d *runnerv1.RunnerDirectives) *runnerv1.UpgradeStatus {
	t.Helper()
	u.Directives(t.Context(), d)
	deadline := time.Now().Add(20 * time.Second)
	for {
		u.mu.Lock()
		checking := u.checking
		u.mu.Unlock()
		if !checking {
			return u.StatusMsg()
		}
		if time.Now().After(deadline) {
			t.Fatal("查询没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUpgradeCheckLatest（PLAN #27.12）：Runner 查询自己的包的 dist-tags.latest 并上报；只认正式版本；包不存在时说明
// 暂无升级包；registry 出错时保留上一次查到的 latest 并上报错误；检查序号、registry 变了时立即重新查询，否则按间隔。
func TestUpgradeCheckLatest(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	u := e.upgraderAt("1.0.0")
	reg := e.srv.URL + "/"
	queries := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.queries
	}
	set := func(f func()) {
		e.mu.Lock()
		defer e.mu.Unlock()
		f()
	}
	set(func() { e.latest = "1.2.0" })
	st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 2})
	if st.Latest != "1.2.0" || st.CheckError != "" || st.CheckedAt == 0 || st.CheckSeq != 2 || st.Package != u.pkg ||
		!strings.HasPrefix(st.Package, "@yuebanlaosiji/kilnflow-runner-") {
		t.Fatalf("应上报查到的 latest：%+v", st)
	}
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 2}); queries() != 1 || st.Latest != "1.2.0" {
		t.Fatalf("间隔之内不应重新查询：%d 次", queries())
	}
	// 管理员点了检查更新：预发布不算
	set(func() { e.latest = "1.3.0-rc.1" })
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 3}); st.Latest != "" ||
		!strings.Contains(st.CheckError, "不是正式版本") || st.CheckSeq != 3 {
		t.Fatalf("latest 是预发布时不应作为最新版本：%+v", st)
	}
	set(func() { e.latest, e.gone = "1.2.0", true })
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 4}); st.Latest != "" ||
		!strings.Contains(st.CheckError, "暂无升级包") {
		t.Fatalf("包不存在：%+v", st)
	}
	set(func() { e.gone = false })
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 5}); st.Latest != "1.2.0" || st.CheckError != "" {
		t.Fatalf("恢复：%+v", st)
	}
	set(func() { e.status = http.StatusBadGateway })
	checkedAt := u.StatusMsg().CheckedAt
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 6}); st.Latest != "1.2.0" ||
		st.CheckError == "" || st.CheckedAt != checkedAt {
		t.Fatalf("registry 出错时应保留上一次的 latest 和成功时间并上报错误：%+v", st)
	}
	// 失败后按较短的间隔重试；registry 变了立即查询（空表示官方 registry，这里换成同一个替身的另一种写法）
	n := queries()
	u.mu.Lock()
	u.lastCheck = time.Now().Add(-checkRetryInterval)
	u.mu.Unlock()
	waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: reg, UpgradeCheck: 6})
	if queries() != n+1 {
		t.Fatalf("失败后到了重试间隔应重新查询：%d 次", queries()-n)
	}
	set(func() { e.status = 0 })
	if st := waitCheck(t, u, &runnerv1.RunnerDirectives{UpgradeRegistry: e.srv.URL, UpgradeCheck: 6}); queries() != n+2 || st.CheckError != "" {
		t.Fatalf("registry 变了应立即查询：%d 次 %+v", queries()-n, st)
	}
	if u.registry != e.srv.URL {
		t.Fatalf("下载应使用下发的 registry：%s", u.registry)
	}
	def := e.upgraderAt("1.0.0")
	def.checking = true // 不真的查询官方 registry
	if def.Directives(t.Context(), &runnerv1.RunnerDirectives{}) || def.registry != upgrade.DefaultRegistry {
		t.Fatalf("没有设置 registry 时应使用官方 registry：%s", def.registry)
	}
}

// TestUpgradeStatusReportsDisabled：upgrade = false 与不经过启动器分开上报（Server 据此给出具体的处理步骤）。
func TestUpgradeStatusReportsDisabled(t *testing.T) {
	t.Parallel()
	e := newUpEnv(t)
	off := false
	if st := e.upgraderAt("1.0.0", func(c *Common) { c.Upgrade = &off }).StatusMsg(); !st.Launcher || !st.Disabled {
		t.Fatalf("经过启动器、本地关闭：%+v", st)
	}
	if st := e.foreground("1.0.0").StatusMsg(); st.Launcher || st.Disabled {
		t.Fatalf("前台运行：%+v", st)
	}
}

// testProgram 是测试用的程序（与 kiln-runner 同名：假程序和 npm 包都按这个名字生成）。
var testProgram = &Program{
	Name: "kiln-runner", Kind: "ci", DirEnv: "KILN_RUNNER_DIR", DefaultDir: "kiln", LogName: "runner",
	LaunchdLabel: "dev.kiln.runner", SystemdUnit: "kiln-runner", Description: "Kiln runner",
	WrongKind: func(kind string) string { return "令牌类型 " + kind },
}

// TestTrialDeadlineCancelsHangingRequest（独立审查发现）：试用的新版本在试用期限到期时取消进行中的认证（Server 或代理
// 返回了响应头、正文一直不结束），退出由启动器计次，不会一直挂着不回滚。
func TestTrialDeadlineCancelsHangingRequest(t *testing.T) {
	t.Parallel()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(func() {
		hang.CloseClientConnections()
		hang.Close()
	})
	e := newUpEnv(t)
	installVersion(t, e.l, "1.1.0", 0)
	if err := e.l.save(&BootState{Current: "1.0.0", Trial: &BootTrial{Version: "1.1.0", Boots: 1}}); err != nil {
		t.Fatal(err)
	}
	c := NewClient(hang.URL, 7, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))
	u := e.upgraderAt("1.1.0")
	u.started = time.Now().Add(-trialTimeout + 200*time.Millisecond) // 200ms 后到期
	errc := make(chan error, 1)
	go func() { errc <- AuthenticateTrial(t.Context(), c, u, func(string, ...any) {}) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("到期时应返回错误")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("试用期限到期后认证仍然挂着")
	}
}
