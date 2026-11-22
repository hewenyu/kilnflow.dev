package fakegithub

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// 模拟 internal/github/auth.go 用到的接口：签发、吊销 installation token。

// Token 是签发过的一个 installation token：所属的 installation、限定的仓库（为空表示全部）、权限（为空表示 App 的全部
// 权限）、过期时间和是否已经吊销（DELETE /installation/token）。
type Token struct {
	Token        string
	Installation int64
	RepoIDs      []int64
	Permissions  map[string]string
	ExpiresAt    time.Time
	Revoked      bool
}

// tokenLifetime 与 GitHub 相同：installation token 1 小时后过期。
const tokenLifetime = time.Hour

// RevokeTokenRoute 是吊销 installation token 的请求（SetFault 用它注入吊销失败）。
const RevokeTokenRoute = "DELETE /installation/token"

// CreateTokenRoute 返回签发 installation 的 token 的请求（SetFault 用它注入签发失败、已签发但响应丢失）。
func CreateTokenRoute(installation int64) string {
	return fmt.Sprintf("POST /app/installations/%d/access_tokens", installation)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	inst, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &body) {
		return
	}
	if !s.reposInInstallation(inst, body.RepositoryIDs) {
		// 与 GitHub 相同：显式列出的仓库必须在这个 installation 的授权范围内（只检查本模拟登记过的仓库）
		writeMessage(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
		return
	}
	tok := &Token{
		Token: fmt.Sprintf("ghs_fake%d", s.id()), Installation: inst, RepoIDs: body.RepositoryIDs, Permissions: body.Permissions,
		ExpiresAt: time.Now().Add(tokenLifetime).UTC(),
	}
	s.mu.Lock()
	s.tokens[tok.Token] = inst
	s.issued = append(s.issued, tok)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok.Token, "expires_at": tok.ExpiresAt})
}

// reposInInstallation 判断 ids 中本模拟登记过的仓库是否都属于 installation inst。没有登记的仓库 ID 放行（不少测试只在
// Kiln 中登记依赖仓库），所以"不存在的仓库被拒绝"不能用本模拟证明。
func (s *Server) reposInInstallation(inst int64, ids []int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rp := range s.repos {
		if slices.Contains(ids, rp.ID) && rp.Installation != inst {
			return false
		}
	}
	return true
}

// revokeToken 吊销请求所用的 token（DELETE /installation/token）：未知或已经吊销的 token 返回 401，与 GitHub 相同。
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.issued {
		if t.Token == tok && !t.Revoked {
			t.Revoked = true
			delete(s.tokens, tok)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeMessage(w, http.StatusUnauthorized, "Bad credentials")
}

// checkToken 按 GitHub 的规则校验请求所用的 installation token，不通过时写出错误响应并返回 false：已经吊销或过期的 token
// 返回 401；写请求（perms 非空）的目标仓库不属于 token 的 installation、token 限定了仓库而不含路径中的仓库，或者限定了
// 权限而没有 perms 中任一项的 write，返回 403。
//
// 这不是完整的权限模型，不能用来证明：读请求不按仓库和权限校验（限定了仓库的 token 也能读别的仓库）；App JWT、不是本模拟
// 签发的 token 和其他认证形式一律放行，所以"任意错误的 token 都被拒绝"也证明不了。要验证读授权，测试应直接断言签发
// 记录（IssuedTokens）中的仓库和权限。
func (s *Server) checkToken(w http.ResponseWriter, r *http.Request, perms []string) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok {
		return true
	}
	status := s.tokenStatus(tok, repoName(r), perms)
	switch status {
	case http.StatusUnauthorized:
		writeMessage(w, status, "Bad credentials")
	case http.StatusForbidden:
		writeMessage(w, status, "Resource not accessible by integration")
	}
	return status == 0
}

// tokenStatus 返回 token 访问仓库 repo（owner/name）应得的错误状态码，通过时为 0。
func (s *Server) tokenStatus(tok, repo string, perms []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.issued, func(t *Token) bool { return t.Token == tok })
	if i < 0 {
		return 0
	}
	t := s.issued[i]
	if t.Revoked || !time.Now().Before(t.ExpiresAt) {
		return http.StatusUnauthorized
	}
	if len(perms) == 0 {
		return 0
	}
	if rp := s.repos[strings.ToLower(repo)]; rp != nil && (rp.Installation != t.Installation || len(t.RepoIDs) > 0 && !slices.Contains(t.RepoIDs, rp.ID)) {
		return http.StatusForbidden
	}
	if t.Permissions != nil && !slices.ContainsFunc(perms, func(p string) bool { return t.Permissions[p] == "write" }) {
		return http.StatusForbidden
	}
	return 0
}

// IssuedTokens 返回签发过的 installation token（按签发顺序的副本），测试据此断言 token 的仓库范围、权限和是否已经吊销。
func (s *Server) IssuedTokens() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Token, 0, len(s.issued))
	for _, t := range s.issued {
		c := *t
		c.RepoIDs = slices.Clone(t.RepoIDs)
		c.Permissions = maps.Clone(t.Permissions)
		out = append(out, c)
	}
	return out
}

// IssuedToken 返回签发过的 token tok；没有签发过时 ok 为 false。
func (s *Server) IssuedToken(tok string) (Token, bool) {
	for _, t := range s.IssuedTokens() {
		if t.Token == tok {
			return t, true
		}
	}
	return Token{}, false
}
