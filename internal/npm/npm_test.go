package npm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakenpm"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
)

const (
	token = "npm_s3cr3t-token-value"
	name  = "@acme/kilnflow-darwin-arm64"
)

func newClient(t *testing.T, registry, tok string) *npm.Client {
	t.Helper()
	c, err := npm.NewClient(registry, tok, "kiln/test", npm.WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func publishReq(ver string, tgz []byte) *npm.PublishRequest {
	return &npm.PublishRequest{
		Name: name, Version: ver, Tarball: tgz, DistTag: "latest", Access: npm.AccessPublic,
		Manifest: map[string]any{"name": name, "description": "Kiln runner", "os": []any{"darwin"}, "cpu": []any{"arm64"}},
	}
}

// ValidShortName 与 ValidName 的短名规则一致（workflow 的 release.npm[].name 共用它）。
func TestValidShortName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"kiln", true},
		{"kiln.js", true},
		{"a-b_c.d", true},
		{"", false},
		{"Kiln", false},
		{"a~b", false},
		{".kiln", false},
		{"_kiln", false},
		{"node_modules", false},
		{"favicon.ico", false},
		{"@scope/name", false},
		{"a/b", false},
		{strings.Repeat("a", npm.MaxNameLength), true},
		{strings.Repeat("a", npm.MaxNameLength+1), false},
	} {
		if err := npm.ValidShortName(tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidShortName(%q) = %v，期望 ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"@acme/kilnflow-darwin-arm64", true},
		{"@a/b", true},
		{"@a-b.c_d/e.f-g_h", true},
		{"kiln", true},
		{"kiln.js", true},
		{"", false},
		{"@acme", false},
		{"@acme/", false},
		{"@/kiln", false},
		{"@acme/Kiln", false},
		{"@Acme/kiln", false},
		{"@acme/kilnflow runner", false},
		{"@acme/kilnflow~x", false},
		{"@acme/kilnflow!", false},
		{"@acme/a/b", false},
		{"@acme/.kiln", false},
		{"@acme/_kiln", false},
		{"@.scope/kiln", false},
		{".kiln", false},
		{"_kiln", false},
		{"node_modules", false},
		{"favicon.ico", false},
		{"kiln%2f", false},
		{"中文", false},
		{"@a/" + strings.Repeat("x", 211), true}, // 214
		{"@a/" + strings.Repeat("x", 212), false},
	} {
		if err := npm.ValidName(tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidName(%q) = %v，期望 ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		ok   bool
		norm string
	}{
		{"@acme", true, "@acme"},
		{"acme", false, "@acme"},
		{"@Acme", false, ""},
		{"@", false, ""},
		{"", false, ""},
		{"@a/b", false, ""},
		{"@_a", false, ""},
	} {
		if err := npm.ValidScope(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidScope(%q) = %v，期望 ok=%v", tc.in, err, tc.ok)
		}
		got, err := npm.NormalizeScope(tc.in)
		if got != tc.norm || (err == nil) != (tc.norm != "") {
			t.Errorf("NormalizeScope(%q) = %q, %v，期望 %q", tc.in, got, err, tc.norm)
		}
	}
	if got := npm.FullName("acme", "kiln"); got != "@acme/kiln" {
		t.Errorf("FullName = %q", got)
	}
	if got := npm.FullName("@acme", "kiln"); got != "@acme/kiln" {
		t.Errorf("FullName = %q", got)
	}
	if got := npm.ShortName(name); got != "kilnflow-darwin-arm64" {
		t.Errorf("ShortName = %q", got)
	}
}

func TestCheckRegistry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{npm.DefaultRegistry, true},
		{"https://npm.pkg.github.com", true},
		{"https://example.com/api/npm/repo/", true},
		{"http://127.0.0.1:4873/", true},
		{"http://localhost:4873", true},
		{"http://[::1]:4873/", true},
		{"http://registry.npmjs.org/", false},
		{"ftp://example.com/", false},
		{"https://user:pass@example.com/", false},
		{"https://example.com/?x=1", false},
		{"https://example.com/#f", false},
		{"https:///npm", false},
		{"registry.npmjs.org", false},
	} {
		if err := npm.CheckRegistry(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckRegistry(%q) = %v，期望 ok=%v", tc.url, err, tc.ok)
		}
	}
}

