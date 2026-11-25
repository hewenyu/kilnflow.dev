package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// 静态资产与页面数据的 gzip 压缩测试（见 ui.go 的 serveAsset、writeJSON）。

// assetDashboard 返回只有日志和内存前端产物的 Dashboard（不访问数据库）：测试不依赖 ui/dist 是否已在本地构建。
func assetDashboard(files fstest.MapFS) *Dashboard {
	return &Dashboard{log: slog.New(slog.DiscardHandler), ui: files}
}

// gunzip 解压 b，失败时终止测试。
func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("不是 gzip 数据：%v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	return out
}

// assetBody 返回长度超过资产压缩阈值、重复 unit 的正文。
func assetBody(unit string) []byte {
	return []byte(strings.Repeat(unit, assetGzipMinSize/len(unit)+10))
}

// TestServeAssetGzip：文本类资产在客户端接受 gzip 时发送预压缩的字节，Content-Type 按扩展名给出，
// 不再经过内容嗅探（gzip 字节会被认成 application/x-gzip），解压后与原文逐字节一致。
func TestServeAssetGzip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, wantType, wantCache string
		body                      []byte
	}{
		{"assets/app-abc123.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", assetBody("console.log('kiln');\n")},
		{"assets/app-abc123.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable", assetBody(".kiln{color:#c96}")},
		{"favicon.svg", "image/svg+xml", "public, max-age=86400", assetBody(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h24v24H0z"/></svg>`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := assetDashboard(fstest.MapFS{"ui/dist/" + c.name: &fstest.MapFile{Data: c.body}})
			r := httptest.NewRequest(http.MethodGet, "/"+c.name, nil)
			r.Header.Set("Accept-Encoding", "gzip, deflate, br")
			w := httptest.NewRecorder()
			d.serveAsset(w, r)

			res := w.Result()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d，应为 200", res.StatusCode)
			}
			if got := res.Header.Get("Content-Encoding"); got != "gzip" {
				t.Errorf("Content-Encoding = %q，应为 gzip", got)
			}
			if got := res.Header.Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q，应为 Accept-Encoding", got)
			}
			if got := res.Header.Get("Content-Type"); got != c.wantType {
				t.Errorf("Content-Type = %q，应为 %q（不能是 application/x-gzip）", got, c.wantType)
			}
			if got := res.Header.Get("Cache-Control"); got != c.wantCache {
				t.Errorf("Cache-Control = %q，应为 %q", got, c.wantCache)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q，应为 nosniff", got)
			}
			if got, want := res.Header.Get("Content-Length"), len(w.Body.Bytes()); got != strconv.Itoa(want) {
				t.Errorf("Content-Length = %q，实际发送 %d 字节", got, want)
			}
			if len(w.Body.Bytes()) >= len(c.body) {
				t.Errorf("压缩后 %d 字节，没有小于原文 %d 字节", len(w.Body.Bytes()), len(c.body))
			}
			if out := gunzip(t, w.Body.Bytes()); !bytes.Equal(out, c.body) {
				t.Errorf("解压后与原文不一致：%d 字节 != %d 字节", len(out), len(c.body))
			}
		})
	}
}

// TestServeAssetIdentity：客户端不接受 gzip 时发送原文，仍声明 Vary，且不残留 Content-Encoding。
func TestServeAssetIdentity(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})

	for _, enc := range []string{"", "identity", "gzip;q=0", "br, gzip;q=0"} {
		t.Run("Accept-Encoding="+enc, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil)
			if enc != "" {
				r.Header.Set("Accept-Encoding", enc)
			}
			w := httptest.NewRecorder()
			d.serveAsset(w, r)

			res := w.Result()
			if got := res.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q，不应压缩", got)
			}
			if got := res.Header.Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q，应为 Accept-Encoding", got)
			}
			if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
				t.Errorf("Content-Type = %q，应为 text/javascript", got)
			}
			if !bytes.Equal(w.Body.Bytes(), body) {
				t.Errorf("正文与原文不一致：%d 字节 != %d 字节", w.Body.Len(), len(body))
			}
		})
	}
}

