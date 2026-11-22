package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ensure 按顺序装好 runtime 并给出环境变量：第一次下载，第二次全部复用、不访问下载源；每一项都报告进度，目录的
// mtime 被刷新（回收按它判断最后使用时间）。
func TestEnsure(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	refs := []Ref{
		{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest},
		{Name: "node", Version: "22.11.0", Platform: "linux/amd64", Digest: rel.nodeLinux.digest},
		{Name: "pnpm", Version: "12.14.0", Platform: "linux/arm64", Digest: rel.pnpmLinuxArm.digest},
	}
	var seen []string
	first, env, err := Ensure(context.Background(), root, refs, func(e *Ensured) { seen = append(seen, e.Name) })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"go", "node", "pnpm"}) {
		t.Errorf("进度 = %v", seen)
	}
	dirs := make([]string, len(first))
	for i, e := range first {
		dirs[i] = e.Dir
		if e.Reused || e.Ref != refs[i] || e.Dir != filepath.Join(root, strings.TrimPrefix(refs[i].Digest, "sha256:")) {
			t.Errorf("第一次的第 %d 项 = %+v", i, e)
		}
	}
	want := &Env{
		Set:     map[string]string{"GOROOT": dirs[0], "GOTOOLCHAIN": "local"},
		Prepend: map[string][]string{"PATH": {filepath.Join(dirs[0], "bin"), filepath.Join(dirs[1], "bin"), filepath.Join(dirs[2], shimDir)}},
		Order:   []string{"PATH"},
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("环境变量 = %+v，期望 %+v", env, want)
	}

	// 把目录的 mtime 调到过去：再次 Ensure 时刷新
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range dirs {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	hits := map[string]int{}
	for _, r := range []fakeRelease{rel.goLinux, rel.nodeLinux, rel.pnpmLinuxArm} {
		hits[r.url] = f.hitCount(t, r.url)
	}
	second, env2, err := Ensure(context.Background(), root, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range second {
		if !e.Reused || e.Dir != dirs[i] {
			t.Errorf("第二次的第 %d 项 = %+v，期望复用 %s", i, e, dirs[i])
		}
		if st, err := os.Stat(e.Dir); err != nil || !st.ModTime().After(old.Add(time.Hour)) {
			t.Errorf("%s 的 mtime 没有刷新：%v, %v", e.Dir, st.ModTime(), err)
		}
	}
	for u, n := range hits {
		if got := f.hitCount(t, u); got != n {
			t.Errorf("复用时访问了下载源 %s（%d → %d 次）", u, n, got)
		}
	}
	if !reflect.DeepEqual(env2, want) {
		t.Errorf("第二次的环境变量 = %+v", env2)
	}
	// 环境变量的 JSON（kiln-runner toolchain ensure 的输出）不含 Order
	b, err := json.Marshal(env)
	if err != nil || strings.Contains(string(b), "Order") || !strings.Contains(string(b), `"prepend":{"PATH":[`) {
		t.Errorf("JSON = %s, %v", b, err)
	}
}

// 多个进程（这里是多个 goroutine）同时 Ensure 同一组 runtime：每个发行包只下载一次，结果相同。
func TestEnsureConcurrent(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	root := t.TempDir()
	refs := []Ref{{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest}}
	arrived, release := f.holdRequests(t, rel.goLinux.url)
	const n = 6
	results := make([][]*Ensured, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i], _, errs[i] = Ensure(context.Background(), root, refs, nil) })
	}
	<-arrived
	release()
	wg.Wait()
	reused := 0
	for i := range n {
		if errs[i] != nil || len(results[i]) != 1 || results[i][0].Dir != results[0][0].Dir {
			t.Fatalf("第 %d 个 = %+v, %v", i, results[i], errs[i])
		}
		if results[i][0].Reused {
			reused++
		}
	}
	if reused != n-1 {
		t.Errorf("复用 %d 个，期望 %d 个（只有一个下载安装）", reused, n-1)
	}
	if got := f.hitCount(t, rel.goLinux.url); got != 1 {
		t.Errorf("发行包被下载 %d 次，期望 1 次", got)
	}
}

// 某一项失败时返回已经完成的项和带有 runtime 信息的错误，不给环境变量。
func TestEnsureError(t *testing.T) {
	f := newFakeUpstream(t)
	rel := registerAll(t, f)
	refs := []Ref{
		{Name: "go", Version: "1.24.4", Platform: "linux/amd64", Digest: rel.goLinux.digest},
		{Name: "node", Version: "22.11.0", Platform: "linux/amd64", Digest: "sha256:" + strings.Repeat("0", 64)},
	}
	done, env, err := Ensure(context.Background(), t.TempDir(), refs, nil)
	if err == nil || !strings.Contains(err.Error(), "node") || env != nil {
		t.Fatalf("Ensure = %v, %+v，期望 node 的错误", err, env)
	}
	if len(done) != 1 || done[0].Name != "go" {
		t.Errorf("已完成 = %+v", done)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Ensure(ctx, t.TempDir(), refs[:1], nil); !errors.Is(err, context.Canceled) {
		t.Errorf("取消后 Ensure = %v", err)
	}
}

func TestSourceText(t *testing.T) {
	t.Parallel()
	if got := SourceText(true, time.Minute); got != "已安装，直接使用" {
		t.Errorf("复用 = %q", got)
	}
	if got := SourceText(false, 1500*time.Microsecond); got != "下载并安装，用时 2ms" {
		t.Errorf("下载 = %q", got)
	}
}

func TestResolveRefs(t *testing.T) {
	t.Parallel()
	l := &Lock{Entries: map[string]*LockEntry{
		"go":   {Name: "go", Version: "1.27.3", Platforms: map[string]string{"darwin/arm64": "sha256:aa", "linux/arm64": "sha256:bb"}},
		"pnpm": {Name: "pnpm", Version: "12.14.0", Platforms: map[string]string{"any": "sha256:cc"}},
	}}
	refs, err := ResolveRefs(l, []string{"pnpm", "go"}, "darwin/arm64")
	want := []Ref{{Name: "pnpm", Version: "12.14.0", Platform: "any", Digest: "sha256:cc"}, {Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: "sha256:aa"}}
	if err != nil || !reflect.DeepEqual(refs, want) {
		t.Errorf("ResolveRefs = %+v, %v", refs, err)
	}
	for _, tc := range []struct {
		lock     *Lock
		names    []string
		platform string
		want     string
	}{
		{lock: nil, names: []string{"go"}, platform: "darwin/arm64", want: "仓库中没有 .kiln/runtimes.lock"},
		{lock: l, names: []string{"go", "node"}, platform: "darwin/arm64", want: "runtime node 不在"},
		{lock: l, names: []string{"go"}, platform: "linux/amd64", want: "没有平台 linux/amd64 的条目"},
	} {
		if _, err := ResolveRefs(tc.lock, tc.names, tc.platform); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ResolveRefs(%v, %s) = %v，期望包含 %q", tc.names, tc.platform, err, tc.want)
		}
	}
	if !Builtin("go") || Builtin("python") || !slices.Equal(BuiltinNames(), []string{"bun", "go", "node", "pnpm"}) {
		t.Errorf("内置 runtime = %v", BuiltinNames())
	}
}
