package runner

import (
	"errors"
	"fmt"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

var (
	// ErrRevoked 表示 Runner 已被吊销（cmd/kiln-runner 据此以退出码 0 退出）。
	ErrRevoked = runnerkit.ErrRevoked
	// errTimeout 是 job 超时时 context 的 cause。
	errTimeout = errors.New("timeout")
	// errCancelled 表示 job 被取消（取消指令、超时或 Runner 退出，最终结果由 job.run 判断）：
	// 收到取消指令时作为 context 的 cause，执行 step 时被取消则由 runSteps 返回。
	errCancelled = errors.New("已取消")
	// errDisconnected 是连续连不上 Server 超过上限、Runner 自行停止 job 时 context 的 cause（PLAN #6.3）。
	errDisconnected = errors.New("连不上 Server")
)

// 上报给 Server 的错误分类（CompleteRequest.error_class）。
const (
	classUser           = "user"
	classTimeout        = "timeout"
	classRunnerShutdown = "infra.runner_shutdown"
	classDisconnected   = "infra.disconnected"
	classPrepare        = "infra.prepare"
	classCredentials    = "infra.credentials" //nolint:gosec // 错误分类名，不是凭证
	classFetch          = "infra.fetch"
	classRuntime        = "infra.runtime"
	classExec           = "infra.exec"
	classUpload         = "infra.upload"
)

// userError 是由用户的配置或代码导致的失败（job 结果为 failed，而不是基础设施错误）。
type userError struct {
	msg      string
	exitCode int // step 的非零退出码；其他用户错误为 0
}

func (e *userError) Error() string { return e.msg }

func userErrorf(format string, args ...any) error {
	return &userError{msg: fmt.Sprintf(format, args...)}
}

// infraError 是带分类的基础设施错误。
type infraError struct {
	class string
	err   error
}

func (e *infraError) Error() string { return e.err.Error() }
func (e *infraError) Unwrap() error { return e.err }

// infra 给错误加上分类；用户错误保持原样。
func infra(class string, err error) error {
	if _, ok := errors.AsType[*userError](err); ok {
		return err
	}
	return &infraError{class: class, err: err}
}

// resultFromErr 把执行过程中的错误转换为 job 结果。
func resultFromErr(err error) *jobResult {
	if errors.Is(err, errCancelled) {
		return &jobResult{outcome: runnerv1.Outcome_OUTCOME_CANCELLED, msg: err.Error()}
	}
	if ue, ok := errors.AsType[*userError](err); ok {
		return &jobResult{outcome: runnerv1.Outcome_OUTCOME_FAILED, class: classUser, exitCode: ue.exitCode, msg: ue.msg}
	}
	class := classExec
	if ie, ok := errors.AsType[*infraError](err); ok {
		class = ie.class
	}
	return &jobResult{outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: class, msg: err.Error()}
}
