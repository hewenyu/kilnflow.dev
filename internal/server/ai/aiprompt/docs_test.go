package aiprompt

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// TestDevelopAndReviewIncludeDocs：任务带冻结文档引用时，开发（领取后说明）与审计 prompt 都要给出文档入口。
func TestDevelopAndReviewIncludeDocs(t *testing.T) {
	spec := &aispec.DevelopSpec{
		Mode: aispec.DevImplement, Agent: "claude-code", WorkItemID: 1, Branch: "kiln/dev-1", BaseRef: "main",
		Docs: []aispec.DocRef{{ID: "d1", Title: "需求文档", Revision: 2}},
	}
	in := DevInput{Repository: "a/b", Spec: spec}
	got := Develop(in, 1, "kiln/dev-1", devreport.PhaseImplement)
	if !strings.Contains(got, "doc get") || !strings.Contains(got, "需求文档") {
		t.Fatalf("Develop 应包含文档入口：\n%s", got)
	}
	rev := Review(ReviewInput{Repository: "a/b", Spec: &aispec.ReviewSpec{Agent: "codex", PRNumber: 3, Docs: []aispec.DocRef{{ID: "d1", Title: "需求文档", Revision: 2}}}})
	if !strings.Contains(rev, "doc get") || !strings.Contains(rev, "需求文档") {
		t.Fatalf("Review 应包含文档入口：\n%s", rev)
	}
}
