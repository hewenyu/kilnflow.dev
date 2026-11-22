package aidev

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// claimFakeDocs 是首次领取冻结测试用的进程内文档源。
type claimFakeDocs struct {
	docs        []docprovider.Document
	delay       time.Duration // GetDocument 的固定耗时（模拟慢文档集）
	mu          sync.Mutex
	cur         int
	maxInFlight int
}

func (*claimFakeDocs) ListCollections(context.Context) ([]docprovider.Collection, error) {
	return nil, nil
}

// claimDoc 返回一篇处于绑定集合 c1、已发布的文档：测试数据显式给出完整状态，假文档源原样返回。
func claimDoc(id, title string, revision int64, content string) docprovider.Document {
	return docprovider.Document{
		ID: id, Title: title, Revision: revision, Content: content,
		CollectionID: "c1", Published: true,
	}
}

func (f *claimFakeDocs) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	return slices.Clone(f.docs), nil
}

func (f *claimFakeDocs) GetDocument(_ context.Context, id string) (docprovider.Document, error) {
	f.mu.Lock()
	f.cur++
	if f.cur > f.maxInFlight {
		f.maxInFlight = f.cur
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.cur--
		f.mu.Unlock()
	}()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	for _, d := range f.docs {
		if d.ID == id {
			d.SHA256 = docprovider.Sum(d.Content)
			return d, nil
		}
	}
	return docprovider.Document{}, nil
}

// TestClaimFreezesBoundDocs：仓库绑定文档集后，首次选题领取（同一 attempt 直接开发）也要冻结文档，并在领取后的说明里
// 给出文档入口。这覆盖 planImpl / planFix 之外的首次开发 / 设计入口。
func TestClaimFreezesBoundDocs(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "需求", "正文")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	fp := &claimFakeDocs{docs: []docprovider.Document{claimDoc("d1", "需求文档", 2, "# 需求\n正文")}}
	h.svc.docs = docsync.New(docsync.Deps{
		DB: h.db, MasterKey: key,
		Provider: func(string, string) (docprovider.Provider, error) { return fp, nil },
	})
	if err := store.PutDocBinding(h.ctx, h.db, key, store.DocBinding{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example",
		CollectionID: "c1", CollectionName: "设计", CreatedBy: "admin",
	}, "tok"); err != nil {
		t.Fatal(err)
	}

	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	h.offer(aid)
	res, err := h.svc.Claim(h.ctx, h.taskOfAttempt(aid), aid, &airunnerv2.ClaimDevIssuesRequest{
		Issues: []int32{1}, Selection: selection([]int{1}, nil, nil),
	})
	if err != nil || !res.GetClaimed() {
		t.Fatalf("Claim = %+v, %v", res, err)
	}
	if n := h.queryInt(`SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, res.GetWorkItemId()); n != 1 {
		t.Fatalf("首次领取应冻结 1 篇文档，实际 %d", n)
	}
	if !strings.Contains(res.GetMessage(), "doc get") || !strings.Contains(res.GetMessage(), "需求文档") {
		t.Fatalf("领取后的说明应给出文档入口：%q", res.GetMessage())
	}
	// 冻结的是 provider 的正文
	var content string
	if err := h.db.QueryRow(h.ctx, `SELECT content FROM work_item_doc_snapshots WHERE work_item_id=$1 AND doc_id='d1'`, res.GetWorkItemId()).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "# 需求\n正文" {
		t.Fatalf("快照正文 = %q", content)
	}
}

// TestClaimFreezesSlowDocsWithinRPCDeadline：整批文档正文的总读取时间超过逐篇串行相加时，首次领取仍要在一次普通 RPC 期限
// 内成功（有界并发 + 冻结在独立预算下完成）；同时快照要完整冻结。对应 AI 审计 finding：首次领取的整批读取被 60s RPC 期限
// 反复中断。
func TestClaimFreezesSlowDocsWithinRPCDeadline(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "需求", "正文")
	key := make([]byte, 32)
	fp := &claimFakeDocs{delay: 200 * time.Millisecond}
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		fp.docs = append(fp.docs, claimDoc(id, id, 1, id))
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
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	h.offer(aid)
	ctx, cancel := context.WithTimeout(h.ctx, 500*time.Millisecond)
	defer cancel()
	res, err := h.svc.Claim(ctx, h.taskOfAttempt(aid), aid, &airunnerv2.ClaimDevIssuesRequest{
		Issues: []int32{1}, Selection: selection([]int{1}, nil, nil),
	})
	if err != nil || !res.GetClaimed() {
		t.Fatalf("Claim = %+v, %v", res, err)
	}
	if n := h.queryInt(`SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, res.GetWorkItemId()); n != 4 {
		t.Fatalf("应冻结 4 篇，实际 %d", n)
	}
	if fp.maxInFlight < 2 {
		t.Fatalf("整批正文应并发读取，最大并发只有 %d", fp.maxInFlight)
	}
}

