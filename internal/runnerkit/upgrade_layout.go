package runnerkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"

	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// 自升级的本地布局和启动器（PLAN #27.7）。数据目录下：
//
//	bin/launcher/<程序名>       启动器（service install 时复制当前程序，自动升级不替换它）
//	bin/versions/<版本>/<程序名> 各版本的程序（只保留 current、previous 和正在试的版本）
//	bin/state.json              current、previous、trial{version, boots, generation}、failed
//	bin/tmp/                    下载和解包的临时文件（启动时清掉）；解包在 bin/tmp/staging/<版本>/，核对后 rename 到 versions/
//	                            （与 versions/ 中的版本目录分开：任何合法版本名都不会被当成残留删掉）
//
// state.json 总是先写临时文件、fsync、rename、再 fsync 目录（writeFileAtomic）：任何时刻读到的要么是旧内容要么是新内容。

const (
	binDirName      = "bin"
	launcherDirName = "launcher"
	versionsDirName = "versions"
	bootStateName   = "state.json"
	binTmpDirName   = "tmp"
	// maxTrialBoots：新版本连续这么多次启动都没有确认就回滚（U3）。
	maxTrialBoots = 3
	// launcherEnv 是启动器传给程序的环境变量：设置了才会自动切换版本（否则只下载、核对并上报需要手动升级）。
	launcherEnv = "KILN_LAUNCHER"
	// maxFailedVersions 是 state.json 中保留的失败版本数。
	maxFailedVersions = 20
)

// localVersionRe 是 current 允许的目录名：合法版本，或者 service install 时开发构建的版本名（如 dev-abc123）。
// 升级用的合法版本（upgrade.Valid，不超过 upgrade.MaxVersionLen）都满足它。
var localVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

// BootTrial 是正在试的新版本。
type BootTrial struct {
	Version    string `json:"version"`
	Boots      int    `json:"boots"`
	Generation int64  `json:"generation"`
}

// FailedVersion 是失败过的版本（回滚、integrity 不符）和当时的 generation。
type FailedVersion struct {
	Version    string `json:"version"`
	Generation int64  `json:"generation"`
	Reason     string `json:"reason,omitempty"`
}

// BootState 是 bin/state.json。
type BootState struct {
	Current  string          `json:"current"`
	Previous string          `json:"previous,omitempty"`
	Trial    *BootTrial      `json:"trial,omitempty"`
	Failed   []FailedVersion `json:"failed,omitempty"`
	// RolledBack 是最近一次回滚（启动器写入，Runner 上报 rolled_back，开始新的升级时清掉）。
	RolledBack *FailedVersion `json:"rolled_back,omitempty"`
}

// failedAt 判断 version 在 generation 下是否失败过（generation 变大之前不再安装，U3）。
func (s *BootState) failedAt(version string, generation int64) bool {
	return slices.ContainsFunc(s.Failed, func(f FailedVersion) bool { return f.Version == version && f.Generation >= generation })
}

// addFailed 记下失败的版本（同一个版本只保留最新的一条）。
func (s *BootState) addFailed(f FailedVersion) {
	s.Failed = slices.DeleteFunc(s.Failed, func(x FailedVersion) bool { return x.Version == f.Version })
	s.Failed = append(s.Failed, f)
	if n := len(s.Failed); n > maxFailedVersions {
		s.Failed = s.Failed[n-maxFailedVersions:]
	}
}

// layout 是一个程序在数据目录下的升级布局。
type layout struct {
	dir  string // 数据目录下的 bin
	prog string
}

func newLayout(dataDir, prog string) layout {
	return layout{dir: filepath.Join(dataDir, binDirName), prog: prog}
}

func (l layout) launcher() string           { return filepath.Join(l.dir, launcherDirName, l.prog) }
func (l layout) versionDir(v string) string { return filepath.Join(l.dir, versionsDirName, v) }
func (l layout) program(v string) string    { return filepath.Join(l.versionDir(v), l.prog) }
func (l layout) statePath() string          { return filepath.Join(l.dir, bootStateName) }
func (l layout) tmpDir() string             { return filepath.Join(l.dir, binTmpDirName) }
func (l layout) stagingDir(v string) string { return filepath.Join(l.tmpDir(), "staging", v) }
func (l layout) usable(v string) bool       { return l.isLocalName(v) && executable(l.program(v)) }
func (l layout) installed() bool            { return executable(l.launcher()) }
func (l layout) save(st *BootState) error   { return writeJSONAtomic(l.statePath(), st) }

// bootLockName 是 bin/ 下的布局锁（flock）。
const bootLockName = "state.lock"

