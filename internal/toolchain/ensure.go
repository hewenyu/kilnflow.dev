package toolchain

import (
	"context"
	"os"
	"time"
)

// Ensured 是 Ensure 中一个 runtime 的结果。
type Ensured struct {
	Ref
	// Dir 是 runtime 的目录（<root>/<digest hex>）。
	Dir string
	// Reused 为 true 表示已经装好，这次没有访问下载源。
	Reused bool
	// Duration 是这一项的用时。
	Duration time.Duration
}

// Ensure 确保 refs 中的 runtime 都已装到 root（按顺序逐个 Install：已装好的直接复用，缺少的下载、校验、安装；同一个
// runtime 同一时刻只安装一次，见 Install），刷新每个目录的最后使用时间（mtime，GC 据此回收），并按 refs 的顺序计算
// 环境变量（Environment，Mount 即目录本身）。progress 不为 nil 时每完成一项调用一次。AI 执行节点的任务用它准备工具链
// （kiln-runner toolchain ensure，PLAN #33.12）。出错时返回已经完成的项和错误（错误中带有 runtime 的名称、版本和平台）。
func Ensure(ctx context.Context, root string, refs []Ref, progress func(*Ensured)) ([]*Ensured, *Env, error) {
	out := make([]*Ensured, 0, len(refs))
	mounts := make([]Mounted, 0, len(refs))
	for _, ref := range refs {
		start := time.Now()
		in, err := Install(ctx, root, ref)
		if err != nil {
			return out, nil, err
		}
		now := time.Now()
		// 记录最后使用的时间；失败只影响回收的先后，不影响这次使用
		_ = os.Chtimes(in.Dir, now, now)
		e := &Ensured{Ref: ref, Dir: in.Dir, Reused: in.Reused, Duration: now.Sub(start)}
		out = append(out, e)
		mounts = append(mounts, Mounted{Mount: in.Dir, Meta: in.Meta})
		if progress != nil {
			progress(e)
		}
	}
	env, err := Environment(mounts)
	if err != nil {
		return out, nil, err
	}
	return out, env, nil
}

// SourceText 说明 runtime 是怎么来的（写进任务日志）：已经装好的直接使用，否则是这次下载并安装的，d 是用时。
// CI 的 job 日志和 AI 任务的日志用同一种写法。
func SourceText(reused bool, d time.Duration) string {
	if reused {
		return "已安装，直接使用"
	}
	return "下载并安装，用时 " + d.Round(time.Millisecond).String()
}
