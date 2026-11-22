package upload_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakeupload"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
)

const token = "lc_upload_s3cr3t-token"

func newClient(t *testing.T, rawURL, tok string) *upload.Client {
	t.Helper()
	c, err := upload.NewClient(rawURL, tok, "kiln/test", upload.WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func asset(name, content string) (upload.Asset, func() (io.ReadCloser, error)) {
	sum := sha256.Sum256([]byte(content))
	return upload.Asset{Name: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/api/releases", true},
		{"https://example.com", true},
		{"http://127.0.0.1:8080/api", true},
		{"http://localhost/api", true},
		{"http://[::1]:9000/x", true},
		{"http://example.com/api", false},
		{"ftp://example.com/api", false},
		{"https://user:pass@example.com/api", false},
		{"https://example.com/api?x=1", false},
		{"https://example.com/api#f", false},
		{"https:///api", false},
		{"example.com/api", false},
	} {
		if err := upload.CheckURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckURL(%q) = %v，期望 ok=%v", tc.url, err, tc.ok)
		}
	}
}

// 首次上传 201、重试幂等 200、查询、提交、重复提交仍是 200；请求带 Authorization 和 User-Agent。
func TestClient_stageAndPublish(t *testing.T) {
	t.Parallel()
	s := fakeupload.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL+"/", token) // 结尾的 / 去掉
	ctx := t.Context()

	if rel, err := c.Get(ctx, "v1.0.0"); err != nil || rel != nil {
		t.Fatalf("还没有记录时 Get = %+v, %v，期望 nil, nil", rel, err)
	}
	a, open := asset("app-v1.0.0.dmg", "dmg-bytes")
	if created, err := c.Put(ctx, "v1.0.0", a, open); err != nil || !created {
		t.Fatalf("首次 Put = %v, %v，期望新建", created, err)
	}
	if created, err := c.Put(ctx, "v1.0.0", a, open); err != nil || created {
		t.Fatalf("重复 Put = %v, %v，期望 200（已有相同内容）", created, err)
	}
	rel, err := c.Get(ctx, "v1.0.0")
	if err != nil || rel.State != upload.StateStaging || len(rel.Assets) != 1 || rel.Assets[0].SHA256 != a.SHA256 {
		t.Fatalf("暂存后 Get = %+v, %v", rel, err)
	}
	req := &upload.PublishRequest{Commit: strings.Repeat("a", 40), RunID: 7, Latest: true, Assets: []upload.Asset{a}}
	res, err := c.Publish(ctx, "v1.0.0", req)
	if err != nil || len(res.Assets) != 1 || res.Assets[0].URL == "" || res.PublishedAt == "" {
		t.Fatalf("Publish = %+v, %v", res, err)
	}
	if _, err := c.Publish(ctx, "v1.0.0", req); err != nil {
		t.Fatalf("同一清单重复提交应成功：%v", err)
	}
	if s.State("v1.0.0") != upload.StatePublished || s.Latest() != "v1.0.0" {
		t.Fatalf("服务端状态 %s，latest %s", s.State("v1.0.0"), s.Latest())
	}
	if rel, err := c.Get(ctx, "v1.0.0"); err != nil || rel.State != upload.StatePublished || rel.PublishedAt == "" {
		t.Fatalf("发布后 Get = %+v, %v", rel, err)
	}
	for _, h := range s.AuthorizationHeaders() {
		if h != "Bearer "+token {
			t.Fatalf("Authorization = %q", h)
		}
	}
	if uas := s.UserAgents(); len(uas) == 0 || uas[0] != "kiln/test" {
		t.Fatalf("User-Agent = %v", uas)
	}
}

