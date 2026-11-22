package fakenpm_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakenpm"
)

const (
	token = "fake-npm-token"
	name  = "@acme/tool"
)

// doc 按 npm publish 的格式生成发布文档；edit 可以在编码前修改。
func doc(ver string, tgz []byte, edit func(d map[string]any)) []byte {
	s512 := sha512.Sum512(tgz)
	s1 := sha1.Sum(tgz)
	d := map[string]any{
		"_id": name, "name": name, "access": "public",
		"dist-tags": map[string]any{"latest": ver},
		"versions": map[string]any{ver: map[string]any{
			"_id": name + "@" + ver, "name": name, "version": ver, "bin": map[string]any{"tool": "tool"},
			"dist": map[string]any{
				"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
				"shasum":    hex.EncodeToString(s1[:]),
			},
		}},
		"_attachments": map[string]any{name + "-" + ver + ".tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(tgz),
			"length":       len(tgz),
		}},
	}
	if edit != nil {
		edit(d)
	}
	b, _ := json.Marshal(d)
	return b
}

// sub 沿 keys 取出嵌套的 JSON 对象（文档由 doc 生成，结构确定，类型不对时 panic）。
func sub(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			panic("不是 JSON 对象：" + k)
		}
		m = next
	}
	return m
}

func send(t *testing.T, method, url, tok string, body []byte) (int, string) {
	t.Helper()
	return sendCtx(t.Context(), t, method, url, tok, body)
}

func sendCtx(ctx context.Context, t *testing.T, method, url, tok string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServer_publishGetDownload(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	if !strings.HasSuffix(s.URL, "/") {
		t.Fatalf("URL = %q，应以 / 结尾", s.URL)
	}
	if code, body := send(t, http.MethodGet, s.URL+"@acme%2ftool", "", nil); code != http.StatusNotFound || !strings.Contains(body, "Not found") {
		t.Fatalf("不存在时 GET = %d %s", code, body)
	}
	tgz := []byte("tarball")
	if code, body := send(t, http.MethodPut, s.URL+"@acme%2ftool", token, doc("1.0.0", tgz, nil)); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	// 两种路径写法都可以
	for _, p := range []string{"@acme%2ftool", "@acme%2Ftool", "@acme/tool"} {
		code, body := send(t, http.MethodGet, s.URL+p, "", nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, code, body)
		}
		var pk struct {
			Name     string            `json:"name"`
			DistTags map[string]string `json:"dist-tags"`
			Versions map[string]struct {
				Bin  map[string]string `json:"bin"`
				Dist struct{ Integrity, Shasum, Tarball string }
			} `json:"versions"`
		}
		if err := json.Unmarshal([]byte(body), &pk); err != nil {
			t.Fatal(err)
		}
		v := pk.Versions["1.0.0"]
		if pk.Name != name || pk.DistTags["latest"] != "1.0.0" || v.Bin["tool"] != "tool" || v.Dist.Tarball != s.URL+"@acme/tool/-/tool-1.0.0.tgz" {
			t.Fatalf("packument = %s", body)
		}
		code, got := send(t, http.MethodGet, v.Dist.Tarball, "", nil)
		if code != http.StatusOK || got != string(tgz) {
			t.Fatalf("下载 = %d %q", code, got)
		}
	}
	if code, _ := send(t, http.MethodGet, s.URL+"@acme/tool/-/tool-9.9.9.tgz", "", nil); code != http.StatusNotFound {
		t.Fatalf("下载不存在的版本 = %d", code)
	}
	if !bytes.Equal(s.Tarball(name, "1.0.0"), tgz) || s.Access(name) != "public" || s.DistTags(name)["latest"] != "1.0.0" {
		t.Fatal("测试辅助方法的结果不对")
	}
	if pk := s.Packument(name); pk == nil || pk["name"] != name {
		t.Fatalf("Packument = %v", pk)
	}
	if got := s.Versions(name); len(got) != 1 || got[0] != "1.0.0" {
		t.Fatalf("Versions = %v", got)
	}
	if len(s.PublishDocs()) != 1 {
		t.Fatalf("PublishDocs = %d", len(s.PublishDocs()))
	}
	// 下载不记录 Authorization
	if n, m := len(s.AuthorizationHeaders()), len(s.Requests()); n >= m {
		t.Fatalf("Authorization %d 个，请求 %d 个", n, m)
	}
}

