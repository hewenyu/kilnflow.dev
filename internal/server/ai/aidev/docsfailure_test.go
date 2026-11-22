package aidev

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// failingDocs 是按 HTTP 状态码失败的文档源（模拟提供方的读取错误）：status 非 0 时 GetDocument 返回
// docprovider.Error，0 时正常返回快照正文。
type failingDocs struct {
	docs   []docprovider.Document
	mu     sync.Mutex
	status int
	gets   int
}

func (*failingDocs) ListCollections(context.Context) ([]docprovider.Collection, error) {
	return nil, nil
}

func (f *failingDocs) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	return slices.Clone(f.docs), nil
}

func (f *failingDocs) GetDocument(_ context.Context, id string) (docprovider.Document, error) {
	f.mu.Lock()
	f.gets++
	status := f.status
	f.mu.Unlock()
	if status != 0 {
		return docprovider.Document{}, &docprovider.Error{Status: status}
	}
	for _, d := range f.docs {
		if d.ID == id {
			d.SHA256 = docprovider.Sum(d.Content)
			return d, nil
		}
	}
	return docprovider.Document{}, &docprovider.Error{Status: http.StatusNotFound}
}

// setStatus 切换提供方之后的读取结果（0 表示恢复正常）。
func (f *failingDocs) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// calls 返回 GetDocument 的调用次数：用来确认受阻后不再重复请求文档。
func (f *failingDocs) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

// bindFailingDocs 给仓库绑定一个文档集，冻结时的 Provider 由 fp 提供。
func bindFailingDocs(t *testing.T, h *harness, fp *failingDocs) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	h.svc.docs = docsync.New(docsync.Deps{
		DB: h.db, MasterKey: key,
		Provider: func(string, string) (docprovider.Provider, error) { return fp, nil },
	})
	if err := store.PutDocBinding(h.ctx, h.db, key, store.DocBinding{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example", CollectionID: "c1", CreatedBy: "admin",
	}, "tok"); err != nil {
		t.Fatal(err)
	}
}

// newFailingDocs 返回一份只有一个已发布文档的失败文档源。
func newFailingDocs(status int) *failingDocs {
	return &failingDocs{docs: []docprovider.Document{claimDoc("d1", "需求文档", 2, "# 需求\n正文")}, status: status}
}

// TestFixDocsPermanentFailureBlocks：返修规划时提供方明确拒绝读取（HTTP 403 / 404）是永久失败：工作项转受阻并记录文档错误，
// 人可以修正外部文档后 retry；受阻后不再重复请求文档。
func TestFixDocsPermanentFailureBlocks(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "加功能", "要做 a")
			w, pr := h.developed(1)
			fp := newFailingDocs(status)
			bindFailingDocs(t, h, fp)
			h.failingRun(pr, w.HeadSHA)
			h.mustProcess()
			w = h.wantState(w.ID, StateBlocked)
			if w.TaskID != nil || !strings.Contains(w.Reason, "无法拉取设计文档") || !strings.Contains(w.Reason, strconv.Itoa(status)) {
				t.Fatalf("HTTP %d 应转为受阻并记录文档错误：%s", status, jsonString(w))
			}
			gets := fp.calls()
			h.mustProcess()
			if again := h.workItem(w.ID); again.State != StateBlocked || again.TaskID != nil {
				t.Fatalf("受阻后再次处理 = %s", jsonString(again))
			}
			if fp.calls() != gets {
				t.Fatalf("受阻后仍重复请求文档：%d → %d", gets, fp.calls())
			}
		})
	}
}

// TestImplDocsPermanentFailureBlocks：实现规划时提供方明确拒绝读取（HTTP 403 / 404）同样转受阻并记录文档错误，不开始实现。
func TestImplDocsPermanentFailureBlocks(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "升级历史", "显示升级历史")
			w, res, designPR := h.designPROpen()
			w, _ = h.mergeDesign(w, res, designPR)
			fp := newFailingDocs(status)
			bindFailingDocs(t, h, fp)
			h.mustProcess()
			w = h.wantState(w.ID, StateBlocked)
			if w.TaskID != nil || w.RoundRan || !strings.Contains(w.Reason, "无法拉取设计文档") || !strings.Contains(w.Reason, strconv.Itoa(status)) {
				t.Fatalf("HTTP %d 应转为受阻并记录文档错误：%s", status, jsonString(w))
			}
			gets := fp.calls()
			h.mustProcess()
			if again := h.workItem(w.ID); again.State != StateBlocked || again.TaskID != nil {
				t.Fatalf("受阻后再次处理 = %s", jsonString(again))
			}
			if fp.calls() != gets {
				t.Fatalf("受阻后仍重复请求文档：%d → %d", gets, fp.calls())
			}
		})
	}
}

// TestFixDocsTemporaryFailureRetries：提供方 503 是临时失败：规划返回错误交给 outbox 重试，工作项保持 fixing（没有任务），
// 不被打成受阻；提供方恢复后正常规划返修任务。
func TestFixDocsTemporaryFailureRetries(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	fp := newFailingDocs(http.StatusServiceUnavailable)
	bindFailingDocs(t, h, fp)
	h.failingRun(pr, w.HeadSHA)

	err := h.process()
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(http.StatusServiceUnavailable)) {
		t.Fatalf("临时失败应返回错误交给 outbox 重试：%v", err)
	}
	if w = h.workItem(w.ID); w.State != StateFixing || w.TaskID != nil {
		t.Fatalf("临时失败不应受阻：%s", jsonString(w))
	}
	// 再重试一次仍是临时失败：仍保持 fixing、没有任务
	if err := h.process(); err == nil {
		t.Fatal("提供方仍故障时应继续返回错误")
	}
	h.wantState(w.ID, StateFixing)

	// 提供方恢复：正常规划返修任务
	fp.setStatus(0)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if w.TaskID == nil || !w.RoundRan {
		t.Fatalf("恢复后应规划返修任务：%s", jsonString(w))
	}
	if spec := h.specOf(*w.TaskID); spec.Mode != aispec.DevFix {
		t.Fatalf("规划的应是返修任务：%s", jsonString(spec))
	}
}
