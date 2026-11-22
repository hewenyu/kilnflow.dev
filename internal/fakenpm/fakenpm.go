// Package fakenpm 是 npm registry 的内存替身，供 internal/npm、internal/server/ci/publish 等包的单元测试使用。
//
// 端点（与 registry.npmjs.org 相同）：
//   - GET /<name>：packument（名字可以写成 @scope%2fname 或 @scope/name）；不存在时 404 {"error":"Not found"}，
//     restricted 的包没带正确的 token 时也是 404。
//   - PUT /<name>：npm publish 的文档；token 错误 401，文档无效 400，版本已存在 403。
//   - GET /<name>/-/<file>.tgz：下载 tarball（不需要认证，模拟公开下载；SetTarballPrefix 可以改变下载路径）。
package fakenpm

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // 核对 npm 的 dist.shasum（规定用 sha1）
	"crypto/sha512"
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
)

// Server 是内存中的 npm registry。
type Server struct {
	// URL 是 registry 地址，以 / 结尾，形如 http://127.0.0.1:<port>/。
	URL string

	srv      *httptest.Server
	token    string
	done     chan struct{} // Close 时关闭，让挂起的请求返回
	closing  sync.Once
	mu       sync.Mutex
	pkgs     map[string]*pkg
	faults   []fault
	requests []string
	auth     []string
	uas      []string
	docs     []map[string]any
	// tarballPrefix 是下载路径的前缀（SetTarballPrefix），默认为空：与 npmjs 相同。
	tarballPrefix string
}

type pkg struct {
	distTags map[string]string
	versions map[string]*version
	access   string
}

type version struct {
	manifest  map[string]any // 不含 dist
	tgz       []byte
	integrity string
	shasum    string
}

// 注入故障的方式。
const (
	faultBefore = iota // 不处理请求，直接返回 status
	faultAfter         // 照常处理请求，但返回 status（模拟"已生效但客户端没收到成功响应"）
	faultHang          // 照常处理请求，然后挂起直到客户端放弃（模拟超时）
)

// fault 是注入的故障：接下来 n 个方法为 method（空表示任意）的请求按 mode 处理。
type fault struct {
	method string
	mode   int
	status int
	n      int
}

// New 启动 registry：发布和读取 restricted 的包需要 Authorization: Bearer <token>。
func New(token string) *Server {
	s := &Server{token: token, done: make(chan struct{}), pkgs: map[string]*pkg{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + "/"
	return s
}

// Close 关闭 registry（挂起的请求立即返回）。
func (s *Server) Close() {
	s.closing.Do(func() { close(s.done) })
	s.srv.Close()
}

// SetTarballPrefix 让 tarball 的下载地址改为 <registry><prefix><name>/-/<file>.tgz（模拟下载地址与 npmjs 格式不同的
// registry）：dist.tarball 返回新地址，npmjs 格式的地址返回 404。prefix 以 / 结尾，如 "downloads/"。
func (s *Server) SetTarballPrefix(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tarballPrefix = prefix
}

// FailNext 让接下来 n 个方法为 method（空表示任意）的请求得到 status，不做任何处理（429、503 带 Retry-After: 0）。
// 不影响 tarball 下载。
func (s *Server) FailNext(method string, status, n int) { s.addFault(method, faultBefore, status, n) }

// FailAfterApply 让接下来 n 个方法为 method 的请求照常处理（例如 PUT 已保存），但返回 status（如 502）。
func (s *Server) FailAfterApply(method string, status, n int) {
	s.addFault(method, faultAfter, status, n)
}

// HangNext 让下一个方法为 method 的请求照常处理，然后不返回响应，直到客户端放弃（超时或取消）或 Close。
func (s *Server) HangNext(method string) { s.addFault(method, faultHang, 0, 1) }

func (s *Server) addFault(method string, mode, status, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, fault{method: method, mode: mode, status: status, n: n})
}

// Requests 返回收到的请求（"METHOD 路径"，路径为转义后的原始形式，含 tarball 下载）。
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// AuthorizationHeaders 返回收到的 Authorization 头（不含 tarball 下载）。
func (s *Server) AuthorizationHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auth)
}

// UserAgents 返回收到的 User-Agent 头（不含 tarball 下载）。
func (s *Server) UserAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uas)
}

// PublishDocs 返回收到的 PUT 请求体（解析后的 JSON，含无效和被拒绝的），用于检查发布文档的结构。
func (s *Server) PublishDocs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.docs)
}

// Packument 返回 GET /<name> 的内容（不检查权限），包不存在时为 nil。
func (s *Server) Packument(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pkgs[name]
	if p == nil {
		return nil
	}
	return s.packument(name, p)
}

// Tarball 返回已发布版本的 tarball，不存在时为 nil。
func (s *Server) Tarball(name, ver string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pkgs[name]; p != nil && p.versions[ver] != nil {
		return bytes.Clone(p.versions[ver].tgz)
	}
	return nil
}

