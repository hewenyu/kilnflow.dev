package web

import (
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// TestUpgradeReasonText：upgradectl 的每个原因码都映射为自己的消息（不是兜底），版本号和上报的错误原文作为参数原样传递；
// 消息与 CLI 的中文说明（Reason.String）一致，同一个判断给出两种文字。
func TestUpgradeReasonText(t *testing.T) {
	cases := []struct {
		r    upgradectl.Reason
		want msgID
	}{
		{upgradectl.Reason{Code: upgradectl.ReasonAINode}, msgUpgradeReasonAINode},
		{upgradectl.Reason{Code: upgradectl.ReasonRevoked}, msgUpgradeReasonRevoked},
		{upgradectl.Reason{Code: upgradectl.ReasonUnsupported}, msgUpgradeReasonUnsupported},
		{upgradectl.Reason{Code: upgradectl.ReasonNoLauncher}, msgUpgradeReasonNoLauncher},
		{upgradectl.Reason{Code: upgradectl.ReasonDisabled}, msgUpgradeReasonDisabled},
		{upgradectl.Reason{Code: upgradectl.ReasonCheckFailed, Detail: "npm 上没有这个包"}, msgUpgradeReasonCheckFailed},
		{upgradectl.Reason{Code: upgradectl.ReasonNotChecked}, msgUpgradeReasonNotChecked},
		{upgradectl.Reason{Code: upgradectl.ReasonUpToDate}, msgUpgradeReasonUpToDate},
		{upgradectl.Reason{Code: upgradectl.ReasonNewer, Current: "v0.1.43", Latest: "0.1.42"}, msgUpgradeReasonNewer},
		{upgradectl.Reason{Code: upgradectl.ReasonUpgrading, Version: "0.1.42"}, msgUpgradeReasonUpgrading},
		{upgradectl.Reason{Code: upgradectl.ReasonLatestChanged, Latest: "0.1.43", Expected: "0.1.42"}, msgUpgradeReasonLatestChanged},
		{upgradectl.Reason{Code: upgradectl.ReasonNoPending}, msgUpgradeReasonNoPending},
		{upgradectl.Reason{Code: upgradectl.ReasonSwitching, Version: "0.1.42"}, msgUpgradeReasonSwitching},
	}
	for _, c := range cases {
		got := upgradeReasonText(c.r)
		if got.id != c.want {
			t.Errorf("%s：消息 %q，应为 %q", c.r.Code, got.id, c.want)
			continue
		}
		// AI 执行节点的说明在 CLI 中带 npm 的安装命令（含 <scope> 占位），页面上不写占位：其余与 CLI 的中文说明相同
		if c.r.Code != upgradectl.ReasonAINode {
			if zh := textZH(t, got); zh != c.r.String() {
				t.Errorf("%s：页面上是 %q，CLI 的说明是 %q", c.r.Code, zh, c.r.String())
			}
		}
		// 能作为升级操作被拒绝后的提示放进地址
		if _, err := flashValue(fitFlash(got)); err != nil {
			t.Errorf("%s：不能作为提示：%v", c.r.Code, err)
		}
	}
	if !upgradeReasonText(upgradectl.Reason{}).IsZero() {
		t.Error("没有原因时应为空")
	}
	if got := upgradeReasonText(upgradectl.Reason{Code: "future_code"}); got.id != msgUpgradeReasonUnknown || got.params[argCode] != "future_code" {
		t.Errorf("不认识的原因码：%+v", got)
	}
}

// TestCacheNoteText：清理预览的每个原因码都映射为自己的消息；时间是 Unix 秒（前端按语言和时区显示），其余与 CLI 的说明相同。
func TestCacheNoteText(t *testing.T) {
	at := time.Date(2026, 11, 5, 4, 41, 10, 0, time.UTC).Unix()
	cases := []struct {
		n    admin.CacheNote
		want msgID
	}{
		{admin.CacheNote{Code: admin.CacheNoteNoReport}, msgCacheNoteNoReport},
		{admin.CacheNote{Code: admin.CacheNoteReport, GeneratedAt: at}, msgCacheNoteReport},
		{admin.CacheNote{Code: admin.CacheNoteBusy, Runner: "mini-01", Jobs: 2}, msgCacheNoteBusy},
		{admin.CacheNote{Code: admin.CacheNoteSharedKept}, msgCacheNoteSharedKept},
	}
	for _, c := range cases {
		got := cacheNoteText(c.n)
		if got.id != c.want {
			t.Errorf("%s：消息 %q，应为 %q", c.n.Code, got.id, c.want)
			continue
		}
		if c.n.Code == admin.CacheNoteReport {
			if got.params[argAt] != at {
				t.Errorf("清单时间应为 Unix 秒：%+v", got.params)
			}
			continue
		}
		if zh := textZH(t, got); zh != c.n.String() {
			t.Errorf("%s：页面上是 %q，CLI 的说明是 %q", c.n.Code, zh, c.n.String())
		}
	}
	if got := cacheNoteText(admin.CacheNote{Code: "future"}); got.id != msgCacheNoteUnknown {
		t.Errorf("不认识的原因码：%+v", got)
	}
}

// TestSeconds：页面数据和提示中的时长是秒数，前端按 {name, duration} 显示（textZH 与前端的规则相同）。
func TestSeconds(t *testing.T) {
	if seconds(25*time.Second) != 25 || seconds(2*time.Minute) != 120 || seconds(1500*time.Millisecond) != 2 {
		t.Error("seconds")
	}
	msg := message(msgRunnerOpDrained, strArg(argName, "mini-01"), intArg(argPoll, seconds(2*time.Minute)))
	if got := textZH(t, msg); got != "已暂停 mini-01 接收新任务（它正在进行的一轮领取结束后生效，最长约 2 分钟）。它现在没有正在执行的任务；需要时点“恢复接单”。" {
		t.Errorf("时长参数：%q", got)
	}
}
