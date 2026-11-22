// Package auth 管理 Server 的签名密钥（PLAN #9.3）：Runner access token 的签发与校验、
// CAS 预签名 URL、Dashboard 会话签名、CLI admin token 的校验，以及 Runner 注册令牌的创建。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/randtoken"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// kv 表中保存密钥的键。
const (
	tokenKeyName = "runner_token_key"
	casKeyName   = "cas_url_key"
)

// Keys 持有 Server 的签名密钥和 admin token。
type Keys struct {
	// tokenKey 用于签发 Runner access token 和 Dashboard 会话。
	tokenKey []byte
	// casKey 用于 CAS 预签名 URL。
	casKey     []byte
	adminToken string
}

// NewKeys 用给定的密钥和 admin token 创建 Keys，不读写数据库（供其他包的测试使用；Server 用 Load）。
func NewKeys(tokenKey, casKey []byte, adminToken string) *Keys {
	return &Keys{tokenKey: tokenKey, casKey: casKey, adminToken: adminToken}
}

// Load 从数据库读取（首次启动时生成）签名密钥。adminToken 是 CLI 使用的 admin token。
func Load(ctx context.Context, db *store.DB, adminToken string) (*Keys, error) {
	k := &Keys{adminToken: adminToken}
	var err error
	if k.tokenKey, err = loadSecretKey(ctx, db, tokenKeyName); err != nil {
		return nil, err
	}
	if k.casKey, err = loadSecretKey(ctx, db, casKeyName); err != nil {
		return nil, err
	}
	return k, nil
}

// loadSecretKey 读取或生成保存在 kv 表中的随机密钥（用于签发 token 和 CAS 预签名 URL）。
func loadSecretKey(ctx context.Context, db *store.DB, name string) ([]byte, error) {
	v := randtoken.New(32)
	if _, err := db.Exec(ctx, `INSERT INTO kv (key, value) VALUES ($1,$2) ON CONFLICT DO NOTHING`, name, v); err != nil {
		return nil, fmt.Errorf("生成密钥 %s：%w", name, err)
	}
	if err := db.QueryRow(ctx, `SELECT value FROM kv WHERE key=$1`, name).Scan(&v); err != nil {
		return nil, fmt.Errorf("读取密钥 %s：%w", name, err)
	}
	return []byte(v), nil
}

// Claims 是 access token 携带的声明。
type Claims struct {
	Sub int64  `json:"sub"`
	Exp int64  `json:"exp"`
	Aud string `json:"aud"`
}

// SignToken 签发 token。
func (k *Keys) SignToken(c Claims) string {
	body := base64.RawURLEncoding.EncodeToString(store.MustJSON(c))
	m := hmac.New(sha256.New, k.tokenKey)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifyToken 校验 token 的签名、用途（aud）和有效期。
func (k *Keys) VerifyToken(tok, aud string) (*Claims, error) {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errors.New("token 格式错误")
	}
	m := hmac.New(sha256.New, k.tokenKey)
	m.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return nil, errors.New("token 签名无效")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("token 格式错误：%w", err)
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("token 内容无法解析：%w", err)
	}
	if c.Aud != aud {
		return nil, errors.New("token 用途不符")
	}
	if time.Now().Unix() > c.Exp {
		return nil, errors.New("token 已过期")
	}
	return &c, nil
}

// SessionSig 返回 Dashboard 会话（及 CSRF token）载荷的签名。
func (k *Keys) SessionSig(payload string) string {
	m := hmac.New(sha256.New, k.tokenKey)
	m.Write([]byte("session:" + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CAS 预签名 URL：/cas/<digest>?op=put|get&exp=<unix>&sig=<hmac>

// CAS 预签名 URL 的操作（CASURL、CheckCASSig 的 op）。
const (
	// CASOpPut 是上传（PUT）。
	CASOpPut = "put"
	// CASOpGet 是下载（GET、HEAD）。
	CASOpGet = "get"
)

// CASURL 返回 CAS 对象的预签名 URL（相对路径），ttl 后失效。op 为 CASOpPut 或 CASOpGet。
func (k *Keys) CASURL(op, digest string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("/cas/%s?op=%s&exp=%d&sig=%s", digest, op, exp, k.casSig(op, digest, exp))
}

func (k *Keys) casSig(op, digest string, exp int64) string {
	m := hmac.New(sha256.New, k.casKey)
	fmt.Fprintf(m, "%s\n%s\n%d", op, digest, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CheckCASSig 校验预签名 URL 的签名和有效期。
func (k *Keys) CheckCASSig(op, digest, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sig), []byte(k.casSig(op, digest, exp))) == 1
}

// ValidAdminToken 判断 tok 是否为 admin token（Dashboard 用 admin token 登录时使用）。
func (k *Keys) ValidAdminToken(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(k.adminToken)) == 1
}

// CheckAdmin 校验 CLI 请求头中的 admin token。
func (k *Keys) CheckAdmin(h http.Header) error {
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || !k.ValidAdminToken(tok) {
		return errors.New("admin token 无效")
	}
	return nil
}
