package fakegithub

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

const (
	// webhookTimeout 是发送一个 webhook 的超时。
	webhookTimeout = 30 * time.Second
	// maxErrorBody 是 webhook 失败时读取的响应体上限。
	maxErrorBody = 4 << 10
)

var webhookClient = &http.Client{Timeout: webhookTimeout}

// SendWebhook 以 GitHub 的格式向 url 发送一个 webhook（delivery ID 每次不同）。
func SendWebhook(ctx context.Context, url, secret, event string, payload any) error {
	return SendWebhookDelivery(ctx, url, secret, event, fmt.Sprintf("delivery-%d", time.Now().UnixNano()), payload)
}

// SendWebhookDelivery 以指定的 delivery ID 发送 webhook，用于模拟 GitHub 重复投递同一个事件。
func SendWebhookDelivery(ctx context.Context, url, secret, event, delivery string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码 webhook：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造 webhook 请求：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.HeaderEvent, event)
	req.Header.Set(github.HeaderDelivery, delivery)
	req.Header.Set(github.HeaderSignature, github.Sign([]byte(secret), body))
	resp, err := webhookClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送 webhook：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("webhook：HTTP %d %s", resp.StatusCode, b)
	}
	return nil
}

func (s *Server) userJSON() map[string]any {
	return map[string]any{"login": s.Account, "id": s.AccountID, "type": "User"}
}

// authorJSON 返回 PR 作者 login 的账号（空为 s.Account；以 [bot] 结尾时类型是 Bot）。
func (s *Server) authorJSON(login string) map[string]any {
	if login == "" {
		return s.userJSON()
	}
	typ := "User"
	if strings.HasSuffix(login, "[bot]") {
		typ = "Bot"
	}
	return map[string]any{"login": login, "id": s.AccountID + 1000, "type": typ}
}

// PullRequestPayload 构造 pull_request 事件（同仓库的分支）。
func (s *Server) PullRequestPayload(r *Repo, action string, number int, headRef, headSHA, baseRef, baseSHA string) map[string]any {
	s.mu.Lock()
	s.prHeads[strconv.Itoa(number)] = headSHA
	s.mu.Unlock()
	repo := s.repoJSON(r)
	user := s.userJSON()
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	return map[string]any{
		"action": action, "number": number,
		"pull_request": map[string]any{
			"number": number, "user": user, "state": state,
			"head": map[string]any{"ref": headRef, "sha": headSHA, "repo": repo},
			"base": map[string]any{"ref": baseRef, "sha": baseSHA, "repo": repo},
		},
		"repository": repo, "installation": map[string]any{"id": r.Installation}, "sender": user,
	}
}

// PushPayload 构造 push 事件。
func (s *Server) PushPayload(r *Repo, ref, before, after string) map[string]any {
	return map[string]any{
		"ref": ref, "before": before, "after": after, "deleted": false,
		"repository":   s.repoJSON(r),
		"installation": map[string]any{"id": r.Installation},
		"sender":       s.userJSON(),
	}
}

// RepositoryPayload 构造 repository 事件（例如 action 为 deleted）。
func (s *Server) RepositoryPayload(r *Repo, action string) map[string]any {
	return map[string]any{
		"action":       action,
		"repository":   s.repoJSON(r),
		"installation": map[string]any{"id": r.Installation},
		"sender":       s.userJSON(),
	}
}

// CheckSuitePayload 构造 check_suite 事件（例如 action 为 rerequested，对应 GitHub 上的 "Re-run all checks"）。
func (s *Server) CheckSuitePayload(r *Repo, action, headSHA string) map[string]any {
	return map[string]any{
		"action":       action,
		"check_suite":  map[string]any{"head_sha": headSHA},
		"repository":   s.repoJSON(r),
		"installation": map[string]any{"id": r.Installation},
		"sender":       s.userJSON(),
	}
}

// InstallationPayload 构造 installation 事件（例如 action 为 created）。与 GitHub 一样，repositories 中是
// 这个 installation 的仓库的简要信息（没有默认分支和 clone 地址）。
func (s *Server) InstallationPayload(action string, installation int64) map[string]any {
	s.mu.Lock()
	var repos []*Repo
	for _, r := range s.repos {
		if r.Installation == installation {
			repos = append(repos, r)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(repos, func(a, b *Repo) int { return cmp.Compare(a.ID, b.ID) })
	return map[string]any{
		"action":       action,
		"installation": map[string]any{"id": installation, "account": s.userJSON()},
		"repositories": briefRepos(repos),
		"sender":       s.userJSON(),
	}
}

// InstallationRepositoriesPayload 构造 installation_repositories 事件（action 为 added 或 removed）。
func (s *Server) InstallationRepositoriesPayload(action string, installation int64, added, removed []*Repo) map[string]any {
	return map[string]any{
		"action":               action,
		"installation":         map[string]any{"id": installation, "account": s.userJSON()},
		"repository_selection": "selected",
		"repositories_added":   briefRepos(added),
		"repositories_removed": briefRepos(removed),
		"sender":               s.userJSON(),
	}
}

// briefRepos 返回 installation 事件中的仓库列表（只有 ID、名称和可见性）。
func briefRepos(repos []*Repo) []any {
	list := make([]any, 0, len(repos))
	for _, r := range repos {
		list = append(list, map[string]any{"id": r.ID, "name": r.Name, "full_name": r.FullName(), "private": r.Private})
	}
	return list
}

func (s *Server) repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id": r.ID, "name": r.Name, "full_name": r.FullName(), "private": r.Private, "default_branch": r.DefaultBranch,
		"clone_url": "file://" + r.Dir, "owner": map[string]any{"login": r.Owner, "id": s.AccountID, "type": "User"},
	}
}
