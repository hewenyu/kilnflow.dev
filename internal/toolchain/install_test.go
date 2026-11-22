package toolchain

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// installed 描述安装后应存在的一个文件。
type installed struct {
	path    string
	content string // 普通文件的内容
	link    string // 非空时是符号链接，值为目标
	exec    bool
}

// 四种内置 runtime 的完整安装：下载、校验、解包（tar.gz、zip，去掉顶层目录）、PostExtract、写入元数据。
func TestInstall(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	cases := []struct {
		name  string
		ref   Ref
		files []installed
	}{
		{
			name: "go",
			ref:  Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest},
			files: []installed{
				{path: "bin/go", content: "#!/bin/sh\necho go1.24.4\n", exec: true},
				{path: "VERSION", content: "go1.24.4"},
			},
		},
		{
			name: "node",
			ref:  Ref{Name: "node", Version: "22.11.0", Platform: "linux/amd64", Digest: rel.nodeLinux.digest},
			files: []installed{
				{path: "bin/node", content: "node", exec: true},
				{path: "bin/npm", link: "../lib/node_modules/npm/bin/npm-cli.js"},
			},
		},
		{
			name: "pnpm（平台无关，生成启动脚本）",
			ref:  Ref{Name: "pnpm", Version: "9.12.0", Platform: anyPlatform, Digest: rel.pnpm.digest},
			files: []installed{
				{path: "bin/pnpm.cjs", content: "pnpm"},
				{path: ".kiln-bin/pnpm", content: "#!/bin/sh\nexec node \"$(dirname \"$0\")/../bin/pnpm.cjs\" \"$@\"\n", exec: true},
				{path: ".kiln-bin/pnpx", content: "#!/bin/sh\nexec node \"$(dirname \"$0\")/../bin/pnpx.cjs\" \"$@\"\n", exec: true},
			},
		},
		{
			name: "pnpm（按平台发行的原生可执行文件）",
			ref:  Ref{Name: "pnpm", Version: "12.14.0", Platform: "linux/arm64", Digest: rel.pnpmLinuxArm.digest},
			files: []installed{
				{path: "pnpm", content: "native pnpm", exec: true},
				{path: ".kiln-bin/pnpm", link: "../pnpm"},
				{path: ".kiln-bin/pnpx", content: "#!/bin/sh\nexec \"$(dirname \"$0\")/pnpm\" dlx \"$@\"\n", exec: true},
			},
		},
		{
			name: "bun（zip，bunx 链接到 bun）",
			ref:  Ref{Name: "bun", Version: "1.1.30", Platform: "linux/amd64", Digest: rel.bunLinux.digest},
			files: []installed{
				{path: "bun", content: "bun", exec: true},
				{path: "bunx", link: "bun"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir, m, err := installDir(context.Background(), root, tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(root, strings.TrimPrefix(tc.ref.Digest, cas.DigestPrefix)); dir != want {
				t.Errorf("目录 = %s，期望 %s", dir, want)
			}
			wantMeta := &Meta{Name: tc.ref.Name, Version: tc.ref.Version, Platform: tc.ref.Platform, Digest: tc.ref.Digest, Env: importers[tc.ref.Name].Env()}
			if !reflect.DeepEqual(m, wantMeta) {
				t.Errorf("Meta = %+v，期望 %+v", m, wantMeta)
			}
			// 元数据写在目录中，下次直接读取
			if onDisk, err := readMeta(dir); err != nil || !reflect.DeepEqual(onDisk, wantMeta) {
				t.Errorf("磁盘上的元数据 = %+v, %v", onDisk, err)
			}
			// runtime 目录所有用户可读
			if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o755 {
				t.Errorf("目录权限 = %v, %v，期望 0755", st.Mode().Perm(), err)
			}
			for _, want := range tc.files {
				assertInstalled(t, dir, want)
			}
			assertNoTemp(t, root)
		})
	}
}

// 已安装的 runtime 不再下载；第二次的结果标明是复用。
func TestInstallCached(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	first, err := Install(context.Background(), root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused {
		t.Error("第一次安装不应标为复用")
	}
	second, err := Install(context.Background(), root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused {
		t.Error("第二次安装应标为复用")
	}
	if first.Dir != second.Dir || !reflect.DeepEqual(first.Meta, second.Meta) {
		t.Errorf("第二次安装 = %s %+v，期望与第一次相同", second.Dir, second.Meta)
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 1 {
		t.Errorf("发行包被下载 %d 次，期望 1 次", n)
	}
}

// 另一个进程持有安装锁时等待；它装好并释放后直接复用，不再下载。
func TestInstallWaitsForOtherProcess(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	hex := strings.TrimPrefix(ref.Digest, cas.DigestPrefix)
	// 另一个文件描述符上的 flock 与另一个进程持有的效果相同
	unlock, err := lockInstall(context.Background(), root, hex)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *Installed, 1)
	errc := make(chan error, 1)
	go func() {
		in, err := Install(context.Background(), root, ref)
		errc <- err
		done <- in
	}()
	select {
	case err := <-errc:
		t.Fatalf("锁被持有时 Install 不应返回：%v", err)
	case <-time.After(3 * lockPollInterval):
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 0 {
		t.Fatalf("等锁期间下载了 %d 次", n)
	}
	// "另一个进程"装好后释放
	want := &Meta{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: ref.Digest, Env: goImporter{}.Env()}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, hex), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, hex, metaFile), string(b))
	unlock()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if in := <-done; !in.Reused || !reflect.DeepEqual(in.Meta, want) {
		t.Errorf("Install = %+v，期望复用另一个进程的安装", in)
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 0 {
		t.Errorf("发行包被下载 %d 次，期望 0 次", n)
	}
}

// 等安装锁时 ctx 结束：返回 ctx 的错误，不安装。
func TestInstallLockCanceled(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	unlock, err := lockInstall(context.Background(), root, strings.TrimPrefix(ref.Digest, cas.DigestPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*lockPollInterval)
	defer cancel()
	if _, err := Install(ctx, root, ref); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Install = %v，期望 context.DeadlineExceeded", err)
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 0 {
		t.Errorf("发行包被下载 %d 次，期望 0 次", n)
	}
}

// 并发安装同一个 runtime 只下载一次，所有调用都得到同一个结果。
func TestInstallConcurrent(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	arrived, release := f.holdRequests(t, rel.goLinux.url)

	const n = 8
	dirs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			dirs[i], _, errs[i] = installDir(context.Background(), root, ref)
		})
	}
	// 第一个下载请求到达后再放行；这段时间里其他调用在等待同一个安装
	<-arrived
	release()
	wg.Wait()

	for i := range n {
		if errs[i] != nil || dirs[i] != dirs[0] {
			t.Errorf("第 %d 个调用 = %s, %v", i, dirs[i], errs[i])
		}
	}
	if got := f.hitCount(t, rel.goLinux.url); got != 1 {
		t.Errorf("发行包被下载 %d 次，期望 1 次", got)
	}
	assertNoTemp(t, root)
}

