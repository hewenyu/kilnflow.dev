// Package toolchainctl 是 kiln-runner toolchain 子命令（PLAN #33.12）：AI 执行节点的连接器 kiln-orca 用它在自己的数据
// 目录中安装、回收任务用的工具链（Go、Node、pnpm 等内置 runtime）。安装和回收的规则只在 internal/toolchain 中定义一次，
// 与 CI Runner 共用；这里只是命令行接口（stdin / stdout 上的 JSON）。
//
// 这些子命令不读 Runner 的配置和注册信息、不取数据目录锁、不连接 Server：AI 执行节点不是 CI Runner，只是借用同一个
// 二进制中的安装代码（C20：只共用代码层面的机制），目录（--root）由调用方给出，与 CI Runner 的 rt/ 各自独立。
// 同一个 root 上并发的 ensure 由 toolchain.Install 的文件锁互斥；gc 不能与同一个 root 上的 ensure 同时进行（调用方保证）。
package toolchainctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// Format 是输出 JSON 的格式版本：连接器只接受它认识的版本（kiln-runner toolchain info 报告）。
const Format = 1

// maxInput 是 ensure 从 stdin 读取的 JSON 的大小上限。
const maxInput = 1 << 20

// Usage 是子命令的用法。
const Usage = `  toolchain info                                    输出 JSON：版本和工具链子命令的格式版本（AI 执行节点的连接器检查它）
  toolchain ensure --root DIR                       从 stdin 读 {"tools":[{name,version,platform,digest}]}，装到 DIR/<digest hex>
                                                    （已装好的直接复用），stdout 输出每项的目录、是否复用、用时和环境变量
  toolchain gc --root DIR [--keep sha256:…]… [--max-bytes N]
                                                    按 CI Runner 的规则回收 DIR（PLAN #12.8：长期没用的、超过容量上限时
                                                    最久没用的、过期的安装残留）；--keep 的不删，stdout 输出结果
`

// UsageError 是用法错误（退出码 2）。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Options 是由调用方（cmd/kiln-runner）给出的、属于 Runner 端共用约定的值。
type Options struct {
	// TempRetention 是安装残留的保留期（过程数据，C10）。
	TempRetention time.Duration
	// Remove 删除一个条目（目录整个删除，包括只读的文件）。
	Remove func(path string) error
}

// Info 是 info 的输出。
type Info struct {
	Format  int    `json:"format"`
	Version string `json:"version"`
}

// Tool 是 ensure 的一项输入（与 airunner.v2 的 ToolRef 相同）。
type Tool struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
}

// EnsureInput 是 ensure 从 stdin 读取的内容。
type EnsureInput struct {
	Tools []Tool `json:"tools"`
}

// EnsuredTool 是 ensure 的一项输出。
type EnsuredTool struct {
	Tool
	Dir    string `json:"dir"`
	Reused bool   `json:"reused"`
	Millis int64  `json:"millis"`
}

// EnsureOutput 是 ensure 的输出：Env.Set 中的变量直接设置，Env.Prepend 中的变量把路径按顺序加到原值之前。
type EnsureOutput struct {
	Format int            `json:"format"`
	Tools  []*EnsuredTool `json:"tools"`
	Env    *toolchain.Env `json:"env"`
}

// GCOutput 是 gc 的输出。
type GCOutput struct {
	Format int `json:"format"`
	*toolchain.GCResult
}

// Run 执行 toolchain 的子命令 args。结果 JSON 写到 stdout，进度和说明写到 stderr。
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, o Options) error {
	if len(args) == 0 {
		return &UsageError{Msg: "需要子命令：info、ensure 或 gc"}
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "info":
		if len(rest) > 0 {
			return &UsageError{Msg: fmt.Sprintf("多余的参数 %q", rest)}
		}
		return writeJSON(stdout, Info{Format: Format, Version: buildinfo.Version()})
	case "ensure":
		return ensure(ctx, rest, stdin, stdout, stderr)
	case "gc":
		return gc(rest, stdout, stderr, o)
	default:
		return &UsageError{Msg: "未知的子命令 toolchain " + sub}
	}
}

