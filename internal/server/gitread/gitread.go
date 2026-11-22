// Package gitread 读取仓库在 GitHub 上的 git 对象（tree、blob、commit 时间）并缓存，解析分支、tag 和 commit。CI 的规划和
// AI 审计的规划共用（只是读取机制，不含领域判断）。
package gitread

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 从 GitHub 读取的对象的缓存容量（条目数）。按 commit 或 blob 寻址，内容不会变化，满了整体清空。
const (
	// treeCacheSize 是 git tree 的缓存容量；tree 较大，容量最小。
	treeCacheSize = 256
	// blobCacheSize 是文件内容的缓存容量。
	blobCacheSize = 2048
	// commitTimeCacheSize 是 commit 时间的缓存容量。
	commitTimeCacheSize = 4096
)

// Reader 读取并缓存 git 对象。可以并发使用。
type Reader struct {
	trees   *boundedCache[*gh.Tree]
	blobs   *boundedCache[[]byte]
	commits *boundedCache[int64]
}

// New 返回 Reader。
func New() *Reader {
	return &Reader{
		trees:   newBoundedCache[*gh.Tree](treeCacheSize),
		blobs:   newBoundedCache[[]byte](blobCacheSize),
		commits: newBoundedCache[int64](commitTimeCacheSize),
	}
}

// cacheKey 返回仓库中按 sha 寻址的对象的缓存键。
func cacheKey(repo *store.Repo, sha string) string {
	return fmt.Sprintf("%d:%s", repo.ID, sha)
}

// Tree 返回 commit 的完整 git tree。
func (r *Reader) Tree(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) (*gh.Tree, error) {
	return r.trees.getOrLoad(cacheKey(repo, sha), func() (*gh.Tree, error) {
		return inst.GetTree(ctx, repo.FullName(), sha, true)
	})
}

// Blob 返回 blob 的内容。
func (r *Reader) Blob(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) ([]byte, error) {
	return r.blobs.getOrLoad(cacheKey(repo, sha), func() ([]byte, error) {
		return inst.GetBlob(ctx, repo.FullName(), sha)
	})
}

// CommitTime 返回 commit 的提交时间（Unix 秒）。
func (r *Reader) CommitTime(ctx context.Context, inst *gh.Inst, repo *store.Repo, sha string) (int64, error) {
	return r.commits.getOrLoad(cacheKey(repo, sha), func() (int64, error) {
		c, err := inst.GetCommit(ctx, repo.FullName(), sha)
		if err != nil {
			return 0, err
		}
		return c.Committer.Date.Unix(), nil
	})
}

// FindBlob 返回 tree 中 path 文件的 blob sha，没有时返回空。
func FindBlob(t *gh.Tree, path string) string {
	for _, e := range t.Tree {
		if e.Type == "blob" && e.Path == path {
			return e.SHA
		}
	}
	return ""
}

// RefError 表示 ref 找不到（调用方据此返回参数错误）。
type RefError struct{ Err error }

func (e *RefError) Error() string { return e.Err.Error() }
func (e *RefError) Unwrap() error { return e.Err }

// IsRefError 判断 err 是否为 *RefError。
func IsRefError(err error) bool {
	var re *RefError
	return errors.As(err, &re)
}

// ResolveRef 解析分支、tag 或不带前缀的 ref（先当作分支，再当作 tag），返回完整的 ref 和 commit。ref 为空时为默认分支。
// 找不到时返回 *RefError。
func ResolveRef(ctx context.Context, inst *gh.Inst, repo *store.Repo, ref string) (fullRef, sha string, err error) {
	if ref == "" {
		ref = gitref.BranchPrefix + repo.DefaultBranch
	}
	branchSHA := func(branch string) (string, error) {
		r, err := inst.GetRef(ctx, repo.FullName(), "heads/"+branch)
		if err != nil {
			return "", err
		}
		return r.Object.SHA, nil
	}
	if tag, ok := strings.CutPrefix(ref, gitref.TagPrefix); ok {
		sha, _, err = inst.ResolveTag(ctx, repo.FullName(), tag)
	} else if branch, ok := strings.CutPrefix(ref, gitref.BranchPrefix); ok {
		sha, err = branchSHA(branch)
	} else {
		sha, fullRef, err = resolveShortRef(ctx, inst, repo, ref, branchSHA)
		if err == nil {
			return fullRef, sha, nil
		}
	}
	if err != nil {
		return "", "", &RefError{Err: fmt.Errorf("找不到 ref %s：%w", ref, err)}
	}
	return ref, sha, nil
}

// resolveShortRef 解析不带前缀的 ref：先当作分支，再当作 tag。都找不到时返回分支的错误。
func resolveShortRef(ctx context.Context, inst *gh.Inst, repo *store.Repo, ref string, branchSHA func(string) (string, error)) (sha, fullRef string, err error) {
	sha, err = branchSHA(ref)
	if err == nil {
		return sha, gitref.BranchPrefix + ref, nil
	}
	if tagSHA, _, terr := inst.ResolveTag(ctx, repo.FullName(), ref); terr == nil {
		return tagSHA, gitref.TagPrefix + ref, nil
	}
	return "", "", err
}

// ResolveRefOrCommit 与 ResolveRef 相同，只是 40 位十六进制的 ref 当作 commit sha（返回的 ref 为空）。
func ResolveRefOrCommit(ctx context.Context, inst *gh.Inst, repo *store.Repo, ref string) (fullRef, sha string, err error) {
	if IsFullSHA(ref) {
		if _, err := inst.GetCommit(ctx, repo.FullName(), ref); err != nil {
			return "", "", &RefError{Err: fmt.Errorf("找不到 commit %s：%w", ref, err)}
		}
		return "", ref, nil
	}
	return ResolveRef(ctx, inst, repo, ref)
}

// fullSHALen 是 SHA-1 commit sha 的十六进制长度。
const fullSHALen = 40

// IsFullSHA 判断 s 是否是 40 位小写十六进制的 commit sha。
func IsFullSHA(s string) bool {
	if len(s) != fullSHALen {
		return false
	}
	return !slices.ContainsFunc([]byte(s), func(c byte) bool { return (c < '0' || c > '9') && (c < 'a' || c > 'f') })
}

// boundedCache 是一个简单的有界缓存（满了就整体清空）。
type boundedCache[V any] struct {
	mu    sync.Mutex
	limit int
	m     map[string]V
}

func newBoundedCache[V any](limit int) *boundedCache[V] {
	return &boundedCache[V]{limit: limit, m: map[string]V{}}
}

func (c *boundedCache[V]) Get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *boundedCache[V]) Put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.limit {
		c.m = map[string]V{}
	}
	c.m[k] = v
}

// getOrLoad 返回 k 的缓存值；没有时调用 load 读取，成功后放入缓存。load 不在锁内执行，
// 并发读取同一个 k 时可能各自调用一次 load。
func (c *boundedCache[V]) getOrLoad(k string, load func() (V, error)) (V, error) {
	if v, ok := c.Get(k); ok {
		return v, nil
	}
	v, err := load()
	if err != nil {
		return v, err
	}
	c.Put(k, v)
	return v, nil
}