// 同一进程内另一个调用正在安装同一个 runtime（卡在下载）时，带超时 ctx 的 Install 等锁到期即返回 ctx 的错误，不再下载。
func TestInstallConcurrentCanceled(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	arrived, release := f.holdRequests(t, rel.goLinux.url)
	first := make(chan error, 1)
	go func() {
		_, err := Install(context.Background(), root, ref)
		first <- err
	}()
	<-arrived // 第一个调用拿到锁、正在下载

	ctx, cancel := context.WithTimeout(context.Background(), 2*lockPollInterval)
	defer cancel()
	second := make(chan error, 1)
	go func() {
		_, err := Install(ctx, root, ref)
		second <- err
	}()
	// 到期后及时返回，不等第一个调用的下载结束
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Install = %v，期望 context.DeadlineExceeded", err)
		}
	case <-time.After(10 * lockPollInterval):
		t.Error("ctx 到期后 Install 仍在等待另一个调用的安装")
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 1 {
		t.Errorf("发行包被下载 %d 次，期望 1 次（只有第一个调用）", n)
	}
}

// 安装失败：不留下 runtime 目录和临时文件，错误中带有 runtime 的名称、版本和平台。
func TestInstallErrors(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	// 一个路径越界的 pnpm tarball：上游内容被替换，lockfile 的 digest 也随之更新（只验证解包的拒绝）
	evil := tarGzBytes(t, []tarEntry{{name: "package/../../evil", typ: tar.TypeReg, body: "x"}})
	evilLink := tarGzBytes(t, []tarEntry{
		{name: "package/a", typ: tar.TypeSymlink, link: "../../outside"},
	})
	emptyPnpm := tarGzBytes(t, []tarEntry{{name: "package/package.json", typ: tar.TypeReg, body: `{"name":"pnpm"}`}})
	cases := []struct {
		name  string
		setup func(t *testing.T)
		ref   Ref
		want  []string
	}{
		{
			name: "不是内置 runtime",
			ref:  Ref{Name: "deno", Version: "2.0.0", Platform: "linux/amd64", Digest: rel.goLinux.digest},
			want: []string{"runtime deno@2.0.0（linux/amd64）", "只支持内置 runtime"},
		},
		{
			name: "digest 无效",
			ref:  Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: "sha256:xyz"},
			want: []string{"digest 无效"},
		},
		{
			name: "sha256 与 lockfile 不一致",
			ref:  Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: cas.Digest([]byte("other"))},
			want: []string{"runtime go@1.24.4（linux/amd64）", "与 lockfile 中的", "不一致"},
		},
		{
			name: "平台没有发行包",
			ref:  Ref{Name: "go", Version: "1.24.4", Platform: "windows/amd64", Digest: rel.goLinux.digest},
			want: []string{"没有 windows/amd64 平台"},
		},
		{
			name: "下载 404",
			setup: func(t *testing.T) {
				t.Helper()
				f.set(t, "https://registry.npmjs.org/pnpm/9.12.1", []byte(`{"dist":{"tarball":"https://registry.npmjs.org/pnpm/-/pnpm-9.12.1.tgz"}}`))
			},
			ref:  Ref{Name: "pnpm", Version: "9.12.1", Platform: anyPlatform, Digest: rel.pnpm.digest},
			want: []string{"404"},
		},
		{
			// 旧的生成方式把 pnpm 12 的包装锁成了 any：包装里没有程序，运行时会去下载，安装时拒绝
			name: "按平台发行的 pnpm 被锁成 any",
			ref:  Ref{Name: "pnpm", Version: "12.14.0", Platform: anyPlatform, Digest: rel.pnpmWrapper.digest},
			want: []string{"runtime pnpm@12.14.0（any）", "按平台发行原生可执行文件", "kiln runtime lock"},
		},
		{
			name:  "pnpm 发行包中既没有入口脚本也没有可执行文件",
			setup: func(t *testing.T) { t.Helper(); f.set(t, rel.pnpm.url, emptyPnpm) },
			ref:   Ref{Name: "pnpm", Version: "9.12.0", Platform: anyPlatform, Digest: cas.Digest(emptyPnpm)},
			want:  []string{"发行包中没有 pnpm 的可执行文件或入口脚本"},
		},
		{
			name:  "归档中的路径越界",
			setup: func(t *testing.T) { t.Helper(); f.set(t, rel.pnpm.url, evil) },
			ref:   Ref{Name: "pnpm", Version: "9.12.0", Platform: anyPlatform, Digest: cas.Digest(evil)},
			want:  []string{"解包", "路径越界"},
		},
		{
			name:  "归档中的符号链接越界",
			setup: func(t *testing.T) { t.Helper(); f.set(t, rel.pnpm.url, evilLink) },
			ref:   Ref{Name: "pnpm", Version: "9.12.0", Platform: anyPlatform, Digest: cas.Digest(evilLink)},
			want:  []string{"解包", "指向解包目录之外"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			root := t.TempDir()
			_, _, err := installDir(context.Background(), root, tc.ref)
			if err == nil {
				t.Fatal("期望安装失败")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误 %q 中没有 %q", err, w)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return e.Name() == LocksDirName })
			if len(entries) != 0 {
				t.Errorf("安装失败后留下了 %v", entries)
			}
		})
	}
}

