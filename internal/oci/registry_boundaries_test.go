package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// 外部上传地址和重定向目标都不能用认证质询取得 registry 的用户名和密码。
func TestForeignRegistryChallenges(t *testing.T) {
	t.Parallel()
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirect=%v", redirect), func(t *testing.T) {
			t.Parallel()
			var tokenCalls, foreignCredentials atomic.Int32
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					foreignCredentials.Add(1)
				}
				if r.URL.Path == "/token" {
					tokenCalls.Add(1)
					fmt.Fprint(w, `{"token":"foreign-token"}`)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer storage.Close()
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, storage.URL+"/upload", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Location", storage.URL+"/upload")
				w.WriteHeader(http.StatusAccepted)
			}))
			defer registry.Close()
			name, err := ParseName(strings.TrimPrefix(registry.URL, "http://") + "/team/app")
			if err != nil {
				t.Fatal(err)
			}
			c := NewClient(name, testUser, testPass, nil)
			c.backoff = 0
			if redirect {
				_, _, err = c.ManifestDigest(t.Context(), "v1")
			} else {
				body := "blob"
				err = c.PushBlob(t.Context(), Descriptor{Digest: Digest([]byte(body)), Size: int64(len(body))}, func(context.Context) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader(body)), nil
				})
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v，期望 ErrUnauthorized", err)
			}
			if got := tokenCalls.Load(); got != 0 {
				t.Errorf("向外部质询指定的 token 地址请求了 %d 次", got)
			}
			if got := foreignCredentials.Load(); got != 0 {
				t.Errorf("向外部主机发送凭据 %d 次", got)
			}
		})
	}
}

func TestManifestDigestSizeLimit(t *testing.T) {
	t.Parallel()
	for _, size := range []int{maxManifestSize, maxManifestSize + 1, maxManifestSize + 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			body := strings.Repeat("x", size)
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					fmt.Fprint(w, body)
				}
			}))
			defer registry.Close()
			name, err := ParseName(strings.TrimPrefix(registry.URL, "http://") + "/team/app")
			if err != nil {
				t.Fatal(err)
			}
			c := NewClient(name, "", "", nil)
			digest, found, err := c.ManifestDigest(t.Context(), "v1")
			if size > maxManifestSize {
				if err == nil || found || digest != "" {
					t.Fatalf("超限 manifest 返回了 %q, %v, %v", digest, found, err)
				}
				return
			}
			if err != nil || !found || digest != Digest([]byte(body)) {
				t.Fatalf("上限内 manifest 返回了 %q, %v, %v", digest, found, err)
			}
		})
	}
}