// lock 锁住 bin/ 的布局：state.json 的读改写、版本目录的清理。service install、启动器和运行中的 Runner（后台下载完成
// 后清理、确认、切换）是不同的进程，都在这把锁下重新读磁盘上的 state.json 再改，不按各自缓存的状态覆盖或删除对方刚
// 写下的版本（避免 service install 与后台下载并发时旧进程删掉刚安装的 current）。只在改动期间持有，
// 与数据目录锁（runner.lock，run 期间一直持有）不同。
func (l layout) lock() (func(), error) {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return nil, err
	}
	return flockWait(filepath.Join(l.dir, bootLockName))
}
func (l layout) versionsRoot() string        { return filepath.Join(l.dir, versionsDirName) }
func (l layout) programIn(dir string) string { return filepath.Join(dir, l.prog) }
func (l layout) isLocalName(v string) bool   { return localVersionRe.MatchString(v) }

// load 读取 state.json；不存在时返回 fs.ErrNotExist。current、previous 不是合法的目录名时报错；trial 不合法时照常返回
// （usable 不认它，启动器按"不能执行"回滚到 current，旧版本照常启动）。
func (l layout) load() (*BootState, error) {
	data, err := os.ReadFile(l.statePath())
	if err != nil {
		return nil, err
	}
	var st BootState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s 损坏：%w", l.statePath(), err)
	}
	for _, v := range []string{st.Current, st.Previous} {
		if v != "" && !l.isLocalName(v) {
			return nil, fmt.Errorf("%s 中的版本 %q 无效", l.statePath(), v)
		}
	}
	return &st, nil
}

// executable 判断 p 是可执行的普通文件。
func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// writeJSONAtomic 原子写 JSON：临时文件 + fsync + rename + fsync 目录。
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // rename 成功后已经不存在
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// localName 返回 service install 时当前版本的目录名：合法版本用 npm 版本号，开发构建用版本名（不合法时为 local）。
func localName(version string) string {
	if v := upgrade.Normalize(version); v != "" {
		return v
	}
	if localVersionRe.MatchString(version) {
		return version
	}
	return "local"
}

// installLayout 建立自升级的目录（service install，PLAN #27.7"首次迁移"）：把 exe 复制为启动器和当前版本，
// state.json 的 current 指向它（原来的 current 还能启动时记为 previous，清掉正在试的版本）。返回启动器的路径。
func installLayout(dataDir, prog, exe, version string) (string, error) {
	l := newLayout(dataDir, prog)
	name := localName(version)
	unlock, err := l.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := copyExecutable(exe, l.program(name)); err != nil {
		return "", err
	}
	if err := copyExecutable(exe, l.launcher()); err != nil {
		return "", err
	}
	st, err := l.load()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st = &BootState{}
	case err != nil:
		return "", err
	}
	if st.Current != name {
		if l.usable(st.Current) {
			st.Previous = st.Current
		}
		st.Current = name
	}
	st.Trial, st.RolledBack = nil, nil
	if err := l.save(st); err != nil {
		return "", err
	}
	return l.launcher(), nil
}

// copyExecutable 原子地把 src 复制为可执行的 dst（src 和 dst 是同一个文件时什么也不做）。
func copyExecutable(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data, 0o755)
}

// BootDecision 是启动器的决定：启动 Version；State 是启动前要写回的 state.json（Changed 为真时）。
type BootDecision struct {
	Version string
	State   *BootState
	Changed bool
	// RolledBack 非空时这次启动回滚了这个试用版本。
	RolledBack string
}

// ErrNoBootable 表示没有可以启动的版本。
var ErrNoBootable = errors.New("没有可以启动的版本")

// chooseBoot 决定启动哪个版本（PLAN #27.7 第 4 步，纯函数）：有 trial 时先计次再启动它；已经启动过 maxTrialBoots 次、
// 或者它的文件不存在、不能执行时回滚（记入 failed）。然后依次尝试 current、previous。
func chooseBoot(in BootState, usable func(string) bool) (BootDecision, error) {
	st := in
	st.Failed = slices.Clone(in.Failed)
	d := BootDecision{State: &st}
	if t := in.Trial; t != nil {
		if t.Boots < maxTrialBoots && usable(t.Version) {
			nt := *t
			nt.Boots++
			st.Trial = &nt
			d.Version, d.Changed = t.Version, true
			return d, nil
		}
		reason := fmt.Sprintf("启动 %d 次都没有完成确认", t.Boots)
		if !usable(t.Version) {
			reason = "程序文件不存在或不能执行"
		}
		f := FailedVersion{Version: t.Version, Generation: t.Generation, Reason: reason}
		st.addFailed(f)
		st.RolledBack, st.Trial = &f, nil
		d.Changed, d.RolledBack = true, t.Version
	}
	if usable(in.Current) {
		d.Version = in.Current
		return d, nil
	}
	if usable(in.Previous) {
		// current 启动不了、改为启动 previous：把它记为 current（previous 清空）。否则之后再升级、确认时 previous
		// 会是那个启动不了的 current，清理时反而删掉了实际在运行、唯一能启动的版本
		st.Current, st.Previous = in.Previous, ""
		d.Version, d.Changed = in.Previous, true
		return d, nil
	}
	return d, ErrNoBootable
}

