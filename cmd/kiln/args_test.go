package main

import (
	"bytes"
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantPos []string
		wantF   bool
		wantS   string
		wantErr string
	}{
		{name: "没有参数", args: nil, wantPos: nil},
		{name: "只有位置参数", args: []string{"a", "b"}, wantPos: []string{"a", "b"}},
		{name: "flag 在前", args: []string{"-f", "--s", "x", "a"}, wantPos: []string{"a"}, wantF: true, wantS: "x"},
		{name: "flag 在中间和后面", args: []string{"a", "-s=x", "b", "-f"}, wantPos: []string{"a", "b"}, wantF: true, wantS: "x"},
		{name: "-- 之后全部是位置参数", args: []string{"a", "--", "-f", "--s", "x"}, wantPos: []string{"a", "-f", "--s", "x"}},
		{name: "-- 在最前", args: []string{"--", "-f"}, wantPos: []string{"-f"}},
		{name: "-- 之前的 flag 仍然生效", args: []string{"-f", "--", "-s"}, wantPos: []string{"-s"}, wantF: true},
		{name: "单独的 - 是位置参数", args: []string{"-", "-f"}, wantPos: []string{"-"}, wantF: true},
		{name: "未知 flag", args: []string{"a", "--nope"}, wantErr: "kiln t：flag provided but not defined: -nope"},
		{name: "flag 缺少值", args: []string{"a", "-s"}, wantErr: "flag needs an argument: -s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &app{stdout: &bytes.Buffer{}}
			fs := newFlags("t")
			f := fs.Bool("f", false, "")
			s := fs.String("s", "", "")
			pos, err := a.parse(fs, tt.args)
			if tt.wantErr != "" {
				if _, ok := errors.AsType[*usageError](err); !ok || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse() 错误 = %v，期望包含 %q 的用法错误", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pos, tt.wantPos) || *f != tt.wantF || *s != tt.wantS {
				t.Errorf("parse() = %q, f = %v, s = %q；期望 %q, %v, %q", pos, *f, *s, tt.wantPos, tt.wantF, tt.wantS)
			}
		})
	}
}

// -h 输出参数说明到 stdout 并返回 flag.ErrHelp（退出码 0）。
func TestParseHelp(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	a := &app{stdout: &out}
	fs := newFlags("rerun")
	fs.String("mode", "failed", "failed | all")
	_, err := a.parse(fs, []string{"12", "-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parse(-h) 错误 = %v，期望 flag.ErrHelp", err)
	}
	for _, want := range []string{"kiln rerun 的参数：", "-mode", "failed | all"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("帮助输出 = %q，应包含 %q", out.String(), want)
		}
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "12", want: 12},
		{in: "#12", want: 12},
		{in: "0", want: 0},
		{in: "", wantErr: true},
		{in: "#", wantErr: true},
		{in: "##1", wantErr: true},
		{in: "1.5", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseID(tt.in)
			if tt.wantErr {
				if _, ok := errors.AsType[*usageError](err); !ok {
					t.Errorf("parseID(%q) 错误 = %v，期望用法错误", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("parseID(%q) = %d, %v；期望 %d", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestInt32Flag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int32
		wantErr string
	}{
		{in: "7", want: 7},
		{in: "-1", want: -1},
		{in: "2147483647", want: 2147483647},
		{in: "-2147483648", want: -2147483648},
		{in: "2147483648", wantErr: "超出范围"},
		{in: "-2147483649", wantErr: "超出范围"},
		{in: "x", wantErr: "需要整数"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			fs := newFlags("t")
			n := int32Flag(fs, "n", 30, "")
			err := fs.Parse([]string{"-n", tt.in})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("-n %s 错误 = %v，应包含 %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil || *n != tt.want {
				t.Errorf("-n %s = %d, %v；期望 %d", tt.in, *n, err, tt.want)
			}
		})
	}
}

func TestInt32FlagDefault(t *testing.T) {
	t.Parallel()
	fs := newFlags("t")
	n := int32Flag(fs, "n", 30, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *n != 30 || fs.Lookup("n").DefValue != "30" {
		t.Errorf("默认值 = %d（DefValue %q），期望 30", *n, fs.Lookup("n").DefValue)
	}
}

func TestMultiFlag(t *testing.T) {
	t.Parallel()
	fs := newFlags("t")
	var m multi
	fs.Var(&m, "i", "")
	if err := fs.Parse([]string{"-i", "a=1", "-i", "b=2"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m, multi{"a=1", "b=2"}) || m.String() != "a=1,b=2" {
		t.Errorf("multi = %q（String %q），期望按顺序保留每个值", m, m.String())
	}
}

func TestSplitSub(t *testing.T) {
	t.Parallel()
	if sub, rest := splitSub(nil); sub != "" || rest != nil {
		t.Errorf("splitSub(nil) = %q, %q", sub, rest)
	}
	if sub, rest := splitSub([]string{"set", "A", "B"}); sub != "set" || !slices.Equal(rest, []string{"A", "B"}) {
		t.Errorf("splitSub() = %q, %q", sub, rest)
	}
}

// 标准输入不是终端（测试中是 strings.Reader 或管道）时不当作终端处理。
func TestTerminalFd(t *testing.T) {
	t.Parallel()
	if _, ok := terminalFd(strings.NewReader("x")); ok {
		t.Error("strings.Reader 不应被当作终端")
	}
}
