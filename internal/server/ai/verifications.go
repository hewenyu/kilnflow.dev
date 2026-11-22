package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// CI 验证的查询（PLAN #30.3、#33.9）：Agent 用 kiln-orca task ci 查看任务所在 PR（或指定的 commit）最新的一次 CI 验证。
// AI 领域不读 CI 的表：接口在 AI 领域定义，CI 领域实现，在组装层接线（与 aidev.CIResults 相同的方式）。

// Verifications 是 CI 领域提供给 AI 的只读查询。Latest 返回 repoID 中 commit sha 最新的 CI 验证：每个验证（workflow、
// 事件、ref、目标分支相同的 run）只看最新的 run，状态由它们汇总（定义见 engine.LatestVerification，PLAN #33.9）；没有验证时
// State 为 VerificationNone。logTailBytes 是每个失败 job 返回的日志尾部字节数上限。
type Verifications interface {
	Latest(ctx context.Context, repoID int64, sha string, logTailBytes int64) (*Verification, error)
}

// 验证状态的取值（GetVerificationResponse.state）。
const (
	VerificationNone      = "none"
	VerificationPending   = "pending"
	VerificationSucceeded = "succeeded"
	VerificationFailed    = "failed"
	VerificationCancelled = "cancelled"
)

// Verification 是 commit 上最新的 CI 验证的汇总。
type Verification struct {
	SHA   string
	State string
	// FailedJobs 是各个验证最新的 run 中失败的 job（只在 State 为 failed 时返回）。
	FailedJobs []FailedJob
}

// FailedJob 是验证中失败的一个 job。
type FailedJob struct {
	Name   string
	Reason string
	// ErrClass 是错误分类（infra.* 是基础设施的问题，不需要改代码）。
	ErrClass string
	// LogTail 是日志尾部（已去掉终端控制字符；CI 日志在写入时已脱敏）；日志已过保留期时为空且 LogExpired 为 true。
	LogTail    []byte
	LogExpired bool
	// URL 是 Dashboard 中这个 job 的地址。
	URL string
}

// 日志尾部的默认值和上限（GetVerificationRequest.log_tail_bytes）。
const (
	defaultLogTailBytes = 8 << 10
	maxLogTailBytes     = 64 << 10
)

// SetVerifications 设置 CI 验证的查询（组装时调用，在 Server 开始服务之前）。
func (c *Core) SetVerifications(v Verifications) { c.verifications = v }

// GetVerification 查询任务所在 PR（或指定的 commit）最新的一次 CI 验证：状态、失败的 job、错误分类和日志尾部。只读。
func (s *Service) GetVerification(ctx context.Context, req *connect.Request[airunnerv2.GetVerificationRequest]) (*connect.Response[airunnerv2.GetVerificationResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	c := s.c
	if c.verifications == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("没有配置 CI 验证的查询（Server 没有注入 Verifications）"))
	}
	_, t, err := c.attemptTask(ctx, c.db, id, req.Msg.GetRef(), false)
	if err != nil {
		return nil, err
	}
	r, err := GetRequest(ctx, c.db, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(req.Msg.GetSha())
	if sha == "" {
		if sha, err = c.verificationSHA(ctx, t, r); err != nil {
			return nil, err
		}
	}
	v, err := c.verifications.Latest(ctx, r.RepoID, sha, clampLogTail(req.Msg.GetLogTailBytes()))
	if err != nil {
		return nil, err
	}
	resp := &airunnerv2.GetVerificationResponse{Sha: v.SHA, State: v.State}
	for _, j := range v.FailedJobs {
		resp.FailedJobs = append(resp.FailedJobs, &airunnerv2.FailedJob{
			Name: j.Name, Reason: j.Reason, ErrorClass: j.ErrClass, LogTail: j.LogTail, LogExpired: j.LogExpired, Url: j.URL,
		})
	}
	return connect.NewResponse(resp), nil
}

// clampLogTail 把请求的日志尾部大小限制在默认值和上限之间。
func clampLogTail(n int32) int64 {
	switch {
	case n <= 0:
		return defaultLogTailBytes
	case n > maxLogTailBytes:
		return maxLogTailBytes
	}
	return int64(n)
}

// verificationSHA 返回默认要查询的 commit：任务的 PR 当前的 head（ref_heads 中记下的 pull ref），没有 PR 时是任务的 sha。
func (c *Core) verificationSHA(ctx context.Context, t *Task, r *Request) (string, error) {
	pr, err := prNumberOf(ctx, c.db, t.RequestID)
	if err != nil || pr == 0 {
		return r.SHA, err
	}
	var sha string
	err = c.db.QueryRow(ctx, `SELECT sha FROM ref_heads WHERE repo_id=$1 AND ref=$2`,
		r.RepoID, fmt.Sprintf("%s%d/head", gitref.PullPrefix, pr)).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.SHA, nil
	}
	return sha, err
}

// prNumberOf 返回请求所在 PR 的编号（审计的 PR、开发工作项的 PR），没有时为 0。
func prNumberOf(ctx context.Context, q store.Querier, requestID int64) (int, error) {
	var pr int
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT pr_number FROM ai_review_requests WHERE request_id=$1),
		(SELECT w.pr_number FROM ai_dev_requests d JOIN dev_work_items w ON w.id=d.work_item_id WHERE d.request_id=$1), 0)`, requestID).Scan(&pr)
	return pr, err
}