// 上次失败留下的、没有元数据的目录会被重新安装覆盖。
func TestInstallReplacesIncompleteDir(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	ref := Ref{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}
	stale := filepath.Join(root, strings.TrimPrefix(ref.Digest, cas.DigestPrefix))
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stale, "garbage"), "x")
	dir, _, err := installDir(context.Background(), root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "garbage")); !os.IsNotExist(err) {
		t.Errorf("残留文件仍然存在：%v", err)
	}
	// 元数据损坏（例如写到一半断电）时同样重新安装
	writeFile(t, filepath.Join(dir, metaFile), "{")
	writeFile(t, filepath.Join(dir, "garbage"), "x")
	if dir, _, err = installDir(context.Background(), root, ref); err != nil {
		t.Fatal(err)
	}
	if n := f.hitCount(t, rel.goLinux.url); n != 2 {
		t.Errorf("发行包被下载 %d 次，期望 2 次", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "garbage")); !os.IsNotExist(err) {
		t.Errorf("残留文件仍然存在：%v", err)
	}
	assertInstalled(t, dir, installed{path: "bin/go", content: "#!/bin/sh\necho go1.24.4\n", exec: true})
}

// 目录中已有元数据时直接使用，不访问上游（Runner 重启后复用已安装的 runtime）。
func TestInstallUsesExistingMeta(t *testing.T) {
	f := newFakeUpstream(t)
	root := t.TempDir()
	digest := cas.Digest([]byte("preinstalled"))
	dir := filepath.Join(root, strings.TrimPrefix(digest, cas.DigestPrefix))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := &Meta{Name: "node", Version: "22.11.0", Platform: "linux/amd64", Digest: digest, Env: nodeImporter{}.Env()}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, metaFile), string(b))
	gotDir, m, err := installDir(context.Background(), root, Ref{Name: "node", Version: "22.11.0", Platform: "linux/amd64", Digest: digest})
	if err != nil || gotDir != dir || !reflect.DeepEqual(m, want) {
		t.Fatalf("Install = %s %+v, %v", gotDir, m, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) != 0 {
		t.Errorf("不应访问上游：%v", f.hits)
	}
}

