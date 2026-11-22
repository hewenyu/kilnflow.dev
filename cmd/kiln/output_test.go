package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	a := &app{stdout: &out}
	a.table("A\tBB\tC", [][]string{{"1", "2", "3"}, {"long-value", "x", ""}})
	want := "A           BB  C\n1           2   3\nlong-value  x   \n"
	if out.String() != want {
		t.Errorf("table() 输出 =\n%q\n期望\n%q", out.String(), want)
	}
}

func TestAgo(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	tests := []struct {
		name string
		unix int64
		want string
	}{
		{name: "没有时间", unix: 0, want: "-"},
		// 取各区间中间的值，测试执行耗时不会改变结果
		{name: "分钟", unix: now.Add(-5*time.Minute - 30*time.Second).Unix(), want: "5m"},
		{name: "小时", unix: now.Add(-3*time.Hour - 30*time.Minute).Unix(), want: "3h"},
		{name: "两天以内仍按小时", unix: now.Add(-47*time.Hour - 30*time.Minute).Unix(), want: "47h"},
		{name: "更早显示日期", unix: old.Unix(), want: old.Format("2006-01-02")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ago(tt.unix); got != tt.want {
				t.Errorf("ago() = %q，期望 %q", got, tt.want)
			}
		})
	}
}

func TestAgoSeconds(t *testing.T) {
	t.Parallel()
	if got := ago(time.Now().Add(-20 * time.Second).Unix()); !strings.HasSuffix(got, "s") || got == "s" {
		t.Errorf("ago(20 秒前) = %q，期望以秒为单位", got)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{in: "short", n: 10, want: "short"},
		{in: "exactly10!", n: 10, want: "exactly10!"},
		{in: "eleven chars", n: 10, want: "eleven cha…"},
		{in: "多行\n错误", n: 10, want: "多行 错误"},
		{in: "中文按字符截断不切坏编码", n: 4, want: "中文按字…"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q，期望 %q", tt.in, tt.n, got, tt.want)
		}
	}
}