// parse 解析子命令的参数；--root 必填。
func parse(name string, args []string, stderr io.Writer, define func(*flag.FlagSet)) (root string, err error) {
	fs := flag.NewFlagSet("toolchain "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&root, "root", "", "工具链的安装目录")
	if define != nil {
		define(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", err
		}
		return "", &UsageError{} // flag 已经输出了错误和参数说明
	}
	if fs.NArg() > 0 {
		return "", &UsageError{Msg: fmt.Sprintf("多余的参数 %q", fs.Args())}
	}
	if root == "" {
		return "", &UsageError{Msg: "需要 --root"}
	}
	return root, nil
}

func ensure(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	root, err := parse("ensure", args, stderr, nil)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxInput+1))
	if err != nil {
		return fmt.Errorf("读取 stdin：%w", err)
	}
	if len(b) > maxInput {
		return fmt.Errorf("stdin 超过 %d 字节", maxInput)
	}
	var in EnsureInput
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("stdin 不是工具列表（{\"tools\":[…]}）：%w", err)
	}
	refs := make([]toolchain.Ref, 0, len(in.Tools))
	for _, t := range in.Tools {
		refs = append(refs, toolchain.Ref{Name: t.Name, Version: t.Version, Platform: t.Platform, Digest: t.Digest})
	}
	done, env, err := toolchain.Ensure(ctx, root, refs, func(e *toolchain.Ensured) {
		fmt.Fprintf(stderr, "%s %s（%s）：%s\n", e.Name, e.Version, e.Platform, toolchain.SourceText(e.Reused, e.Duration))
	})
	if err != nil {
		return err
	}
	out := &EnsureOutput{Format: Format, Tools: make([]*EnsuredTool, 0, len(done)), Env: env}
	for _, e := range done {
		out.Tools = append(out.Tools, &EnsuredTool{
			Name: e.Name, Version: e.Version, Platform: e.Platform, Digest: e.Digest,
			Dir: e.Dir, Reused: e.Reused, Millis: e.Duration.Milliseconds(),
		})
	}
	return writeJSON(stdout, out)
}

// keepList 是可以重复的 --keep 参数。
type keepList []string

func (k *keepList) String() string { return strings.Join(*k, ",") }

func (k *keepList) Set(v string) error {
	*k = append(*k, v)
	return nil
}

func gc(args []string, stdout, stderr io.Writer, o Options) error {
	var keep keepList
	var maxBytes int64
	root, err := parse("gc", args, stderr, func(fs *flag.FlagSet) {
		fs.Var(&keep, "keep", "正在使用、不能删除的 runtime 的 digest（sha256:…），可以重复")
		fs.Int64Var(&maxBytes, "max-bytes", 0, "装好的 runtime 的总量上限（字节），0 表示默认上限（toolchain.DefaultMaxSize）")
	})
	if err != nil {
		return err
	}
	if maxBytes < 0 {
		return &UsageError{Msg: "--max-bytes 不能为负数"}
	}
	res, err := toolchain.GC(root, toolchain.GCOptions{MaxBytes: maxBytes, TempRetention: o.TempRetention, Keep: keep, Remove: o.Remove})
	if err != nil {
		return err
	}
	for _, e := range res.Removed {
		fmt.Fprintf(stderr, "删除 %s（%d 字节，最后使用 %s）\n", e.Path, e.Size, e.LastUsed.Format(time.RFC3339))
	}
	if err := writeJSON(stdout, GCOutput{Format: Format, GCResult: res}); err != nil {
		return err
	}
	if len(res.Failures) > 0 {
		return fmt.Errorf("%d 项删除失败，下次回收时重试", len(res.Failures))
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