func TestApplyEnv(t *testing.T) {
	t.Parallel()
	goMeta := &Meta{Env: goImporter{}.Env()}
	nodeMeta := &Meta{Env: nodeImporter{}.Env()}
	pnpmMeta := &Meta{Env: pnpmImporter{}.Env()}
	bunMeta := &Meta{Env: bunImporter{}.Env()}
	cases := []struct {
		name    string
		base    map[string]string
		mounts  []Mounted
		want    map[string]string
		wantErr string
	}{
		{
			name:   "go：PATH 加 bin，GOROOT 为挂载点，GOTOOLCHAIN=local",
			base:   map[string]string{"PATH": "/usr/bin:/bin"},
			mounts: []Mounted{{Mount: "/opt/kiln/rt/go/", Meta: goMeta}},
			want:   map[string]string{"PATH": "/opt/kiln/rt/go/bin:/usr/bin:/bin", "GOROOT": "/opt/kiln/rt/go/", "GOTOOLCHAIN": "local"},
		},
		{
			name: "多个 runtime 的 PATH 按声明顺序叠加",
			base: map[string]string{"PATH": "/usr/bin"},
			mounts: []Mounted{
				{Mount: "/rt/node", Meta: nodeMeta},
				{Mount: "/rt/pnpm", Meta: pnpmMeta},
				{Mount: "/rt/bun", Meta: bunMeta},
			},
			want: map[string]string{"PATH": "/rt/node/bin:/rt/pnpm/.kiln-bin:/rt/bun:/usr/bin"},
		},
		{
			name:   "原来没有 PATH",
			base:   map[string]string{},
			mounts: []Mounted{{Mount: "/rt/node", Meta: nodeMeta}},
			want:   map[string]string{"PATH": "/rt/node/bin"},
		},
		{
			name: "同一个变量的值相同不算冲突",
			base: map[string]string{},
			mounts: []Mounted{
				{Mount: "/rt/a", Meta: &Meta{Env: map[string]EnvSpec{"MODE": {Value: "x", IsValue: true}}}},
				{Mount: "/rt/b", Meta: &Meta{Env: map[string]EnvSpec{"MODE": {Value: "x", IsValue: true}}}},
			},
			want: map[string]string{"MODE": "x"},
		},
		{
			name: "空字符串也是一个值",
			base: map[string]string{"EMPTY": "old"},
			mounts: []Mounted{
				{Mount: "/rt/a", Meta: &Meta{Env: map[string]EnvSpec{"EMPTY": {IsValue: true}}}},
			},
			want: map[string]string{"EMPTY": ""},
		},
		{
			name: "path 冲突",
			mounts: []Mounted{
				{Mount: "/rt/go1", Meta: goMeta},
				{Mount: "/rt/go2", Meta: goMeta},
			},
			wantErr: "GOROOT",
		},
		{
			name: "value 冲突",
			mounts: []Mounted{
				{Mount: "/rt/a", Meta: &Meta{Env: map[string]EnvSpec{"MODE": {Value: "x", IsValue: true}}}},
				{Mount: "/rt/b", Meta: &Meta{Env: map[string]EnvSpec{"MODE": {Value: "y", IsValue: true}}}},
			},
			wantErr: "MODE",
		},
		{
			name: "相对路径带 ./ 前缀",
			base: map[string]string{},
			mounts: []Mounted{
				{Mount: "/rt/x/", Meta: &Meta{Env: map[string]EnvSpec{"HOME_X": {Path: "./share"}, "PATH": {PrependPaths: []string{"./bin", "."}}}}},
			},
			want: map[string]string{"HOME_X": "/rt/x/share", "PATH": "/rt/x/bin:/rt/x/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := maps.Clone(tc.base)
			if env == nil {
				env = map[string]string{}
			}
			err := ApplyEnv(env, tc.mounts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ApplyEnv = %v，期望关于 %s 的冲突错误", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(env, tc.want) {
				t.Errorf("env = %v，期望 %v", env, tc.want)
			}
		})
	}
}

