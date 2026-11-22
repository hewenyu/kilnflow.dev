package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// table 以对齐的列输出到 stdout；header 和每行的列用制表符分隔。
func (a *app) table(header string, rows [][]string) {
	w := tabwriter.NewWriter(a.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, header)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush() // 写 stdout 失败时没有更合适的地方报告
}

func ago(unix int64) string {
	if unix == 0 {
		return "-"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return time.Unix(unix, 0).Format("2006-01-02")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

func boolText(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
