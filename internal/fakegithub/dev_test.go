package fakegithub

import "testing"

// TestCreateIssueHandler：POST /issues 分配递增且未占用的编号（已有最大编号 +1），返回 title/body/labels 并登记到 s.issues。
func TestCreateIssueHandler(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	const repo = demo
	// 先登记一个编号较大的 issue，创建时应从最大编号 +1 开始，而不是从 1 开始
	f.s.SetIssue(repo, Issue{Number: 5, Title: "旧"})
	first, err := f.inst.CreateIssue(ctx, repo, "标题", "正文", []string{"kiln:doc-update"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Number != 6 {
		t.Fatalf("编号 = %d", first.Number)
	}
	if first.Title != "标题" || first.Body != "正文" || len(first.LabelNames()) != 1 || first.LabelNames()[0] != "kiln:doc-update" {
		t.Fatalf("Issue = %+v", first)
	}
	got, ok := f.s.GetIssue(repo, 6)
	if !ok || got.Title != "标题" || got.Body != "正文" || len(got.Labels) != 1 || got.Labels[0] != "kiln:doc-update" {
		t.Fatalf("登记的 issue = %+v, %v", got, ok)
	}
	// 再建一个：编号继续递增
	second, err := f.inst.CreateIssue(ctx, repo, "标题2", "正文2", nil)
	if err != nil || second.Number != 7 {
		t.Fatalf("第二个 issue = %+v, %v", second, err)
	}
}
