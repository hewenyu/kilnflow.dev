package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// maxTagDepth 是解开 annotated tag 的最大层数。
	maxTagDepth = 10
	// maxRefPages 是列出匹配 ref 时最多请求的页数。
	maxRefPages = 50
	// compareMaxFiles 是 compare 接口最多返回的文件数，达到时列表可能被截断。
	compareMaxFiles = 300
	// pullMaxFiles 是 PR 文件列表接口最多返回的文件数。
	pullMaxFiles = 3000
	// pullFilePages 是读取 PR 文件列表时最多请求的页数（pullMaxFiles / perPage）。
	pullFilePages = pullMaxFiles / perPage
)

// TreeEntry 是 tree 中的一项。
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"` // blob | tree | commit
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree 是 git tree 对象。
type Tree struct {
	SHA       string      `json:"sha"`
	Tree      []TreeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// GetTree 读取递归 tree；sha 可以是 commit sha。
func (i *Inst) GetTree(ctx context.Context, repo, sha string, recursive bool) (*Tree, error) {
	p := fmt.Sprintf("/repos/%s/git/trees/%s", repo, esc(sha))
	if recursive {
		p += "?recursive=1"
	}
	return call[Tree](ctx, i.Do, http.MethodGet, p, nil)
}

// GetBlob 读取 blob 的内容。
func (i *Inst) GetBlob(ctx context.Context, repo, sha string) ([]byte, error) {
	b, err := call[encodedContent](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/git/blobs/%s", repo, esc(sha)), nil)
	if err != nil {
		return nil, err
	}
	return b.decode()
}

// GitCommit 是 git commit 对象。
type GitCommit struct {
	SHA       string `json:"sha"`
	Message   string `json:"message"`
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// GetCommit 读取 commit 对象。
func (i *Inst) GetCommit(ctx context.Context, repo, sha string) (*GitCommit, error) {
	return call[GitCommit](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repo, esc(sha)), nil)
}

// GitObject 是 ref 指向的对象。
type GitObject struct {
	Type string `json:"type"` // commit | tag | tree | blob
	SHA  string `json:"sha"`
}

// GitRef 是一个 git ref。
type GitRef struct {
	Ref    string    `json:"ref"`
	Object GitObject `json:"object"`
}

// GetRef 读取一个 ref，例如 tags/v1.0.0、heads/main。
func (i *Inst) GetRef(ctx context.Context, repo, ref string) (*GitRef, error) {
	return call[GitRef](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/git/ref/%s", repo, escPath(ref)), nil)
}

// MatchingRefs 列出前缀匹配的 ref，例如 tags。
func (i *Inst) MatchingRefs(ctx context.Context, repo, prefix string) ([]GitRef, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/git/matching-refs/%s", repo, escPath(prefix)), maxRefPages, unwrapList[GitRef])
}

// CreateRef 创建 ref（ref 是完整的名字，如 refs/tags/v1.0.0）。ref 已经存在时 GitHub 返回 422（"Reference already exists"），
// 不会移动已有的 ref：这是带条件的写操作，重复请求不会建出第二个 ref。
func (i *Inst) CreateRef(ctx context.Context, repo, ref, sha string) error {
	return i.Do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/refs", repo), map[string]string{"ref": ref, "sha": sha}, nil)
}

// ResolveTag 把 tag 解析到 commit（annotated tag 逐层解开，PLAN #7.4）。返回 commit sha 和 ref 指向的对象 sha。
func (i *Inst) ResolveTag(ctx context.Context, repo, tag string) (commit, object string, err error) {
	r, err := i.GetRef(ctx, repo, "tags/"+tag)
	if err != nil {
		return "", "", err
	}
	return i.PeelObject(ctx, repo, r.Object)
}

// PeelObject 把 ref 指向的对象解开到 commit。
func (i *Inst) PeelObject(ctx context.Context, repo string, obj GitObject) (commit, object string, err error) {
	object = obj.SHA
	for range maxTagDepth {
		switch obj.Type {
		case "commit":
			return obj.SHA, object, nil
		case "tag":
			var t struct {
				Object GitObject `json:"object"`
			}
			if err := i.get(ctx, fmt.Sprintf("/repos/%s/git/tags/%s", repo, esc(obj.SHA)), &t); err != nil {
				return "", "", err
			}
			obj = t.Object
		default:
			return "", "", fmt.Errorf("tag 指向的对象类型是 %s，不是 commit", obj.Type)
		}
	}
	return "", "", errors.New("tag 嵌套过深")
}

// changedFile 是 compare、PR 文件列表接口返回的一个变更文件。
type changedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

// fileNames 返回变更涉及的全部路径（重命名的文件同时包含新旧路径）。
func fileNames(files []changedFile) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Filename)
		if f.PreviousFilename != "" {
			out = append(out, f.PreviousFilename)
		}
	}
	return out
}

// CompareFiles 返回 base...head 之间变更的文件。complete=false 表示列表被截断（PLAN #7.2：视为匹配）。
func (i *Inst) CompareFiles(ctx context.Context, repo, base, head string) ([]string, bool, error) {
	var cmp struct {
		Files        []changedFile `json:"files"`
		TotalCommits int           `json:"total_commits"`
	}
	p := fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=%d", repo, esc(base), esc(head), perPage)
	if err := i.get(ctx, p, &cmp); err != nil {
		return nil, false, err
	}
	return fileNames(cmp.Files), len(cmp.Files) < compareMaxFiles, nil
}

// PullFiles 返回 PR 变更的文件（最多 3000 个）。
func (i *Inst) PullFiles(ctx context.Context, repo string, number int) ([]string, bool, error) {
	files, err := paginate(ctx, i.get, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, number), pullFilePages, unwrapList[changedFile])
	if err != nil {
		return nil, false, err
	}
	return fileNames(files), len(files) < pullMaxFiles, nil
}