// TestClaimDocsByProtocol：首次领取是否冻结并注入设计文档只看节点是否支持 kiln-orca task doc（协议 TaskDocMinProtocol
// 起），不跟当前的 AIProtocol 漂移：Server 升级到协议 10 后，协议 9 的在途选题任务仍要能冻结并读取文档；协议 8 的节点
// 没有这个命令，不冻结、不注入，消息里没有 doc get。
func TestClaimDocsByProtocol(t *testing.T) {
	// 边界是独立于被测常量的契约：协议 9 起支持文档工具（TaskDocMinProtocol 必须正好是 9）
	if agentcfg.TaskDocMinProtocol != 9 {
		t.Fatalf("TaskDocMinProtocol = %d，应为 9", agentcfg.TaskDocMinProtocol)
	}
	// 独立硬编码的期望：覆盖被淘汰的协议、边界协议与当前的最高协议
	cases := []struct {
		protocol int
		wantDocs bool
	}{
		{protocol: 8, wantDocs: false},
		{protocol: 9, wantDocs: true},
		{protocol: 10, wantDocs: true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("protocol-%d", c.protocol), func(t *testing.T) {
			wantDocs := c.wantDocs
			h := newHarness(t, devConfigYML)
			h.issue(1, "需求", "正文")
			key := make([]byte, 32)
			fp := &claimFakeDocs{docs: []docprovider.Document{claimDoc("d1", "需求文档", 1, "# 需求")}}
			h.svc.docs = docsync.New(docsync.Deps{
				DB: h.db, MasterKey: key,
				Provider: func(string, string) (docprovider.Provider, error) { return fp, nil },
			})
			if err := store.PutDocBinding(h.ctx, h.db, key, store.DocBinding{
				RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example", CollectionID: "c1", CreatedBy: "admin",
			}, "tok"); err != nil {
				t.Fatal(err)
			}
			aid := h.acquire(h.taskOf(h.startSelect()).ID)
			h.exec(`UPDATE ai_runners SET protocol = $2 WHERE runner_id = $1`, h.runnerID, c.protocol)
			h.offer(aid)
			res, err := h.svc.Claim(h.ctx, h.taskOfAttempt(aid), aid, &airunnerv2.ClaimDevIssuesRequest{
				Issues: []int32{1}, Selection: selection([]int{1}, nil, nil),
			})
			if err != nil || !res.GetClaimed() {
				t.Fatalf("Claim = %+v, %v", res, err)
			}
			n := h.queryInt(`SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, res.GetWorkItemId())
			hasDoc := strings.Contains(res.GetMessage(), "doc get")
			switch {
			case wantDocs && n != 1:
				t.Fatalf("协议 %d 应冻结 1 篇文档，实际 %d", c.protocol, n)
			case wantDocs && !hasDoc:
				t.Fatalf("协议 %d 的领取说明应给出文档入口：%q", c.protocol, res.GetMessage())
			case !wantDocs && n != 0:
				t.Fatalf("协议 %d 不应冻结文档：%d", c.protocol, n)
			case !wantDocs && hasDoc:
				t.Fatalf("协议 %d 的领取说明不应要求 doc get：%q", c.protocol, res.GetMessage())
			}
		})
	}
}
