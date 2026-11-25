// Package fakegithub 是一个用本地 git 仓库模拟 GitHub REST API 的最小实现，
// 只覆盖 kiln-server 用到的接口，用于单元测试和本地试用。
//
// 文件划分：fake.go（状态和对外方法）、handlers.go（路由和通用辅助）、webhook.go（发送 webhook）、
// ratelimit.go（模拟限流）；各接口的实现与 internal/github 的文件对应：auth.go、repos.go、git.go、
// checks.go、releases.go、pulls.go、reviews.go、issues.go（issue 的评论、关闭，以及 GraphQL 的 closingIssuesReferences）。
package fakegithub

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// unhandledPrefix 是没有对应路由的请求在请求记录中的前缀。
const unhandledPrefix = "UNHANDLED "

// Repo 是一个模拟的仓库，内容来自本地 git 仓库 Dir。
type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Dir           string
	Private       bool
	DefaultBranch string
	// Installation 是仓库所属的 installation；0 表示 Server.InstallationID。
	Installation int64
}

// FullName 返回 owner/name。
func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }

// CheckRun 是记录下来的 Check Run。
type CheckRun struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	HeadSHA    string          `json:"head_sha"`
	ExternalID string          `json:"external_id"`
	DetailsURL string          `json:"details_url"`
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion"`
	Output     json.RawMessage `json:"output,omitempty"`
	// StartedAt、CompletedAt 与 GitHub 相同：创建时没有给 started_at 就用创建时间。
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Repo        string     `json:"-"`
	UpdateCount int        `json:"-"`
}

// Asset 是 Release 的一个 asset。
type Asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
	Data   []byte `json:"-"`
}

// Release 是模拟的 Release。
type Release struct {
	ID              int64    `json:"id"`
	TagName         string   `json:"tag_name"`
	TargetCommitish string   `json:"target_commitish"`
	Name            string   `json:"name"`
	Body            string   `json:"body"`
	Draft           bool     `json:"draft"`
	Prerelease      bool     `json:"prerelease"`
	MakeLatest      string   `json:"make_latest,omitempty"`
	HTMLURL         string   `json:"html_url"`
	Assets          []*Asset `json:"assets"`
	Repo            string   `json:"-"`
}

// clone 返回可以在锁外读取的副本（asset 列表也复制一份）。
func (r *Release) clone() Release {
	c := *r
	c.Assets = make([]*Asset, len(r.Assets))
	for i, a := range r.Assets {
		ac := *a
		c.Assets[i] = &ac
	}
	return c
}

// Server 是模拟的 GitHub。
type Server struct {
	*httptest.Server
	AppID          int64
	AppSlug        string
	InstallationID int64
	Account        string
	AccountID      int64

	nextID atomic.Int64

	mu        sync.Mutex
	repos     map[string]*Repo
	checkRuns map[int64]*CheckRun
	releases  map[int64]*Release
	prHeads   map[string]string
	pulls     map[string]*Pull
	issues    map[string]*Issue
	reviews   []*Review
	// requests 记录收到的请求（"METHOD path"）。
	requests []string
	// extraInstallations 是 InstallationID 之外的 installation（AddInstallation）。
	extraInstallations []int64
	// tokens 记录签发的 installation token 属于哪个 installation。
	tokens map[string]int64
	// issued 是签发过的 token（含仓库范围、权限和是否已经吊销，见 auth.go）。
	issued []*Token
	// limits、counts 是模拟的限流（SetRateLimit）和按 installation 统计的请求数。
	limits map[int64]*RateLimit
	counts map[int64]*RequestCount
	// faults 是注入的故障（SetFault），hiddenRefs 是列表接口暂时看不到的 ref（HideRefs），见 faults.go。
	faults     map[string]*Fault
	hiddenRefs []string
	// objects 是完整 SHA 的对象内容缓存（见 objectcache.go），随这个模拟 GitHub 一起释放。
	objects map[objectCacheKey]map[string]any
	// staleReviews 为真时列出 PR review 的接口读到稍旧的数据（看不到已有的 review，HideReviews）。
	staleReviews bool
}

// New 启动模拟 GitHub。
func New(account string) *Server {
	s := &Server{
		AppID: 4242, AppSlug: "kiln-test", InstallationID: 777, Account: account, AccountID: 9001,
		repos: map[string]*Repo{}, checkRuns: map[int64]*CheckRun{}, releases: map[int64]*Release{}, prHeads: map[string]string{},
		issues: map[string]*Issue{},
		tokens: map[string]int64{}, limits: map[int64]*RateLimit{}, counts: map[int64]*RequestCount{},
	}
	s.nextID.Store(1000)
	s.Server = httptest.NewServer(s.routes())
	return s
}

// AddRepo 登记一个仓库。
func (s *Server) AddRepo(r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if r.Installation == 0 {
		r.Installation = s.InstallationID
	}
	s.repos[strings.ToLower(r.FullName())] = r
}

// RemoveRepo 删除一个仓库（模拟仓库被删除或移出 App 的安装范围），之后对它的请求返回 404。
func (s *Server) RemoveRepo(r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, strings.ToLower(r.FullName()))
}

// AddInstallation 增加一个 installation（账号与默认 installation 相同），仓库通过 Repo.Installation 归属于它。
func (s *Server) AddInstallation(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraInstallations = append(s.extraInstallations, id)
}

// MoveRepo 把仓库移到 installation inst 的授权范围内；inst 为 -1 表示不在任何 installation 中（仓库仍然存在）。
func (s *Server) MoveRepo(r *Repo, inst int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Installation = inst
}

// SetDefaultBranch 修改仓库的默认分支（模拟在 GitHub 上修改仓库设置）。
func (s *Server) SetDefaultBranch(r *Repo, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.DefaultBranch = branch
}

// RemoveInstallation 删除一个由 AddInstallation 增加的 installation（模拟卸载），之后 GET /app/installations/{id} 返回 404，
// 它的仓库也不再能被访问。
func (s *Server) RemoveInstallation(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraInstallations = slices.DeleteFunc(s.extraInstallations, func(x int64) bool { return x == id })
	for k, r := range s.repos {
		if r.Installation == id {
			delete(s.repos, k)
		}
	}
}

// id 分配一个新的对象 ID（并发安全）。
func (s *Server) id() int64 { return s.nextID.Add(1) }

func (s *Server) record(req string) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
}

// Requests 返回收到的全部请求（"METHOD path"）的副本。
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// UnhandledRequests 返回没有对应路由的请求（"METHOD url"）。
func (s *Server) UnhandledRequests() []string {
	var out []string
	for _, r := range s.Requests() {
		if rest, ok := strings.CutPrefix(r, unhandledPrefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// CheckRuns 返回某个 sha 上的全部 Check Run。
func (s *Server) CheckRuns(sha string) []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckRun
	for _, c := range s.checkRuns {
		if c.HeadSHA == sha {
			out = append(out, *c)
		}
	}
	slices.SortFunc(out, func(a, b CheckRun) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Releases 返回某个仓库的全部 Release。
func (s *Server) Releases(repo string) []Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Release
	for _, r := range s.releases {
		if strings.EqualFold(r.Repo, repo) {
			out = append(out, r.clone())
		}
	}
	slices.SortFunc(out, func(a, b Release) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// AssetData 返回 asset 内容的副本；没有时返回 nil。
func (s *Server) AssetData(releaseID int64, name string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.releases[releaseID]; r != nil {
		for _, a := range r.Assets {
			if a.Name == name {
				return bytes.Clone(a.Data)
			}
		}
	}
	return nil
}
