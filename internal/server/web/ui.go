package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Dashboard 的前端是 React 单页应用（仓库根目录的 web/，构建产物在 ui/dist/，内嵌在二进制中）：
//   - 浏览器打开页面地址（/、/runs/12 等）时返回前端的 index.html；前端再用同一个地址、带请求头 X-Kiln-UI: 1 取页面数据（JSON）。
//   - 带这个请求头的请求，页面处理器照常执行（登录、权限、CSRF、校验和状态码都不变），只是把页面数据写成 JSON（uiPage），
//     重定向写成 {"Redirect": 地址}（uiRedirect），由前端跳转。表单仍按 application/x-www-form-urlencoded 提交，带 csrf 字段。
//   - 页面数据沿用 Go 的字段名；模板时代由方法计算的值（例如 runPage.History）由各类型的 MarshalJSON 一并输出。

// uiHeader 是前端请求页面数据时带的请求头（值为 "1"）。跨站请求不能带自定义请求头，也读不到 JSON。
const uiHeader = "X-Kiln-UI"

// isUI 表示请求来自前端，要的是页面数据（JSON）而不是 index.html。
func isUI(r *http.Request) bool { return r.Header.Get(uiHeader) == "1" }

//go:embed all:ui
var uiFS embed.FS

// uiPage 是一个页面的数据。
type uiPage struct {
	// Page 是页面名称（前端据此选择页面组件），Status 是 HTTP 状态码。
	Page string
	// Title 是页面标题（消息或原样的文字，见 text.go）。
	Title  uiText
	Nav    string
	Status int
	// Session 是当前登录的身份；未登录时为 nil。
	Session *uiSession
	// CSRF 是表单提交时要带的 csrf 字段；未登录时为空。
	CSRF  string
	OAuth bool
	// Active 是正在进行或排队的 run 数（导航中"运行"旁的数字）。
	Active int
	// Flash 是跳转后显示一次的提示（地址中的 ?flash=，经 parseFlash 校验）。
	Flash uiText
	Data  any
}

// uiSession 是页面上显示的当前身份（不含会话 cookie 中的其他内容）。
type uiSession struct {
	Login string
	Admin bool
	// ViaToken 表示用 admin token 登录；Bound 表示这是管理员绑定的 GitHub 账号。
	ViaToken bool
	Bound    bool
}

// uiRedirect 是前端请求的重定向：前端跳转到 Redirect（站内地址在前端内跳转，GitHub 等外部地址整页跳转）。
type uiRedirect struct {
	Redirect string
}

func newUISession(se *session) *uiSession {
	if se == nil {
		return nil
	}
	return &uiSession{Login: se.Login, Admin: se.Admin(), ViaToken: se.ViaToken(), Bound: se.Bound}
}

const (
	// jsonGzipMinSize 是启用 gzip 的最小 JSON 长度：更小的响应压缩后（加上 gzip 头）通常反而更大。
	jsonGzipMinSize = 1024
	// assetGzipMinSize 是启用 gzip 的最小内嵌资产长度：更小的文件压缩后反而更大，直接发原文。
	assetGzipMinSize = 1024
)

// acceptsGzip 判断请求是否接受 gzip：Accept-Encoding 中的 gzip（或 *）且质量因子不为 0。
// 明确写出 gzip 的项比 * 更具体（RFC 9110），优先于它：gzip;q=0, * 不接受 gzip。
func acceptsGzip(r *http.Request) bool {
	var gzipQ float64
	gzipSeen, wildcard := false, false
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for p := range strings.SplitSeq(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				q = f
			}
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			gzipSeen, gzipQ = true, q
		case "*":
			wildcard = q > 0
		}
	}
	if gzipSeen {
		return gzipQ > 0
	}
	return wildcard
}

// newGzipWriter 返回用 BestSpeed 压缩的 gzip.Writer（页面数据是高频轮询，压缩速度优先）。BestSpeed 是合法的级别，
// NewWriterLevel 不会返回错误。
func newGzipWriter() *gzip.Writer {
	w, err := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	if err != nil {
		panic(err)
	}
	return w
}

// writeJSON 发送 JSON（r 用来判断客户端是否接受 gzip）。页面数据随登录身份变化，不缓存。
func (d *Dashboard) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		d.log.Error("页面数据无法编码为 JSON", "err", err)
		http.Error(w, "服务器出错了，请稍后重试。", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", uiHeader+", Accept-Encoding")
	body := buf.Bytes()
	if len(body) >= jsonGzipMinSize && acceptsGzip(r) {
		body = d.gzipJSON(body)
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		d.log.Debug("发送页面数据", "err", err)
	}
}

// gzipJSON 用 Dashboard 上的 gzip.Writer 池压缩页面数据。
func (d *Dashboard) gzipJSON(body []byte) []byte {
	var out bytes.Buffer
	zw, ok := d.gzipWriters.Get().(*gzip.Writer)
	if !ok {
		zw = newGzipWriter()
	}
	zw.Reset(&out)
	_, _ = zw.Write(body)
	_ = zw.Close()
	// 归还前解除对本次 bytes.Buffer 的引用，避免池中的 writer 钉住大缓冲区。
	zw.Reset(io.Discard)
	d.gzipWriters.Put(zw)
	return out.Bytes()
}

// redirect 重定向到 to：前端请求返回 uiRedirect，浏览器请求返回 303。
func (d *Dashboard) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isUI(r) {
		d.writeJSON(w, r, http.StatusOK, uiRedirect{Redirect: to})
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // 调用方只传站内路径或 GitHub 授权地址
}

