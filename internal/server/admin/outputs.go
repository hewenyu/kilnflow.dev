package admin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
)

// job 的产物（结果 manifest 中的命名输出）：Dashboard 的产物列表和下载使用。下载本身走 CAS 的预签名 URL。

// maxListedOutputFiles 是每个输出最多列出的文件数（按路径排序）；超出的只计入总数和总大小。
const maxListedOutputFiles = 200

// JobOutputs 是一个 job 的产物。
type JobOutputs struct {
	// Manifest 是结果 manifest 的 digest；job 没有结果时为空。
	Manifest string
	// Missing 表示结果 manifest 已不在 CAS 中（无法列出产物）。
	Missing bool
	Outputs []*JobOutput
}

// JobOutput 是一个命名输出。
type JobOutput struct {
	Name string
	// Digest 是这个输出的 manifest digest。
	Digest string
	// Missing 表示输出的 manifest 已不在 CAS 中。
	Missing bool
	// Files 是按路径排序的前 maxListedOutputFiles 个文件。
	Files []OutputFile
	// FileCount、Size 是全部文件的个数和总字节数（符号链接不计大小）。
	FileCount int
	Size      int64
}

// OutputFile 是输出中的一个文件。
type OutputFile struct {
	cas.FileEntry
	// Missing 表示文件内容已不在 CAS 中。
	Missing bool
}

// Single 表示输出恰好是一个普通文件（模板据此直接给出下载按钮）。
func (o *JobOutput) Single() bool {
	return o.FileCount == 1 && len(o.Files) == 1 && o.Files[0].Symlink == ""
}

// Hidden 返回没有列出的文件数。
func (o *JobOutput) Hidden() int { return o.FileCount - len(o.Files) }

// JobOutputs 列出 job 的产物。job 没有结果（未成功、发布 job、没有声明 outputs 的 job 也会有空结果）时 Manifest 为空。
func (s *Service) JobOutputs(ctx context.Context, j *cistore.Job) (*JobOutputs, error) {
	res := &JobOutputs{}
	if j.ResultManifest == nil || *j.ResultManifest == "" {
		return res, nil
	}
	res.Manifest = *j.ResultManifest
	var rm cas.ResultManifest
	if err := cas.ReadJSON(ctx, s.cas, res.Manifest, &rm); err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			res.Missing = true
			return res, nil
		}
		return nil, fmt.Errorf("读取结果 manifest：%w", err)
	}
	names := make([]string, 0, len(rm.Outputs))
	for n := range rm.Outputs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		o, err := s.readOutput(ctx, n, rm.Outputs[n])
		if err != nil {
			return nil, err
		}
		res.Outputs = append(res.Outputs, o)
	}
	return res, nil
}

func (s *Service) readOutput(ctx context.Context, name, digest string) (*JobOutput, error) {
	o := &JobOutput{Name: name, Digest: digest}
	var om cas.OutputManifest
	if err := cas.ReadJSON(ctx, s.cas, digest, &om); err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			o.Missing = true
			return o, nil
		}
		return nil, fmt.Errorf("读取输出 %s 的 manifest：%w", name, err)
	}
	files := slices.Clone(om.Files)
	slices.SortFunc(files, func(a, b cas.FileEntry) int { return strings.Compare(a.Path, b.Path) })
	o.FileCount = len(files)
	for i, f := range files {
		if f.Symlink == "" {
			o.Size += f.Size
		}
		if i >= maxListedOutputFiles {
			continue
		}
		of := OutputFile{FileEntry: f}
		if f.Symlink == "" {
			ok, err := s.cas.Has(ctx, f.Digest)
			if err != nil {
				return nil, err
			}
			of.Missing = !ok
		}
		o.Files = append(o.Files, of)
	}
	return o, nil
}

// ErrOutputGone 表示产物文件的内容已不在 CAS 中。
var ErrOutputGone = errors.New("产物已不在存储中")

// JobOutputFile 返回 job 的输出 output 中路径为 path 的文件。
// job、输出或文件不存在时返回 NotFound；文件内容已不在 CAS 中时返回 ErrOutputGone。
func (s *Service) JobOutputFile(ctx context.Context, jobID int64, output, path string) (*cas.FileEntry, error) {
	j, err := cistore.GetJob(ctx, s.db, jobID, false)
	if isNotFound(err) {
		return nil, notFound("没有 job #%d", jobID)
	}
	if err != nil {
		return nil, err
	}
	if j.ResultManifest == nil || *j.ResultManifest == "" {
		return nil, notFound("job #%d 没有产物", jobID)
	}
	var rm cas.ResultManifest
	if err := cas.ReadJSON(ctx, s.cas, *j.ResultManifest, &rm); err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			return nil, ErrOutputGone
		}
		return nil, err
	}
	d, ok := rm.Outputs[output]
	if !ok {
		return nil, notFound("job #%d 没有名为 %s 的输出", jobID, output)
	}
	var om cas.OutputManifest
	if err := cas.ReadJSON(ctx, s.cas, d, &om); err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			return nil, ErrOutputGone
		}
		return nil, err
	}
	i := slices.IndexFunc(om.Files, func(f cas.FileEntry) bool { return f.Path == path })
	if i < 0 || om.Files[i].Symlink != "" {
		return nil, notFound("输出 %s 中没有文件 %s", output, path)
	}
	f := om.Files[i]
	ok, err = s.cas.Has(ctx, f.Digest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrOutputGone
	}
	return &f, nil
}
