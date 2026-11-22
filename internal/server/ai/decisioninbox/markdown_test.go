package decisioninbox

import (
	"strings"
	"testing"
)

// Markdown 生成与精确查证。

func TestInsertEntryAnchorTopAndPreservesCRLF(t *testing.T) {
	t.Parallel()
	doc := "标题\r\n\r\n## 待拍板\r\n\r\n旧条目\r\n"
	block := "#### 新条目\n\n`kiln-dec:42:wi7:t2:0`"
	out, multiple := InsertEntry(doc, block)
	if multiple {
		t.Error("只有一个锚点")
	}
	if !strings.Contains(out, "## 待拍板\r\n#### 新条目") {
		t.Errorf("新条目应紧贴在标题下方：\n%s", out)
	}
	if strings.Index(out, "#### 新条目") > strings.Index(out, "旧条目") {
		t.Error("最新条目应在旧条目之前")
	}
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Errorf("插入应保留 CRLF 换行风格：%q", out)
	}
}

func TestInsertEntryAppendsSection(t *testing.T) {
	t.Parallel()
	out, _ := InsertEntry("只有内容", "块")
	want := "只有内容\n\n## 待拍板\n\n块\n"
	if out != want {
		t.Errorf("InsertEntry = %q，期望 %q", out, want)
	}
}

func TestAnchorLinesTopLevelOnly(t *testing.T) {
	t.Parallel()
	positive := []string{
		"## 待拍板\n",
		"  ## 待拍板  \n",
		"##\t待拍板\n",
		"## 待拍板 ###\n",
	}
	for _, doc := range positive {
		if got := anchorLines(doc); len(got) != 1 {
			t.Errorf("anchorLines(%q) = %v，期望 1 个", doc, got)
		}
	}
	negative := map[string]string{
		"fenced 反引号": "```\n## 待拍板\n```\n",
		"fenced 波浪线": "~~~\n## 待拍板\n~~~\n",
		"缩进代码":       "    ## 待拍板\n",
		"引用块":        "> ## 待拍板\n",
		"列表续行":       "- 项目\n  ## 待拍板\n",
		"一级标题":       "# 待拍板\n",
		"三级标题":       "### 待拍板\n",
		"没有空格":       "##待拍板\n",
	}
	for name, doc := range negative {
		if got := anchorLines(doc); len(got) != 0 {
			t.Errorf("%s：anchorLines(%q) = %v，不应匹配", name, doc, got)
		}
	}
}

func TestInsertEntryMultipleAnchorsUsesFirst(t *testing.T) {
	t.Parallel()
	doc := "## 待拍板\n\n旧\n\n## 待拍板\n"
	lines := anchorLines(doc)
	if len(lines) != 2 {
		t.Fatalf("anchorLines = %v，期望 2 个", lines)
	}
	out, multiple := InsertEntry(doc, "新块")
	if !multiple {
		t.Error("应报告有多个锚点")
	}
	if !strings.Contains(out, "## 待拍板\n新块") {
		t.Errorf("应插在第一个锚点下方：\n%s", out)
	}
}

func TestHasMarkerExactAndSkipsCode(t *testing.T) {
	t.Parallel()
	m := Marker(42, 7, 2, 1)
	if !MarkerInDocument("前言\n`"+m+"`\n", m) {
		t.Error("应找到精确 marker")
	}
	// :1 不能命中 :10。
	if MarkerInDocument("`"+Marker(42, 7, 2, 10)+"`", m) {
		t.Error(":1 不应命中 :10")
	}
	// 围栏代码块里的 marker 不算。
	if MarkerInDocument("```\n`"+m+"`\n```\n", m) {
		t.Error("围栏代码块里的 marker 不应算作已写入")
	}
	// 转义的反引号不算行内代码。
	if MarkerInDocument("\\`"+m+"\\`", m) {
		t.Error("转义的反引号不应算作 marker")
	}
	// 双反引号包裹的也不是单反引号标记。
	if MarkerInDocument("``"+m+"``", m) {
		t.Error("双反引号不应命中单反引号标记")
	}
}