func TestServer_publishRejects(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	tgz := []byte("tarball")
	u := s.URL + "@acme%2ftool"
	for _, tc := range []struct {
		desc string
		tok  string
		body []byte
		code int
	}{
		{"token 错误", "wrong", doc("1.0.0", tgz, nil), http.StatusUnauthorized},
		{"没有 token", "", doc("1.0.0", tgz, nil), http.StatusUnauthorized},
		{"无法解析", token, []byte("{"), http.StatusBadRequest},
		{"名字不一致", token, doc("1.0.0", tgz, func(d map[string]any) { d["name"] = "@acme/other" }), http.StatusBadRequest},
		{"integrity 不符", token, doc("1.0.0", tgz, func(d map[string]any) {
			sub(d, "versions", "1.0.0", "dist")["integrity"] = "sha512-xx"
		}), http.StatusBadRequest},
		{"shasum 不符", token, doc("1.0.0", tgz, func(d map[string]any) {
			sub(d, "versions", "1.0.0", "dist")["shasum"] = "00"
		}), http.StatusBadRequest},
		{"长度不符", token, doc("1.0.0", tgz, func(d map[string]any) {
			sub(d, "_attachments", name+"-1.0.0.tgz")["length"] = 1
		}), http.StatusBadRequest},
		{"两个版本", token, doc("1.0.0", tgz, func(d map[string]any) {
			sub(d, "versions")["1.0.1"] = map[string]any{}
		}), http.StatusBadRequest},
		{"没有附件", token, doc("1.0.0", tgz, func(d map[string]any) { d["_attachments"] = map[string]any{} }), http.StatusBadRequest},
		{"dist-tag 指向别的版本", token, doc("1.0.0", tgz, func(d map[string]any) { d["dist-tags"] = map[string]any{"latest": "0.9.0"} }), http.StatusBadRequest},
		{"access 无效", token, doc("1.0.0", tgz, func(d map[string]any) { d["access"] = "private" }), http.StatusBadRequest},
	} {
		if code, body := send(t, http.MethodPut, u, tc.tok, tc.body); code != tc.code {
			t.Errorf("%s：PUT = %d %s，期望 %d", tc.desc, code, body, tc.code)
		}
	}
	if s.Packument(name) != nil {
		t.Fatal("被拒绝的发布不应保存")
	}

	// 不含 scope 的附件名也接受；同一版本再发布 403
	short := doc("1.0.0", tgz, func(d map[string]any) {
		atts := sub(d, "_attachments")
		atts["tool-1.0.0.tgz"] = atts[name+"-1.0.0.tgz"]
		delete(atts, name+"-1.0.0.tgz")
	})
	if code, body := send(t, http.MethodPut, u, token, short); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	code, body := send(t, http.MethodPut, u, token, doc("1.0.0", []byte("other"), nil))
	if code != http.StatusForbidden || !strings.Contains(body, `cannot publish over previously published version`) {
		t.Fatalf("重复发布 = %d %s", code, body)
	}
}

func TestServer_restrictedAndPreset(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	// 带 scope 的新包不写 access 时默认 restricted
	body := doc("1.0.0", []byte("a"), func(d map[string]any) { delete(d, "access") })
	if code, b := send(t, http.MethodPut, s.URL+"@acme%2ftool", token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, b)
	}
	if s.Access(name) != "restricted" {
		t.Fatalf("access = %q", s.Access(name))
	}
	if code, _ := send(t, http.MethodGet, s.URL+"@acme%2ftool", "", nil); code != http.StatusNotFound {
		t.Fatalf("没有 token 时 GET = %d，期望 404", code)
	}
	if code, _ := send(t, http.MethodGet, s.URL+"@acme%2ftool", "wrong", nil); code != http.StatusNotFound {
		t.Fatalf("token 错误时 GET = %d，期望 404", code)
	}
	if code, _ := send(t, http.MethodGet, s.URL+"@acme%2ftool", token, nil); code != http.StatusOK {
		t.Fatalf("有 token 时 GET = %d", code)
	}

	s.Preset("@acme/other", "2.0.0", []byte("x"), map[string]any{"description": "d", "dist": "ignored"})
	pk := s.Packument("@acme/other")
	v, _ := pk["versions"].(map[string]any)["2.0.0"].(map[string]any)
	dist, _ := v["dist"].(map[string]any)
	if v["description"] != "d" || dist["integrity"] == nil || s.Access("@acme/other") != "public" || s.DistTags("@acme/other")["latest"] != "2.0.0" {
		t.Fatalf("Preset 后 packument = %v", pk)
	}
	s.SetAccess("@acme/other", "restricted")
	if code, _ := send(t, http.MethodGet, s.URL+"@acme%2fother", "", nil); code != http.StatusNotFound {
		t.Fatalf("SetAccess 后没有 token 时 GET = %d", code)
	}
}

func TestServer_faults(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	u := s.URL + "@acme%2ftool"

	s.FailNext(http.MethodPut, http.StatusServiceUnavailable, 1)
	if code, _ := send(t, http.MethodPut, u, token, doc("1.0.0", []byte("a"), nil)); code != http.StatusServiceUnavailable {
		t.Fatalf("FailNext：PUT = %d", code)
	}
	if s.Packument(name) != nil {
		t.Fatal("FailNext 不应处理请求")
	}

	s.FailAfterApply(http.MethodPut, http.StatusBadGateway, 1)
	if code, _ := send(t, http.MethodPut, u, token, doc("1.0.0", []byte("a"), nil)); code != http.StatusBadGateway {
		t.Fatalf("FailAfterApply：PUT = %d", code)
	}
	if s.Tarball(name, "1.0.0") == nil {
		t.Fatal("FailAfterApply 应照常处理请求")
	}

	s.HangNext(http.MethodPut)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if code, _ := sendCtx(ctx, t, http.MethodPut, u, token, doc("1.1.0", []byte("b"), nil)); code != 0 {
		t.Fatalf("HangNext：PUT 应超时，实际 %d", code)
	}
	if s.Tarball(name, "1.1.0") == nil {
		t.Fatal("HangNext 应照常处理请求")
	}
	// 故障用完后恢复正常
	if code, _ := send(t, http.MethodGet, u, token, nil); code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
}

// HangNext 挂起的请求在 Close 时返回，Close 不会卡住。
func TestServer_closeReleasesHang(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	s.HangNext("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(s.URL + "@acme%2ftool")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	for len(s.Requests()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	s.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 后挂起的请求没有返回")
	}
}
