// Package ocitest 提供测试用的内存 registry（OCI Distribution 的推送和查询部分，支持 Basic / Bearer 认证）
// 和 OCI image layout 生成器，供 internal/oci、internal/server/ci/publish 等包的单元测试使用。
package ocitest

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// 支持的认证方式。
const (
	AuthNone   = ""
	AuthBasic  = "basic"
	AuthBearer = "bearer"
)

// Registry 是内存中的 registry。
type Registry struct {
	// URL 是 http://127.0.0.1:<port>；Host 是 127.0.0.1:<port>，用作镜像名的 registry 部分。
	URL  string
	Host string

	srv      *httptest.Server
	user     string
	pass     string
	auth     string
	mu       sync.Mutex
	blobs    map[string][]byte
	manifest map[string]map[string]stored // 仓库 → ref（tag 或 digest）→ manifest
	uploads  map[string]string            // 上传 ID → 仓库
	tokens   map[string]bool
	failNext int
	requests []string
	authSeen []string
}

type stored struct {
	mediaType string
	body      []byte
}

// New 启动 registry。auth 为 AuthBasic 或 AuthBearer 时要求用 user / pass 认证。
func New(auth, user, pass string) *Registry {
	r := &Registry{
		user: user, pass: pass, auth: auth,
		blobs: map[string][]byte{}, manifest: map[string]map[string]stored{}, uploads: map[string]string{}, tokens: map[string]bool{},
	}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	r.URL = r.srv.URL
	r.Host = strings.TrimPrefix(r.srv.URL, "http://")
	return r
}

// Close 关闭 registry。
func (r *Registry) Close() { r.srv.Close() }

// FailNext 让接下来的 n 个 /v2/ 请求返回 503（用于测试重试）。
func (r *Registry) FailNext(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext = n
}

// Requests 返回收到的请求（"METHOD 路径"），不含 token 请求。
func (r *Registry) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// AuthorizationHeaders 返回收到的全部 Authorization 头（用于检查凭据没有发到别处）。
func (r *Registry) AuthorizationHeaders() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.authSeen...)
}

// Tag 返回仓库中 tag 指向的 manifest digest（不存在时为空）。
func (r *Registry) Tag(repo, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.manifest[repo][tag]
	if !ok {
		return ""
	}
	return digest(m.body)
}

// SetTag 直接把 tag 指向一个内容任意的 manifest，返回它的 digest（用于测试 tag 冲突）。
func (r *Registry) SetTag(repo, tag string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manifest[repo] == nil {
		r.manifest[repo] = map[string]stored{}
	}
	s := stored{mediaType: "application/vnd.oci.image.manifest.v1+json", body: body}
	r.manifest[repo][tag] = s
	r.manifest[repo][digest(body)] = s
	return digest(body)
}

// Manifest 返回 ref 对应的 manifest 内容和类型。
func (r *Registry) Manifest(repo, ref string) (mediaType string, body []byte, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.manifest[repo][ref]
	return m.mediaType, m.body, ok
}

// Blob 返回 blob 内容。
func (r *Registry) Blob(d string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blobs[d]
	return b, ok
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}}) // 测试用，写失败无妨
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		r.serveToken(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	if h := req.Header.Get("Authorization"); h != "" {
		r.authSeen = append(r.authSeen, h)
	}
	if r.failNext > 0 {
		r.failNext--
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}
	if !r.authorized(req) {
		if r.auth == AuthBearer {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ocitest"`, r.URL))
		} else {
			w.Header().Set("WWW-Authenticate", `Basic realm="ocitest"`)
		}
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	p := strings.TrimPrefix(req.URL.Path, "/v2/")
	switch {
	case p == "" || p == "/":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(p, "/blobs/uploads/"):
		r.serveUpload(w, req, p)
	case strings.Contains(p, "/blobs/"):
		repo, d, _ := strings.Cut(p, "/blobs/")
		r.serveBlob(w, req, repo, d)
	case strings.Contains(p, "/manifests/"):
		repo, ref, _ := strings.CutLast(p, "/manifests/")
		r.serveManifest(w, req, repo, ref)
	default:
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "not found")
	}
}

func (r *Registry) authorized(req *http.Request) bool {
	h := req.Header.Get("Authorization")
	switch r.auth {
	case AuthBasic:
		u, p, ok := req.BasicAuth()
		return ok && u == r.user && p == r.pass
	case AuthBearer:
		t, ok := strings.CutPrefix(h, "Bearer ")
		return ok && r.tokens[t]
	}
	return true
}