func TestIntegrityAndShasum(t *testing.T) {
	t.Parallel()
	// 与 `printf hello | openssl dgst -sha512 -binary | base64`、`printf hello | shasum` 一致
	if got := npm.Integrity([]byte("hello")); got != "sha512-m3HSJL1i83hdltRq0+o9czGb+8KJDKra4t/3JRlnPKcjI8PZm6XBHXx6zG4UuMXaDEZjR1wuXDre9G9zvN7AQw==" {
		t.Errorf("Integrity = %q", got)
	}
	if got := npm.Shasum([]byte("hello")); got != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
		t.Errorf("Shasum = %q", got)
	}
}

// 首次查询 404 → 发布 → 查询得到一致的 integrity；请求路径和发布文档与 npm publish 相同。
func TestClient_publishAndGet(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, strings.TrimSuffix(s.URL, "/"), token) // 结尾的 / 补上
	ctx := t.Context()
	tgz := []byte("fake-tgz-bytes")

	if p, err := c.Get(ctx, name); err != nil || p != nil {
		t.Fatalf("还没有发布时 Get = %+v, %v，期望 nil, nil", p, err)
	}
	if err := c.Publish(ctx, publishReq("1.2.3", tgz)); err != nil {
		t.Fatal(err)
	}
	p, err := c.Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := p.Versions["1.2.3"]
	if !ok || p.Name != name || p.DistTags["latest"] != "1.2.3" || v.Version != "1.2.3" || v.Name != name {
		t.Fatalf("Get = %+v", p)
	}
	if v.Dist.Integrity != npm.Integrity(tgz) || v.Dist.Shasum != npm.Shasum(tgz) {
		t.Fatalf("dist = %+v", v.Dist)
	}
	if want := c.TarballURL(name, "1.2.3"); v.Dist.Tarball != want || want != s.URL+name+"/-/kilnflow-darwin-arm64-1.2.3.tgz" {
		t.Fatalf("tarball = %q，TarballURL = %q", v.Dist.Tarball, want)
	}
	if !bytes.Equal(s.Tarball(name, "1.2.3"), tgz) || s.Access(name) != npm.AccessPublic {
		t.Fatalf("服务端 tarball %q，access %q", s.Tarball(name, "1.2.3"), s.Access(name))
	}
	reqs := s.Requests()
	for _, want := range []string{"GET /@acme%2fkilnflow-darwin-arm64", "PUT /@acme%2fkilnflow-darwin-arm64"} {
		if !slices.Contains(reqs, want) {
			t.Fatalf("请求 %v 中没有 %q", reqs, want)
		}
	}
	for _, h := range s.AuthorizationHeaders() {
		if h != "Bearer "+token {
			t.Fatalf("Authorization = %q", h)
		}
	}
	if uas := s.UserAgents(); len(uas) == 0 || uas[0] != "kiln/test" {
		t.Fatalf("User-Agent = %v", uas)
	}
	checkPublishDoc(t, s.PublishDocs(), tgz)
}

// checkPublishDoc 检查发布文档与 npm publish 相同。
func checkPublishDoc(t *testing.T, docs []map[string]any, tgz []byte) {
	t.Helper()
	if len(docs) != 1 {
		t.Fatalf("收到 %d 个发布文档", len(docs))
	}
	doc := docs[0]
	if doc["_id"] != name || doc["name"] != name || doc["description"] != "Kiln runner" || doc["access"] != "public" {
		t.Fatalf("文档 = %v", doc)
	}
	ver, _ := doc["versions"].(map[string]any)["1.2.3"].(map[string]any)
	if ver["_id"] != name+"@1.2.3" || ver["version"] != "1.2.3" || ver["description"] != "Kiln runner" || ver["os"] == nil {
		t.Fatalf("versions[1.2.3] = %v", ver)
	}
	att, _ := doc["_attachments"].(map[string]any)[name+"-1.2.3.tgz"].(map[string]any)
	if att["content_type"] != "application/octet-stream" || att["length"] != float64(len(tgz)) {
		t.Fatalf("_attachments = %v", doc["_attachments"])
	}
}