// Launch 是启动器（`<程序名> launch`）：读 state.json 选版本，把计次或回滚写回后 exec 它（同一个进程，服务管理器只看到
// 一个进程），并设置 KILN_LAUNCHER=1。exec 为 nil 时用 syscall.Exec。只在 exec 失败时返回。
//
// exec 明确失败（程序损坏、架构不对等，文件看起来可以执行但内核拒绝）时把这个版本当成不能执行，重新选择：试用版本
// 回滚、current 改为 previous（chooseBoot），都不行才返回错误。暂时的错误（内存不足等）直接返回，
// 由服务管理器重启，不因此改动启动状态。
func Launch(dataDir, prog string, args []string, exec func(argv0 string, argv []string, env []string) error) error {
	if exec == nil {
		exec = syscall.Exec
	}
	l := newLayout(dataDir, prog)
	rejected := map[string]bool{}
	usable := func(v string) bool { return !rejected[v] && l.usable(v) }
	env := append(os.Environ(), launcherEnv+"=1")
	for {
		d, err := l.choose(usable)
		if err != nil {
			return err
		}
		if d.RolledBack != "" {
			fmt.Fprintf(os.Stderr, "kiln launcher：%s 没有完成确认，回滚到 %s\n", d.RolledBack, d.Version)
		}
		argv := append([]string{l.program(d.Version)}, args...)
		err = exec(argv[0], argv, env)
		if err == nil || !execRejected(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "kiln launcher：%s 不能执行（%v），改为启动其他版本\n", d.Version, err)
		rejected[d.Version] = true
	}
}

// execRejected 判断 exec 的错误是否说明程序本身不能执行（格式、权限、文件不存在等），而不是暂时的失败。
func execRejected(err error) bool {
	for _, transient := range []error{syscall.ETXTBSY, syscall.ENOMEM, syscall.EAGAIN, syscall.EINTR, syscall.EMFILE, syscall.ENFILE} {
		if errors.Is(err, transient) {
			return false
		}
	}
	return true
}

// choose 在布局锁下读 state.json、按 usable 选版本，把计次或回滚写回（Launch 的一次选择）。
func (l layout) choose(usable func(string) bool) (BootDecision, error) {
	unlock, err := l.lock()
	if err != nil {
		// 锁不住（例如 bin/ 只读，建不了锁文件）时照常启动：这时别的进程也改不了布局；不能因此什么都不启动
		fmt.Fprintf(os.Stderr, "kiln launcher：锁住启动状态失败，不加锁继续：%v\n", err)
		unlock = func() {}
	}
	defer unlock()
	st, err := l.load()
	if err != nil {
		return BootDecision{}, fmt.Errorf("读取启动状态（需要先执行 %s service install）：%w", l.prog, err)
	}
	d, err := chooseBoot(*st, usable)
	if d.Changed {
		if werr := l.save(d.State); werr != nil {
			// 写不了计次时不启动试用版本（否则坏版本可能无限循环）：按没有 trial 选 current、previous（写不了，不写回）。
			// 回滚或普通启动时照常启动（下次启动会再回滚一次，没有副作用）（独立审查发现：以前回滚时写不了就什么也
			// 不启动；current 也启动不了时还要试 previous）
			if trial := st.Trial; trial != nil && err == nil && d.Version == trial.Version {
				nd, nerr := chooseBoot(BootState{Current: st.Current, Previous: st.Previous}, usable)
				if nerr != nil {
					return nd, fmt.Errorf("写启动状态：%w", werr)
				}
				d = nd
			}
		}
	}
	if err != nil {
		return d, fmt.Errorf("%w：current=%q previous=%q（%s）", err, st.Current, st.Previous, l.dir)
	}
	return d, nil
}

// ProgramPath 返回数据目录 dataDir 中程序 prog 的版本 version 的路径（bin/versions/<版本>/<程序名>）。
func ProgramPath(dataDir, prog, version string) string {
	return newLayout(dataDir, prog).program(version)
}

// LauncherPath 返回数据目录 dataDir 中程序 prog 的启动器（bin/launcher/<程序名>）。
func LauncherPath(dataDir, prog string) string { return newLayout(dataDir, prog).launcher() }

// SaveBootState 写数据目录 dataDir 中程序 prog 的 bin/state.json。
func SaveBootState(dataDir, prog string, st *BootState) error {
	return newLayout(dataDir, prog).save(st)
}

// LoadBootState 读数据目录 dataDir 中程序 prog 的 bin/state.json。
func LoadBootState(dataDir, prog string) (*BootState, error) { return newLayout(dataDir, prog).load() }

// WriteFileAtomic 原子地写文件：先写临时文件、fsync、rename，再 fsync 目录。
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeFileAtomic(path, data, perm)
}