// Versions 返回包已发布的版本（按字符串排序）。
func (s *Server) Versions(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pkgs[name]; p != nil {
		return slices.Sorted(maps.Keys(p.versions))
	}
	return nil
}

// DistTags 返回包的 dist-tags，包不存在时为 nil。
func (s *Server) DistTags(name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pkgs[name]; p != nil {
		return maps.Clone(p.distTags)
	}
	return nil
}

// Access 返回包的可见性（public / restricted），包不存在时为空。
func (s *Server) Access(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pkgs[name]; p != nil {
		return p.access
	}
	return ""
}

// SetAccess 修改已有包的可见性。
func (s *Server) SetAccess(name, access string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pkgs[name]; p != nil {
		p.access = access
	}
}

// Preset 直接把 tgz 作为 name@ver 的已发布内容（模拟 registry 上已有的版本），并把 latest 指向它。
// 新包的可见性为 public（用 SetAccess 修改）。manifest 是 package.json 的内容，可以为 nil。
func (s *Server) Preset(name, ver string, tgz []byte, manifest map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := maps.Clone(manifest)
	if m == nil {
		m = map[string]any{}
	}
	delete(m, "dist")
	m["_id"], m["name"], m["version"] = name+"@"+ver, name, ver
	p := s.pkgs[name]
	if p == nil {
		p = &pkg{distTags: map[string]string{}, versions: map[string]*version{}, access: "public"}
		s.pkgs[name] = p
	}
	p.versions[ver] = &version{manifest: m, tgz: bytes.Clone(tgz), integrity: integrity(tgz), shasum: shasum(tgz)}
	p.distTags["latest"] = ver
}

func integrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func shasum(b []byte) string {
	sum := sha1.Sum(b) //nolint:gosec // npm 的 dist.shasum 规定用 sha1
	return hex.EncodeToString(sum[:])
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method == http.MethodPut {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读取请求体失败")
			return
		}
		body = b
	}
	if s.handle(w, r, body) {
		// 已处理但不返回响应：等到客户端断开（请求的 context 取消）或 Close
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
	}
}

// handle 在锁内处理请求并写响应；返回 true 表示请求已处理但应挂起（没有写响应）。
func (s *Server) handle(w http.ResponseWriter, r *http.Request, body []byte) (hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := r.URL.EscapedPath()
	s.requests = append(s.requests, r.Method+" "+raw)
	p, err := url.PathUnescape(strings.TrimPrefix(raw, "/"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "路径无效")
		return false
	}
	if name, file, ok := strings.Cut(p, "/-/"); ok && r.Method == http.MethodGet {
		if name, ok = strings.CutPrefix(name, s.tarballPrefix); ok {
			s.download(w, name, file)
		} else {
			writeErr(w, http.StatusNotFound, "Not found")
		}
		return false
	}
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.uas = append(s.uas, r.Header.Get("User-Agent"))
	if r.Method == http.MethodPut {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			s.docs = append(s.docs, doc)
		}
	}
	f := s.takeFault(r.Method)
	if f != nil && f.mode == faultBefore {
		writeInjected(w, f.status)
		return false
	}
	out := w
	if f != nil {
		out = discard{} // 照常处理，响应另外决定
	}
	switch r.Method {
	case http.MethodGet:
		s.get(out, r, p)
	case http.MethodPut:
		s.publish(out, r, p, body)
	default:
		writeErr(out, http.StatusMethodNotAllowed, "method not allowed")
	}
	if f == nil {
		return false
	}
	if f.mode == faultHang {
		return true
	}
	writeInjected(w, f.status)
	return false
}

// takeFault 取出匹配 method 的下一个故障（调用方持有锁）。
func (s *Server) takeFault(method string) *fault {
	for i := range s.faults {
		f := &s.faults[i]
		if f.n > 0 && (f.method == "" || f.method == method) {
			f.n--
			c := *f
			return &c
		}
	}
	return nil
}

func writeInjected(w http.ResponseWriter, status int) {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "0")
	}
	writeErr(w, status, "injected fault")
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.token
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, name string) {
	p := s.pkgs[name]
	if p == nil || (p.access == "restricted" && !s.authorized(r)) {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	writeJSON(w, http.StatusOK, s.packument(name, p))
}

// packument 生成 GET /<name> 的内容（调用方持有锁）。
func (s *Server) packument(name string, p *pkg) map[string]any {
	versions := map[string]any{}
	for v, ver := range p.versions {
		m := maps.Clone(ver.manifest)
		m["dist"] = map[string]any{"integrity": ver.integrity, "shasum": ver.shasum, "tarball": s.tarballURL(name, v)}
		versions[v] = m
	}
	return map[string]any{"_id": name, "name": name, "dist-tags": maps.Clone(p.distTags), "versions": versions}
}

// tarballURL 返回下载地址：<registry><前缀><name>/-/<不含 scope 的名字>-<version>.tgz（前缀默认为空，即 npmjs 格式）。
func (s *Server) tarballURL(name, ver string) string {
	return s.URL + s.tarballPrefix + name + "/-/" + shortName(name) + "-" + ver + ".tgz"
}

func shortName(name string) string {
	if strings.HasPrefix(name, "@") {
		if _, short, ok := strings.Cut(name, "/"); ok {
			return short
		}
	}
	return name
}

func (s *Server) download(w http.ResponseWriter, name, file string) {
	if p := s.pkgs[name]; p != nil {
		for v, ver := range p.versions {
			if file == shortName(name)+"-"+v+".tgz" {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(ver.tgz)))
				_, _ = w.Write(ver.tgz) // 测试服务器，写失败无妨
				return
			}
		}
	}
	writeErr(w, http.StatusNotFound, "Not found")
}

