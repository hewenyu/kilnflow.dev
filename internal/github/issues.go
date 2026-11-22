package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 合并队列合并 PR 后关闭关联的 issue（PLAN #28.3）：GitHub 只在用户合并时按关键字关闭 issue，
// App 用 installation token 合并时不会，需要 Server 自己关闭。读取关联用 GraphQL（REST 没有 closingIssuesReferences），
// 关闭和评论用 REST；都需要 App 的 Issues 读写权限。

// GraphQLError 是 GraphQL 响应中的错误（HTTP 状态为 200，响应带 errors）。
type GraphQLError struct {
	// Type 是第一个错误的类型（如 FORBIDDEN、NOT_FOUND）；GitHub 有时不填。
	Type string
	// Message 是全部错误的信息（用"；"连接）。
	Message string
}

func (e *GraphQLError) Error() string {
	if e.Type == "" {
		return "GitHub GraphQL：" + e.Message
	}
	return fmt.Sprintf("GitHub GraphQL：%s %s", e.Type, e.Message)
}

// GraphQL 错误的类型。
const (
	// GraphQLForbidden 表示没有权限（例如 App 没有 Issues 权限时读取 issue）。
	GraphQLForbidden = "FORBIDDEN"
	// graphQLRateLimited 表示 GraphQL 的配额已经用完（HTTP 状态仍为 200）。
	graphQLRateLimited = "RATE_LIMITED"
)

// graphQLResponse 是 GraphQL 的响应。
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// rateLimited 实现 bodyRateLimited：GraphQL 的限流以 HTTP 200、errors.type=RATE_LIMITED 返回。
func (r *graphQLResponse) rateLimited() (string, bool) {
	for _, e := range r.Errors {
		if e.Type == graphQLRateLimited {
			return "GraphQL " + e.Type + " " + e.Message, true
		}
	}
	return "", false
}

// GraphQL 以 installation token 执行一个 GraphQL 查询，把 data 解码到 out。响应带 errors 时返回 *GraphQLError；
// 被限流（包括 HTTP 200 的 RATE_LIMITED）时返回 *RateLimitError，GraphQL 的配额与 REST 分开记录。
// 只支持 github.com（GraphQL 地址是 API 地址下的 /graphql）。
func (i *Inst) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp graphQLResponse
	if err := i.doKey(ctx, graphQLLimitKey(i.ID), http.MethodPost, "/graphql",
		map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return &GraphQLError{Type: resp.Errors[0].Type, Message: strings.Join(msgs, "；")}
	}
	if out == nil || len(resp.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("解析 GitHub GraphQL 响应：%w", err)
	}
	return nil
}

// closingIssuesPerPR 是读取 PR 关联的 issue 时取的数量上限（GraphQL 单页上限是 100）。关联超过它的 PR 只处理前面的。
const closingIssuesPerPR = 50

var closingIssuesQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      closingIssuesReferences(first: %d) {
        nodes {
          number
          state
          repository { nameWithOwner }
        }
      }
    }
  }
}`, closingIssuesPerPR)

// ClosingIssue 是 PR 合并时会被关闭的一个 issue（GraphQL closingIssuesReferences：PR 描述中用关键字关联的，
// 以及在 PR 页面上手动关联的）。
type ClosingIssue struct {
	Number int
	// Repo 是 issue 所在的仓库（owner/name），可能与 PR 不同。
	Repo string
	// Open 表示 issue 还没有关闭。
	Open bool
}

// ListClosingIssues 返回 PR 合并时会被关闭的 issue（最多 closingIssuesPerPR 个，包括其他仓库的）。
func (i *Inst) ListClosingIssues(ctx context.Context, repo string, number int) ([]ClosingIssue, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("仓库名 %q 不是 owner/name", repo)
	}
	var data struct {
		Repository *struct {
			PullRequest *struct {
				ClosingIssuesReferences struct {
					Nodes []*struct {
						Number     int    `json:"number"`
						State      string `json:"state"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := i.GraphQL(ctx, closingIssuesQuery, map[string]any{"owner": owner, "name": name, "number": number}, &data); err != nil {
		return nil, fmt.Errorf("读取 %s PR #%d 关联的 issue：%w", repo, number, err)
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("读取 %s PR #%d 关联的 issue：PR 不存在", repo, number)
	}
	var out []ClosingIssue
	for _, n := range data.Repository.PullRequest.ClosingIssuesReferences.Nodes {
		if n == nil {
			continue
		}
		out = append(out, ClosingIssue{Number: n.Number, Repo: n.Repository.NameWithOwner, Open: n.State == "OPEN"})
	}
	return out, nil
}

// IssueOpen 读取 issue（或 PR）当前是否打开（REST GET /issues/{n}）。
func (i *Inst) IssueOpen(ctx context.Context, repo string, number int) (bool, error) {
	var is struct {
		State string `json:"state"`
	}
	if err := i.get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number), &is); err != nil {
		return false, err
	}
	return is.State == "open", nil
}

// IssueStateReasonCompleted 是以"已完成"关闭 issue 的 state_reason。
const IssueStateReasonCompleted = "completed"

// CloseIssue 关闭 issue，reason 是 state_reason（如 IssueStateReasonCompleted）。issue 已经关闭时 GitHub 照常返回成功。
func (i *Inst) CloseIssue(ctx context.Context, repo string, number int, reason string) error {
	return i.Do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number),
		map[string]string{"state": "closed", "state_reason": reason}, nil)
}