func TestUTF16Units(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int64{
		"":    0,
		"abc": 3,
		"待拍板": 3,
		"a😀":  3, // emoji 在 UTF-16 中占 2 个 code unit
		"😀😀":  4,
		"aé":  2,
	} {
		if got := UTF16Units(s); got != want {
			t.Errorf("UTF16Units(%q) = %d，期望 %d", s, got, want)
		}
	}
	if decisionDocWarnUnits != 250_000 || decisionDocMaxUnits != 1_536_000 {
		t.Errorf("篇幅阈值 = %d / %d", decisionDocWarnUnits, decisionDocMaxUnits)
	}
}

func TestRenderEntryEscapesUntrustedContent(t *testing.T) {
	t.Parallel()
	m := Marker(42, 7, 2, 0)
	p := Payload{
		SchemaVersion: 1, Repo: "alice/demo", WorkItemID: 7, TaskID: 2, Phase: "implement", ImplRound: 1,
		EntryKind: EntryDecision, Category: "confirmed_decision", Ref: "C20",
		Context: "# 伪造标题\n```\n`" + Marker(9, 9, 9, 9) + "`\n~~~\n",
		Choice:  "选择",
	}
	out := RenderEntry(m, p)
	if !strings.Contains(out, "`"+m+"`") {
		t.Error("应包含自己的精确 marker")
	}
	if strings.Contains(out, "`"+Marker(9, 9, 9, 9)+"`") {
		t.Error("不可信文本不应伪造出别的 marker")
	}
	if strings.Contains(out, "\n# 伪造标题") {
		t.Error("不可信文本不应产生顶层标题")
	}
	if !strings.Contains(out, "kiln:hold") {
		t.Error("confirmed_decision 应醒目标注 kiln:hold 门禁")
	}
	if !strings.Contains(out, "C20") {
		t.Error("应写明被改动的已确认决定编号")
	}
}

// 锚点在最后一行且没有末尾换行时，必须先补换行，不能把条目粘在标题后面。
func TestInsertEntryAnchorWithoutTrailingNewline(t *testing.T) {
	t.Parallel()
	doc := "## 待拍板"
	out, _ := InsertEntry(doc, "第一块")
	if !strings.HasPrefix(out, "## 待拍板\n第一块") {
		t.Fatalf("第一次插入 = %q", out)
	}
	if got := anchorLines(out); len(got) != 1 {
		t.Fatalf("插入后锚点 = %v，锚点被破坏", got)
	}
	out2, _ := InsertEntry(out, "第二块")
	if !strings.HasPrefix(out2, "## 待拍板\n第二块\n\n第一块") {
		t.Fatalf("第二次插入 = %q", out2)
	}

	// 只有标题、但文档其它地方有 CRLF 时，补的换行跟随文档风格。
	crlf := "前言\r\n\r\n## 待拍板"
	out3, _ := InsertEntry(crlf, "块\n行")
	if !strings.HasSuffix(out3, "## 待拍板\r\n块\r\n行\r\n") {
		t.Fatalf("CRLF 文档的插入 = %q", out3)
	}
}

// CommonMark：围栏的开启和关闭只允许 0–3 个前导空格；4 个空格是缩进代码，不能开启或关闭围栏。
func TestAnchorLinesFenceIndentRules(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		doc  string
		want int
	}{
		"缩进 4 空格的反引号不关闭围栏": {"```\n    ```\n## 待拍板\n", 0},
		"列表内的围栏里是假锚点":      {"- 项目\n  ```\n  ## 待拍板\n  ```\n", 0},
		"列表内的围栏正常关闭后仍可识别":  {"- 项目\n  ```\n  x\n  ```\n\n## 待拍板\n", 1},
		"缩进 3 空格的关闭围栏有效":   {"```\n   ```\n## 待拍板\n", 1},
	}
	for name, tc := range cases {
		if got := anchorLines(tc.doc); len(got) != tc.want {
			t.Errorf("%s：anchorLines(%q) = %v，期望 %d 个", name, tc.doc, got, tc.want)
		}
	}
}

// 缩进代码块（4 空格或 Tab）里的 marker 不算已送达。
func TestHasMarkerSkipsIndentedCode(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	inside := "## 待拍板\n\n    `" + marker + "`\n"
	if hasMarker(inside, marker) {
		t.Error("缩进代码块里的 marker 不应算送达")
	}
	fenceInList := "## 待拍板\n\n- 项目\n  ```\n  `" + marker + "`\n  ```\n"
	if hasMarker(fenceInList, marker) {
		t.Error("列表内围栏里的 marker 不应算送达")
	}
	quoted := "## 待拍板\n\n> `" + marker + "`\n"
	if hasMarker(quoted, marker) {
		t.Error("引用块里的 marker 不应算送达")
	}
	body := "## 待拍板\n\n`" + marker + "`\n"
	if !hasMarker(body, marker) {
		t.Error("正文里的 marker 应算送达")
	}
}

