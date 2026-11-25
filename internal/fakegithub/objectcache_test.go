package fakegithub

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// 对象内容缓存（objectcache.go）：完整的 SHA 命中缓存，ref、分支、短 SHA 不缓存；故障注入照常生效；读不到的对象不缓存。

func TestFullSHA(t *testing.T) {
	ok := strings.Repeat("a1", 20)
	for sha, want := range map[string]bool{
		ok:                      true,
		strings.Repeat("f", 40): true,
		strings.ToUpper(ok):     false, // 大写的 SHA 不是 git 的完整 SHA 写法
		ok[:39]:                 false,
		ok + "0":                false,
		"main":                  false,
		"heads/main":            false,
		strings.Repeat("g", 40): false,
		"refs/heads/" + ok:      false,
	} {
		if got := fullSHA(sha); got != want {
			t.Errorf("fullSHA(%q) = %v，应为 %v", sha, got, want)
		}
	}
}

// 完整的 SHA 读一次之后进了缓存：把仓库目录挪走也照常返回；同样的目录、接口、SHA 只占一条。
func TestObjectCacheFullSHA(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tree := gitOut(t, f.dir, "rev-parse", "HEAD^{tree}")
	if _, err := f.inst.GetTree(ctx, demo, tree, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inst.GetTree(ctx, demo, tree, true); err != nil {
		t.Fatal(err)
	}
	f.s.mu.Lock()
	if len(f.s.objects) != 1 {
		t.Fatalf("同一个对象应只有一条缓存，实际 %d 条", len(f.s.objects))
	}
	for k := range f.s.objects {
		if k != (objectCacheKey{dir: f.dir, op: objectTree, sha: tree}) {
			t.Fatalf("缓存键应为仓库目录、接口和完整 SHA，实际 %+v", k)
		}
	}
	f.s.mu.Unlock()

	away := f.dir + ".away"
	if err := os.Rename(f.dir, away); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(away, f.dir) })
	// 仓库不在原地：缓存命中的完整 SHA 照常返回，没缓存过的短 SHA 读不到。
	if tr, err := f.inst.GetTree(ctx, demo, tree, true); err != nil || len(tr.Tree) == 0 {
		t.Fatalf("缓存命中的完整 SHA 应照常返回：%v %v", tr, err)
	}
	if _, err := f.inst.GetTree(ctx, demo, tree[:8], true); !github.IsNotFound(err) {
		t.Fatalf("短 SHA 不缓存，仓库不在时应返回 404：%v", err)
	}
}

// ref、分支名可能指向新的 commit：每次都重新读，不返回旧内容；完整 SHA 的缓存不受影响。
func TestObjectCacheRefsNotCached(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	first := gitOut(t, f.dir, "rev-parse", "HEAD")
	cm, err := f.inst.GetCommit(ctx, demo, "main")
	if err != nil || cm.SHA != first {
		t.Fatalf("GetCommit(main) = %+v %v", cm, err)
	}
	// 新的 commit 推到 main 上
	gitOut(t, f.dir, "checkout", "-q", "main")
	gitOut(t, f.dir, "commit", "-q", "--allow-empty", "-m", "three")
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	if cm, err := f.inst.GetCommit(ctx, demo, "main"); err != nil || cm.SHA != head {
		t.Fatalf("分支名不应命中缓存：GetCommit(main) = %+v %v", cm, err)
	}
	// 旧的完整 SHA 内容不变，仍从缓存返回
	if cm, err := f.inst.GetCommit(ctx, demo, first); err != nil || cm.SHA != first || cm.Message != "two" {
		t.Fatalf("完整 SHA 应仍返回原来的 commit：%+v %v", cm, err)
	}
}

// 读不到的对象不缓存：之后写入的对象照常读到，已有的缓存也不受影响。
func TestObjectCacheNotFoundNotCached(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	if _, err := f.inst.GetCommit(ctx, demo, head); err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("0", 40)
	if _, err := f.inst.GetCommit(ctx, demo, missing); !github.IsNotFound(err) {
		t.Fatalf("不存在的对象应返回 404：%v", err)
	}
	f.s.mu.Lock()
	n := len(f.s.objects)
	f.s.mu.Unlock()
	if n != 1 {
		t.Fatalf("读不到的对象不应缓存，只应有之前读到的那条，实际 %d 条", n)
	}
	// 新的 commit 是一个新的对象，照常读到；之前读到的仍从缓存返回
	gitOut(t, f.dir, "commit", "-q", "--allow-empty", "-m", "three")
	next := gitOut(t, f.dir, "rev-parse", "HEAD")
	if cm, err := f.inst.GetCommit(ctx, demo, next); err != nil || cm.SHA != next || cm.Message != "three" {
		t.Fatalf("之后写入的对象应能读到：%+v %v", cm, err)
	}
	if cm, err := f.inst.GetCommit(ctx, demo, head); err != nil || cm.SHA != head {
		t.Fatalf("已有的缓存不应受影响：%+v %v", cm, err)
	}
}

// 故障注入在缓存查找之前生效：注入的状态码不读对象、不缓存；Lost 照常执行（并缓存），客户端只看到 502。
func TestObjectCacheWithFaults(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tree := gitOut(t, f.dir, "rev-parse", "HEAD^{tree}")
	route := fmt.Sprintf("GET /repos/%s/git/trees/%s", demo, tree)

	f.s.SetFault(route, Fault{Status: 503})
	if _, err := f.inst.GetTree(ctx, demo, tree, true); err == nil {
		t.Fatal("注入的故障应让请求失败")
	}
	f.s.mu.Lock()
	n := len(f.s.objects)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("故障的请求不应缓存，实际 %d 条", n)
	}

	f.s.SetFault(route, Fault{Lost: true})
	if _, err := f.inst.GetTree(ctx, demo, tree, true); err == nil {
		t.Fatal("响应丢失时应返回错误")
	}
	if tr, err := f.inst.GetTree(ctx, demo, tree, true); err != nil || len(tr.Tree) == 0 {
		t.Fatalf("Lost 的请求照常执行并缓存了对象，重试应成功：%v %v", tr, err)
	}
}

// 并发读同一个对象：都成功，只占一条缓存（-race 下不能有数据竞争）。
func TestObjectCacheConcurrent(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tree := gitOut(t, f.dir, "rev-parse", "HEAD^{tree}")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.inst.GetTree(ctx, demo, tree, true)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求失败：%v", i, err)
		}
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if len(f.s.objects) != 1 {
		t.Fatalf("并发读同一个对象应只有一条缓存，实际 %d 条", len(f.s.objects))
	}
}
