package fakegithub

import (
	"net/http"
)

// 对象内容缓存：按完整 SHA 读取 tree、blob、commit 的内容在测试中非常频繁（同一个 commit 的 tree、同一个文件的 blob
// 会被反复读）。git 对象是内容寻址的，完整 SHA 对应的内容不会变，可以缓存；ref、分支、HEAD、短 SHA 可能指向别的内容，
// 不缓存。缓存键里有仓库目录，不同仓库、不同模拟 GitHub 实例之间不共用，Server 关闭时随之释放。

// 缓存键中的接口名。
const (
	objectTree   = "tree"
	objectBlob   = "blob"
	objectCommit = "commit"
)

// objectCacheKey 是一次按完整 SHA 读取对象内容的键。
type objectCacheKey struct {
	dir string
	op  string
	sha string
}

// fullSHA 判断 sha 是否是完整的 40 位小写十六进制 SHA：只有这样的值才是内容寻址、永不改变的。
func fullSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// cachedObject 返回按 SHA 读取对象内容的结果。完整的 SHA 命中缓存时直接返回；没有时调用 load 读取并缓存成功的结果。
// 只在故障注入之后调用（serveWithFault 先执行），所以注入的故障照常生效。不缓存失败：读取不到的对象可能是测试之后
// 才写入的，404 不能留着。load 返回 false 时已经写出了响应（404 或 500）。
func (s *Server) cachedObject(r *http.Request, rp *Repo, op string, load func() (map[string]any, bool)) (map[string]any, bool) {
	sha := r.PathValue("sha")
	if !fullSHA(sha) {
		return load()
	}
	key := objectCacheKey{dir: rp.Dir, op: op, sha: sha}
	s.mu.Lock()
	v, ok := s.objects[key]
	s.mu.Unlock()
	if ok {
		return v, true
	}
	v, ok = load()
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	if s.objects == nil {
		s.objects = map[objectCacheKey]map[string]any{}
	}
	s.objects[key] = v
	s.mu.Unlock()
	return v, true
}
