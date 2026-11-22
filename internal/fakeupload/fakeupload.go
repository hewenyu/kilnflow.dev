// Package fakeupload 是 HTTP 上传协议 v1（docs/UPLOAD-PROTOCOL.md）的内存服务端，供 internal/upload、
// internal/server/ci/publish 等包的单元测试使用，也是第三方实现服务端时的参考。
//
// 端点：GET {base}/{tag}、PUT {base}/{tag}/assets/{name}、POST {base}/{tag}/publish；
// 已发布的文件可以从 GET /download/{tag}/{name} 下载（不需要认证，模拟官网的下载页）。
package fakeupload

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BasePath 是上传 API 的路径前缀：URL = <服务器地址> + BasePath。
const BasePath = "/api/releases"

// Server 是内存中的上传服务端。
type Server struct {
	// URL 是上传地址（release.uploads[].url），形如 http://127.0.0.1:<port>/api/releases。
	URL string

	srv      *httptest.Server
	token    string
	mu       sync.Mutex
	releases map[string]*release // tag → release
	latest   string
	maxSize  int64
	fail     []failure
	requests []string
	auth     []string
	uas      []string
	now      func() time.Time
}

type release struct {
	staged      map[string][]byte // 暂存的文件（含已发布的）
	published   bool
	publishedAt string
	manifest    []assetJSON // 发布时的清单（按名称排序）
	commit      string
	runID       int64
	prerelease  bool
}

type assetJSON struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url,omitempty"`
}

// failure 是注入的故障：先放行 skip 个匹配的请求，之后 n 个匹配的请求得到 status（429、503 带 Retry-After: 0）。
type failure struct {
	method string // 空表示任意方法
	skip   int
	status int
	n      int
}

// New 启动服务端：请求必须带 Authorization: Bearer <token>。
func New(token string) *Server {
	s := &Server{token: token, releases: map[string]*release{}, now: time.Now}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + BasePath
	return s
}

// Close 关闭服务端。
func (s *Server) Close() { s.srv.Close() }

// SetMaxSize 设置单个文件的大小上限（超过时返回 413 too_large），0 表示不限制。
func (s *Server) SetMaxSize(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxSize = n
}

// FailNext 让接下来 n 个方法为 method（空表示任意）的请求得到 status，不做任何处理。
func (s *Server) FailNext(method string, status, n int) { s.FailAfter(method, 0, status, n) }

// FailAfter 先放行 skip 个方法为 method 的请求，之后 n 个得到 status。
func (s *Server) FailAfter(method string, skip, status, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{method: method, skip: skip, status: status, n: n})
}

// Requests 返回收到的请求（"METHOD 路径"，路径为转义后的原始形式）。
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// AuthorizationHeaders 返回收到的 Authorization 头。
func (s *Server) AuthorizationHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auth)
}

// UserAgents 返回收到的 User-Agent 头。
func (s *Server) UserAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uas)
}

// Latest 返回当前的"最新版"（没有时为空）。
func (s *Server) Latest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// State 返回 tag 的状态：""（没有记录）、staging 或 published。
func (s *Server) State(tag string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.releases[tag]
	switch {
	case r == nil:
		return ""
	case r.published:
		return "published"
	}
	return "staging"
}

// Published 返回已发布的 tag 中各文件的内容（未发布时为 nil）。
func (s *Server) Published(tag string) map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.releases[tag]
	if r == nil || !r.published {
		return nil
	}
	out := map[string][]byte{}
	for _, a := range r.manifest {
		out[a.Name] = r.staged[a.Name]
	}
	return out
}

// Staged 返回 tag 下暂存的文件名（含已发布的），按名称排序。
func (s *Server) Staged(tag string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.releases[tag]; r != nil {
		return slices.Sorted(maps.Keys(r.staged))
	}
	return nil
}

// PublishInfo 返回发布时提交的 commit、run_id 和 prerelease。
func (s *Server) PublishInfo(tag string) (commit string, runID int64, prerelease bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.releases[tag]; r != nil {
		return r.commit, r.runID, r.prerelease
	}
	return "", 0, false
}

