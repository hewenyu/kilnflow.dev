package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// doc 管理仓库与外部设计文档集（Outline）的绑定：bind（token 从标准输入读取）、list、unbind。
// 绑定之后 AI 开发与审计在规划任务时冻结文档快照，Agent 经 kiln-orca task doc 读取，执行机器上不需要文档凭据。
func (a *app) doc(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "list", "":
		return a.docList(ctx)
	case "bind":
		return a.docBind(ctx, rest)
	case "unbind":
		return a.docUnbind(ctx, rest)
	case "decision-doc":
		return a.docDecisionDoc(ctx, rest)
	case "decision-sync":
		return a.docDecisionSync(ctx, rest)
	}
	return usageErrorf("未知的子命令 doc %s\n用法：kiln doc list | bind REPO --base-url URL --collection ID [--name NAME] | unbind REPO | decision-doc set|clear|list | decision-sync list|reconcile|resolve", sub)
}

func (a *app) docList(ctx context.Context) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListDocBindings(ctx, req(&apiv1.ListDocBindingsRequest{}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Bindings))
	for _, b := range resp.Msg.Bindings {
		rows = append(rows, []string{
			b.Repo, b.Provider, b.BaseUrl, b.CollectionId, b.CollectionName,
			b.UpdatedBy, time.Unix(b.UpdatedAt, 0).Format("2006-01-02 15:04"),
		})
	}
	a.table("仓库\t提供方\t地址\t文档集\t名称\t更新者\t更新时间", rows)
	return nil
}

func (a *app) docBind(ctx context.Context, args []string) error {
	fs := newFlags("doc bind")
	baseURL := fs.String("base-url", "", "文档系统地址，例如 https://outline.example.com")
	collection := fs.String("collection", "", "文档集（Outline Collection）的 id")
	name := fs.String("name", "", "文档集名称（可选，仅展示）")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "doc bind REPO --base-url URL --collection ID [--name NAME]（token 从标准输入读取）"); err != nil {
		return err
	}
	if *baseURL == "" || *collection == "" {
		return usageErrorf("用法：kiln doc bind REPO --base-url URL --collection ID [--name NAME]（token 从标准输入读取）")
	}
	if _, ok := terminalFd(a.stdin); ok {
		fmt.Fprintln(a.stderr, "输入 Outline API token，以 Ctrl-D 结束：")
	}
	b, err := io.ReadAll(io.LimitReader(a.stdin, maxSecretSize+1))
	if err != nil {
		return fmt.Errorf("读取 token：%w", err)
	}
	if len(b) > maxSecretSize {
		return usageErrorf("token 超过 %d 字节", maxSecretSize)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return usageErrorf("token 为空")
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.BindDocCollection(ctx, req(&apiv1.BindDocCollectionRequest{
		Repo: pos[0], BaseUrl: *baseURL, CollectionId: *collection, CollectionName: *name, Token: token,
	}))
	if err != nil {
		return err
	}
	bd := resp.Msg.GetBinding()
	fmt.Fprintf(a.stdout, "已保存 %s 的文档绑定：%s 文档集 %s（%s）\n", bd.GetRepo(), bd.GetBaseUrl(), bd.GetCollectionId(), bd.GetCollectionName())
	return nil
}

