package github

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePrivateKey(t *testing.T) {
	key := testKey()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(typ string, der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}) }

	tests := []struct {
		name    string
		pem     []byte
		wantErr string // 为空表示应成功
	}{
		{"PKCS#1", encode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), ""},
		{"PKCS#8", encode("PRIVATE KEY", pkcs8), ""},
		{"不是 PEM", []byte("not a key"), "不是 PEM"},
		{"空输入", nil, "不是 PEM"},
		{"不是 RSA 的 PKCS#8", encode("PRIVATE KEY", ecDER), "不是 RSA"},
		{"内容损坏", encode("PRIVATE KEY", []byte("garbage")), "解析私钥"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePrivateKey(tt.pem)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !got.Equal(key) {
					t.Error("解析出的私钥与原私钥不同")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误应包含 %q，实际 %v", tt.wantErr, err)
			}
			if got != nil {
				t.Error("出错时应返回 nil")
			}
		})
	}
}

// App JWT 是 RS256 签名、iss 为 App ID 的 JWT，可以用 App 公钥验证。
func TestAppJWTSignature(t *testing.T) {
	f, c, now := newFakeAPI(t)
	f.handle("GET /app", jsonReply(http.StatusOK, map[string]any{"id": 42, "slug": "kiln", "name": "Kiln"}))
	app, err := c.GetApp(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != 42 || app.Slug != "kiln" || app.Name != "Kiln" {
		t.Errorf("GetApp = %+v", app)
	}
	jwt, ok := strings.CutPrefix(f.only(t).Header.Get("Authorization"), "Bearer ")
	if !ok {
		t.Fatalf("App 请求应使用 Bearer JWT")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应有 3 段：%q", jwt)
	}
	decode := func(s string) []byte {
		t.Helper()
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var header, claims map[string]any
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Errorf("JWT 头部：%v", header)
	}
	if claims["iss"] != "42" || claims["exp"] != float64(now.Add(jwtLifetime).Unix()) {
		t.Errorf("JWT claims：%v", claims)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, sum[:], decode(parts[2])); err != nil {
		t.Errorf("JWT 签名无法用 App 公钥验证：%v", err)
	}
}

// CreateToken 可以限定仓库和权限；为空时请求体中不带这些字段。
func TestCreateToken(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	tests := []struct {
		name     string
		repoIDs  []int64
		perms    map[string]string
		wantBody map[string]any
	}{
		{"全部仓库和权限", nil, nil, map[string]any{}},
		{"限定仓库和权限", []int64{3, 5}, map[string]string{"contents": "read"}, map[string]any{
			"repository_ids": []any{float64(3), float64(5)}, "permissions": map[string]any{"contents": "read"},
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := c.CreateToken(t.Context(), 8, tt.repoIDs, tt.perms)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(tok.Token, "tok") || !tok.ExpiresAt.Equal(f.start.Add(time.Hour)) {
				t.Errorf("token = %+v", tok)
			}
			r := f.tokens()[i]
			if r.Path != "/app/installations/8/access_tokens" || r.Method != http.MethodPost {
				t.Errorf("请求：%s", r.key())
			}
			body := r.jsonBody(t)
			if len(body) != len(tt.wantBody) {
				t.Errorf("请求体 = %v，应为 %v", body, tt.wantBody)
			}
			for k, want := range tt.wantBody {
				gb, _ := json.Marshal(body[k])
				wb, _ := json.Marshal(want)
				if string(gb) != string(wb) {
					t.Errorf("请求体 %s = %s，应为 %s", k, gb, wb)
				}
			}
		})
	}
	// CreateToken 每次都签发新 token，不使用缓存
	if n := len(f.tokens()); n != len(tests) {
		t.Errorf("签发 %d 次，应为 %d 次", n, len(tests))
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("不应有其他请求，实际 %d 个", n)
	}
}

// RevokeToken 用被吊销的 token 自己认证（DELETE /installation/token）；token 已经失效（401）时同样成功，其它错误原样返回。
func TestRevokeToken(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	c := &Client{AppID: 1, Key: testKey(), APIURL: srv.URL}
	if err := c.RevokeToken(t.Context(), 8, "ghs_secret"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "token ghs_secret" || gotMethod != http.MethodDelete || gotPath != "/installation/token" {
		t.Errorf("请求 = %s %s（%s）", gotMethod, gotPath, gotAuth)
	}
	status = http.StatusUnauthorized
	if err := c.RevokeToken(t.Context(), 8, "ghs_gone"); err != nil {
		t.Errorf("已经失效的 token：%v", err)
	}
	status = http.StatusInternalServerError
	if err := c.RevokeToken(t.Context(), 8, "ghs_x"); err == nil {
		t.Error("GitHub 出错时应返回错误")
	}
}
