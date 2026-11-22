package runner

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 结果说明超过上限（4000 字节，runnerkit.TruncateMsg）时截断。
func TestTruncateMsg(t *testing.T) {
	t.Parallel()
	const maxResultMessage = 4000
	if s := runnerkit.TruncateMsg("short"); s != "short" {
		t.Errorf("TruncateMsg = %q", s)
	}
	long := strings.Repeat("x", maxResultMessage+10)
	if s := runnerkit.TruncateMsg(long); s != long[:maxResultMessage]+"…" {
		t.Errorf("TruncateMsg 长度 %d", len(s))
	}
	// 中文（每个字 3 字节）在上限处不能被截断成半个字符，否则 Complete 请求无法序列化
	zh := "xx" + strings.Repeat("错", maxResultMessage)
	s := runnerkit.TruncateMsg(zh)
	if !utf8.ValidString(s) || len(s) > maxResultMessage+len("…") {
		t.Errorf("TruncateMsg 截断了多字节字符（长度 %d）", len(s))
	}
	if _, err := proto.Marshal(&runnerv1.CompleteRequest{Message: s}); err != nil {
		t.Errorf("截断后的结果说明无法序列化：%v", err)
	}
}

func TestClampInt32(t *testing.T) {
	t.Parallel()
	cases := map[int]int32{0: 0, -5: -5, math.MaxInt32 + 1: math.MaxInt32, math.MinInt32 - 1: math.MinInt32}
	for in, want := range cases {
		if got := runnerkit.ClampInt32(in); got != want {
			t.Errorf("runnerkit.ClampInt32(%d) = %d，期望 %d", in, got, want)
		}
	}
}

func TestOutcomeLabel(t *testing.T) {
	t.Parallel()
	cases := map[runnerv1.Outcome]string{
		runnerv1.Outcome_OUTCOME_SUCCEEDED:   "成功",
		runnerv1.Outcome_OUTCOME_FAILED:      "失败",
		runnerv1.Outcome_OUTCOME_INFRA_ERROR: "基础设施错误",
		runnerv1.Outcome_OUTCOME_CANCELLED:   "已取消",
		runnerv1.Outcome_OUTCOME_TIMED_OUT:   "超时",
		runnerv1.Outcome_OUTCOME_UNSPECIFIED: "",
	}
	for o, want := range cases {
		if got := outcomeLabel(o); got != want {
			t.Errorf("outcomeLabel(%s) = %q，期望 %q", o, got, want)
		}
	}
}

func TestDurIf(t *testing.T) {
	t.Parallel()
	if s := durIf(500 * time.Millisecond); s != "" {
		t.Errorf("不足 1 秒时 durIf = %q", s)
	}
	if s := durIf(2400 * time.Millisecond); s != "，下载用时 2s" {
		t.Errorf("durIf = %q", s)
	}
}