// tag 和文件名按 path segment 转义；文件名不能含路径分隔符（客户端先拒绝，服务端也会拒绝）。
func TestClient_escapesPath(t *testing.T) {
	t.Parallel()
	s := fakeupload.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	a, open := asset("app 1.0#x.zip", "z")
	if _, err := c.Put(t.Context(), "v1+b", a, open); err != nil {
		t.Fatal(err)
	}
	if got := s.Staged("v1+b"); !slices.Equal(got, []string{"app 1.0#x.zip"}) {
		t.Fatalf("服务端暂存 %v", got)
	}
	// 含路径分隔符的文件名在客户端就被拒绝，不会发出请求（服务端同样不接受，见 fakeupload）
	bad, openBad := asset("dir/app.zip", "z")
	if _, err := c.Put(t.Context(), "v1+b", bad, openBad); err == nil || !strings.Contains(err.Error(), "路径分隔符") {
		t.Fatalf("期望文件名被拒绝：%v", err)
	}
	reqs := s.Requests()
	for _, want := range []string{"PUT " + fakeupload.BasePath + "/v1+b/assets/app%201.0%23x.zip"} {
		if !slices.Contains(reqs, want) {
			t.Fatalf("请求 %v，期望包含 %q", reqs, want)
		}
	}
	if len(reqs) != 1 {
		t.Fatalf("应有 1 个请求，实际 %v", reqs)
	}
}

// 分层 tag（Git 的 app/v1.0.0）整体按一个 path segment 转义：三个端点都发成 app%2Fv1.0.0 这一段落
// （docs/UPLOAD-PROTOCOL.md「公共约定」），不会把 tag 拆成两段路径；接受与否由服务端决定。
func TestClient_hierarchicalTag(t *testing.T) {
	t.Parallel()
	const tag = "app/v1.0.0"
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// 还没有这个 tag
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","message":"没有这个 tag"}`)
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"tag":"`+tag+`","published_at":"2026-01-01T00:00:00Z","assets":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL+"/releases", token)

	if rel, err := c.Get(t.Context(), tag); err != nil || rel != nil {
		t.Fatalf("Get = %+v, %v，期望 nil, nil（404）", rel, err)
	}
	a, open := asset("app-v1.0.0.dmg", "dmg-bytes")
	if created, err := c.Put(t.Context(), tag, a, open); err != nil || !created {
		t.Fatalf("Put = %v, %v，期望新建", created, err)
	}
	if _, err := c.Publish(t.Context(), tag, &upload.PublishRequest{Assets: []upload.Asset{a}}); err != nil {
		t.Fatalf("Publish 出错：%v", err)
	}
	want := []string{
		"GET /releases/app%2Fv1.0.0",
		"PUT /releases/app%2Fv1.0.0/assets/app-v1.0.0.dmg",
		"POST /releases/app%2Fv1.0.0/publish",
	}
	mu.Lock()
	requests := slices.Clone(got)
	mu.Unlock()
	if !slices.Equal(requests, want) {
		t.Fatalf("请求 %v，期望 %v", requests, want)
	}
}

