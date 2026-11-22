package ocitest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/oci/ocitest"
)

// ocitest 是 internal/oci、internal/server/ci/publish 等包的测试替身；这里只测它被依赖的约束：
// 校验上传内容的 digest、manifest 引用的内容必须已存在、记录请求和凭据，以及 layout 写盘。

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// result 是读完并关闭响应体后的响应。
type result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func do(t *testing.T, method, url string, body []byte, header ...string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Body: b}
}

// upload 按 POST + PUT 上传 blob，返回 PUT 的状态码。
func upload(t *testing.T, r *ocitest.Registry, repo string, body []byte, digest string) int {
	t.Helper()
	resp := do(t, http.MethodPost, r.URL+"/v2/"+repo+"/blobs/uploads/", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST 状态码 %d", resp.StatusCode)
	}
	return do(t, http.MethodPut, r.URL+resp.Header.Get("Location")+"&digest="+digest, body).StatusCode
}

func TestRegistryUploadAndManifest(t *testing.T) {
	r := ocitest.New(ocitest.AuthNone, "", "")
	t.Cleanup(r.Close)
	layer := []byte("layer")

	if s := upload(t, r, "a/b", layer, sha([]byte("other"))); s != http.StatusBadRequest {
		t.Fatalf("digest 不一致时状态码 = %d，期望 400", s)
	}
	if _, ok := r.Blob(sha(layer)); ok {
		t.Fatal("digest 不一致的内容不应保存")
	}
	if s := upload(t, r, "a/b", layer, sha(layer)); s != http.StatusCreated {
		t.Fatalf("上传状态码 = %d", s)
	}
	if got, ok := r.Blob(sha(layer)); !ok || !bytes.Equal(got, layer) {
		t.Fatalf("Blob = %q, %v", got, ok)
	}

	// 未知的上传 ID
	if s := do(t, http.MethodPut, r.URL+"/v2/a/b/blobs/uploads/nope?digest="+sha(layer), layer).StatusCode; s != http.StatusNotFound {
		t.Fatalf("未知上传 ID 的状态码 = %d，期望 404", s)
	}

	// manifest 引用的 blob 不存在时拒绝
	missing := []byte(`{"schemaVersion":2,"config":{"digest":"` + sha([]byte("cfg")) + `"},"layers":[]}`)
	resp := do(t, http.MethodPut, r.URL+"/v2/a/b/manifests/v1", missing, "Content-Type", "application/vnd.oci.image.manifest.v1+json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("引用不存在的 config 时状态码 = %d，期望 400", resp.StatusCode)
	}
	// index 引用的 manifest 不存在时拒绝
	idx := []byte(`{"schemaVersion":2,"manifests":[{"digest":"` + sha([]byte("m")) + `"}]}`)
	if s := do(t, http.MethodPut, r.URL+"/v2/a/b/manifests/v1", idx).StatusCode; s != http.StatusBadRequest {
		t.Fatalf("引用不存在的 manifest 时状态码 = %d，期望 400", s)
	}
	// 按 digest 推送时内容必须匹配
	ok := []byte(`{"schemaVersion":2,"layers":[{"digest":"` + sha(layer) + `"}]}`)
	if s := do(t, http.MethodPut, r.URL+"/v2/a/b/manifests/"+sha([]byte("x")), ok).StatusCode; s != http.StatusBadRequest {
		t.Fatalf("digest 不一致时状态码 = %d，期望 400", s)
	}
	resp = do(t, http.MethodPut, r.URL+"/v2/a/b/manifests/v1", ok, "Content-Type", "application/vnd.oci.image.manifest.v1+json")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != sha(ok) {
		t.Fatalf("推送 manifest：%d %s", resp.StatusCode, resp.Header.Get("Docker-Content-Digest"))
	}
	if r.Tag("a/b", "v1") != sha(ok) {
		t.Fatalf("Tag = %s", r.Tag("a/b", "v1"))
	}
	// GET 返回内容和类型
	resp = do(t, http.MethodGet, r.URL+"/v2/a/b/manifests/v1", nil)
	if !bytes.Equal(resp.Body, ok) || resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("GET manifest = %q %s", resp.Body, resp.Header.Get("Content-Type"))
	}

	want := []string{"POST /v2/a/b/blobs/uploads/", "PUT /v2/a/b/blobs/uploads/"}
	if got := r.Requests(); len(got) < 2 || got[0] != want[0] || !strings.HasPrefix(got[1], want[1]) {
		t.Fatalf("Requests = %v", got)
	}
	if h := r.AuthorizationHeaders(); len(h) != 0 {
		t.Fatalf("没有发送凭据，AuthorizationHeaders = %v", h)
	}
}

func TestRegistryAuth(t *testing.T) {
	r := ocitest.New(ocitest.AuthBearer, "u", "p")
	t.Cleanup(r.Close)

	resp := do(t, http.MethodGet, r.URL+"/v2/", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), r.URL+"/token") {
		t.Fatalf("未认证：%d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// token 必须申请 push 权限
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if s := do(t, http.MethodGet, r.URL+"/token?scope=repository:a/b:pull", nil, "Authorization", basic).StatusCode; s != http.StatusBadRequest {
		t.Fatalf("没有 push 权限的 token 请求状态码 = %d，期望 400", s)
	}
	// 密码错误时不签发 token
	wrong := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:x"))
	if s := do(t, http.MethodGet, r.URL+"/token?scope=repository:a/b:pull,push", nil, "Authorization", wrong).StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("密码错误时 token 请求状态码 = %d，期望 401", s)
	}
	// 伪造的 token 不被接受，并被记录
	if s := do(t, http.MethodGet, r.URL+"/v2/", nil, "Authorization", "Bearer forged").StatusCode; s != http.StatusUnauthorized {
		t.Fatalf("伪造 token 的状态码 = %d", s)
	}
	if h := r.AuthorizationHeaders(); !slices.Equal(h, []string{"Bearer forged"}) {
		t.Fatalf("AuthorizationHeaders = %v", h)
	}
}

func TestLayoutWrite(t *testing.T) {
	l := ocitest.NewLayout("linux", "amd64", "hi")
	dir := t.TempDir()
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}
	for p, want := range l.Files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s：%v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(l.Root, "sha256:"))); err != nil {
		t.Fatalf("缺少 manifest：%v", err)
	}
	// 目标是文件时报错
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(f); err == nil {
		t.Fatal("目标不是目录时应报错")
	}
}