// 列表项首行的围栏（`- ``` `）：围栏里的 marker 不算送达，后面真实的 `## 待拍板` 也不能被当成围栏内容吞掉。
func TestScannerTracksListFenceOnMarkerLine(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	doc := "- ```\n  `" + marker + "`\n  ```\n\n## 待拍板\n"
	if hasMarker(doc, marker) {
		t.Error("列表项首行围栏里的 marker 不应算送达")
	}
	anchors := anchorLines(doc)
	if len(anchors) != 1 || anchors[0] != 4 {
		t.Fatalf("anchorLines = %v，期望只剩第 5 行（0 起算 4）的真实标题", anchors)
	}
	out, _ := InsertEntry(doc, "条目")
	if !strings.Contains(out, "## 待拍板\n条目") {
		t.Errorf("应插到真实标题下：\n%s", out)
	}
	if !strings.HasPrefix(out, "- ```\n  `"+marker+"`\n  ```\n") {
		t.Errorf("列表里的围栏应原样保留：\n%s", out)
	}
}

// 嵌套列表里的围栏：内容按内层列表的内容列缩进，marker 仍不算送达，容器结束后的标题仍能识别。
func TestScannerTracksNestedListFence(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	doc := "- 项目\n  - ```\n    `" + marker + "`\n    ```\n\n## 待拍板\n"
	if hasMarker(doc, marker) {
		t.Error("嵌套列表围栏里的 marker 不应算送达")
	}
	if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 5 {
		t.Fatalf("anchorLines = %v，期望只剩真实的标题", anchors)
	}
}

// 容器按缩进回退 / 引用结束正确退出：围栏结束后回到顶层，顶层 marker 与标题按顶层规则处理。
func TestScannerExitsContainers(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	// 围栏结束、列表容器也因缩进回退结束之后的顶层 marker 才算送达。
	after := "- ```\n  围栏\n  ```\n\n`" + marker + "`\n"
	if !hasMarker(after, marker) {
		t.Error("围栏结束后顶层的 marker 应算送达")
	}
	if hasMarker("- ```\n  `"+marker+"`\n  ```\n", marker) {
		t.Error("围栏里的 marker 不应算送达")
	}
	// 引用容器结束（空行之后不再带 >）后，顶层标题重新可见。
	quoted := "> ```\n> 围栏\n> ```\n\n## 待拍板\n"
	if anchors := anchorLines(quoted); len(anchors) != 1 || anchors[0] != 4 {
		t.Errorf("引用结束后的顶层标题应被识别：%v", anchors)
	}
	// 列表项里的假标题仍不算顶层标题。
	if anchors := anchorLines("- 项目\n  ## 待拍板\n"); len(anchors) != 0 {
		t.Errorf("列表项里的标题不应算顶层标题：%v", anchors)
	}
}

// Tab 缩进：列宽不是字节下标，`-\tx` 这类合法输入不能让扫描器越界 panic。
func TestScannerTabIndentIsByteSafe(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	// 只要求不 panic、不越界，且顶层标题仍能被找到。
	for _, doc := range []string{
		"-\tx\n",
		"*\t\tx\n",
		"1.\tx\n",
		"1)\t\tx\n",
		">\t-\tx\n",
		"> \t## 待拍板\n",
		">\t`" + marker + "`\n",
		"-\t中\t`" + marker + "`\n",
		"-\t\t\n",
		"\t\t\n",
		"-",
		"1.",
	} {
		_ = hasMarker(doc, marker)
		_ = anchorLines(doc)
		if out, _ := InsertEntry(doc, "块"); out == "" {
			t.Errorf("InsertEntry(%q) 返回空串", doc)
		}
	}
	// 有序列表首行的围栏（标记后是 Tab）：内容按 Tab 展开后的内容列判断。
	doc := "1.\t```\n\t`" + marker + "`\n\t```\n## 待拍板\n"
	if hasMarker(doc, marker) {
		t.Error("Tab 缩进的列表围栏里的 marker 不应算送达")
	}
	if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 3 {
		t.Fatalf("anchorLines = %v，期望只找到第 4 行的标题", anchors)
	}
}