func (a *app) docUnbind(ctx context.Context, args []string) error {
	if err := need(args, 1, "doc unbind REPO"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.UnbindDocCollection(ctx, req(&apiv1.UnbindDocCollectionRequest{Repo: args[0]})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已删除", args[0], "的文档绑定")
	return nil
}

// docDecisionDoc 管理「待决策清单」的文档配置：set（token 从标准输入读取）、clear、list。
// 这个清单只是通知面：同步失败的条目不会改变 confirmed_decision 的判定或 PR 的 kiln:hold 门禁。
func (a *app) docDecisionDoc(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "list", "":
		return a.decisionDocList(ctx)
	case "set":
		return a.decisionDocSet(ctx, rest)
	case "clear":
		return a.decisionDocClear(ctx, rest)
	}
	return usageErrorf("未知的子命令 doc decision-doc %s\n用法：kiln doc decision-doc set REPO --doc-id UUID [--base-url URL] | clear REPO | list", sub)
}

func (a *app) decisionDocList(ctx context.Context) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListDecisionDocs(ctx, req(&apiv1.ListDecisionDocsRequest{}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.DecisionDocs))
	for _, d := range resp.Msg.DecisionDocs {
		rows = append(rows, []string{
			d.Repo, d.BaseUrl, d.DocId, d.CollectionId, dashText(d.WarningCode),
			strconv.FormatInt(d.PendingCount, 10), strconv.FormatInt(d.UnknownCount, 10),
			d.UpdatedBy, time.Unix(d.UpdatedAt, 0).Format("2006-01-02 15:04"),
		})
	}
	a.table("仓库\t地址\t文档\t集合\t告警\t待处理\t结果不明\t更新者\t更新时间", rows)
	return nil
}

func (a *app) decisionDocSet(ctx context.Context, args []string) error {
	fs := newFlags("doc decision-doc set")
	docID := fs.String("doc-id", "", "目标文档的 UUID")
	baseURL := fs.String("base-url", "", "文档系统地址（留空时沿用只读绑定的地址）")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "doc decision-doc set REPO --doc-id UUID [--base-url URL]（token 从标准输入读取）"); err != nil {
		return err
	}
	if *docID == "" {
		return usageErrorf("用法：kiln doc decision-doc set REPO --doc-id UUID [--base-url URL]（token 从标准输入读取）")
	}
	token, err := a.readSecret("输入 Outline 写 token（只用于待决策清单），以 Ctrl-D 结束：")
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.SetDecisionDoc(ctx, req(&apiv1.SetDecisionDocRequest{
		Repo: pos[0], BaseUrl: *baseURL, DocId: *docID, Token: token,
	}))
	if err != nil {
		return err
	}
	d := resp.Msg.GetDecisionDoc()
	fmt.Fprintf(a.stdout, "已保存 %s 的待决策清单：%s 文档 %s（集合 %s）\n", d.GetRepo(), d.GetBaseUrl(), d.GetDocId(), d.GetCollectionId())
	if resp.Msg.CancelledCount > 0 || resp.Msg.UnknownCount > 0 {
		fmt.Fprintf(a.stdout, "文档轮转：%d 条待处理条目已取消，%d 条结果不明（需要只读复查或人工结案）\n",
			resp.Msg.CancelledCount, resp.Msg.UnknownCount)
	}
	return nil
}