// TestServeAssetAcceptEncoding 覆盖 Accept-Encoding 的解析：* 和 q>0 接受，q=0 不接受。
func TestServeAssetAcceptEncoding(t *testing.T) {
	t.Parallel()
	body := assetBody(".kiln{color:#c96}")
	for _, c := range []struct {
		header string
		gzip   bool
	}{
		{"*", true},
		{"gzip;q=0.001", true},
		{"br, gzip;q=0.5", true},
		{"br, gzip;q=0", false},
		{"*, gzip;q=0", false},
		{"gzip;q=0, *;q=1", false},
		{"GZIP", true},
	} {
		t.Run(c.header, func(t *testing.T) {
			d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.css": &fstest.MapFile{Data: body}})
			r := httptest.NewRequest(http.MethodGet, "/assets/app-abc123.css", nil)
			r.Header.Set("Accept-Encoding", c.header)
			w := httptest.NewRecorder()
			d.serveAsset(w, r)
			if got := w.Result().Header.Get("Content-Encoding") == "gzip"; got != c.gzip {
				t.Errorf("Accept-Encoding: %s 时是否压缩 = %v，应为 %v", c.header, got, c.gzip)
			}
		})
	}
}

// TestAcceptsGzipMultiLine：Accept-Encoding 可以有多行（RFC 9110 §5.3），每一行都要参与判断；
// 只看第一行会在拒绝与通配、或 gzip 落在第二行时判断错。
func TestAcceptsGzipMultiLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		headers []string
		gzip    bool
	}{
		{"通配与拒绝分在两行", []string{"*", "gzip;q=0"}, false},
		{"gzip 在第二行", []string{"br", "gzip"}, true},
		{"拒绝与通配分在两行", []string{"gzip;q=0", "*"}, false},
		{"第二行带质量因子", []string{"br", "deflate, gzip;q=0.5"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, h := range c.headers {
				r.Header.Add("Accept-Encoding", h)
			}
			if got := acceptsGzip(r); got != c.gzip {
				t.Errorf("Accept-Encoding=%v 时 acceptsGzip = %v，应为 %v", c.headers, got, c.gzip)
			}
		})
	}
}

// TestAcceptsGzipInvalidQuality：非法的 q 值（无法解析、负值、超过 1、非有限数）按 0 处理，不压缩。
func TestAcceptsGzipInvalidQuality(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"gzip;q=invalid", "gzip;q=-1", "gzip;q=2", "gzip;q=NaN", "gzip;q=+Inf", "gzip;q="} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", header)
			if acceptsGzip(r) {
				t.Errorf("Accept-Encoding=%q 时不应压缩", header)
			}
		})
	}
}

// TestDashboardConcurrentCompress：同一个 Dashboard 并发处理静态资产（assetsGz 缓存）与页面数据
// （gzip.Writer 池）时不能有数据竞争；在 -race 下运行。
func TestDashboardConcurrentCompress(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})
	big := struct{ Body string }{Body: strings.Repeat("kiln", jsonGzipMinSize)}

	const workers = 8
	const rounds = 5
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for range rounds {
				w := httptest.NewRecorder()
				if i%2 == 0 {
					r := httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil)
					r.Header.Set("Accept-Encoding", "gzip")
					d.serveAsset(w, r)
				} else {
					r := httptest.NewRequest(http.MethodGet, "/", nil)
					r.Header.Set("Accept-Encoding", "gzip")
					d.writeJSON(w, r, http.StatusOK, big)
				}
				if got := w.Result().Header.Get("Content-Encoding"); got != "gzip" {
					t.Errorf("并发请求的 Content-Encoding = %q，应为 gzip", got)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(d.assetsGz) != 1 {
		t.Errorf("并发压缩后缓存了 %d 个资产，应为 1", len(d.assetsGz))
	}
}

// TestServeAssetHead：压缩路径的 HEAD 只写头部，Content-Length 是压缩后的大小，不发送正文。
func TestServeAssetHead(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})
	r := httptest.NewRequest(http.MethodHead, "/assets/app-abc123.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	d.serveAsset(w, r)

	res := w.Result()
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q，应为 gzip", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不应有正文，实际 %d 字节", w.Body.Len())
	}
	if got, want := res.Header.Get("Content-Length"), len(d.gzipAsset("assets/app-abc123.js", body)); got != strconv.Itoa(want) {
		t.Errorf("Content-Length = %q，应为压缩后的 %d", got, want)
	}
}

