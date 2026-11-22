package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// newFlags 返回不自行输出、不自行退出的 FlagSet，错误由 parse 统一处理。
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse 允许参数和 flag 混排，返回位置参数。
// -h 时输出参数说明并返回 flag.ErrHelp；其他解析错误返回 usageError。
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(a.stdout, "kiln %s 的参数：\n", fs.Name())
				fs.SetOutput(a.stdout)
				fs.PrintDefaults()
				return nil, err
			}
			return nil, usageErrorf("kiln %s：%v", fs.Name(), err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// fs.Parse 遇到单独的 "--" 时停止解析并丢弃它：按惯例之后的参数全部是位置参数，不再当作 flag。
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// splitSub 把参数拆成子命令和其余参数。
func splitSub(args []string) (sub string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	return args[0], args[1:]
}

// need 在位置参数不足时返回用法错误。
func need(pos []string, n int, what string) error {
	if len(pos) < n {
		return usageErrorf("用法：kiln %s", what)
	}
	return nil
}

// parseID 解析数字 ID，允许 #123 的写法。
func parseID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil {
		return 0, usageErrorf("需要数字 ID：%s", s)
	}
	return n, nil
}

// multi 是可以重复给出的字符串 flag。
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// int32Value 是 int32 类型的 flag，对应 proto 中的 int32 字段，解析时检查范围。
type int32Value int32

func (v *int32Value) String() string { return strconv.FormatInt(int64(*v), 10) }

func (v *int32Value) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("需要整数")
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return errors.New("超出范围")
	}
	*v = int32Value(n)
	return nil
}

func int32Flag(fs *flag.FlagSet, name string, value int32, usage string) *int32 {
	p := new(int32)
	*p = value
	fs.Var((*int32Value)(p), name, usage)
	return p
}

// scopeFlags 注册 --account、--repo、--env，返回的函数在解析后构造 Scope。
func scopeFlags(fs *flag.FlagSet) func() (*apiv1.Scope, error) {
	acc := fs.String("account", "", "账号级")
	repo := fs.String("repo", "", "仓库级（owner/name）")
	env := fs.String("env", "", "environment 级")
	return func() (*apiv1.Scope, error) {
		if *acc == "" && *repo == "" {
			return nil, usageErrorf("需要 --account 或 --repo")
		}
		return &apiv1.Scope{Account: *acc, Repo: *repo, Environment: *env}, nil
	}
}

// terminalFd 在 r 是终端时返回它的文件描述符。
func terminalFd(r io.Reader) (int, bool) {
	f, ok := r.(*os.File)
	if !ok {
		return 0, false
	}
	fd := f.Fd()
	if fd > math.MaxInt32 {
		return 0, false
	}
	n := int(fd)
	return n, term.IsTerminal(n)
}
