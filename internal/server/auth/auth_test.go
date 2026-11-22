package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func testKeys() *Keys {
	return &Keys{tokenKey: []byte("token-key"), casKey: []byte("cas-key"), adminToken: "admin-token-0123456789"}
}

// signRaw 用 k 的 token 密钥为任意 body 签名，构造"签名正确但内容有问题"的 token。
func signRaw(k *Keys, body string) string {
	m := hmac.New(sha256.New, k.tokenKey)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// flipLast 改掉 s 的最后一个字符（保证与原值不同）。
func flipLast(s string) string {
	last := byte('A')
	if s[len(s)-1] == 'A' {
		last = 'B'
	}
	return s[:len(s)-1] + string(last)
}

// 第一次 Load 生成密钥并保存到 kv 表，之后的 Load（Server 重启）读到同一套密钥：
// 重启前签发的 access token、CAS 地址仍然有效；另一个数据库的密钥不同。
func TestLoad(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := store.New(pgtest.Pool(t))
	first, err := Load(ctx, db, "admin-1")
	if err != nil {
		t.Fatalf("第一次 Load：%v", err)
	}
	if len(first.tokenKey) == 0 || len(first.casKey) == 0 || string(first.tokenKey) == string(first.casKey) {
		t.Fatalf("应生成两把不同的非空密钥：token=%q cas=%q", first.tokenKey, first.casKey)
	}
	again, err := Load(ctx, db, "admin-2")
	if err != nil {
		t.Fatalf("第二次 Load：%v", err)
	}
	tok := first.SignToken(Claims{Sub: 1, Exp: time.Now().Add(time.Minute).Unix(), Aud: "runner-api"})
	if _, err := again.VerifyToken(tok, "runner-api"); err != nil {
		t.Fatalf("重启后应能校验之前签发的 token：%v", err)
	}
	u, _ := url.Parse(first.CASURL(CASOpGet, "sha256:abc", time.Hour))
	if !again.CheckCASSig(CASOpGet, "sha256:abc", u.Query().Get("exp"), u.Query().Get("sig")) {
		t.Fatal("重启后应能校验之前签发的 CAS 地址")
	}
	if !again.ValidAdminToken("admin-2") || again.ValidAdminToken("admin-1") {
		t.Fatal("admin token 取自参数，不保存在数据库中")
	}

	other, err := Load(ctx, store.New(pgtest.Pool(t)), "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VerifyToken(tok, "runner-api"); err == nil {
		t.Fatal("另一个数据库生成的密钥不应能校验这个 token")
	}
}

func TestKeys_VerifyToken(t *testing.T) {
	t.Parallel()
	k := testKeys()
	valid := k.SignToken(Claims{Sub: 7, Exp: time.Now().Add(time.Minute).Unix(), Aud: "runner-api"})
	body, sig, _ := strings.Cut(valid, ".")
	// 把 Sub 改成 8，签名不变
	forgedBody := base64.RawURLEncoding.EncodeToString(store.MustJSON(Claims{Sub: 8, Exp: time.Now().Add(time.Minute).Unix(), Aud: "runner-api"}))
	tests := []struct {
		name    string
		keys    *Keys
		tok     string
		aud     string
		wantErr string // 空表示应成功
	}{
		{"有效", k, valid, "runner-api", ""},
		{"用途不符", k, valid, "dashboard", "用途不符"},
		{"密钥不同", NewKeys([]byte("other"), nil, ""), valid, "runner-api", "签名无效"},
		{"已过期", k, k.SignToken(Claims{Sub: 7, Exp: time.Now().Add(-time.Second).Unix(), Aud: "runner-api"}), "runner-api", "已过期"},
		{"篡改内容", k, forgedBody + "." + sig, "runner-api", "签名无效"},
		{"篡改签名", k, body + "." + flipLast(sig), "runner-api", "签名无效"},
		{"缺少签名", k, body, "runner-api", "格式错误"},
		{"空 token", k, "", "runner-api", "格式错误"},
		{"签名正确但不是 base64", k, signRaw(k, "!!!"), "runner-api", "格式错误"},
		{"签名正确但不是 JSON", k, signRaw(k, base64.RawURLEncoding.EncodeToString([]byte("not json"))), "runner-api", "无法解析"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := tc.keys.VerifyToken(tc.tok, tc.aud)
			if tc.wantErr == "" {
				if err != nil || c.Sub != 7 || c.Aud != "runner-api" {
					t.Fatalf("VerifyToken = %+v, %v，期望成功且 Sub=7", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("VerifyToken 错误 = %v，期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// 会话签名与 access token 用同一把密钥，但加了前缀，不能互相冒用。
func TestKeys_SessionSig(t *testing.T) {
	t.Parallel()
	k := testKeys()
	if a, b := k.SessionSig("u:1"), testKeys().SessionSig("u:1"); a != b {
		t.Fatal("同一密钥、同一载荷的签名应稳定")
	}
	if k.SessionSig("u:1") == k.SessionSig("u:2") {
		t.Fatal("不同载荷的签名应不同")
	}
	if k.SessionSig("u:1") == NewKeys([]byte("other"), nil, "").SessionSig("u:1") {
		t.Fatal("不同密钥的签名应不同")
	}
	_, tokSig, _ := strings.Cut(signRaw(k, "u:1"), ".")
	if k.SessionSig("u:1") == tokSig {
		t.Fatal("会话签名不应与对同一内容的 token 签名相同")
	}
}

func TestKeys_CASURL(t *testing.T) {
	t.Parallel()
	k := testKeys()
	const digest = "sha256:abc"
	parse := func(t *testing.T, raw string) (path string, q url.Values) {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u.Path, u.Query()
	}
	path, put := parse(t, k.CASURL(CASOpPut, digest, time.Hour))
	if path != "/cas/"+digest || put.Get("op") != CASOpPut {
		t.Fatalf("地址应为 /cas/<digest>?op=put：%s %v", path, put)
	}
	_, get := parse(t, k.CASURL(CASOpGet, digest, time.Hour))
	_, expired := parse(t, k.CASURL(CASOpGet, digest, -time.Second))
	later := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)

	tests := []struct {
		name          string
		keys          *Keys
		op, digest    string
		exp, sig      string
		want          bool
		failureReason string
	}{
		{"上传地址用于上传", k, CASOpPut, digest, put.Get("exp"), put.Get("sig"), true, ""},
		{"下载地址用于下载", k, CASOpGet, digest, get.Get("exp"), get.Get("sig"), true, ""},
		{"上传地址不能下载", k, CASOpGet, digest, put.Get("exp"), put.Get("sig"), false, "操作不同"},
		{"下载地址不能上传", k, CASOpPut, digest, get.Get("exp"), get.Get("sig"), false, "操作不同"},
		{"换成其他对象", k, CASOpGet, "sha256:abd", get.Get("exp"), get.Get("sig"), false, "digest 不同"},
		{"已过期", k, CASOpGet, digest, expired.Get("exp"), expired.Get("sig"), false, "过期"},
		{"延长有效期", k, CASOpGet, digest, later, get.Get("sig"), false, "exp 被改"},
		{"exp 不是数字", k, CASOpGet, digest, "soon", get.Get("sig"), false, "exp 无效"},
		{"缺少签名", k, CASOpGet, digest, get.Get("exp"), "", false, "没有签名"},
		{"密钥不同", NewKeys(nil, []byte("other"), ""), CASOpGet, digest, get.Get("exp"), get.Get("sig"), false, "密钥不同"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.keys.CheckCASSig(tc.op, tc.digest, tc.exp, tc.sig); got != tc.want {
				t.Fatalf("CheckCASSig = %v，期望 %v（%s）", got, tc.want, tc.failureReason)
			}
		})
	}
}

func TestKeys_CheckAdmin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		keys   *Keys
		header string // 空表示不设置 Authorization
		ok     bool
	}{
		{"正确的 token", testKeys(), "Bearer admin-token-0123456789", true},
		{"缺少 token", testKeys(), "", false},
		{"错误的 token", testKeys(), "Bearer admin-token-wrong", false},
		{"token 前缀", testKeys(), "Bearer admin-token", false},
		{"只有 Bearer", testKeys(), "Bearer ", false},
		{"裸 token 没有 Bearer 前缀", testKeys(), "admin-token-0123456789", false},
		{"没有配置 admin token 时拒绝空 token", NewKeys(nil, nil, ""), "Bearer ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			if tc.header != "" {
				h.Set("Authorization", tc.header)
			}
			if err := tc.keys.CheckAdmin(h); (err == nil) != tc.ok {
				t.Fatalf("CheckAdmin(%q) = %v，期望通过 = %v", tc.header, err, tc.ok)
			}
		})
	}
	if testKeys().ValidAdminToken("") {
		t.Fatal("空 token 应无效")
	}
}