// Preset 直接把 files 作为 tag 的已发布内容（模拟服务端上已有的版本）。
func (s *Server) Preset(tag string, files map[string][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &release{staged: map[string][]byte{}, published: true, publishedAt: s.now().UTC().Format(time.RFC3339)}
	for name, b := range files {
		r.staged[name] = b
		r.manifest = append(r.manifest, describe(name, b))
	}
	slices.SortFunc(r.manifest, func(a, b assetJSON) int { return strings.Compare(a.Name, b.Name) })
	s.releases[tag] = r
}

func describe(name string, b []byte) assetJSON {
	sum := sha256.Sum256(b)
	return assetJSON{Name: name, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.EscapedPath()
	s.requests = append(s.requests, r.Method+" "+path)
	if rest, ok := strings.CutPrefix(path, "/download/"); ok && r.Method == http.MethodGet {
		s.download(w, rest)
		return
	}
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.uas = append(s.uas, r.Header.Get("User-Agent"))
	for i := range s.fail {
		f := &s.fail[i]
		if f.n == 0 || (f.method != "" && f.method != r.Method) {
			continue
		}
		if f.skip > 0 {
			f.skip--
			break
		}
		f.n--
		if f.status == http.StatusTooManyRequests || f.status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "0")
		}
		writeErr(w, f.status, "injected", "注入的故障")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "token 无效")
		return
	}
	rest, ok := strings.CutPrefix(path, BasePath+"/")
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "")
		return
	}
	segs := strings.Split(rest, "/")
	tag, err := url.PathUnescape(segs[0])
	if err != nil || !validName(tag) {
		writeErr(w, http.StatusBadRequest, "bad_name", "tag 无效")
		return
	}
	switch {
	case len(segs) == 1 && r.Method == http.MethodGet:
		s.get(w, tag)
	case len(segs) == 3 && segs[1] == "assets" && r.Method == http.MethodPut:
		name, err := url.PathUnescape(segs[2])
		if err != nil || !validName(name) {
			writeErr(w, http.StatusBadRequest, "bad_name", "文件名无效")
			return
		}
		s.put(w, r, tag, name)
	case len(segs) == 2 && segs[1] == "publish" && r.Method == http.MethodPost:
		s.publish(w, r, tag)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

// validName：非空、不含 / 和控制字符、不是 . 或 ..。
func validName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsFunc(s, func(r rune) bool { return r == '/' || r == '\\' || r < 0x20 })
}

func (s *Server) get(w http.ResponseWriter, tag string) {
	r := s.releases[tag]
	if r == nil {
		writeErr(w, http.StatusNotFound, "not_found", "没有这个 tag")
		return
	}
	out := map[string]any{"tag": tag, "state": "staging"}
	var assets []assetJSON
	if r.published {
		out["state"], out["published_at"] = "published", r.publishedAt
		assets = s.withURLs(tag, r.manifest)
	} else {
		for _, name := range slices.Sorted(maps.Keys(r.staged)) {
			assets = append(assets, describe(name, r.staged[name]))
		}
	}
	out["assets"] = nonNil(assets)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) put(w http.ResponseWriter, req *http.Request, tag, name string) {
	want, ok := parseDigest(req.Header.Get("Content-Digest"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "digest_mismatch", "缺少或无法解析 Content-Digest（sha-256）")
		return
	}
	if req.ContentLength < 0 {
		writeErr(w, http.StatusLengthRequired, "length_required", "缺少 Content-Length")
		return
	}
	if s.maxSize > 0 && req.ContentLength > s.maxSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("超过 %d 字节", s.maxSize))
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || int64(len(body)) != req.ContentLength {
		writeErr(w, http.StatusBadRequest, "digest_mismatch", "内容长度与 Content-Length 不符")
		return
	}
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		writeErr(w, http.StatusBadRequest, "digest_mismatch", "内容与 Content-Digest 不符")
		return
	}
	r := s.releases[tag]
	if r == nil {
		r = &release{staged: map[string][]byte{}}
		s.releases[tag] = r
	}
	if old, ok := r.staged[name]; ok && bytes.Equal(old, body) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.published {
		writeErr(w, http.StatusConflict, "published", "tag 已发布，不能修改")
		return
	}
	r.staged[name] = body
	w.WriteHeader(http.StatusCreated)
}

