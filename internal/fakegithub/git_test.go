package fakegithub

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// ref 按完整名称查找，指向的对象类型与 git 一致（annotated tag 是 tag，其他是 commit）。
func TestRefs(t *testing.T) {
	f := newFixture(t)
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	gitOut(t, f.dir, "tag", "light")
	gitOut(t, f.dir, "branch", "feature/x", "HEAD~1")
	first := gitOut(t, f.dir, "rev-parse", "HEAD~1")
	tagObj := gitOut(t, f.dir, "rev-parse", "v1")

	tests := []struct {
		name     string
		ref      string
		wantType string
		wantSHA  string
	}{
		{"分支", "heads/main", "commit", head},
		{"带 / 的分支", "heads/feature/x", "commit", first},
		{"lightweight tag", "tags/light", "commit", head},
		{"annotated tag", "tags/v1", "tag", tagObj},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := f.inst.GetRef(t.Context(), demo, tt.ref)
			if err != nil {
				t.Fatal(err)
			}
			if r.Ref != "refs/"+tt.ref || r.Object.Type != tt.wantType || r.Object.SHA != tt.wantSHA {
				t.Errorf("GetRef(%s) = %+v，应为 %s %s", tt.ref, r, tt.wantType, tt.wantSHA)
			}
		})
	}
	for _, missing := range []string{"heads/nope", "tags/v", "heads/feature"} {
		if _, err := f.inst.GetRef(t.Context(), demo, missing); !github.IsNotFound(err) {
			t.Errorf("GetRef(%s) 应返回 404：%v", missing, err)
		}
	}

	refs, err := f.inst.MatchingRefs(t.Context(), demo, "tags")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range refs {
		names = append(names, r.Ref)
	}
	if !slices.Equal(names, []string{"refs/tags/light", "refs/tags/v1"}) {
		t.Errorf("MatchingRefs(tags) = %v", names)
	}
	if refs, err := f.inst.MatchingRefs(t.Context(), demo, "tags/none"); err != nil || len(refs) != 0 {
		t.Errorf("没有匹配时应返回空列表：%v %v", refs, err)
	}

	// annotated tag 解开到 commit
	commit, obj, err := f.inst.ResolveTag(t.Context(), demo, "v1")
	if err != nil || commit != head || obj != tagObj {
		t.Errorf("ResolveTag(v1) = %s %s %v", commit, obj, err)
	}
	commit, obj, err = f.inst.ResolveTag(t.Context(), demo, "light")
	if err != nil || commit != head || obj != head {
		t.Errorf("ResolveTag(light) = %s %s %v", commit, obj, err)
	}
}

func TestCommitsAndBranches(t *testing.T) {
	f := newFixture(t)
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	tree := gitOut(t, f.dir, "rev-parse", "HEAD^{tree}")
	gitOut(t, f.dir, "branch", "dev", "HEAD~1")

	cm, err := f.inst.GetCommit(t.Context(), demo, head)
	if err != nil {
		t.Fatal(err)
	}
	if cm.SHA != head || cm.Message != "two" || cm.Tree.SHA != tree || cm.Committer.Date.IsZero() {
		t.Errorf("GetCommit = %+v", cm)
	}
	// 短 sha 和分支名也可以
	if cm, err := f.inst.GetCommit(t.Context(), demo, "main"); err != nil || cm.SHA != head {
		t.Errorf("GetCommit(main) = %+v %v", cm, err)
	}
	if _, err := f.inst.GetCommit(t.Context(), demo, strings.Repeat("0", 40)); !github.IsNotFound(err) {
		t.Errorf("不存在的 commit 应返回 404：%v", err)
	}

	bs, err := f.inst.ListBranches(t.Context(), demo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bs {
		got[b.Name] = b.Commit.SHA
	}
	if len(got) != 2 || got["main"] != head || got["dev"] != gitOut(t, f.dir, "rev-parse", "dev") {
		t.Errorf("ListBranches = %v", got)
	}

	blob := gitOut(t, f.dir, "rev-parse", "HEAD:src/b.txt")
	if data, err := f.inst.GetBlob(t.Context(), demo, blob); err != nil || string(data) != "b\n" {
		t.Errorf("GetBlob = %q %v", data, err)
	}
	if _, err := f.inst.GetFileContent(t.Context(), demo, "src/b.txt", "HEAD~1"); !github.IsNotFound(err) {
		t.Errorf("旧 commit 中没有的文件应返回 404：%v", err)
	}
	if _, err := f.inst.GetTree(t.Context(), demo, "nope", true); !github.IsNotFound(err) {
		t.Errorf("不存在的 tree 应返回 404：%v", err)
	}
}

// PR 的文件列表是 PullRequestPayload 登记的 head 相对其父 commit 的变更。
func TestPullFiles(t *testing.T) {
	f := newFixture(t)
	head := gitOut(t, f.dir, "rev-parse", "HEAD")
	base := gitOut(t, f.dir, "rev-parse", "HEAD~1")
	payload := f.s.PullRequestPayload(f.repo, "opened", 3, "feat", head, "main", base)
	pr, ok := payload["pull_request"].(map[string]any)
	if !ok || pr["number"] != 3 {
		t.Fatalf("payload：%v", payload)
	}
	files, complete, err := f.inst.PullFiles(t.Context(), demo, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !slices.Equal(files, []string{"src/b.txt"}) {
		t.Errorf("PullFiles = %v %v", files, complete)
	}
	if _, _, err := f.inst.CompareFiles(t.Context(), demo, "nope", head); !github.IsNotFound(err) {
		t.Errorf("无效的 base 应返回 404：%v", err)
	}
}

// 不存在的仓库的 git 接口都返回 404。
func TestGitUnknownRepo(t *testing.T) {
	f := newFixture(t)
	f.s.RemoveRepo(f.repo)
	calls := map[string]func() error{
		"GetTree":      func() error { _, err := f.inst.GetTree(t.Context(), demo, "HEAD", true); return err },
		"GetBlob":      func() error { _, err := f.inst.GetBlob(t.Context(), demo, "HEAD"); return err },
		"GetCommit":    func() error { _, err := f.inst.GetCommit(t.Context(), demo, "HEAD"); return err },
		"GetRef":       func() error { _, err := f.inst.GetRef(t.Context(), demo, "heads/main"); return err },
		"MatchingRefs": func() error { _, err := f.inst.MatchingRefs(t.Context(), demo, "tags"); return err },
		"ListBranches": func() error { _, err := f.inst.ListBranches(t.Context(), demo); return err },
		"PullFiles":    func() error { _, _, err := f.inst.PullFiles(t.Context(), demo, 1); return err },
		"CompareFiles": func() error { _, _, err := f.inst.CompareFiles(t.Context(), demo, "a", "b"); return err },
		"Contents":     func() error { _, err := f.inst.GetFileContent(t.Context(), demo, "a.txt", "HEAD"); return err },
		"Tag": func() error {
			_, _, err := f.inst.PeelObject(t.Context(), demo, github.GitObject{Type: "tag", SHA: "x"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !github.IsNotFound(err) {
			t.Errorf("%s 应返回 404：%v", name, err)
		}
	}
}
