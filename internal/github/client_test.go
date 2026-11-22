package github

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient 启动一个模拟 GitHub：第一个签发的 token 会被上传接口拒绝（401）。
func newTestClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issued, uploads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		n := issued.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("t%d", n), "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /repos/o/r/releases/1/assets", func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") == "token t1" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Bad credentials"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "name": r.URL.Query().Get("name"), "size": len(data)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{AppID: 1, Key: key, APIURL: srv.URL, UploadURL: srv.URL}, &uploads
}

func TestUploadRetryAfterUnauthorized(t *testing.T) {
	const content = "hello asset"
	file := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		open      func(t *testing.T) io.Reader
		wantErr   bool
		wantCalls int32
	}{
		{"可以 Seek 的文件重试成功", func(t *testing.T) io.Reader {
			t.Helper()
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
			return f
		}, false, 2},
		{"不能 Seek 的 reader 不重试", func(*testing.T) io.Reader {
			return io.MultiReader(strings.NewReader(content))
		}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, uploads := newTestClient(t)
			a, err := client.Inst(1).UploadReleaseAsset(t.Context(), "o/r", 1, "a.bin", c.open(t), int64(len(content)))
			if c.wantErr {
				if !isStatus(err, http.StatusUnauthorized) {
					t.Fatalf("应返回 401，实际 %v", err)
				}
				if a != nil {
					t.Fatalf("出错时应返回 nil，实际 %+v", a)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if a.Size != int64(len(content)) {
					t.Fatalf("重试时请求体不完整：size=%d", a.Size)
				}
			}
			if got := uploads.Load(); got != c.wantCalls {
				t.Fatalf("上传请求 %d 次，应为 %d 次", got, c.wantCalls)
			}
		})
	}
}

func TestSignature(t *testing.T) {
	secret, body := []byte("s"), []byte(`{"a":1}`)
	sig := Sign(secret, body)
	if !VerifySignature(secret, body, sig) {
		t.Fatal("签名校验失败")
	}
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 {
		t.Errorf("签名格式：%q", sig)
	}
	if VerifySignature(secret, []byte(`{"a":2}`), sig) {
		t.Error("内容被篡改后不应通过校验")
	}
	for _, bad := range []string{"", "sha1=00", "sha256=zz", Sign([]byte("x"), body)} {
		if VerifySignature(secret, body, bad) {
			t.Errorf("%q 不应通过校验", bad)
		}
	}
	// 没有配置密钥时任何人都能算出签名：一律拒绝
	if VerifySignature(nil, body, Sign(nil, body)) {
		t.Error("空密钥不应通过校验")
	}
}

// appJWT 的 iat、exp 和缓存都按 Client 的时钟计算。
func TestAppJWTUsesClock(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := &Client{AppID: 7, Key: key, clock: func() time.Time { return now }}
	claims := func(jwt string) map[string]any {
		t.Helper()
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Fatalf("JWT 格式不对：%q", jwt)
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first, err := c.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	m := claims(first)
	if m["iat"] != float64(now.Add(-jwtClockSkew).Unix()) || m["exp"] != float64(now.Add(jwtLifetime).Unix()) || m["iss"] != "7" {
		t.Fatalf("claims 不对：%v", m)
	}
	now = now.Add(jwtLifetime - jwtRefreshBefore - time.Second)
	if again, _ := c.appJWT(); again != first {
		t.Fatal("有效期内应复用缓存的 JWT")
	}
	now = now.Add(2 * time.Second)
	renewed, err := c.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first || claims(renewed)["iat"] != float64(now.Add(-jwtClockSkew).Unix()) {
		t.Fatal("快到期时应按当前时钟重新签发")
	}
}
