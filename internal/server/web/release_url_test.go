package web

import "testing"

// 外部上传服务返回的 assets[].url 不可信：只保留 http(s) 且有主机名的链接，其他的从数据中删除
// （模板因此不会渲染成可点击的链接），非对象元素原样保留。清洗后的形态必须能被前端 uploadDetailOf 解析：
// 不安全的 asset 保留 name、没有 url，同批其他 asset 和 latest 不受影响。
func TestSanitizeReleaseDetailURLs(t *testing.T) {
	t.Parallel()
	detail := map[string]any{
		"latest": true,
		"assets": []any{
			map[string]any{"name": "ok.zip", "url": "https://example.com/ok.zip"},
			map[string]any{"name": "bad.zip", "url": "javascript:void(0)"},
			map[string]any{"name": "data.zip", "url": "data:text/html,x"},
			map[string]any{"name": "nourl.zip"},
			"not-an-object",
		},
	}
	sanitizeReleaseDetailURLs(detail)
	assets, ok := detail["assets"].([]any)
	if !ok || len(assets) != 5 {
		t.Fatalf("assets = %#v", detail["assets"])
	}
	byName := map[string]map[string]any{}
	for _, a := range assets {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := m["name"].(string); ok {
			byName[name] = m
		}
	}
	if got := byName["ok.zip"]["url"]; got != "https://example.com/ok.zip" {
		t.Errorf("安全的地址应保留：%v", got)
	}
	bad, ok := byName["bad.zip"]
	if !ok || bad["name"] != "bad.zip" {
		t.Fatalf("不安全的 asset 应保留 name：%#v", bad)
	}
	if _, ok := bad["url"]; ok {
		t.Errorf("不安全地址应删除：%#v", bad)
	}
	for _, name := range []string{"data.zip", "nourl.zip"} {
		if _, ok := byName[name]["url"]; ok {
			t.Errorf("%s 不应有 url", name)
		}
	}
	if detail["latest"] != true {
		t.Errorf("latest 应保留：%#v", detail["latest"])
	}
	// 不是 map 的 detail 或没有 assets 时不 panic、不做修改
	sanitizeReleaseDetailURLs(nil)
	sanitizeReleaseDetailURLs(map[string]any{"assets": "x"})
}