// 围栏里的空行（无空格 / 只有空格）不关闭围栏，也不触发列表容器回退。
func TestScannerBlankLinesInsideFence(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	for name, blank := range map[string]string{"无空格空行": "", "仅空格空行": "   ", "仅 Tab 空行": "\t"} {
		doc := "- ```\n" + blank + "\n  `" + marker + "`\n  ```\n\n## 待拍板\n"
		if hasMarker(doc, marker) {
			t.Errorf("%s：列表围栏里的 marker 不应算送达", name)
		}
		if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 5 {
			t.Errorf("%s：anchorLines = %v，期望只找到真实的标题", name, anchors)
		}
	}
	// 嵌套列表里的围栏同样可以带空行。
	nested := "- 项目\n  - ```\n\n    `" + marker + "`\n    ```\n\n## 待拍板\n"
	if hasMarker(nested, marker) {
		t.Error("嵌套列表围栏（带空行）里的 marker 不应算送达")
	}
	if anchors := anchorLines(nested); len(anchors) != 1 || anchors[0] != 6 {
		t.Fatalf("anchorLines = %v，期望只找到真实的标题", anchors)
	}
}

// 围栏内部的内容以 > 开头时不能被当成新的引用容器，否则下一行会把围栏状态清掉。
func TestScannerQuoteInsideFenceIsContent(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	top := "```\n> 示例\n> > 嵌套\n`" + marker + "`\n```\n\n## 待拍板\n"
	if hasMarker(top, marker) {
		t.Error("顶层围栏里的 marker 不应算送达")
	}
	if anchors := anchorLines(top); len(anchors) != 1 || anchors[0] != 6 {
		t.Fatalf("anchorLines = %v，期望只找到围栏外的标题", anchors)
	}
	// 围栏里的假标题也不能被当成锚点。
	fake := "```\n> 示例\n## 待拍板\n```\n\n## 待拍板\n"
	if anchors := anchorLines(fake); len(anchors) != 1 || anchors[0] != 5 {
		t.Fatalf("anchorLines = %v，期望只找到围栏外的标题", anchors)
	}
	// 引用里打开的围栏：> 前缀按围栏所属的层数剥离，内部再多的 > 都是代码。
	quoted := "> ```\n> > 嵌套\n> `" + marker + "`\n> ```\n\n## 待拍板\n"
	if hasMarker(quoted, marker) {
		t.Error("引用围栏里的 marker 不应算送达")
	}
	if anchors := anchorLines(quoted); len(anchors) != 1 || anchors[0] != 5 {
		t.Fatalf("anchorLines = %v，期望只找到引用外的标题", anchors)
	}
	// 外层引用中断（空行后不再带 >）：围栏随之自动闭合，后面的标题重新可见。
	broken := "> ```\n> 内部\n\n## 待拍板\n"
	if anchors := anchorLines(broken); len(anchors) != 1 || anchors[0] != 3 {
		t.Fatalf("anchorLines = %v，期望引用中断后能找到标题", anchors)
	}
	// 插入位置必须在围栏外。
	out, _ := InsertEntry(top, "条目")
	if !strings.Contains(out, "```\n\n## 待拍板\n条目") {
		t.Errorf("条目应插到围栏外的标题下：\n%s", out)
	}
}