func (r *Registry) serveToken(w http.ResponseWriter, req *http.Request) {
	u, p, ok := req.BasicAuth()
	if !ok || u != r.user || p != r.pass {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"details":"incorrect username or password"}`) // 测试用
		return
	}
	if !strings.Contains(req.URL.Query().Get("scope"), "push") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand 不会失败
	tok := base64.RawURLEncoding.EncodeToString(b)
	r.mu.Lock()
	r.tokens[tok] = true
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok}) // 测试用
}

func (r *Registry) serveUpload(w http.ResponseWriter, req *http.Request, p string) {
	repo, id, _ := strings.Cut(p, "/blobs/uploads/")
	switch req.Method {
	case http.MethodPost:
		b := make([]byte, 8)
		_, _ = rand.Read(b) // crypto/rand 不会失败
		id := hex.EncodeToString(b)
		r.uploads[id] = repo
		w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id+"?_state=x")
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		if r.uploads[id] != repo {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
			return
		}
		delete(r.uploads, id)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		d := req.URL.Query().Get("digest")
		if digest(body) != d {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
			return
		}
		r.blobs[d] = body
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, _, d string) {
	b, ok := r.blobs[d]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Docker-Content-Digest", d)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if req.Method == http.MethodGet {
		_, _ = w.Write(b) // 测试用
	}
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	switch req.Method {
	case http.MethodHead, http.MethodGet:
		m, ok := r.manifest[repo][ref]
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", digest(m.body))
		if req.Method == http.MethodGet {
			_, _ = w.Write(m.body) // 测试用
		}
	case http.MethodPut:
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		if missing := r.missingRefs(repo, body); missing != "" {
			writeError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", missing)
			return
		}
		d := digest(body)
		if strings.HasPrefix(ref, "sha256:") && ref != d {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
			return
		}
		if r.manifest[repo] == nil {
			r.manifest[repo] = map[string]stored{}
		}
		s := stored{mediaType: req.Header.Get("Content-Type"), body: body}
		r.manifest[repo][ref] = s
		r.manifest[repo][d] = s
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// missingRefs 检查 manifest 引用的 blob、index 引用的 manifest 都已存在。
func (r *Registry) missingRefs(repo string, body []byte) string {
	var m struct {
		Config    *struct{ Digest string }  `json:"config"`
		Layers    []struct{ Digest string } `json:"layers"`
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "invalid json"
	}
	if m.Config != nil {
		if _, ok := r.blobs[m.Config.Digest]; !ok {
			return m.Config.Digest
		}
	}
	for _, l := range m.Layers {
		if _, ok := r.blobs[l.Digest]; !ok {
			return l.Digest
		}
	}
	for _, c := range m.Manifests {
		if _, ok := r.manifest[repo][c.Digest]; !ok {
			return c.Digest
		}
	}
	return ""
}

// Layout 是生成的 OCI image layout：相对路径 → 内容。
type Layout struct {
	Files map[string][]byte
	// Root 是 index.json 中唯一镜像（单平台 manifest）的 digest。
	Root string
	// Config 和 Layer 是 config、layer blob 的 digest。
	Config, Layer string
}

// NewLayout 生成一个单平台镜像的 layout，镜像里只有一个文件 /hello，内容为 content。
func NewLayout(goos, goarch, content string) *Layout {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "hello", Mode: 0o644, Size: int64(len(content))}) // 写入内存，不会失败
	_, _ = io.WriteString(tw, content)
	_ = tw.Close()
	layer := tb.Bytes()
	cfg := mustJSON(map[string]any{
		"architecture": goarch, "os": goos,
		"config": map[string]any{"Cmd": []string{"cat", "/hello"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{digest(layer)}},
	})
	mf := mustJSON(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": desc("application/vnd.oci.image.config.v1+json", cfg),
		"layers": []any{desc("application/vnd.oci.image.layer.v1.tar", layer)},
	})
	index := mustJSON(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{desc("application/vnd.oci.image.manifest.v1+json", mf)},
	})
	l := &Layout{Files: map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": index,
	}, Root: digest(mf), Config: digest(cfg), Layer: digest(layer)}
	for _, b := range [][]byte{layer, cfg, mf} {
		l.Files["blobs/sha256/"+strings.TrimPrefix(digest(b), "sha256:")] = b
	}
	return l
}

// Write 把 layout 写到目录 dir 下。
func (l *Layout) Write(dir string) error {
	for p, b := range l.Files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func desc(mt string, b []byte) map[string]any {
	return map[string]any{"mediaType": mt, "digest": digest(b), "size": len(b)}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