func TestClient_errors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("摘要不符", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		a, _ := asset("a.bin", "expected")
		_, err := newClient(t, s.URL, token).Put(ctx, "v1", a, func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("tampered")), nil
		})
		if !upload.IsCode(err, upload.CodeDigestMismatch) {
			t.Fatalf("期望 digest_mismatch：%v", err)
		}
	})

	t.Run("已发布的内容不能覆盖", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		s.Preset("v1", map[string][]byte{"a.bin": []byte("old")})
		c := newClient(t, s.URL, token)
		a, open := asset("a.bin", "new")
		if _, err := c.Put(ctx, "v1", a, open); !upload.IsCode(err, upload.CodePublished) || !upload.IsStatus(err, http.StatusConflict) {
			t.Fatalf("期望 409 published：%v", err)
		}
		// 内容相同的重试仍然是 200
		same, openSame := asset("a.bin", "old")
		if created, err := c.Put(ctx, "v1", same, openSame); err != nil || created {
			t.Fatalf("已发布且内容相同时应返回 200：%v %v", created, err)
		}
		// 用不同清单再次发布
		if _, err := c.Publish(ctx, "v1", &upload.PublishRequest{Assets: []upload.Asset{a}}); !upload.IsCode(err, upload.CodePublished) {
			t.Fatalf("期望 409 published：%v", err)
		}
	})

	t.Run("清单不全", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		c := newClient(t, s.URL, token)
		a, open := asset("a.bin", "a")
		b, _ := asset("b.bin", "b")
		if _, err := c.Put(ctx, "v1", a, open); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Publish(ctx, "v1", &upload.PublishRequest{Assets: []upload.Asset{a, b}}); !upload.IsCode(err, upload.CodeIncomplete) {
			t.Fatalf("期望 409 incomplete：%v", err)
		}
		if s.State("v1") != upload.StateStaging {
			t.Fatal("清单不全时不应发布")
		}
	})

	t.Run("文件太大", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		s.SetMaxSize(3)
		a, open := asset("a.bin", "four")
		if _, err := newClient(t, s.URL, token).Put(ctx, "v1", a, open); !upload.IsCode(err, upload.CodeTooLarge) {
			t.Fatalf("期望 413 too_large：%v", err)
		}
	})

	t.Run("5xx 和 429 重试后成功", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		s.FailNext(http.MethodPut, http.StatusBadGateway, 1)
		s.FailNext(http.MethodPut, http.StatusTooManyRequests, 1)
		a, open := asset("a.bin", "a")
		if _, err := newClient(t, s.URL, token).Put(ctx, "v1", a, open); err != nil {
			t.Fatalf("重试后应成功：%v", err)
		}
		if n := countMethod(s.Requests(), http.MethodPut); n != 3 {
			t.Fatalf("PUT 应发送 3 次，实际 %d 次", n)
		}
	})

	t.Run("5xx 重试次数用完", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New(token)
		t.Cleanup(s.Close)
		s.FailNext("", http.StatusServiceUnavailable, 10)
		_, err := newClient(t, s.URL, token).Get(ctx, "v1")
		if !upload.IsStatus(err, http.StatusServiceUnavailable) {
			t.Fatalf("期望 503：%v", err)
		}
		if n := len(s.Requests()); n != 3 {
			t.Fatalf("应尝试 3 次，实际 %d 次", n)
		}
	})

	t.Run("401 不重试，错误信息不含 token", func(t *testing.T) {
		t.Parallel()
		s := fakeupload.New("other-token")
		t.Cleanup(s.Close)
		a, open := asset("a.bin", "a")
		_, err := newClient(t, s.URL, token).Put(ctx, "v1", a, open)
		if !upload.IsStatus(err, http.StatusUnauthorized) {
			t.Fatalf("期望 401：%v", err)
		}
		if n := len(s.Requests()); n != 1 {
			t.Fatalf("401 不应重试，实际 %d 次请求", n)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("错误信息中出现了 token：%v", err)
		}
	})
}

// 服务端在错误说明中回显 token、或返回重定向时：token 被替换成 ***，不跟随重定向（token 不会发给别的地址）。
func TestClient_redactsAndNoRedirect(t *testing.T) {
	t.Parallel()
	var other []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		other = append(other, r.Header.Get("Authorization"))
	}))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/publish") {
			http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden","message":"token `+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+` 没有权限"}`)
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL, token)
	_, err := c.Get(t.Context(), "v1")
	if !upload.IsStatus(err, http.StatusForbidden) || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("错误应为 403 且 token 已脱敏：%v", err)
	}
	_, err = c.Publish(t.Context(), "v1", &upload.PublishRequest{})
	var ue *upload.Error
	if !errors.As(err, &ue) || ue.Status != http.StatusTemporaryRedirect || !strings.Contains(err.Error(), "不跟随重定向") {
		t.Fatalf("重定向应作为错误返回：%v", err)
	}
	if len(other) != 0 {
		t.Fatalf("不应请求重定向的目标：%v", other)
	}
}

// token 正好落在截断位置时（错误码、错误说明、重定向地址、读取上限）也不能泄露 token 的任何一部分：
// 先去掉 token 再截断（以前先截断，被切开的前半段 token 留在错误信息中）。
func TestClient_redactsTruncatedToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc     string
		location string
		body     string
	}{
		{"错误说明", "", strings.Repeat("x", 190) + token + " 没有权限"},
		{"JSON 错误说明", "", `{"error":"forbidden","message":"` + strings.Repeat("x", 490) + token + `"}`},
		{"JSON 错误码", "", `{"error":"` + strings.Repeat("x", 56) + token + `"}`},
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
		_, err := newClient(t, srv.URL, token).Get(t.Context(), "v1")
		srv.Close()
		if err == nil || strings.Contains(err.Error(), token[:6]) {
			t.Errorf("%s：错误中不应有 token 的一部分：%v", tc.desc, err)
		}
	}
}