// 行内代码片段可以跨行（CommonMark #6.1）：双 / 三反引号代码片段里出现的单反引号 marker 不算送达。
// 代码片段内部不做反斜杠转义，`\“ 不会让闭合反引号失效；未闭合的双反引号也不会吞掉后面的真实 marker。
func TestHasMarkerSkipsMultilineCodeSpans(t *testing.T) {
	t.Parallel()
	m := Marker(42, 7, 2, 0)
	negative := map[string]string{
		"双反引号跨行":    "``示例\n`" + m + "`\n``\n",
		"三反引号跨行":    "文本 ```示例\n`" + m + "`\n结束 ``` 文本\n",
		"代码片段内含反斜杠": "``a\\`\n`" + m + "`\n``\n",
		"列表项内的跨行片段": "- ``示例\n  `" + m + "`\n  ``\n",
	}
	for name, doc := range negative {
		if hasMarker(doc, m) {
			t.Errorf("%s：代码片段里的 marker 不应算送达：\n%s", name, doc)
		}
	}
	// 未闭合的双反引号只是普通文本：同一段里后面真实的单反引号 marker 仍然算送达。
	if !hasMarker("``示例\n`"+m+"`\n", m) {
		t.Error("未闭合的双反引号不应吞掉同段后面的真实 marker")
	}
	// 空行结束段落：后面段落里的真实 marker 照样算送达。
	if !hasMarker("``示例\n\n`"+m+"`\n", m) {
		t.Error("未闭合的双反引号不应吞掉后续段落的真实 marker")
	}
	// 代码片段跨行时，段落里的其它单反引号内容不会被误判。
	if hasMarker("``a`b\n`"+m+"`\n``\n", m) {
		t.Error("双反引号片段内部的单反引号不应被当成 marker")
	}
}

// 同一行的嵌套列表 / 引用容器围栏（`- - ``` `、`- > ``` `、`> - - ``` ` 等）里的 marker 不算送达，
// 容器结束后的真实顶层标题与真实 marker 仍能识别。
func TestScannerNestedContainersOnOneLine(t *testing.T) {
	t.Parallel()
	inner, top := Marker(42, 7, 2, 0), Marker(42, 7, 2, 1)
	cases := []struct {
		name string
		open string
		body string
		// anchor 是真实顶层标题的行号（0 起算）。
		anchor int
	}{
		{"两层列表", "- - ```", "    `" + inner + "`\n    ```\n", 5},
		{"三层列表", "- - - ```", "      `" + inner + "`\n      ```\n", 5},
		{"引用里的两层列表", "> - - ```", ">     `" + inner + "`\n>     ```\n", 5},
		{"有序列表里的列表", "1. - ```", "     `" + inner + "`\n     ```\n", 5},
		{"列表里的引用", "- > ```", "  > `" + inner + "`\n  > ```\n", 5},
	}
	for _, tc := range cases {
		doc := tc.open + "\n" + tc.body + "`" + top + "`\n\n## 待拍板\n"
		if hasMarker(doc, inner) {
			t.Errorf("%s：围栏里的 marker 不应算送达：\n%s", tc.name, doc)
		}
		if !hasMarker(doc, top) {
			t.Errorf("%s：容器结束后的真实 marker 应算送达：\n%s", tc.name, doc)
		}
		if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != tc.anchor {
			t.Errorf("%s：anchorLines = %v，期望只剩第 %d 行的真实标题", tc.name, anchors, tc.anchor)
		}
	}
	// 列表里的引用行：引用里的 marker 是引用内容，不是条目；后面的顶层 marker 仍算送达。
	doc := "- > `" + inner + "`\n\n`" + top + "`\n\n## 待拍板\n"
	if hasMarker(doc, inner) {
		t.Errorf("列表里的引用内容不应算送达：\n%s", doc)
	}
	if !hasMarker(doc, top) {
		t.Errorf("引用结束后的真实 marker 应算送达：\n%s", doc)
	}
	if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 4 {
		t.Errorf("anchorLines = %v，期望只剩真实的顶层标题", anchors)
	}
}

// 列表容器匹配失败必须回滚游标：`100. item`（内容列 5）之后只有 4 个空格的行是列表外的缩进代码，
// 不能被当成正文（否则示例 marker 会被误判为已送达，`## 待拍板` 也会被误判为顶层锚点）。
func TestScannerRollsBackFailedListMatch(t *testing.T) {
	t.Parallel()
	m := Marker(42, 7, 2, 0)
	for name, doc := range map[string]string{
		"空格缩进":   "100. item\n\n    `" + m + "`\n",
		"Tab 缩进": "100. item\n\n\t`" + m + "`\n",
	} {
		if hasMarker(doc, m) {
			t.Errorf("%s：列表外的缩进代码里的 marker 不应算送达：\n%s", name, doc)
		}
	}
	if anchors := anchorLines("100. item\n\n    ## 待拍板\n"); len(anchors) != 0 {
		t.Errorf("列表外的缩进代码里的标题不应算顶层锚点：%v", anchors)
	}
	// 缩进达到内容列（5 列）的行仍在列表项里，是正文。
	if !hasMarker("100. item\n\n     `"+m+"`\n", m) {
		t.Error("缩进达到列表内容列的正文 marker 应算送达")
	}
	// 退回外层列表后只剩外层内容列（2 列）的缩进：相对外层还有 4 列，是缩进代码。
	nested := "- outer\n  100. inner\n\n      `" + m + "`\n"
	if hasMarker(nested, m) {
		t.Error("退回外层列表后的缩进代码 marker 不应算送达")
	}
	if anchors := anchorLines("- outer\n  100. inner\n\n      ## 待拍板\n"); len(anchors) != 0 {
		t.Errorf("退回外层列表后的缩进代码标题不应算顶层锚点：%v", anchors)
	}
}

