package engine

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// PRVerifications 返回 PR pr 的 head 针对 baseRef 的 CI 验证（pull_request run，PLAN #30.3 的 Verifications）；withJobs 时
// 带上各 run 当前这次运行的 job。run 和 job 在同一条语句中读取。实现 store.VerificationSource。
func PRVerifications(ctx context.Context, q store.Querier, repoID int64, pr int, head, baseRef string, withJobs bool) ([]store.Verification, error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.workflow_name, r.workflow, r.state,
			COALESCE(array_agg(j.job_name ORDER BY j.id) FILTER (WHERE j.id IS NOT NULL), '{}'),
			COALESCE(array_agg(j.state::text ORDER BY j.id) FILTER (WHERE j.id IS NOT NULL), '{}')
		FROM runs r LEFT JOIN jobs j ON $6 AND j.run_id=r.id AND j.rerun=r.current_rerun
		WHERE r.repo_id=$1 AND r.event=$2 AND r.pr_number=$3 AND r.sha=$4 AND r.github_ctx->>'base_ref'=$5
		GROUP BY r.id ORDER BY r.id`,
		repoID, workflow.EventPullRequest, pr, head, baseRef, withJobs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Verification, error) {
		var v store.Verification
		var names, states []string
		var file string
		if err := row.Scan(&v.ID, &v.Name, &file, &v.State, &names, &states); err != nil {
			return v, err
		}
		v.Source = workflow.BaseName(file)
		for i, n := range names {
			v.Jobs = append(v.Jobs, store.VerificationJob{Name: n, State: states[i]})
		}
		return v, nil
	})
}

// PRFailure 是 PR head 上一个失败（failed、timed_out）的 CI job，或者一个因配置错误直接失败的 run。
type PRFailure struct {
	// JobID 是失败的 job；run 自身因配置错误失败（没有 job）时为 0，Name 是 workflow 名。
	JobID int64
	Name  string
	// Reason 是 job 的结束说明（配置错误时是 run 的配置错误）；ErrClass 是它当前 attempt 的错误分类。
	Reason   string
	ErrClass string
}

// PRSettled 返回 PR pr 在 head 上的 CI 结果（PLAN #30.3 的 CIResults，组装层转给 AI 开发的返修）：runs 是 event=pull_request
// 的 run 数，done 表示都已结束；都结束时 failures 是各 run 最近一次运行中失败的 job（按 job ID 排序），以及因配置错误
// 直接失败的 run（这类 run 没有 job，但 AI 开发必须知道它失败了，否则会当成通过）。"失败的 job"与 LatestVerification
// 用同一条规则（store.FailedJobStates：failed、timed_out）。
// run 的状态和失败的 job 在同一条语句中读取（同一个快照）：分两条语句读时，中间新开始的重跑或刚变化的 job 会让 done 和
// failures 互相矛盾（done 为真但带着上一次运行的失败）。
func PRSettled(ctx context.Context, q store.Querier, repoID int64, pr int, head string) (runs int, done bool, failures []PRFailure, err error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.state, coalesce(r.workflow_name, ''), coalesce(r.config_error, ''),
			j.id, coalesce(j.display_name, ''), coalesce(j.reason, ''), coalesce(a.error_class, '')
		FROM runs r
		LEFT JOIN jobs j ON j.run_id=r.id AND j.rerun=r.current_rerun AND j.state = ANY($5)
		LEFT JOIN attempts a ON a.id=j.current_attempt_id
		WHERE r.repo_id=$1 AND r.pr_number=$2 AND r.sha=$3 AND r.event=$4
		ORDER BY r.id, j.id`,
		repoID, pr, head, workflow.EventPullRequest, store.FailedJobStateArgs())
	if err != nil {
		return 0, false, nil, err
	}
	// 每行是一个 run（没有失败的 job 时）或一个失败的 job 和它所在的 run
	type row struct {
		RunID        int64
		RunState     store.RunState
		WorkflowName string
		ConfigError  string
		JobID        *int64
		Name         string
		Reason       string
		ErrClass     string
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return 0, false, nil, err
	}
	open := false
	var last int64
	for _, f := range found {
		if f.RunID != last {
			runs++
			last = f.RunID
		}
		if !f.RunState.Terminal() {
			open = true
		}
		if f.JobID != nil {
			failures = append(failures, PRFailure{JobID: *f.JobID, Name: f.Name, Reason: f.Reason, ErrClass: f.ErrClass})
			continue
		}
		if f.RunState == store.RunFailed && f.ConfigError != "" {
			failures = append(failures, PRFailure{Name: f.WorkflowName, Reason: configFailurePrefix + f.ConfigError})
		}
	}
	if open || runs == 0 {
		return runs, false, nil, nil
	}
	return runs, true, failures, nil
}

// configFailurePrefix 是配置错误失败的 run 在 CIResults 中的说明前缀。
const configFailurePrefix = "运行配置错误："

// 验证状态的取值（ai.Verifications）。
const (
	VerifyNone      = "none"
	VerifyPending   = "pending"
	VerifySucceeded = "succeeded"
	VerifyFailed    = "failed"
	VerifyCancelled = "cancelled"
)