// publishDoc 是 npm publish 的文档中用到的字段。
type publishDoc struct {
	ID          string                    `json:"_id"`
	Name        string                    `json:"name"`
	DistTags    map[string]string         `json:"dist-tags"`
	Versions    map[string]map[string]any `json:"versions"`
	Access      *string                   `json:"access"`
	Attachments map[string]struct {
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
		Length      int    `json:"length"`
	} `json:"_attachments"`
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if !s.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var doc publishDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "文档无法解析："+err.Error())
		return
	}
	ver, m, tgz, msg := checkDoc(name, &doc)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	p := s.pkgs[name]
	if p != nil && p.versions[ver] != nil {
		writeErr(w, http.StatusForbidden, fmt.Sprintf("cannot publish over previously published version %q", ver))
		return
	}
	if p == nil {
		access := "public"
		if strings.HasPrefix(name, "@") {
			access = "restricted" // npm 对带 scope 的新包默认 restricted
		}
		if doc.Access != nil && *doc.Access != "" {
			access = *doc.Access
		}
		p = &pkg{distTags: map[string]string{}, versions: map[string]*version{}, access: access}
		s.pkgs[name] = p
	}
	delete(m, "dist")
	p.versions[ver] = &version{manifest: m, tgz: tgz, integrity: integrity(tgz), shasum: shasum(tgz)}
	maps.Copy(p.distTags, doc.DistTags)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// checkDoc 校验发布文档：名字一致，恰好一个版本和一个附件，附件的长度、sha512 integrity 和 sha1 shasum 与 dist 一致。
// 返回版本号、版本的 manifest、tarball；msg 不为空表示文档无效。
func checkDoc(name string, doc *publishDoc) (ver string, m map[string]any, tgz []byte, msg string) {
	switch {
	case doc.Name != name || (doc.ID != "" && doc.ID != name):
		return "", nil, nil, fmt.Sprintf("文档中的名字 %q 与路径中的 %q 不一致", doc.Name, name)
	case len(doc.Versions) != 1:
		return "", nil, nil, fmt.Sprintf("必须恰好有一个版本，实际 %d 个", len(doc.Versions))
	case len(doc.Attachments) != 1:
		return "", nil, nil, fmt.Sprintf("必须恰好有一个附件，实际 %d 个", len(doc.Attachments))
	case doc.Access != nil && *doc.Access != "" && *doc.Access != "public" && *doc.Access != "restricted":
		return "", nil, nil, fmt.Sprintf("access %q 无效", *doc.Access)
	}
	for v, vm := range doc.Versions {
		ver, m = v, vm
	}
	if m["name"] != name || m["version"] != ver || (m["_id"] != nil && m["_id"] != name+"@"+ver) {
		return "", nil, nil, fmt.Sprintf("versions[%q] 中的 name、version 或 _id 与文档不一致", ver)
	}
	for tag, v := range doc.DistTags {
		if v != ver {
			return "", nil, nil, fmt.Sprintf("dist-tags[%q] = %q 不是要发布的版本 %q", tag, v, ver)
		}
	}
	if len(doc.DistTags) == 0 {
		return "", nil, nil, "缺少 dist-tags"
	}
	for file, a := range doc.Attachments {
		// npm CLI 用完整包名（@scope/name-1.0.0.tgz），也接受不含 scope 的名字
		if file != name+"-"+ver+".tgz" && file != shortName(name)+"-"+ver+".tgz" {
			return "", nil, nil, fmt.Sprintf("附件名 %q 与版本不符", file)
		}
		b, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil || len(b) != a.Length {
			return "", nil, nil, "附件内容无法解码或长度不符"
		}
		tgz = b
	}
	dist, _ := m["dist"].(map[string]any) // 类型不对时为 nil，下面按缺少处理
	if dist == nil || dist["integrity"] != integrity(tgz) || dist["shasum"] != shasum(tgz) {
		return "", nil, nil, "dist.integrity 或 dist.shasum 与附件不符"
	}
	return ver, m, tgz, ""
}

// discard 是丢弃响应的 ResponseWriter（注入"处理后返回错误"时使用）。
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // 测试服务器，写失败无妨
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