// page 包装页面地址的 GET：浏览器打开时直接返回 index.html（不查询数据库，登录和权限由前端取数据时检查）；前端取数据时交给 h。
func (d *Dashboard) page(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isUI(r) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			d.serveIndex(w, http.StatusOK)
			return
		}
		h(w, r)
	}
}

// indexPlaceholder 是前端没有构建时（只用 go build，没有先执行 make web）返回的页面。
// 占位页不依赖前端的词典（docs/I18N.md）：同时写中文和英文。
const indexPlaceholder = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>Kiln</title></head>
<p>Dashboard 的前端没有构建进这个 kiln-server：在仓库根目录执行 <code>make web</code>，再重新构建 kiln-server。</p>
<p lang="en">The Dashboard frontend is not built into this kiln-server: run <code>make web</code> in the repository root, then rebuild kiln-server.</p></html>`

// serveIndex 返回前端的 index.html（状态码 status）。index.html 引用的脚本和样式带内容摘要，index.html 本身每次向服务端确认。
func (d *Dashboard) serveIndex(w http.ResponseWriter, status int) { d.serveIndexWith(w, status, nil) }

// serveIndexWith 返回内嵌了页面数据 p 的 index.html（p 为 nil 时不内嵌，前端自己取数据）。
// 页面数据放在 <script id="kiln-page" type="application/json"> 中；json 编码转义了 <、>、&，不会提前结束 script。
func (d *Dashboard) serveIndexWith(w http.ResponseWriter, status int, p *uiPage) {
	body, err := fs.ReadFile(d.uiFiles(), "ui/dist/index.html")
	if err != nil {
		body = []byte(indexPlaceholder)
	}
	if p != nil {
		data, err := json.Marshal(p)
		if err != nil {
			d.log.Error("页面数据无法编码为 JSON", "page", p.Page, "err", err)
			http.Error(w, "服务器出错了，请稍后重试。", http.StatusInternalServerError)
			return
		}
		script := append(append([]byte(`<script id="kiln-page" type="application/json">`), data...), "</script></head>"...)
		body = bytes.Replace(body, []byte("</head>"), script, 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 内嵌了页面数据时与 JSON 一样不缓存（数据随登录身份变化，可能含一次性令牌）；只有外壳时可以缓存但每次确认
	if p != nil {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Vary", uiHeader)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 不允许被其他网站嵌入（点击劫持：在透明的 iframe 中诱导管理员点击吊销、删除等按钮）
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		d.log.Debug("发送 index.html", "err", err)
	}
}

// compressibleAsset 返回资产压缩后的 Content-Type 和是否可压缩：按扩展名判断文本类资产，未知扩展名按内容嗅探
// （text/* 也压缩）。压缩后不能用内容嗅探决定类型——gzip 字节会被认成 application/x-gzip。
func compressibleAsset(name string, body []byte) (string, bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8", true
	case ".css":
		return "text/css; charset=utf-8", true
	case ".svg":
		return "image/svg+xml", true
	case ".json", ".map":
		return "application/json", true
	case ".html", ".htm":
		return "text/html; charset=utf-8", true
	case ".txt":
		return "text/plain; charset=utf-8", true
	case ".xml":
		return "application/xml", true
	case ".webmanifest":
		return "application/manifest+json", true
	}
	if ct := http.DetectContentType(body); strings.HasPrefix(ct, "text/") {
		return ct, true
	}
	return "", false
}

// uiFiles 返回前端构建产物：默认是内嵌的 uiFS，测试可以替换为内存文件。
func (d *Dashboard) uiFiles() fs.FS {
	if d.ui != nil {
		return d.ui
	}
	return uiFS
}

// gzipAsset 返回内嵌资产的 gzip 字节：内嵌文件不可变，按文件名懒压缩一次后复用（Dashboard 实例上的缓存，不并发访问模板）。
func (d *Dashboard) gzipAsset(name string, body []byte) []byte {
	d.assetsMu.Lock()
	defer d.assetsMu.Unlock()
	if gz, ok := d.assetsGz[name]; ok {
		return gz
	}
	var out bytes.Buffer
	// 静态资产只压缩一次，用最高压缩级别换更小的传输体积（页面数据用 BestSpeed，见 gzipJSON）。
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		panic(err)
	}
	_, _ = zw.Write(body)
	_ = zw.Close()
	if d.assetsGz == nil {
		d.assetsGz = map[string][]byte{}
	}
	d.assetsGz[name] = out.Bytes()
	return out.Bytes()
}

// serveAsset 发送前端构建产物（ui/dist/assets/ 下带内容摘要的文件名，长期缓存；以及 ui/dist/ 下的图标）。
// 文本类资产在客户端接受 gzip 时发送压缩后的字节；带 Range 的请求照常由 ServeContent 处理（不对部分内容压缩）。
func (d *Dashboard) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "index.html" || strings.HasPrefix(name, ".") {
		d.notFound(w, r, "")
		return
	}
	body, err := fs.ReadFile(d.uiFiles(), "ui/dist/"+name)
	if err != nil {
		d.notFound(w, r, "")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if contentType, ok := compressibleAsset(name, body); ok {
		w.Header().Set("Vary", "Accept-Encoding")
		if len(body) >= assetGzipMinSize && r.Header.Get("Range") == "" && acceptsGzip(r) {
			gz := d.gzipAsset(name, body)
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return
			}
			if _, err := w.Write(gz); err != nil {
				d.log.Debug("发送压缩的静态资产", "name", name, "err", err)
			}
			return
		}
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(body))
}