func (a *app) decisionDocClear(ctx context.Context, args []string) error {
	if err := need(args, 1, "doc decision-doc clear REPO"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ClearDecisionDoc(ctx, req(&apiv1.ClearDecisionDocRequest{Repo: args[0]}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已删除 %s 的待决策清单配置：%d 条待处理条目已取消，%d 条结果不明\n",
		args[0], resp.Msg.CancelledCount, resp.Msg.UnknownCount)
	return nil
}

// docDecisionSync 查看和人工处理决策同步记录：list、reconcile（只读复查）、resolve（人工结案）。
func (a *app) docDecisionSync(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "list", "":
		return a.decisionSyncList(ctx, rest)
	case "reconcile":
		return a.decisionSyncReconcile(ctx, rest)
	case "resolve":
		return a.decisionSyncResolve(ctx, rest)
	}
	return usageErrorf("未知的子命令 doc decision-sync %s\n用法：kiln doc decision-sync list [--repo REPO] [--state STATE] [--limit N] | reconcile ID | resolve ID --state delivered|cancelled --note REASON", sub)
}

func (a *app) decisionSyncList(ctx context.Context, args []string) error {
	fs := newFlags("doc decision-sync list")
	repo := fs.String("repo", "", "仓库")
	state := fs.String("state", "", "pending、attempted、delivered、unknown 或 cancelled")
	limit := int32Flag(fs, "limit", 50, "数量")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		// 绝不静默忽略位置参数而列出全部仓库：多写了仓库名时用户以为在筛选，实际看到的是所有条目。
		return usageErrorf("kiln doc decision-sync list 不接受位置参数 %q\n用法：kiln doc decision-sync list [--repo REPO] [--state STATE] [--limit N]",
			strings.Join(pos, " "))
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListDecisionSyncs(ctx, req(&apiv1.ListDecisionSyncsRequest{Repo: *repo, State: *state, Limit: *limit}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Syncs))
	for _, s := range resp.Msg.Syncs {
		kind := s.EntryKind
		if s.Category != "" {
			kind += "/" + s.Category
		}
		rows = append(rows, []string{
			"#" + strconv.FormatInt(s.Id, 10), s.Repo, s.State, kind,
			strconv.Itoa(int(s.WorkItemId)), strconv.Itoa(int(s.TaskId)), strconv.Itoa(int(s.EntryIdx)),
			s.LastErrorCode, s.Marker, ago(s.CreatedAt),
		})
	}
	a.table("ID\t仓库\t状态\t类型\t工作项\tTask\t序号\t错误\t标记\t创建", rows)
	return nil
}

func (a *app) decisionSyncReconcile(ctx context.Context, args []string) error {
	if err := need(args, 1, "doc decision-sync reconcile ID"); err != nil {
		return err
	}
	id, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ReconcileDecisionSync(ctx, req(&apiv1.ReconcileDecisionSyncRequest{Id: id}))
	if err != nil {
		return err
	}
	s := resp.Msg.GetSync()
	// 只有服务端确认找到 marker 并且状态确实推进到 delivered 时才说“已记为 delivered”，避免与返回状态矛盾。
	if resp.Msg.GetFound() && s.GetState() == "delivered" {
		fmt.Fprintf(a.stdout, "条目 #%d 已在文档中找到精确标记，已记为 delivered\n", s.GetId())
	} else {
		fmt.Fprintf(a.stdout, "条目 #%d 未确认送达，当前状态 %s；可以用 resolve 人工结案\n", s.GetId(), s.GetState())
	}
	return nil
}

func (a *app) decisionSyncResolve(ctx context.Context, args []string) error {
	fs := newFlags("doc decision-sync resolve")
	state := fs.String("state", "", "delivered 或 cancelled")
	note := fs.String("note", "", "人工结案的原因（必填）")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "doc decision-sync resolve ID --state delivered|cancelled --note REASON"); err != nil {
		return err
	}
	if *state == "" || *note == "" {
		return usageErrorf("用法：kiln doc decision-sync resolve ID --state delivered|cancelled --note REASON")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ResolveDecisionSync(ctx, req(&apiv1.ResolveDecisionSyncRequest{Id: id, State: *state, Note: *note}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "条目 #%d 已人工结案为 %s（只影响通知投递记录，不改变 kiln:hold 门禁）\n",
		resp.Msg.GetSync().GetId(), resp.Msg.GetSync().GetState())
	return nil
}

// readSecret 从标准输入读取一段 token（提示信息只在终端上打印）。
func (a *app) readSecret(prompt string) (string, error) {
	if _, ok := terminalFd(a.stdin); ok {
		fmt.Fprintln(a.stderr, prompt)
	}
	b, err := io.ReadAll(io.LimitReader(a.stdin, maxSecretSize+1))
	if err != nil {
		return "", fmt.Errorf("读取 token：%w", err)
	}
	if len(b) > maxSecretSize {
		return "", usageErrorf("token 超过 %d 字节", maxSecretSize)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", usageErrorf("token 为空")
	}
	return token, nil
}