// Setext 标题下划线（`=` / `===` / `--` / `---`）结束当前段落：标题里未闭合的反引号不能跨块吞掉后面的真实 marker。
func TestHasMarkerSetextHeadingBoundary(t *testing.T) {
	t.Parallel()
	m := Marker(42, 7, 2, 0)
	for name, doc := range map[string]string{
		"一级标题 =":   "`a\n=\n`" + m + "`\n`\n",
		"一级标题 ===": "`a\n===\n`" + m + "`\n`\n",
		"二级标题 --":  "`a\n--\n`" + m + "`\n`\n",
		"二级标题 ---": "`a\n---\n`" + m + "`\n`\n",
	} {
		if !hasMarker(doc, m) {
			t.Errorf("%s：Setext 标题后的真实 marker 应算送达：\n%s", name, doc)
		}
	}
	// 列表项里的 Setext 标题同样结束段落。
	if !hasMarker("- `a\n  ===\n  `"+m+"`\n  `\n", m) {
		t.Error("列表项里的 Setext 标题应结束段落，后面的真实 marker 应算送达")
	}
	// 没有打开的段落时 `---` 是主题分隔线，同样不会把前后两段拼在一起。
	if !hasMarker("`a\n\n---\n\n`"+m+"`\n`\n", m) {
		t.Error("主题分隔线后的真实 marker 应算送达")
	}
	// `- - -` 是主题分隔线（不是 Setext 下划线），也不会开启列表容器。
	if hasMarker("## 待拍板\n\n- - -\n    `"+m+"`\n", m) {
		t.Error("主题分隔线之后的缩进代码 marker 不应算送达")
	}
	if anchors := anchorLines("## 待拍板\n\n- - -\n    ## 待拍板\n"); len(anchors) != 1 {
		t.Errorf("主题分隔线之后的缩进标题不应算顶层锚点：%v", anchors)
	}
	if hasMarker("## 待拍板\n\n* * *\n    `"+m+"`\n", m) {
		t.Error("`* * *` 之后的缩进代码 marker 不应算送达")
	}
	// `- - x` 不是主题分隔线，仍是两层列表。
	if !hasMarker("- - `"+m+"`\n", m) {
		t.Error("同一行的两层列表正文 marker 应算送达")
	}
}

// 引用标记后的 Tab 按 CommonMark #2.2 只消费 1 列空白，展开后的其余列作为内容缩进。
func TestQuoteTabConsumesOneColumn(t *testing.T) {
	t.Parallel()
	var sc blockScanner
	if st := sc.step(">\tx"); st.InCode || !st.HasQuote {
		t.Errorf(">\\tx 应是引用里的正文：%+v", st)
	}
	// `>` + Tab 剩余的 2 列加上 2 个空格 = 4 列，是引用里的缩进代码。
	var code blockScanner
	if st := code.step(">\t  x"); !st.InCode {
		t.Errorf(">\\t  + 两空格应构成引用里的缩进代码：%+v", st)
	}
	// 只有 `>` 和 Tab 的空行仍是引用里的空行。
	var blank blockScanner
	if st := blank.step(">\t"); !st.Blank || !st.HasQuote {
		t.Errorf(">\\t 应是引用里的空行：%+v", st)
	}
	// 只有 Tab 的引用空行同样不 panic。
	_ = hasMarker(">\t\n", Marker(42, 7, 2, 0))
	_ = anchorLines(">\t    ## 待拍板\n")
}