// parseDigest 从 Content-Digest（RFC 9530）中取出 sha-256 的值。
func parseDigest(h string) ([]byte, bool) {
	for item := range strings.SplitSeq(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || !strings.EqualFold(k, "sha-256") || len(v) < 2 || v[0] != ':' || v[len(v)-1] != ':' {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v[1 : len(v)-1])
		if err == nil && len(b) == sha256.Size {
			return b, true
		}
	}
	return nil, false
}

func (s *Server) publish(w http.ResponseWriter, req *http.Request, tag string) {
	var m struct {
		Commit     string      `json:"commit"`
		RunID      int64       `json:"run_id"`
		Latest     bool        `json:"latest"`
		Prerelease bool        `json:"prerelease"`
		Assets     []assetJSON `json:"assets"`
	}
	if err := json.NewDecoder(req.Body).Decode(&m); err != nil || len(m.Assets) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "清单无法解析或为空")
		return
	}
	slices.SortFunc(m.Assets, func(a, b assetJSON) int { return strings.Compare(a.Name, b.Name) })
	r := s.releases[tag]
	if r != nil && r.published {
		if !sameManifest(r.manifest, m.Assets) {
			writeErr(w, http.StatusConflict, "published", "tag 已用不同的清单发布")
			return
		}
		s.writePublished(w, tag, r)
		return
	}
	for _, a := range m.Assets {
		var b []byte
		if r != nil {
			b = r.staged[a.Name]
		}
		if b == nil || describe(a.Name, b) != (assetJSON{Name: a.Name, Size: a.Size, SHA256: a.SHA256}) {
			writeErr(w, http.StatusConflict, "incomplete", a.Name+" 没有暂存或大小、摘要不符")
			return
		}
	}
	// 清单之外的暂存文件丢弃
	for name := range r.staged {
		if !slices.ContainsFunc(m.Assets, func(a assetJSON) bool { return a.Name == name }) {
			delete(r.staged, name)
		}
	}
	r.published, r.publishedAt, r.manifest = true, s.now().UTC().Format(time.RFC3339), m.Assets
	r.commit, r.runID, r.prerelease = m.Commit, m.RunID, m.Prerelease
	if m.Latest {
		s.latest = tag
	}
	s.writePublished(w, tag, r)
}

func sameManifest(a, b []assetJSON) bool {
	return slices.EqualFunc(a, b, func(x, y assetJSON) bool { return x.Name == y.Name && x.Size == y.Size && x.SHA256 == y.SHA256 })
}

func (s *Server) writePublished(w http.ResponseWriter, tag string, r *release) {
	writeJSON(w, http.StatusOK, map[string]any{"tag": tag, "published_at": r.publishedAt, "assets": s.withURLs(tag, r.manifest)})
}

func (s *Server) withURLs(tag string, list []assetJSON) []assetJSON {
	out := make([]assetJSON, len(list))
	for i, a := range list {
		a.URL = s.srv.URL + "/download/" + url.PathEscape(tag) + "/" + url.PathEscape(a.Name)
		out[i] = a
	}
	return out
}

func (s *Server) download(w http.ResponseWriter, rest string) {
	tagEsc, nameEsc, _ := strings.Cut(rest, "/")
	tag, err1 := url.PathUnescape(tagEsc)
	name, err2 := url.PathUnescape(nameEsc)
	r := s.releases[tag]
	if err1 != nil || err2 != nil || r == nil || !r.published || r.staged[name] == nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(r.staged[name])))
	_, _ = w.Write(r.staged[name]) // 测试服务器，写失败无妨
}

func nonNil(a []assetJSON) []assetJSON {
	if a == nil {
		return []assetJSON{}
	}
	return a
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // 测试服务器，写失败无妨
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