// TestServeAssetConditional：压缩表示与原文表示对条件请求的处理一致。If-None-Match 命中时都返回 304、
// 没有正文、不留下 Content-Encoding 等表示元数据，Vary 与 Cache-Control 保留；If-Match 不匹配时都返回 412、
// 没有正文。压缩分支不能绕开 ServeContent 直接写 200。
func TestServeAssetConditional(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})

	const cacheControl = "public, max-age=31536000, immutable"
	cases := []struct {
		name       string
		method     string
		encoding   string
		header     string
		value      string
		wantStatus int
	}{
		{"GET 压缩 If-None-Match", http.MethodGet, "gzip", "If-None-Match", "*", http.StatusNotModified},
		{"GET 原文 If-None-Match", http.MethodGet, "identity", "If-None-Match", "*", http.StatusNotModified},
		{"HEAD 压缩 If-None-Match", http.MethodHead, "gzip", "If-None-Match", "*", http.StatusNotModified},
		{"HEAD 原文 If-None-Match", http.MethodHead, "identity", "If-None-Match", "*", http.StatusNotModified},
		{"GET 压缩 If-Match", http.MethodGet, "gzip", "If-Match", `"nope"`, http.StatusPreconditionFailed},
		{"GET 原文 If-Match", http.MethodGet, "identity", "If-Match", `"nope"`, http.StatusPreconditionFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(c.method, "/assets/app-abc123.js", nil)
			r.Header.Set("Accept-Encoding", c.encoding)
			r.Header.Set(c.header, c.value)
			w := httptest.NewRecorder()
			d.serveAsset(w, r)

			res := w.Result()
			if res.StatusCode != c.wantStatus {
				t.Fatalf("状态码 = %d，应为 %d", res.StatusCode, c.wantStatus)
			}
			if w.Body.Len() != 0 {
				t.Errorf("%d 响应不应有正文，实际 %d 字节", c.wantStatus, w.Body.Len())
			}
			if got := res.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q，不应带压缩标记", got)
			}
			if got := res.Header.Get("Content-Type"); got != "" {
				t.Errorf("Content-Type = %q，不应带表示元数据", got)
			}
			if got := res.Header.Get("Content-Length"); got != "" {
				t.Errorf("Content-Length = %q，没有正文时不应声明长度", got)
			}
			if got := res.Header.Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q，应为 Accept-Encoding", got)
			}
			if got := res.Header.Get("Cache-Control"); got != cacheControl {
				t.Errorf("Cache-Control = %q，应为 %q", got, cacheControl)
			}
		})
	}
}

// TestServeAssetRange：带 Range 的请求不对部分内容压缩，交给 ServeContent 返回 206。
func TestServeAssetRange(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})
	r := httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Range", "bytes=0-9")
	w := httptest.NewRecorder()
	d.serveAsset(w, r)

	if got := w.Code; got != http.StatusPartialContent {
		t.Fatalf("状态码 = %d，应为 206", got)
	}
	if got := w.Result().Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Range 请求的 Content-Encoding = %q，不应压缩", got)
	}
	if !bytes.Equal(w.Body.Bytes(), body[:10]) {
		t.Errorf("部分内容 = %q，应为 %q", w.Body.Bytes(), body[:10])
	}
}

// TestServeAssetSmall 与 TestServeAssetBinary：小于阈值的文本资产、二进制资产都发原文。
func TestServeAssetSmall(t *testing.T) {
	t.Parallel()
	small := []byte(".a{color:red}")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/tiny.css": &fstest.MapFile{Data: small}})
	r := httptest.NewRequest(http.MethodGet, "/assets/tiny.css", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	d.serveAsset(w, r)
	if got := w.Result().Header.Get("Content-Encoding"); got != "" {
		t.Errorf("小于阈值的资产 Content-Encoding = %q，不应压缩", got)
	}
	if !bytes.Equal(w.Body.Bytes(), small) {
		t.Errorf("正文 = %q，应为原文", w.Body.Bytes())
	}
	if len(d.assetsGz) != 0 {
		t.Errorf("没有压缩时不应缓存：%v", d.assetsGz)
	}
}

func TestServeAssetBinary(t *testing.T) {
	t.Parallel()
	// PNG 文件头加不可压缩的字节；扩展名 .png 不是文本类，不声明 Vary，也不压缩。
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00, 0xff, 0x80}, 1000)...)
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/logo.png": &fstest.MapFile{Data: png}})
	r := httptest.NewRequest(http.MethodGet, "/assets/logo.png", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	d.serveAsset(w, r)
	if got := w.Result().Header.Get("Content-Encoding"); got != "" {
		t.Errorf("二进制资产的 Content-Encoding = %q，不应压缩", got)
	}
	if got := w.Result().Header.Get("Vary"); got != "" {
		t.Errorf("二进制资产的 Vary = %q，不应声明（不会随 Accept-Encoding 变化）", got)
	}
	if !bytes.Equal(w.Body.Bytes(), png) {
		t.Errorf("正文与原文不一致")
	}
}