// 同一版本再次发布得到 403（版本不可覆盖），dist-tag 合并，Publish 不改动调用方的 Manifest。
func TestClient_publishExistingAndDistTags(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	ctx := t.Context()
	req := publishReq("1.0.0", []byte("a"))
	if err := c.Publish(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Manifest["dist"]; ok {
		t.Fatal("Publish 修改了调用方的 Manifest")
	}
	err := c.Publish(ctx, req)
	if !npm.IsStatus(err, http.StatusForbidden) || npm.Uncertain(err) || !strings.Contains(err.Error(), "previously published") {
		t.Fatalf("重复发布 = %v，期望 403", err)
	}
	next := publishReq("1.1.0-rc.1", []byte("b"))
	next.DistTag = "next"
	if err := c.Publish(ctx, next); err != nil {
		t.Fatal(err)
	}
	if tags := s.DistTags(name); tags["latest"] != "1.0.0" || tags["next"] != "1.1.0-rc.1" {
		t.Fatalf("dist-tags = %v", tags)
	}
}

// 预置的版本：Get 得到它的 integrity，调用方据此判断"已完成"或"不一致"。
func TestClient_getPreset(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	s.Preset(name, "2.0.0", []byte("old"), map[string]any{"description": "x"})
	p, err := newClient(t, s.URL, "").Get(t.Context(), name) // 公开的包不需要 token
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Versions["2.0.0"].Dist.Integrity; got != npm.Integrity([]byte("old")) || got == npm.Integrity([]byte("new")) {
		t.Fatalf("integrity = %q", got)
	}
	if h := s.AuthorizationHeaders(); len(h) != 1 || h[0] != "" {
		t.Fatalf("token 为空时不应带 Authorization：%q", h)
	}
}

// restricted 的包：没有 token 时 404（Get 返回 nil），有 token 时可以查询。
func TestClient_restricted(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	req := publishReq("1.0.0", []byte("a"))
	req.Access = npm.AccessRestricted
	if err := newClient(t, s.URL, token).Publish(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if s.Access(name) != npm.AccessRestricted {
		t.Fatalf("access = %q", s.Access(name))
	}
	if p, err := newClient(t, s.URL, "").Get(t.Context(), name); err != nil || p != nil {
		t.Fatalf("没有 token 时 Get = %+v, %v", p, err)
	}
	if p, err := newClient(t, s.URL, token).Get(t.Context(), name); err != nil || p == nil {
		t.Fatalf("有 token 时 Get = %+v, %v", p, err)
	}
}

// Public 不带 token 查询：公开的包为 true；restricted、不存在、要求认证的 registry 为 false；其他错误照常返回。
func TestClient_public(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	check := func(desc string, want bool) {
		t.Helper()
		if got, err := c.Public(t.Context(), name); err != nil || got != want {
			t.Fatalf("%s：Public = %v, %v，期望 %v", desc, got, err, want)
		}
	}
	check("不存在", false)
	s.Preset(name, "1.0.0", []byte("a"), nil)
	check("公开", true)
	s.SetAccess(name, npm.AccessRestricted)
	check("restricted", false)
	if auth := s.AuthorizationHeaders(); slices.ContainsFunc(auth, func(h string) bool { return h != "" }) {
		t.Errorf("Public 不应带 token：%q", auth)
	}

	for status, wantErr := range map[int]bool{http.StatusUnauthorized: false, http.StatusForbidden: false, http.StatusInternalServerError: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		got, err := newClient(t, srv.URL, token).Public(t.Context(), name)
		srv.Close()
		if got || (err != nil) != wantErr {
			t.Errorf("HTTP %d：Public = %v, %v", status, got, err)
		}
	}
}

func TestClient_publishValidation(t *testing.T) {
	t.Parallel()
	s := fakenpm.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	for _, tc := range []struct {
		desc string
		edit func(*npm.PublishRequest)
	}{
		{"包名无效", func(r *npm.PublishRequest) { r.Name = "@x/Kiln" }},
		{"版本为空", func(r *npm.PublishRequest) { r.Version = "" }},
		{"dist-tag 为空", func(r *npm.PublishRequest) { r.DistTag = "" }},
		{"access 无效", func(r *npm.PublishRequest) { r.Access = "private" }},
		{"tarball 为空", func(r *npm.PublishRequest) { r.Tarball = nil }},
		{"manifest 名字不一致", func(r *npm.PublishRequest) { r.Manifest["name"] = "@x/other" }},
		{"manifest 版本不一致", func(r *npm.PublishRequest) { r.Manifest["version"] = "9.9.9" }},
		{"private: true", func(r *npm.PublishRequest) { r.Manifest["private"] = true }},
	} {
		req := publishReq("1.0.0", []byte("a"))
		tc.edit(req)
		if err := c.Publish(t.Context(), req); err == nil || npm.Uncertain(err) {
			t.Errorf("%s：Publish = %v，期望在发送前报错", tc.desc, err)
		}
	}
	if err := newClient(t, s.URL, "").Publish(t.Context(), publishReq("1.0.0", []byte("a"))); err == nil {
		t.Error("没有 token 时 Publish 应报错")
	}
	if n := len(s.Requests()); n != 0 {
		t.Fatalf("参数无效时不应发出请求，实际 %d 个", n)
	}
	// private 按 JavaScript 的真值判断（与 npm publish 的 if (manifest.private) 相同）
	for _, v := range []any{"true", "false", 1, int64(1), map[string]any{}, []any{}} {
		if !npm.IsPrivate(map[string]any{"private": v}) {
			t.Errorf("private: %#v 应视为私有", v)
		}
	}
	for i, v := range []any{false, "", 0, nil} {
		req := publishReq(fmt.Sprintf("1.0.%d", i), []byte{byte('a' + i)})
		req.Manifest["private"] = v
		if err := c.Publish(t.Context(), req); err != nil {
			t.Errorf("private: %#v 时应发布：%v", v, err)
		}
	}
	if _, err := c.Get(t.Context(), "Bad Name"); err == nil {
		t.Fatal("Get 包名无效时应报错")
	}
}

func TestClient_errors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("Get 对 429、5xx 重试", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New(token)
		t.Cleanup(s.Close)
		s.Preset(name, "1.0.0", []byte("a"), nil)
		s.FailNext(http.MethodGet, http.StatusBadGateway, 1)
		s.FailNext(http.MethodGet, http.StatusTooManyRequests, 1)
		if p, err := newClient(t, s.URL, token).Get(ctx, name); err != nil || p == nil {
			t.Fatalf("Get = %+v, %v", p, err)
		}
		if n := len(s.Requests()); n != 3 {
			t.Fatalf("请求 %d 次，期望 3", n)
		}
	})

	t.Run("Get 重试次数用完", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New(token)
		t.Cleanup(s.Close)
		s.FailNext("", http.StatusServiceUnavailable, 10)
		_, err := newClient(t, s.URL, token).Get(ctx, name)
		if !npm.IsStatus(err, http.StatusServiceUnavailable) || len(s.Requests()) != 3 {
			t.Fatalf("err = %v，请求 %d 次", err, len(s.Requests()))
		}
	})

	t.Run("Publish 不重试，5xx 是不确定的结果", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New(token)
		t.Cleanup(s.Close)
		s.FailNext(http.MethodPut, http.StatusServiceUnavailable, 1)
		c := newClient(t, s.URL, token)
		err := c.Publish(ctx, publishReq("1.0.0", []byte("a")))
		if !npm.IsStatus(err, http.StatusServiceUnavailable) || !npm.Uncertain(err) || len(s.Requests()) != 1 {
			t.Fatalf("err = %v，请求 %d 次", err, len(s.Requests()))
		}
		if p, err := c.Get(ctx, name); err != nil || p != nil {
			t.Fatalf("注入的故障不应发布：%+v, %v", p, err)
		}
	})

	t.Run("Publish 已生效但返回 502：查询得到一致的版本", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New(token)
		t.Cleanup(s.Close)
		s.FailAfterApply(http.MethodPut, http.StatusBadGateway, 1)
		c := newClient(t, s.URL, token)
		tgz := []byte("tgz")
		if err := c.Publish(ctx, publishReq("1.0.0", tgz)); !npm.Uncertain(err) {
			t.Fatalf("err = %v，期望不确定", err)
		}
		p, err := c.Get(ctx, name)
		if err != nil || p == nil || p.Versions["1.0.0"].Dist.Integrity != npm.Integrity(tgz) {
			t.Fatalf("Get = %+v, %v", p, err)
		}
	})

	t.Run("Publish 已生效但超时", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New(token)
		t.Cleanup(s.Close)
		s.HangNext(http.MethodPut)
		c := newClient(t, s.URL, token)
		tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		err := c.Publish(tctx, publishReq("1.0.0", []byte("a")))
		if err == nil || !npm.Uncertain(err) || strings.Contains(err.Error(), token) {
			t.Fatalf("err = %v，期望不确定且不含 token", err)
		}
		if s.Tarball(name, "1.0.0") == nil {
			t.Fatal("挂起前应已保存")
		}
	})

	t.Run("401 不重试，错误信息不含 token", func(t *testing.T) {
		t.Parallel()
		s := fakenpm.New("other-token")
		t.Cleanup(s.Close)
		err := newClient(t, s.URL, token).Publish(ctx, publishReq("1.0.0", []byte("a")))
		if !npm.IsStatus(err, http.StatusUnauthorized) || npm.Uncertain(err) || len(s.Requests()) != 1 {
			t.Fatalf("err = %v，请求 %d 次", err, len(s.Requests()))
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("错误信息中出现了 token：%v", err)
		}
	})
}

