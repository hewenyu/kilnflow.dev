package github

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGetTree(t *testing.T) {
	tests := []struct {
		name      string
		recursive bool
		wantQuery string
	}{
		{"递归", true, "1"},
		{"只读一层", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/git/trees/abc", jsonReply(http.StatusOK, map[string]any{
				"sha": "abc", "truncated": true,
				"tree": []any{map[string]any{"path": "a.go", "mode": "100644", "type": "blob", "sha": "b1", "size": 12}},
			}))
			tree, err := c.Inst(1).GetTree(t.Context(), "o/r", "abc", tt.recursive)
			if err != nil {
				t.Fatal(err)
			}
			want := TreeEntry{Path: "a.go", Mode: "100644", Type: "blob", SHA: "b1", Size: 12}
			if tree.SHA != "abc" || !tree.Truncated || len(tree.Tree) != 1 || tree.Tree[0] != want {
				t.Errorf("tree = %+v", tree)
			}
			if got := f.only(t).Query.Get("recursive"); got != tt.wantQuery {
				t.Errorf("recursive = %q，应为 %q", got, tt.wantQuery)
			}
		})
	}
}

func TestGetBlob(t *testing.T) {
	content := strings.Repeat("hello world\n", 20)
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	// GitHub 返回的 base64 每 60 个字符换行
	var wrapped strings.Builder
	for chunk := range slices.Chunk([]byte(enc), 60) {
		wrapped.Write(chunk)
		wrapped.WriteByte('\n')
	}
	tests := []struct {
		name    string
		reply   map[string]any
		want    string
		wantErr string
	}{
		{"带换行的 base64", map[string]any{"encoding": "base64", "content": wrapped.String()}, content, ""},
		{"utf-8 原文", map[string]any{"encoding": "utf-8", "content": "plain"}, "plain", ""},
		{"损坏的 base64", map[string]any{"encoding": "base64", "content": "!!!"}, "", "解码 base64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/git/blobs/b1", jsonReply(http.StatusOK, tt.reply))
			got, err := c.Inst(1).GetBlob(t.Context(), "o/r", "b1")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("错误应包含 %q，实际 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("内容 = %q，err=%v", got, err)
			}
		})
	}
	_, c, _ := newFakeAPI(t)
	if _, err := c.Inst(1).GetBlob(t.Context(), "o/r", "missing"); !IsNotFound(err) {
		t.Errorf("不存在的 blob 应返回 404：%v", err)
	}
}

func TestGetCommit(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/git/commits/abc", jsonReply(http.StatusOK, map[string]any{
		"sha": "abc", "message": "fix: x\n\nbody", "committer": map[string]any{"date": "2026-10-31T08:00:00Z"}, "tree": map[string]any{"sha": "t1"},
	}))
	cm, err := c.Inst(1).GetCommit(t.Context(), "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if cm.SHA != "abc" || cm.Message != "fix: x\n\nbody" || cm.Tree.SHA != "t1" || !cm.Committer.Date.Equal(time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("commit = %+v", cm)
	}
}

// ref 按段转义：保留 /，其他特殊字符转义。
func TestGetRef(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		wantPath string
	}{
		{"tag", "tags/v1.0.0", "/repos/o/r/git/ref/tags/v1.0.0"},
		{"带 / 的分支", "heads/feature/x", "/repos/o/r/git/ref/heads/feature/x"},
		{"特殊字符", "heads/a b#c?d", "/repos/o/r/git/ref/heads/a%20b%23c%3Fd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET "+tt.wantPath, jsonReply(http.StatusOK, map[string]any{"ref": "refs/" + tt.ref, "object": map[string]any{"type": "commit", "sha": "c1"}}))
			r, err := c.Inst(1).GetRef(t.Context(), "o/r", tt.ref)
			if err != nil {
				t.Fatalf("%v（请求路径 %s）", err, f.only(t).Path)
			}
			if r.Ref != "refs/"+tt.ref || r.Object != (GitObject{Type: "commit", SHA: "c1"}) {
				t.Errorf("ref = %+v", r)
			}
		})
	}
}

func TestMatchingRefs(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/git/matching-refs/tags/v1", pagedReply(150, "", func(i int) any {
		return map[string]any{"ref": fmt.Sprintf("refs/tags/v1.%d", i), "object": map[string]any{"type": "tag", "sha": strconv.Itoa(i)}}
	}))
	refs, err := c.Inst(1).MatchingRefs(t.Context(), "o/r", "tags/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 150 || refs[149].Ref != "refs/tags/v1.149" {
		t.Errorf("返回 %d 个 ref", len(refs))
	}
	if got := strings.Join(pages(t, f.requests()), ","); got != "1,2" {
		t.Errorf("请求的页：%s", got)
	}
}