// 扫描器对任意输入都不能 panic（列宽 / 字节下标混淆是最容易越界的路径）。
func FuzzMarkdownScan(f *testing.F) {
	for _, seed := range []string{
		"```\n> 示例\n```\n",
		"- ```\n\n  `kiln-dec:42:wi7:t2:0`\n  ```\n## 待拍板\n",
		"-\tx\n",
		"*\t\tx\n",
		"1.\tx\n",
		">\t-\tx\n",
		"> \t## 待拍板\n",
		"1.\t```\n\tx\n\t```\n## 待拍板\n",
		"> ```\n> > 嵌套\n\n## 待拍板\n",
		// 跨行行内代码片段与同一行的多层容器（审计）。
		"``示例\n`kiln-dec:42:wi7:t2:0`\n``\n",
		"- - ```\n    x\n    ```\n## 待拍板\n",
		"- > ```\n  > x\n  > ```\n## 待拍板\n",
		"> - - ```\n>     x\n>     ```\n",
		"1) - > ```\n       x\n       ```\n",
		// 列表匹配失败回滚、Setext 下划线与主题分隔线（审阅）。
		"100. item\n\n    `kiln-dec:42:wi7:t2:0`\n",
		"100. item\n\n\t`kiln-dec:42:wi7:t2:0`\n",
		"100. item\n\n    ## 待拍板\n",
		"- outer\n  100. inner\n\n      `kiln-dec:42:wi7:t2:0`\n",
		"`a\n=\n`kiln-dec:42:wi7:t2:0`\n`\n",
		"`a\n---\n`kiln-dec:42:wi7:t2:0`\n`\n",
		"- `a\n  ===\n  `kiln-dec:42:wi7:t2:0`\n  `\n",
		"## 待拍板\n\n- - -\n    `kiln-dec:42:wi7:t2:0`\n",
		">\tx\n",
		">\t  x\n",
		">\t\n",
		">\t    ## 待拍板\n",
	} {
		f.Add(seed, "kiln-dec:42:wi7:t2:0")
	}
	f.Fuzz(func(t *testing.T, doc, marker string) {
		_ = hasMarker(doc, marker)
		_ = anchorLines(doc)
		if out, _ := InsertEntry(doc, marker); out == "" {
			t.Fatalf("InsertEntry(%q) 返回空串", doc)
		}
	})
}

// 缩进代码块优先于列表标记：4 空格 / Tab 开头的行是代码；列表标记后 5 个以上空白是列表内的缩进代码。
func TestScannerIndentedCodeBeatsListMarker(t *testing.T) {
	t.Parallel()
	marker := Marker(42, 7, 2, 0)
	docs := map[string]string{
		"4 空格 + 列表标记":      "    - `" + marker + "`\n\n## 待拍板\n",
		"Tab + 列表标记":       "\t- `" + marker + "`\n\n## 待拍板\n",
		"列表标记后 5 个空格":      "-     `" + marker + "`\n\n## 待拍板\n",
		"列表标记后 5 个空格 + 围栏": "-     ```\n      `" + marker + "`\n      ```\n\n## 待拍板\n",
	}
	for name, doc := range docs {
		if hasMarker(doc, marker) {
			t.Errorf("%s：缩进代码里的 marker 不应算送达", name)
		}
		if anchors := anchorLines(doc); len(anchors) != 1 {
			t.Errorf("%s：anchorLines = %v，期望找到唯一的顶层标题", name, anchors)
		}
	}
	// 标记后的空白在 1–4 列时仍算列表项正文（不是代码）。
	inline := "-   `" + marker + "`\n"
	if !hasMarker(inline, marker) {
		t.Error("列表项正文里的 marker 应算送达")
	}
}

// 锚点判定只读 blockScanner 的状态：列表首行开围栏后，围栏关闭时仍要记得还在列表项里。
func TestAnchorLinesTracksContainersFromScanner(t *testing.T) {
	t.Parallel()
	doc := "- ```\n  x\n  ```\n  ## 待拍板\n\n## 待拍板\n"
	if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 5 {
		t.Fatalf("anchorLines = %v，期望只识别第 6 行（0 起算 5）的顶层锚点", anchors)
	}
	out, _ := InsertEntry(doc, "条目")
	want := "- ```\n  x\n  ```\n  ## 待拍板\n\n## 待拍板\n条目\n"
	if out != want {
		t.Errorf("插入结果 = %q，期望 %q", out, want)
	}
	// 嵌套容器退回后，顶层锚点重新可见。
	nested := "- 项目\n  - ```\n    x\n    ```\n    ## 待拍板\n\n## 待拍板\n"
	if anchors := anchorLines(nested); len(anchors) != 1 || anchors[0] != 6 {
		t.Fatalf("anchorLines = %v，期望只识别第 7 行（0 起算 6）的顶层锚点", anchors)
	}
}