// Verification 是 commit sha 上最新的 CI 验证的汇总（PLAN #33.9，LatestVerification）。
type Verification struct {
	SHA   string
	State string
	// FailedJobs 是各个验证最新的 run 中失败（failed、timed_out）的 job（只在 State 为 failed 时返回）。
	FailedJobs []VerificationJob
}

// VerificationJob 是验证中失败的一个 job。
type VerificationJob struct {
	Name   string
	Reason string
	// ErrClass 是 job 当前 attempt 的错误分类（infra.* 是基础设施的问题）。
	ErrClass string
	// LogTail 是日志尾部（已去掉终端控制字符）；日志已按保留策略清理时为空且 LogExpired 为 true。
	LogTail    []byte
	LogExpired bool
	// URL 是 Dashboard 中这个 job 的地址（没有配置 PublicURL 时为空）。
	URL string
}

// LatestVerification 返回 repoID 中 commit sha 最新的 CI 验证（PLAN #33.9 的 GetVerification）。
//
// 一个验证是 workflow 文件、事件、ref 和目标分支（pull_request 的 base_ref）都相同的 run，"最新"是其中 ID 最大的 run 和它
// 当前这次运行（current_rerun）的 job。这个身份就是 run 的 dedupe_key（planner 的 dedupeKey）去掉 commit 和请求 ID：
// 同一个 commit 上除了手动触发（workflow_dispatch 每次请求一个 run），每个身份只有一个 run，重跑在 run 内递增 current_rerun。
//
// 状态由各个验证最新的 run 汇总：被 paths 排除而跳过的 run 不算验证（与合并队列相同），没有其他 run 时为 none；有没结束的为
// pending；否则有失败的为 failed；有取消的为 cancelled；其余为 succeeded。failed 时带上这些 run 中失败的 job、错误分类和
// 日志尾部（日志编号见 cistore.JobLogID，日志在写入时已脱敏）。run 的状态和失败的 job 在同一条语句中读取。
// 只读，不建 run、不改状态（主动请求验证不在本期）。
//
// "失败的 job"是自己失败、使 run 失败的 job，与 run 的结论用同一条规则（store.FailedJobStates：failed、timed_out）。
// 被连带的 job 不列：fail-fast 或用户取消的（cancelled）、因上游没有成功而跳过的（skipped，原因写明是哪个上游）都没有
// 自己的失败原因，Agent 要看的是导致它们的那个失败的 job。
func LatestVerification(ctx context.Context, q store.Querier, logs *logstore.Store, publicURL string, repoID int64, sha string,
	logTailBytes int64,
) (*Verification, error) {
	rows, err := q.Query(ctx, `WITH latest AS (
			SELECT DISTINCT ON (workflow, event, ref, github_ctx->>'base_ref') id, state, current_rerun, purged_at
			FROM runs WHERE repo_id=$1 AND sha=$2
			ORDER BY workflow, event, ref, github_ctx->>'base_ref', id DESC)
		SELECT r.state, j.id, coalesce(j.kind, ''), coalesce(j.display_name, ''), coalesce(j.reason, ''), coalesce(a.error_class, ''),
			j.current_attempt_id, coalesce(r.purged_at IS NOT NULL AND j.created_at < r.purged_at, false)
		FROM latest r LEFT JOIN jobs j ON j.run_id=r.id AND j.rerun=r.current_rerun AND j.state = ANY($3)
		LEFT JOIN attempts a ON a.id=j.current_attempt_id
		ORDER BY j.id NULLS FIRST`, repoID, sha, store.FailedJobStateArgs())
	if err != nil {
		return nil, err
	}
	// 每行是一个 run（没有失败的 job 时）或一个失败的 job 和它所在的 run
	type row struct {
		RunState   store.RunState
		JobID      *int64
		Kind       workflow.JobKind
		Name       string
		Reason     string
		ErrClass   string
		Attempt    *int64
		LogExpired bool
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	states := make([]store.RunState, 0, len(found))
	for _, f := range found {
		states = append(states, f.RunState)
	}
	v := &Verification{SHA: sha, State: verificationState(states)}
	if v.State != VerifyFailed {
		return v, nil
	}
	for _, f := range found {
		if f.JobID == nil {
			continue
		}
		j := VerificationJob{Name: f.Name, Reason: f.Reason, ErrClass: f.ErrClass, URL: cicheck.JobURL(publicURL, *f.JobID), LogExpired: f.LogExpired}
		if id := cistore.JobLogID(f.Kind, *f.JobID, f.Attempt); !f.LogExpired && id != 0 {
			j.LogTail = logs.TailBytes(id, logTailBytes)
		}
		v.FailedJobs = append(v.FailedJobs, j)
	}
	return v, nil
}

// verificationState 汇总各个验证最新的 run 的状态（LatestVerification）。
func verificationState(runs []store.RunState) string {
	state := VerifyNone
	for _, st := range runs {
		switch {
		case st == store.RunSkipped:
		case !st.Terminal():
			return VerifyPending
		case st == store.RunFailed:
			state = VerifyFailed
		case st == store.RunCancelled && state != VerifyFailed:
			state = VerifyCancelled
		case state == VerifyNone:
			state = VerifySucceeded
		}
	}
	return state
}
