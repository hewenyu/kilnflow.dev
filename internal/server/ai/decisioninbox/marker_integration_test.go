package decisioninbox

import (
	"strings"
	"testing"
)

// 只读查证与投递必须在同一份扫描规则下判断"文档里是否真的有条目"：跨行行内代码片段、同一行嵌套容器里的示例
// marker 都不能造成虚假送达（审计）。两条入口分别覆盖：ReconcileSync 不能把 unknown 误判为 delivered，
// ProcessRepo 不能跳过本该发生的唯一一次写入。

func TestDecisionDocCodeSamplesNotDelivered(t *testing.T) {
	t.Parallel()
	sample := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	cases := map[string]string{
		// 目标文档里只有一段跨行双反引号代码片段，片段里含完整的单反引号 marker。
		"跨行双反引号": "## 待拍板\n\n``示例\n`" + sample + "`\n``\n",
		// 目标文档里只有同一行嵌套列表围栏，围栏里含完整的 marker。
		"同行嵌套列表围栏": "## 待拍板\n\n- - ```\n    `" + sample + "`\n    ```\n",
		// 有序列表内容列是 5，之后只有 4 个空格的行是列表外的缩进代码；容器匹配失败必须回滚游标。
		"列表外缩进代码": "## 待拍板\n\n100. item\n\n    `" + sample + "`\n",
		// 同上，Tab 展开后同样是 4 列。
		"列表外缩进代码 Tab": "## 待拍板\n\n100. item\n\n\t`" + sample + "`\n",
		// 子列表结束后退回外层列表，剩余缩进相对外层内容列仍是缩进代码。
		"退回外层列表后的缩进代码": "## 待拍板\n\n- outer\n  100. inner\n\n      `" + sample + "`\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			t.Run("只读查证", func(t *testing.T) {
				t.Parallel()
				db := newTestDB(t)
				insertFixture(t, db)
				cfg := putConfig(t, db, testDocID)
				id := insertSync(t, db, syncSeed{State: "unknown", TargetGen: cfg.Generation})
				doc := docWith(testDocID)
				doc.Content, doc.Revision = content, 9
				svc := newService(t, db, &fakeWriter{doc: doc})

				res, err := svc.ReconcileSync(t.Context(), id, "admin")
				if err != nil {
					t.Fatalf("ReconcileSync：%v", err)
				}
				if res.Found {
					t.Error("代码示例里的 marker 不应被当成已送达")
				}
				if v := mustSync(t, db, id); v.State != "unknown" {
					t.Errorf("状态不应改变：%+v", v)
				}
			})

			t.Run("投递", func(t *testing.T) {
				t.Parallel()
				db := newTestDB(t)
				insertFixture(t, db)
				cfg := putConfig(t, db, testDocID)
				id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
				doc := docWith(testDocID)
				doc.Content, doc.Revision = content, 9
				w := &fakeWriter{doc: doc}
				svc := newService(t, db, w)

				if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
					t.Fatalf("ProcessRepo：%v", err)
				}
				if _, sets := w.calls(); sets != 1 {
					t.Fatalf("UpdateDocument 调用 %d 次，期望 1 次（不能把代码示例当成已送达）", sets)
				}
				v := mustSync(t, db, id)
				if v.State != "delivered" || v.Attempts != 1 {
					t.Errorf("状态 = %+v，期望 delivered 且 attempts=1", v)
				}
				w.mu.Lock()
				got := w.doc.Content
				w.mu.Unlock()
				if !strings.Contains(got, "`"+v.Marker+"`") {
					t.Errorf("写入的正文应包含条目 marker：\n%s", got)
				}
				// 原有的代码示例必须原样保留。
				want := strings.TrimPrefix(content, "## 待拍板\n\n")
				if !strings.Contains(got, want) {
					t.Errorf("代码示例应原样保留：\n%s", got)
				}
			})
		})
	}
}