// ResolveTag 把 lightweight tag 和（多层）annotated tag 解析到 commit。
func TestResolveTag(t *testing.T) {
	ref := func(typ, sha string) http.HandlerFunc {
		return jsonReply(http.StatusOK, map[string]any{"ref": "refs/tags/v1", "object": map[string]any{"type": typ, "sha": sha}})
	}
	tag := func(typ, sha string) http.HandlerFunc {
		return jsonReply(http.StatusOK, map[string]any{"object": map[string]any{"type": typ, "sha": sha}})
	}
	tests := []struct {
		name       string
		routes     map[string]http.HandlerFunc
		wantCommit string
		wantObject string
		wantErr    string
		notFound   bool
	}{
		{"lightweight tag", map[string]http.HandlerFunc{"GET /repos/o/r/git/ref/tags/v1": ref("commit", "c1")}, "c1", "c1", "", false},
		{"annotated tag", map[string]http.HandlerFunc{
			"GET /repos/o/r/git/ref/tags/v1": ref("tag", "t1"),
			"GET /repos/o/r/git/tags/t1":     tag("commit", "c1"),
		}, "c1", "t1", "", false},
		{"tag 指向 tag", map[string]http.HandlerFunc{
			"GET /repos/o/r/git/ref/tags/v1": ref("tag", "t1"),
			"GET /repos/o/r/git/tags/t1":     tag("tag", "t2"),
			"GET /repos/o/r/git/tags/t2":     tag("commit", "c2"),
		}, "c2", "t1", "", false},
		{"指向 tree", map[string]http.HandlerFunc{"GET /repos/o/r/git/ref/tags/v1": ref("tree", "tr")}, "", "", "对象类型是 tree", false},
		{"tag 不存在", map[string]http.HandlerFunc{}, "", "", "", true},
		{"tag 对象不存在", map[string]http.HandlerFunc{"GET /repos/o/r/git/ref/tags/v1": ref("tag", "t1")}, "", "", "", true},
		{"嵌套过深", map[string]http.HandlerFunc{
			"GET /repos/o/r/git/ref/tags/v1": ref("tag", "t1"),
			"GET /repos/o/r/git/tags/t1":     tag("tag", "t1"), // 自己指向自己
		}, "", "", "嵌套过深", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			for k, h := range tt.routes {
				f.handle(k, h)
			}
			commit, object, err := c.Inst(1).ResolveTag(t.Context(), "o/r", "v1")
			switch {
			case tt.notFound:
				if !IsNotFound(err) {
					t.Fatalf("应返回 404：%v", err)
				}
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("错误应包含 %q，实际 %v", tt.wantErr, err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if commit != tt.wantCommit || object != tt.wantObject {
					t.Errorf("ResolveTag = %s, %s，应为 %s, %s", commit, object, tt.wantCommit, tt.wantObject)
				}
			}
			if err != nil && (commit != "" || object != "") {
				t.Errorf("出错时应返回空值：%q %q", commit, object)
			}
		})
	}
}

func TestCompareFiles(t *testing.T) {
	files := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"filename": fmt.Sprintf("f%d", i)}
		}
		return out
	}
	tests := []struct {
		name         string
		files        []any
		want         []string
		wantComplete bool
	}{
		{"重命名包含新旧路径", []any{
			map[string]any{"filename": "a.go"},
			map[string]any{"filename": "new.go", "previous_filename": "old.go"},
		}, []string{"a.go", "new.go", "old.go"}, true},
		{"没有变更", []any{}, nil, true},
		{"达到 300 个时可能被截断", files(compareMaxFiles), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/compare/base...head", jsonReply(http.StatusOK, map[string]any{"files": tt.files, "total_commits": 2}))
			got, complete, err := c.Inst(1).CompareFiles(t.Context(), "o/r", "base", "head")
			if err != nil {
				t.Fatal(err)
			}
			if complete != tt.wantComplete {
				t.Errorf("complete = %v，应为 %v", complete, tt.wantComplete)
			}
			if tt.want != nil && !slices.Equal(got, tt.want) {
				t.Errorf("文件 = %v，应为 %v", got, tt.want)
			}
			if q := f.only(t).Query.Get("per_page"); q != "100" {
				t.Errorf("per_page = %q", q)
			}
		})
	}
	_, c, _ := newFakeAPI(t)
	if _, _, err := c.Inst(1).CompareFiles(t.Context(), "o/r", "x", "y"); !IsNotFound(err) {
		t.Errorf("应返回 404：%v", err)
	}
}

func TestPullFiles(t *testing.T) {
	tests := []struct {
		name         string
		total        int
		wantPages    int
		wantComplete bool
	}{
		{"一页", 3, 1, true},
		{"多页", 250, 3, true},
		{"达到 3000 个时可能被截断", pullMaxFiles + 500, pullFilePages, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/pulls/7/files", pagedReply(tt.total, "", func(i int) any {
				if i == 0 {
					return map[string]any{"filename": "new.go", "previous_filename": "old.go"}
				}
				return map[string]any{"filename": fmt.Sprintf("f%d", i)}
			}))
			got, complete, err := c.Inst(1).PullFiles(t.Context(), "o/r", 7)
			if err != nil {
				t.Fatal(err)
			}
			read := min(tt.total, pullMaxFiles)
			if len(got) != read+1 || got[0] != "new.go" || got[1] != "old.go" {
				t.Errorf("返回 %d 个路径（前两个 %v），应为 %d 个", len(got), got[:2], read+1)
			}
			if complete != tt.wantComplete || len(f.requests()) != tt.wantPages {
				t.Errorf("complete=%v 请求 %d 页，应为 %v、%d 页", complete, len(f.requests()), tt.wantComplete, tt.wantPages)
			}
		})
	}
	_, c, _ := newFakeAPI(t)
	if _, _, err := c.Inst(1).PullFiles(t.Context(), "o/r", 1); !IsNotFound(err) {
		t.Errorf("应返回 404：%v", err)
	}
}