// 列表容器是栈：子列表里的围栏结束后要退回父列表，而不是一路清空到顶层。
func TestScannerReturnsToParentList(t *testing.T) {
	t.Parallel()
	doc := "- 项目\n  - ```\n    x\n    ```\n  ## 待拍板\n\n## 待拍板\n"
	// 子列表（内容列 4）的围栏结束后，父列表（内容列 2）仍在，父列表里的 `  ## 待拍板` 是假标题。
	if anchors := anchorLines(doc); len(anchors) != 1 || anchors[0] != 6 {
		t.Fatalf("anchorLines = %v，期望只识别第 7 行（0 起算 6）的顶层锚点", anchors)
	}
	out, _ := InsertEntry(doc, "条目")
	want := "- 项目\n  - ```\n    x\n    ```\n  ## 待拍板\n\n## 待拍板\n条目\n"
	if out != want {
		t.Errorf("插入结果 = %q，期望 %q", out, want)
	}

	// 退回父列表后，围栏里的 marker 仍是代码，父列表正文里的 marker 仍算送达。
	inner, outer := Marker(42, 7, 2, 1), Marker(42, 7, 2, 0)
	withMarkers := "- 项目\n  - ```\n    `" + inner + "`\n    ```\n  `" + outer + "`\n  ## 待拍板\n\n## 待拍板\n"
	if hasMarker(withMarkers, inner) {
		t.Error("子列表围栏里的 marker 不应算送达")
	}
	if !hasMarker(withMarkers, outer) {
		t.Error("退回父列表后的正文 marker 应算送达")
	}
	if anchors := anchorLines(withMarkers); len(anchors) != 1 || anchors[0] != 7 {
		t.Fatalf("anchorLines = %v，期望只识别顶层锚点", anchors)
	}
}

// 空列表项（裸标记或标记后只有空白）：内容列固定为标记后一列，尾随空白不算 padding。
func TestScannerEmptyListItem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		marker  string
		anchors []int // 全文的顶层锚点行号
		insert  string
	}{
		{
			name:    "裸连字符",
			marker:  "-",
			anchors: []int{3},
			insert:  "-\n  ## 待拍板\n\n## 待拍板\n条目\n",
		},
		{
			name:    "裸有序标记",
			marker:  "1.", // 内容列是 3，两空格的行已经在列表外
			anchors: []int{1, 3},
			insert:  "1.\n  ## 待拍板\n条目\n\n## 待拍板\n",
		},
		{
			name:    "标记后三个空格",
			marker:  "-   ",
			anchors: []int{3},
			insert:  "-   \n  ## 待拍板\n\n## 待拍板\n条目\n",
		},
		{
			name:    "标记后一个 Tab",
			marker:  "-\t",
			anchors: []int{3},
			insert:  "-\t\n  ## 待拍板\n\n## 待拍板\n条目\n",
		},
	}
	for _, c := range cases {
		doc := c.marker + "\n  ## 待拍板\n\n## 待拍板\n"
		anchors := anchorLines(doc)
		if len(anchors) != len(c.anchors) {
			t.Fatalf("%s：anchorLines = %v，期望 %v", c.name, anchors, c.anchors)
		}
		for i, want := range c.anchors {
			if anchors[i] != want {
				t.Fatalf("%s：anchorLines = %v，期望 %v", c.name, anchors, c.anchors)
			}
		}
		if got, _ := InsertEntry(doc, "条目"); got != c.insert {
			t.Errorf("%s：插入结果 = %q，期望 %q", c.name, got, c.insert)
		}
		// 空列表项里的缩进正文不是缩进代码（内容列是标记后一列，不是尾随空白之后的列）。
		m := Marker(42, 7, 2, 0)
		if !hasMarker(c.marker+"\n  `"+m+"`\n", m) {
			t.Errorf("%s：空列表项里的 marker 应算送达", c.name)
		}
	}
}
