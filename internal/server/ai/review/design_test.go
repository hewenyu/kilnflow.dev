package review

import (
	"strconv"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// 工作项 PR 的审计方式（PLAN #31.5、I12）：按被审 head 判断。设计 PR 的完整改动只有 Markdown 时按设计审计；人之后推送了
// 代码的 head 按实现审计并说明原因；实现 PR 带上已合并的设计；不是工作项的 PR 没有审计方式，摘要与之前相同。

// specOf 规划 ev 的审计，返回唯一任务的 spec。
func (h *harness) specOf(ev *Event) aispec.ReviewSpec {
	h.t.Helper()
	ts := h.tasks(h.planPR(ev))
	if len(ts) != 1 {
		h.t.Fatalf("任务 = %+v", ts)
	}
	var s aispec.ReviewSpec
	if err := ts[0].DecodeSpec(&s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestReviewPhase(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(reviewConfig)

	// 不是工作项的 PR
	head := h.commitOn(featureRef, map[string]string{"docs/a.md": "x\n"}, "文档")
	plain := h.specOf(h.prEvent("opened", head, base))
	if plain.Phase != "" || plain.PhaseNote != "" || plain.PolicyDigest != policyDigest("codex", plain.Instructions, "", plain.DocsManifestDigest) {
		t.Fatalf("不是工作项的 PR：%+v", plain)
	}

	// 设计 PR（还没有记下编号，按设计分支关联）：只有 Markdown
	id := h.devWorkItem(0, agentcfg.AgentCodex)
	h.exec(`UPDATE dev_work_items SET phase='design' WHERE id=$1`, id)
	designEv := func(head string) *Event {
		ev := h.prEvent("synchronize", head, base)
		ev.HeadRef = "kiln/design-" + strconv.FormatInt(id, 10)
		return ev
	}
	head = h.commitOn(featureRef, map[string]string{"docs/b.md": "y\n"}, "设计")
	design := h.specOf(designEv(head))
	if design.Phase != phaseDesign || design.PhaseNote != "" || design.PolicyDigest == plain.PolicyDigest || design.DesignPR != 0 || design.DesignSHA != "" {
		t.Fatalf("设计 PR：%+v", design)
	}
	// 人推送了代码：按实现审计，说明原因；摘要与设计审计不同（结论不互相沿用）
	head = h.commitOn(featureRef, map[string]string{"cmd/x.go": "package x\n"}, "代码")
	mixed := h.specOf(designEv(head))
	// 设计还没有合并：不带"已合并的设计"
	if mixed.Phase != phaseImplement || !strings.Contains(mixed.PhaseNote, "cmd/x.go") || strings.Contains(mixed.PhaseNote, ".md") ||
		mixed.PolicyDigest == design.PolicyDigest || mixed.DesignPR != 0 || mixed.DesignSHA != "" {
		t.Fatalf("改了代码的设计 PR：%+v", mixed)
	}

	// 设计合并后的实现 PR：带上已合并的设计
	h.exec(`UPDATE dev_work_items SET phase='implement', design_pr=3, design_sha='abc', pr_number=$2 WHERE id=$1`, id, aiPR)
	head = h.commitOn(featureRef, map[string]string{"cmd/y.go": "package x\n"}, "实现")
	impl := h.specOf(h.prEvent("synchronize", head, base))
	if impl.Phase != phaseImplement || impl.DesignPR != 3 || impl.DesignSHA != "abc" || impl.PhaseNote != "" {
		t.Fatalf("实现 PR：%+v", impl)
	}
}

// TestWorkItemReview：设计 PR 按编号（合并后 design_pr）或设计分支识别，与工作项当前的阶段无关；实现 PR 带上已合并的设计。
func TestWorkItemReview(t *testing.T) {
	h := newHarness(t)
	id := h.devWorkItem(0, agentcfg.AgentCodex)
	designBranch := "kiln/design-" + strconv.FormatInt(id, 10)
	cases := []struct {
		name    string
		set     string
		pr      int
		headRef string
		want    prWorkItem
	}{
		{"设计分支", `phase='design'`, aiPR, designBranch, prWorkItem{id: id, designPR: true}},
		{"合并后的设计 PR", `phase='implement', design_pr=7, design_sha='abc'`, aiPR, "feat", prWorkItem{id: id, designPR: true}},
		{"实现 PR", `phase='implement', design_pr=3, design_sha='abc', pr_number=7`, aiPR, "feat", prWorkItem{id: id, mergedPR: 3, mergedSHA: "abc"}},
		{"一个 PR 完成", `phase='implement', pr_number=7`, aiPR, "kiln/dev-" + strconv.FormatInt(id, 10), prWorkItem{id: id}},
		{"别的工作项的设计分支", `phase='design'`, aiPR, "kiln/design-" + strconv.FormatInt(id+1, 10), prWorkItem{}},
		{"按编号找到、分支是别的工作项的设计分支", `phase='implement', pr_number=7`, aiPR, "kiln/design-" + strconv.FormatInt(id+1, 10), prWorkItem{id: id}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.exec(`UPDATE dev_work_items SET phase='implement', design_pr=NULL, design_sha='', pr_number=NULL WHERE id=$1`, id)
			h.exec(`UPDATE dev_work_items SET `+c.set+` WHERE id=$1`, id)
			w, err := workItemReview(h.ctx, h.db, testRepoID, c.pr, c.headRef)
			if err != nil {
				t.Fatal(err)
			}
			if c.want.id == 0 {
				if w != nil {
					t.Fatalf("不是工作项的 PR：%+v", w)
				}
				return
			}
			if w == nil || w.id != c.want.id || w.designPR != c.want.designPR || w.mergedPR != c.want.mergedPR || w.mergedSHA != c.want.mergedSHA {
				t.Fatalf("工作项 = %+v，想要 %+v", w, c.want)
			}
		})
	}
}

// TestWorkItemReviewPrefersNumber：PR 编号是一个工作项的，来源分支是另一个还没记下编号的工作项的正式分支时，用按编号找到的
// 工作项。没记下编号的工作项 pr_number、design_pr 为 NULL，比较结果为 NULL，按 "比较结果 DESC" 排序时排在最前（2026-11-17
// 代码审计 D2）。
func TestWorkItemReviewPrefersNumber(t *testing.T) {
	h := newHarness(t)
	byNumber := h.devWorkItem(aiPR, agentcfg.AgentCodex)
	byBranch := h.devWorkItem(0, agentcfg.AgentClaudeCode)
	for _, ref := range []string{"kiln/dev-" + strconv.FormatInt(byBranch, 10), "kiln/design-" + strconv.FormatInt(byBranch, 10)} {
		w, err := workItemReview(h.ctx, h.db, testRepoID, aiPR, ref)
		if err != nil {
			t.Fatal(err)
		}
		if w == nil || w.id != byNumber {
			t.Fatalf("来源分支 %s：工作项 = %+v，想要按编号找到的 #%d（不是按分支找到的 #%d）", ref, w, byNumber, byBranch)
		}
	}
}

// TestReviewPhaseIncompleteFiles：设计 PR 的文件列表不完整（GitHub compare 截断）时，即使看得到的文件都是 Markdown，也按
// 实现审计并说明原因：列表外可能有代码改动。
func TestReviewPhaseIncompleteFiles(t *testing.T) {
	w := &prWorkItem{designPR: true}
	files := make([]string, 300)
	for i := range files {
		files[i] = "docs/" + strconv.Itoa(i) + ".md"
	}
	if phase, note := reviewPhase(w, files, true); phase != phaseDesign || note != "" {
		t.Fatalf("完整的 Markdown 列表：%s %q", phase, note)
	}
	if phase, note := reviewPhase(w, files, false); phase != phaseImplement || !strings.Contains(note, "无法确认只有 Markdown") {
		t.Fatalf("截断的文件列表：%s %q", phase, note)
	}
}