// registry 在错误说明中回显 token、或返回重定向时：token 被替换成 ***，不跟随重定向；401 要求 OTP 时提示换 token。
func TestClient_redactsAndNoRedirect(t *testing.T) {
	t.Parallel()
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/elsewhere/"+name:
			redirected = true
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/moved/"):
			http.Redirect(w, r, "/elsewhere/"+name, http.StatusFound)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden","message":"token `+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+` 没有权限"}`)
		default:
			w.Header().Set("WWW-Authenticate", "OTP")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"This operation requires a one-time password."}`)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(t, srv.URL, token).Get(t.Context(), name)
	if !npm.IsStatus(err, http.StatusForbidden) || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("错误应为 403 且 token 已脱敏：%v", err)
	}
	_, err = newClient(t, srv.URL+"/moved/", token).Get(t.Context(), name)
	if !npm.IsStatus(err, http.StatusFound) || redirected {
		t.Fatalf("不应跟随重定向：%v（跟随了：%v）", err, redirected)
	}
	err = newClient(t, srv.URL, token).Publish(t.Context(), publishReq("1.0.0", []byte("a")))
	if !npm.IsStatus(err, http.StatusUnauthorized) || !strings.Contains(err.Error(), "Automation") {
		t.Fatalf("OTP 错误应提示换 token：%v", err)
	}
}

// token 正好落在截断位置时（错误说明、重定向地址、读取上限）也不能泄露 token 的任何一部分。
func TestClient_redactsTruncatedToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc     string
		location string
		body     string
	}{
		{"错误说明", "", strings.Repeat("x", 190) + token + " 没有权限"},
		{"JSON 错误说明", "", `{"error":"` + strings.Repeat("x", 490) + token + `"}`},
		{"重定向地址", "/x/" + strings.Repeat("x", 190) + token, ""},
		{"读取上限", "", strings.Repeat("x", 4096-8) + token},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			status := http.StatusForbidden
			if tc.location != "" {
				w.Header().Set("Location", tc.location)
				status = http.StatusFound
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := newClient(t, srv.URL, token).Get(t.Context(), name)
		srv.Close()
		if err == nil || strings.Contains(err.Error(), token[:6]) {
			t.Errorf("%s：错误中不应有 token 的一部分：%v", tc.desc, err)
		}
	}
}

// 网络错误：Get 按退避重试，次数用完后返回错误（不含 token）；Publish 的网络错误是不确定的结果。
func TestClient_networkError(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String() + "/"
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, u, token)
	_, err = c.Get(t.Context(), name)
	if _, ok := errors.AsType[*npm.Error](err); !errors.Is(err, npm.ErrNoResponse) || ok || strings.Contains(err.Error(), token) {
		t.Fatalf("Get = %v", err)
	}
	err = c.Publish(t.Context(), publishReq("1.0.0", []byte("a")))
	if !npm.Uncertain(err) || strings.Contains(err.Error(), token) {
		t.Fatalf("Publish = %v", err)
	}
}

// registry 可以带路径前缀（私有 registry）。
func TestClient_pathPrefix(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL+"/api/npm/repo", token)
	if p, err := c.Get(t.Context(), name); err != nil || p != nil {
		t.Fatalf("Get = %+v, %v", p, err)
	}
	if len(paths) != 1 || paths[0] != "/api/npm/repo/@acme%2fkilnflow-darwin-arm64" {
		t.Fatalf("路径 %v", paths)
	}
	if got := c.TarballURL(name, "1.0.0"); got != srv.URL+"/api/npm/repo/"+name+"/-/kilnflow-darwin-arm64-1.0.0.tgz" {
		t.Fatalf("TarballURL = %q", got)
	}
}

func TestUncertain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("参数无效"), false},
		{fmt.Errorf("PUT /x：%w：timeout", npm.ErrNoResponse), true},
		{&npm.Error{Status: 502}, true},
		{&npm.Error{Status: 403}, false},
		{&npm.Error{Status: 400}, false},
	} {
		if got := npm.Uncertain(tc.err); got != tc.want {
			t.Errorf("Uncertain(%v) = %v，期望 %v", tc.err, got, tc.want)
		}
	}
}

// 传输层错误：保留 ErrNoResponse 的判别语义，同时保留 context 的取消/超时语义（errors.Is 都成立）。
func TestClient_noResponseKeepsContext(t *testing.T) {
	t.Parallel()
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		c, err := npm.NewClient("https://registry.example.com/", token, "kiln/test",
			npm.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("dial: %w", want)
			})}),
			npm.WithRetry(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Get(t.Context(), name)
		if !errors.Is(err, npm.ErrNoResponse) || !errors.Is(err, want) {
			t.Errorf("err = %v，应同时满足 ErrNoResponse 和 %v", err, want)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("错误中不应有 token：%v", err)
		}
	}
}

// roundTripFunc 让测试直接返回传输层错误。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
