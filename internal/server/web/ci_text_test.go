package web

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 总览与 CI 运行的消息：服务端按原因码和原始参数给出消息，不解析中文说明（docs/I18N.md）。

func TestDispatchErrorText(t *testing.T) {
	for _, c := range []struct {
		err  *planner.DispatchError
		want string
	}{
		{&planner.DispatchError{Field: "ref", Code: planner.DispatchRefNotFound, Subject: "nope", Err: errors.New("x")}, "找不到 ref nope：确认分支或 tag 存在，或者换一个 ref。"},
		// ref 留空：找不到的是默认分支
		{&planner.DispatchError{Field: "ref", Code: planner.DispatchRefNotFound, Err: errors.New("x")}, "找不到 ref main：确认分支或 tag 存在，或者换一个 ref。"},
		{&planner.DispatchError{Field: "workflow", Code: planner.DispatchWorkflowMissing, Subject: "ci.yml", SHA: "0123456789abcdef0123", Err: errors.New("x")}, "0123456789ab 中没有 .kiln/workflows/ci.yml"},
		{&planner.DispatchError{Field: "workflow", Code: planner.DispatchNotDispatchable, Subject: "push.yml", Err: errors.New("x")}, "push.yml 没有声明 on.workflow_dispatch，不能手动触发"},
		// 不认识的原因码：原样显示领域层的说明
		{&planner.DispatchError{Field: "workflow", Err: errors.New("说明原文")}, "说明原文"},
	} {
		if got := textZH(t, dispatchErrorText(c.err, "main")); got != c.want {
			t.Errorf("dispatchErrorText(%s) = %q，应为 %q", c.err.Code, got, c.want)
		}
	}
}

func TestInputProblemText(t *testing.T) {
	for _, c := range []struct {
		p    workflow.InputProblem
		want string
	}{
		{workflow.InputProblem{Code: workflow.InputMissing, Name: "version"}, "缺少必填的 version"},
		{workflow.InputProblem{Code: workflow.InputNotBoolean, Name: "dry"}, "dry 只能是 true 或 false"},
		{workflow.InputProblem{Code: workflow.InputNotChoice, Name: "mode", Options: []string{"a", "b"}}, "mode 只能是 a, b 之一"},
		{workflow.InputProblem{Code: workflow.InputUndeclared, Name: "x"}, "workflow 没有声明 x"},
	} {
		if got := inputProblemText(c.p); got.id == "" || textZH(t, got) != c.want {
			t.Errorf("inputProblemText(%s) = %+v（%q），应为消息 %q", c.p.Code, got, textZH(t, got), c.want)
		}
	}
}

func TestRunTitle(t *testing.T) {
	for _, c := range []struct {
		n, current int
		want       string
	}{
		{2, 2, "Run #7"},
		{0, 2, "Run #7 · 首次运行"},
		{1, 2, "Run #7 · 第 1 次重跑"},
	} {
		if got := textZH(t, runTitle(7, c.n, c.current)); got != c.want {
			t.Errorf("runTitle(7, %d, %d) = %q，应为 %q", c.n, c.current, got, c.want)
		}
	}
}

func TestRunActivityText(t *testing.T) {
	for _, c := range []struct {
		action string
		detail map[string]any
		want   string
	}{
		{"run.dispatch", nil, "手动启动了这个 run"},
		{"run.cancel", nil, "取消了这个 run"},
		{"run.rerun", map[string]any{"mode": "all", "rerun": float64(2)}, "重跑了这个 run（全部重跑），开始第 2 次重跑"},
		{"run.rerun", map[string]any{"mode": "all_no_cache", "rerun": float64(3)}, "重跑了这个 run（全部重跑，不用缓存），开始第 3 次重跑"},
		{"run.rerun", map[string]any{"mode": "job", "job_id": float64(42), "rerun": float64(4)}, "重跑了这个 run（重跑 job #42），开始第 4 次重跑"},
		{"run.rerun", map[string]any{"rerun": float64(1)}, "重跑了这个 run，开始第 1 次重跑"},
	} {
		e := activityEntry{Action: c.action, Detail: c.detail}
		if got := e.RunText(); got.id == "" || textZH(t, got) != c.want {
			t.Errorf("%s %v：RunText = %+v，应为消息 %q", c.action, c.detail, got, c.want)
		}
	}
}

func TestLogSubjectNotFound(t *testing.T) {
	for _, c := range []struct {
		s           logSubject
		id, attempt int64
		want        string
	}{
		{logOfJob, 3, 0, "没有 job #3"},
		{logOfJob, 3, 9, "job #3 没有尝试 #9"},
		{logOfAITask, 5, 0, "没有 AI 任务 #5"},
		{logOfAITask, 5, 8, "AI 任务 #5 没有 attempt #8"},
	} {
		if got := textZH(t, c.s.notFound(c.id, c.attempt)); got != c.want {
			t.Errorf("notFound(%d, %d) = %q，应为 %q", c.id, c.attempt, got, c.want)
		}
	}
}

func TestFlashArgText(t *testing.T) {
	if got := flashArgText("a\nb\tc"); got != "a b c" {
		t.Errorf("控制字符应换成空格：%q", got)
	}
	long := strings.Repeat("长", maxFlashArgRunes+5)
	got := flashArgText(long)
	if n := utf8.RuneCountInString(got); n != maxFlashArgRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("过长的参数应截断到 %d 个字符：%d", maxFlashArgRunes, n)
	}
	if got := flashArgText(strings.Repeat("x", maxFlashArgRunes)); got != strings.Repeat("x", maxFlashArgRunes) {
		t.Error("不超过上限的参数应原样保留")
	}
	if got := flashArgText("a\xffb"); !utf8.ValidString(got) {
		t.Errorf("不合法的 UTF-8 应替换：%q", got)
	}
}
