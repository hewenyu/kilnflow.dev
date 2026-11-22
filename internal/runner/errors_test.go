package runner

import (
	"errors"
	"fmt"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
)

// 错误分类（PLAN #6.4）：用户错误为 failed，基础设施错误带分类，取消为 cancelled。
func TestResultFromErrOutcome(t *testing.T) {
	t.Parallel()
	base := errors.New("磁盘已满")
	cases := []struct {
		name    string
		err     error
		outcome runnerv1.Outcome
		class   string
		msg     string
	}{
		{name: "用户错误", err: userErrorf("输出 %q 不存在", "dist"), outcome: runnerv1.Outcome_OUTCOME_FAILED, class: classUser, msg: `输出 "dist" 不存在`},
		{name: "包装后的用户错误", err: fmt.Errorf("阶段：%w", userErrorf("x")), outcome: runnerv1.Outcome_OUTCOME_FAILED, class: classUser, msg: "x"},
		{name: "基础设施错误", err: infra(classUpload, base), outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: classUpload, msg: "磁盘已满"},
		{name: "包装后的基础设施错误", err: fmt.Errorf("外层：%w", infra(classRuntime, base)), outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: classRuntime, msg: "外层：磁盘已满"},
		{name: "未分类的错误按 infra.exec", err: base, outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: classExec, msg: "磁盘已满"},
		{name: "取消", err: fmt.Errorf("step：%w", errCancelled), outcome: runnerv1.Outcome_OUTCOME_CANCELLED},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := resultFromErr(c.err)
			if res.outcome != c.outcome || res.class != c.class {
				t.Errorf("resultFromErr = %s / %q，期望 %s / %q", res.outcome, res.class, c.outcome, c.class)
			}
			if c.msg != "" && res.msg != c.msg {
				t.Errorf("msg = %q，期望 %q", res.msg, c.msg)
			}
		})
	}
}

// infra 保留原错误（errors.Is 可用），用户错误不会被改成基础设施错误。
func TestInfra(t *testing.T) {
	t.Parallel()
	base := errors.New("连接被拒绝")
	err := infra(classFetch, base)
	if !errors.Is(err, base) || err.Error() != base.Error() {
		t.Errorf("infra 应包装原错误：%v", err)
	}
	ue := &userError{msg: "配置错误"}
	got := infra(classFetch, ue)
	if g, ok := errors.AsType[*userError](got); !ok || g != ue {
		t.Errorf("infra(userError) = %v，应原样返回", got)
	}
	if _, ok := errors.AsType[*infraError](got); ok {
		t.Error("用户错误不应带基础设施分类")
	}
}