// 网络错误按退避重试，次数用完后返回错误（不含 token）。
func TestClient_networkError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	_, err := newClient(t, u, token).Get(t.Context(), "v1")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("期望网络错误：%v", err)
	}
}

func TestNewClient_rejects(t *testing.T) {
	if _, err := upload.NewClient("http://example.com/x", token, "kiln/test"); err == nil {
		t.Fatal("非本机地址的 http 应被拒绝")
	}
	if _, err := upload.NewClient("https://example.com/x", "", "kiln/test"); err == nil {
		t.Fatal("token 为空应被拒绝")
	}
	if _, err := upload.NewClient("https://example.com/x", token, "kiln/test"); err != nil {
		t.Fatal(err)
	}
	a := upload.Asset{Name: "a", SHA256: "zz"}
	c := newClient(t, "https://example.com/x", token)
	if _, err := c.Put(t.Context(), "v1", a, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil }); err == nil {
		t.Fatal("无效的 sha256 应在发送前报错")
	}
}

func countMethod(reqs []string, method string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

// closeTracker 记录 Close 是否被调用。
type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

// 空文件不发送请求体，open 打开的内容也要关闭（例如文件描述符不能泄漏）。
func TestClient_emptyAssetClosesBody(t *testing.T) {
	t.Parallel()
	s := fakeupload.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	a, _ := asset("empty.txt", "")
	var opened []*closeTracker
	open := func() (io.ReadCloser, error) {
		ct := &closeTracker{Reader: strings.NewReader("")}
		opened = append(opened, ct)
		return ct, nil
	}
	if created, err := c.Put(t.Context(), "v1.0.0", a, open); err != nil || !created {
		t.Fatalf("Put = %v, %v，期望新建", created, err)
	}
	if len(opened) == 0 {
		t.Fatal("open 没有被调用")
	}
	for i, ct := range opened {
		if !ct.closed {
			t.Errorf("第 %d 次打开的内容没有关闭", i+1)
		}
	}
}

// tag 不能为空、不能有点段（. 或 ..）、不能含控制字符（分层 tag 中的 / 是合法的，见 TestClient_hierarchicalTag）；
// 文件名不能为空、不能是 . 或 ..、不能含路径分隔符或控制字符：客户端直接拒绝，不发请求。
func TestClient_rejectsUnsafeSegments(t *testing.T) {
	t.Parallel()
	s := fakeupload.New(token)
	t.Cleanup(s.Close)
	c := newClient(t, s.URL, token)
	a, open := asset("app.zip", "z")

	for _, tag := range []string{"", ".", "..", "../v1", "v1/..", "app/./v1", "v1\n", "v\x001", "v\x7f1"} {
		if _, err := c.Get(t.Context(), tag); err == nil {
			t.Errorf("Get(%q) 应报错", tag)
		}
		if _, err := c.Put(t.Context(), tag, a, open); err == nil {
			t.Errorf("Put(%q) 应报错", tag)
		}
		if _, err := c.Publish(t.Context(), tag, &upload.PublishRequest{Assets: []upload.Asset{a}}); err == nil {
			t.Errorf("Publish(%q) 应报错", tag)
		}
	}
	for _, name := range []string{"", ".", "..", "dir/app.zip", `dir\app.zip`, "app\n.zip", "app\x001.zip", "app\x7f.zip"} {
		bad, open := asset(name, "z")
		if _, err := c.Put(t.Context(), "v1", bad, open); err == nil {
			t.Errorf("Put(name=%q) 应报错", name)
		}
		if _, err := c.Publish(t.Context(), "v1", &upload.PublishRequest{Assets: []upload.Asset{bad}}); err == nil {
			t.Errorf("Publish(name=%q) 应报错", name)
		}
	}
	if reqs := s.Requests(); len(reqs) != 0 {
		t.Fatalf("应有 0 个请求，实际 %v", reqs)
	}
}
