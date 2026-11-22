package publish

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/expr"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 文件类发布目标共用的 asset（release.assets，PLAN #13.4 阶段 0）：在 Server 上确定性打包一次。

const (
	// reproducibleEpoch 是 commit 时间未知时包内条目使用的修改时间（1980-01-01，zip 能表示的最早时间）。
	reproducibleEpoch = 315532800
	// defaultChecksums 是不写 release.checksums 时校验和文件的名字。
	defaultChecksums = "SHA256SUMS"
)

// buildAssets 从上游输出生成全部 asset（确定性打包，PLAN #13.4 阶段 0），并按配置追加校验和文件。
// 各文件类发布目标（GitHub Release、HTTP 上传）发布的是同一份字节和同一个校验和文件。
func (w *Worker) buildAssets(ctx context.Context, pj *publishJob, work string) ([]release.Asset, error) {
	var out []release.Asset
	seen := map[string]bool{}
	mtime := assetModTime(pj.run)
	list, checksums := pj.job.Spec.Release.FileAssets()
	for _, a := range list {
		ups, output, err := w.upstreamJobs(ctx, pj.job, a.From)
		if err != nil {
			return nil, err
		}
		for _, up := range ups {
			ra, nfiles, err := w.packUpstream(ctx, pj, a, up, output, work, mtime)
			if err != nil {
				return nil, err
			}
			if seen[ra.Name] {
				return nil, fmt.Errorf("asset 文件名重复：%s（用 name 引用 matrix 区分各变体）", ra.Name)
			}
			seen[ra.Name] = true
			pj.plog.printf("打包 %s ← %s.%s（%d 个文件，%s）", ra.Name, up.DisplayName, output, nfiles, bytesize.Format(ra.Size))
			out = append(out, *ra)
		}
	}
	slices.SortFunc(out, func(a, b release.Asset) int { return cmp.Compare(a.Name, b.Name) })
	checksums = cmp.Or(checksums, defaultChecksums)
	if checksums != workflow.ChecksumsNone {
		if err := release.CheckFileName(checksums); err != nil {
			return nil, fmt.Errorf("校验和文件名（release.checksums）无效：%w", err)
		}
	}
	if len(out) > 0 && checksums != workflow.ChecksumsNone {
		if seen[checksums] {
			return nil, fmt.Errorf("校验和文件名 %s 与 asset 重名（修改 release.checksums）", checksums)
		}
		sum, err := release.WriteChecksums(work, checksums, out)
		if err != nil {
			return nil, err
		}
		out = append(out, *sum)
	}
	return out, nil
}

// assetModTime 是包内所有条目的修改时间：构建的 commit 时间（没有时用固定的时间），保证打包结果确定。
func assetModTime(run *cistore.Run) time.Time {
	if run.CommitTime == 0 {
		return time.Unix(reproducibleEpoch, 0).UTC()
	}
	return time.Unix(run.CommitTime, 0).UTC()
}

// packUpstream 把一个上游 job 的输出打包成 asset，返回 asset 和输出中的文件数。
func (w *Worker) packUpstream(ctx context.Context, pj *publishJob, a workflow.Asset, up *cistore.Job, output, work string, mtime time.Time) (*release.Asset, int, error) {
	om, err := w.upstreamOutput(ctx, up, output)
	if err != nil {
		return nil, 0, fmt.Errorf("%s.%s：%w", up.DisplayName, output, err)
	}
	name, err := assetName(a, output, up, pj.run)
	if err != nil {
		return nil, 0, err
	}
	if err := checkAssetName(name); err != nil {
		return nil, 0, err
	}
	ra, err := release.Pack(ctx, w.cas, release.PackRequest{Archive: a.Archive, Name: name, Files: om.Files, WorkDir: work, ModTime: mtime})
	if err != nil {
		return nil, 0, fmt.Errorf("打包 %s：%w", name, err)
	}
	return ra, len(om.Files), nil
}

// assetName 计算不含扩展名的 asset 名：name 可引用 matrix（上游变体）和 github；
// 不写时为 <输出名>[-<矩阵取值，按变量名排序>]。
func assetName(a workflow.Asset, output string, up *cistore.Job, run *cistore.Run) (string, error) {
	if a.Name != "" {
		return expr.Interpolate(a.Name, upstreamContext(up, run))
	}
	keys := slices.Sorted(maps.Keys(up.Matrix))
	var b strings.Builder
	b.WriteString(output)
	for _, k := range keys {
		b.WriteString("-" + expr.ToString(up.Matrix[k]))
	}
	return b.String(), nil
}

// checkAssetName 检查 asset 名可以用作文件名：name 引用的 matrix、github（例如带 / 的 tag 名）在发布时才求值，
// 含路径分隔符时打包会写到工作目录之外或者失败，GitHub Release 和 HTTP 上传也不接受。
func checkAssetName(name string) error {
	if err := release.CheckFileName(name); err != nil {
		return fmt.Errorf("%w（检查 name 引用的 matrix、github 取值）", err)
	}
	return nil
}