// assertInstalled 检查 runtime 目录中的一个文件。
func assertInstalled(t *testing.T, dir string, want installed) {
	t.Helper()
	p := filepath.Join(dir, want.path)
	st, err := os.Lstat(p)
	if err != nil {
		t.Errorf("%s：%v", want.path, err)
		return
	}
	if want.link != "" {
		target, err := os.Readlink(p)
		if err != nil || target != want.link {
			t.Errorf("%s → %q, %v，期望 → %q", want.path, target, err, want.link)
		}
		return
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != want.content {
		t.Errorf("%s 的内容 = %q, %v，期望 %q", want.path, b, err, want.content)
	}
	if exec := st.Mode()&0o111 != 0; exec != want.exec {
		t.Errorf("%s 的权限 = %v，期望可执行 = %v", want.path, st.Mode(), want.exec)
	}
}

// assertNoTemp 检查 root 中没有残留的下载和解包临时文件。
func assertNoTemp(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsTemp(e.Name()) {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
}

// 只有 bin/pnpm.mjs 的 JavaScript 发行包：启动脚本执行 .mjs 入口。
func TestPnpmPostExtractMJS(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"pnpm"}`)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "bin", "pnpm.mjs"), "pnpm")
	writeFile(t, filepath.Join(dir, "bin", "pnpx.mjs"), "pnpx")
	if err := (pnpmImporter{}).PostExtract(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"pnpm": "#!/bin/sh\nexec node \"$(dirname \"$0\")/../bin/pnpm.mjs\" \"$@\"\n",
		"pnpx": "#!/bin/sh\nexec node \"$(dirname \"$0\")/../bin/pnpx.mjs\" \"$@\"\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, shimDir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v，期望 %q", name, b, err, want)
		}
	}
}