// TestServeAssetGzipCached：内嵌资产不可变，按文件名只压缩一次，后续请求复用缓存。
func TestServeAssetGzipCached(t *testing.T) {
	t.Parallel()
	body := assetBody("console.log('kiln');\n")
	d := assetDashboard(fstest.MapFS{"ui/dist/assets/app-abc123.js": &fstest.MapFile{Data: body}})
	var first []byte
	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		d.serveAsset(w, r)
		if first == nil {
			first = bytes.Clone(w.Body.Bytes())
		} else if !bytes.Equal(w.Body.Bytes(), first) {
			t.Fatalf("多次请求的压缩结果不一致")
		}
	}
	if len(d.assetsGz) != 1 {
		t.Errorf("缓存了 %d 个资产，应为 1", len(d.assetsGz))
	}
	if !bytes.Equal(gunzip(t, first), body) {
		t.Errorf("解压后与原文不一致")
	}
}

// TestWriteJSONGzip：页面数据超过阈值且客户端接受 gzip 时压缩，Vary 声明两个请求头；
// 解压后与不压缩时的正文逐字节一致。
func TestWriteJSONGzip(t *testing.T) {
	t.Parallel()
	d := &Dashboard{log: slog.New(slog.DiscardHandler)}
	big := struct{ Body string }{Body: strings.Repeat("kiln", jsonGzipMinSize)}

	identity := httptest.NewRecorder()
	d.writeJSON(identity, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, big)
	if got := identity.Result().Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("未接受 gzip 时 Content-Encoding = %q", got)
	}
	if got, want := identity.Result().Header.Get("Vary"), uiHeader+", Accept-Encoding"; got != want {
		t.Errorf("Vary = %q，应为 %q", got, want)
	}
	if identity.Body.Len() < jsonGzipMinSize {
		t.Fatalf("测试正文只有 %d 字节，没有超过阈值", identity.Body.Len())
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	d.writeJSON(w, r, http.StatusOK, big)
	res := w.Result()
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q，应为 gzip", got)
	}
	if got, want := res.Header.Get("Vary"), uiHeader+", Accept-Encoding"; got != want {
		t.Errorf("Vary = %q，应为 %q", got, want)
	}
	if got, want := res.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q，应为 %q", got, want)
	}
	if got, want := res.Header.Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q，应为 %q", got, want)
	}
	if got, want := res.Header.Get("Content-Length"), strconv.Itoa(w.Body.Len()); got != want {
		t.Errorf("Content-Length = %q，实际发送 %s 字节", got, want)
	}
	if w.Body.Len() >= identity.Body.Len() {
		t.Errorf("压缩后 %d 字节，没有小于原文 %d 字节", w.Body.Len(), identity.Body.Len())
	}
	if !bytes.Equal(gunzip(t, w.Body.Bytes()), identity.Body.Bytes()) {
		t.Errorf("解压后与不压缩时的正文不一致")
	}

	// 复用压缩器：再发一次仍然得到可解压、内容一致的响应
	w2 := httptest.NewRecorder()
	d.writeJSON(w2, r, http.StatusOK, big)
	if !bytes.Equal(gunzip(t, w2.Body.Bytes()), identity.Body.Bytes()) {
		t.Errorf("复用压缩器后解压结果不一致")
	}
}

// TestWriteJSONSmall：小于阈值、或客户端不接受 gzip 时发送原文（带 Content-Length）。
func TestWriteJSONSmall(t *testing.T) {
	t.Parallel()
	d := &Dashboard{log: slog.New(slog.DiscardHandler)}
	small := struct{ Body string }{Body: strings.Repeat("x", jsonGzipMinSize-100)}

	for _, enc := range []string{"gzip", "identity", ""} {
		t.Run("Accept-Encoding="+enc, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if enc != "" {
				r.Header.Set("Accept-Encoding", enc)
			}
			w := httptest.NewRecorder()
			d.writeJSON(w, r, http.StatusOK, small)
			res := w.Result()
			if got := res.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q，小响应不应压缩", got)
			}
			if w.Body.Len() >= jsonGzipMinSize {
				t.Errorf("测试正文 %d 字节，没有低于阈值", w.Body.Len())
			}
			if got, want := res.Header.Get("Content-Length"), strconv.Itoa(w.Body.Len()); got != want {
				t.Errorf("Content-Length = %q，实际发送 %s 字节", got, want)
			}
			if got, want := res.Header.Get("Vary"), uiHeader+", Accept-Encoding"; got != want {
				t.Errorf("Vary = %q，应为 %q", got, want)
			}
		})
	}
}
