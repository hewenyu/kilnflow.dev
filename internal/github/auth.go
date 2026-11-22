package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	// jwtLifetime 是 App JWT 的有效期（GitHub 允许的上限是 10 分钟）。
	jwtLifetime = 9 * time.Minute
	// jwtClockSkew 把 iat 往前拨，容忍本机与 GitHub 的时钟偏差。
	jwtClockSkew = 60 * time.Second
	// jwtRefreshBefore 是 JWT 剩余有效期低于该值时重新签发。
	jwtRefreshBefore = time.Minute
	// tokenRefreshBefore 是 installation token 剩余有效期低于该值时重新签发。
	tokenRefreshBefore = 5 * time.Minute
	// TokenLifetime 是 GitHub installation token 的有效期；只用于估计没有拿到响应的 token 何时过期。
	TokenLifetime = time.Hour
)

// ParsePrivateKey 解析 App 私钥（PKCS#1 或 PKCS#8 PEM）。
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("私钥不是 PEM 格式")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥：%w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("私钥不是 RSA 密钥")
	}
	return rk, nil
}

// appJWT 返回 App JWT（有效期 9 分钟，缓存复用）。
func (c *Client) appJWT() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.jwt != "" && c.jwtExp.Sub(now) > jwtRefreshBefore {
		return c.jwt, nil
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtClockSkew).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(c.AppID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("编码 JWT：%w", err)
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("签名 JWT：%w", err)
	}
	c.jwt = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	c.jwtExp = now.Add(jwtLifetime)
	return c.jwt, nil
}

// AccessToken 是 installation token。
type AccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateToken 签发 installation token。repoIDs 为空表示全部仓库；perms 为空表示 App 的全部权限。
//
// 结果分三类：没有错误时拿到了 token（含 GitHub 给出的过期时间）；NotApplied(err) 为真时确定没有签发（GitHub 明确拒绝、
// 仍在限流中或请求没有交给 HTTP 客户端）；其他错误是结果不明（请求已经发出，没有读到可用的响应），GitHub 上可能有一枚
// 谁也没拿到的 token，只能等它过期。响应中有 token 却没有过期时间时不交出，先吊销这枚 token，再按结果不明返回（吊销失败
// 也一样：错误中只有吊销失败的说明，没有它的错误类型）。
// 错误只含安全的信息（tokenEndpointError），不带响应正文。
func (c *Client) CreateToken(ctx context.Context, installationID int64, repoIDs []int64, perms map[string]string) (*AccessToken, error) {
	body := map[string]any{}
	if len(repoIDs) > 0 {
		body["repository_ids"] = repoIDs
	}
	if len(perms) > 0 {
		body["permissions"] = perms
	}
	what := fmt.Sprintf("签发 installation %d 的 token", installationID)
	tok, err := call[AccessToken](ctx, c.App, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), body)
	if err != nil {
		return nil, tokenEndpointError(what, err)
	}
	switch {
	case tok.Token == "":
		return nil, fmt.Errorf("%s：响应中没有 token，结果不明", what)
	case tok.ExpiresAt.IsZero():
		// 拿到了 token 就说明签发已经生效：无论补吊销成败都按结果不明返回。补吊销的错误是另一个请求（DELETE）的结论，只作
		// 诊断文本（只用 Error() 的文本），不带出它的类型（*RateLimitError、*APIError 会让 NotApplied 误判为"确定没有签发"、允许同键再签）。
		if rerr := c.RevokeToken(ctx, installationID, tok.Token); rerr != nil {
			return nil, fmt.Errorf("%s：响应中没有过期时间，不交出；吊销这枚 token 失败（到期自然失效）：%s", what, rerr.Error())
		}
		return nil, fmt.Errorf("%s：响应中没有过期时间，不交出，已吊销这枚 token", what)
	}
	return tok, nil
}

// RevokeToken 吊销一个 installation token（DELETE /installation/token，用这个 token 自己认证）。installationID 只用作限流键。
// token 已经失效（过期、已经吊销，GitHub 返回 401）时同样返回 nil：目的已经达到。错误只含安全的信息（tokenEndpointError）。
func (c *Client) RevokeToken(ctx context.Context, installationID int64, token string) error {
	err := c.do(ctx, installationID, "token "+token, http.MethodDelete, "/installation/token", nil, nil)
	if err == nil || isStatus(err, http.StatusUnauthorized) {
		return nil
	}
	return tokenEndpointError(fmt.Sprintf("吊销 installation %d 的 token", installationID), err)
}

// tokenEndpointError 把凭据端点（签发、吊销）的错误换成只含安全信息的错误：错误响应的正文和底层错误的文本（例如解析
// 响应时的错误会带上响应中的值）都可能含 token，一律不带出。保留判定所需的类型和元数据：*RateLimitError（种类、恢复
// 时间）、*APIError（状态码、方法、地址）、请求没有发出（NotApplied）、context 的取消和超时；其余错误只说明没有读到
// 可用的响应（结果不明）。
func tokenEndpointError(what string, err error) error {
	if rl, ok := errors.AsType[*RateLimitError](err); ok {
		safe := *rl
		if rl.API != nil {
			safe.API = safeAPIError(rl.API)
		}
		return &safe
	}
	if ae, ok := errors.AsType[*APIError](err); ok {
		return safeAPIError(ae)
	}
	if _, ok := errors.AsType[*notSentError](err); ok {
		return &notSentError{fmt.Errorf("%s：请求没有发出", what)}
	}
	for _, ce := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, ce) {
			return fmt.Errorf("%s：没有读到响应：%w", what, ce)
		}
	}
	return fmt.Errorf("%s：没有读到可用的响应（网络错误或响应无法解析）", what)
}

// safeAPIError 返回只有状态码、方法和地址的 APIError（消息用状态码的标准说明，不用响应正文）。
func safeAPIError(ae *APIError) *APIError {
	return &APIError{Status: ae.Status, Method: ae.Method, URL: ae.URL, Message: http.StatusText(ae.Status)}
}

// installationToken 返回缓存的完整权限 installation token（过期前 5 分钟刷新）。
func (c *Client) installationToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[int64]cachedToken{}
	}
	t, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && t.exp.Sub(c.now()) > tokenRefreshBefore {
		return t.token, nil
	}
	tok, err := c.CreateToken(ctx, installationID, nil, nil)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: tok.Token, exp: tok.ExpiresAt}
	c.mu.Unlock()
	return tok.Token, nil
}

// dropToken 清掉缓存的 installation token（例如 token 已被吊销）。
func (c *Client) dropToken(installationID int64) {
	c.mu.Lock()
	delete(c.tokens, installationID)
	c.mu.Unlock()
}

// Inst 以某个 installation 的身份调用 API。
func (c *Client) Inst(id int64) *Inst { return &Inst{c: c, ID: id} }

// InstWithToken 以给定的 installation token 调用 API（例如按操作签发、限定仓库和权限的 token）：不读、不写缓存的
// installation token；token 被拒绝（401）时不换新、不重试，直接返回错误。token 为空时每个请求都在发出前失败
// （NotApplied 为真），不会改用缓存的完整权限 token。
func (c *Client) InstWithToken(id int64, token string) *Inst {
	return &Inst{c: c, ID: id, fixed: true, token: token}
}

// Inst 是 installation 级的客户端。
type Inst struct {
	c  *Client
	ID int64
	// fixed 为真时只用 token 认证（InstWithToken，固定凭据），否则用缓存的完整权限 token（共用缓存）。
	fixed bool
	token string
}
