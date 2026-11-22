package planner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/randtoken"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 手动触发 workflow_dispatch（CLI 和 Dashboard 共用）。

// DispatchRequest 是一次手动触发请求。
type DispatchRequest struct {
	Repo     string // owner/name
	Workflow string // 文件名，可以省略 .yml
	Ref      string // 分支、tag 或完整的 ref；空表示默认分支
	Inputs   map[string]string
	// RequestID 用于幂等：同一个 RequestID 只创建一个 run。空时自动生成。
	RequestID string
	Actor     string
	Source    string // SourceDashboard 或 SourceCLI
}

// DispatchError 表示手动触发的请求本身有问题：Field 为 "ref" 时是 ref 找不到，为 "workflow" 时是 workflow 不存在或没有声明 on.workflow_dispatch。
// Dashboard 据此把错误显示在对应的字段旁。
type DispatchError struct {
	Field string
	Err   error
}

func (e *DispatchError) Error() string { return e.Err.Error() }

func (e *DispatchError) Unwrap() error { return e.Err }

// Dispatch 手动触发 workflow_dispatch，返回 run ID。
func (p *Planner) Dispatch(ctx context.Context, req *DispatchRequest) (int64, error) {
	if p.gh == nil {
		return 0, errNoGitHubApp
	}
	if req.RequestID == "" {
		req.RequestID = randtoken.New(16)
	}
	dedupe := dispatchDedupeKey(req.RequestID)
	if id, err := p.runByDedupeKey(ctx, dedupe); err != nil || id != 0 {
		return id, err
	}
	repo, err := repodir.Find(ctx, p.db, p.gh, req.Repo)
	if err != nil {
		return 0, err
	}
	wf := req.Workflow
	if !strings.HasSuffix(wf, ".yml") && !strings.HasSuffix(wf, ".yaml") {
		wf += ".yml"
	}
	ref, sha, err := p.resolveDispatchRef(ctx, repo, req.Ref)
	if err != nil {
		return 0, err
	}
	ids, err := p.PlanEvent(ctx, &Request{
		Repo: repo, Event: workflow.EventWorkflowDispatch, Ref: ref, SHA: sha, SenderLogin: req.Actor,
		Source: req.Source, Inputs: req.Inputs, Workflow: wf, RequestID: req.RequestID,
	})
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		// 并发的同一请求已经创建了 run
		id, err := p.runByDedupeKey(ctx, dedupe)
		if err != nil {
			return 0, err
		}
		if id == 0 {
			return 0, errors.New("没有创建 run")
		}
		return id, nil
	}
	p.auditBestEffort(ctx, req.Actor, "run.dispatch", map[string]any{"run_id": ids[0], "repo": repo.FullName(), "workflow": wf, "ref": ref})
	return ids[0], nil
}

// runByDedupeKey 返回 dedupe_key 对应的 run，不存在时返回 0。
func (p *Planner) runByDedupeKey(ctx context.Context, key string) (int64, error) {
	var id int64
	err := p.db.QueryRow(ctx, `SELECT id FROM runs WHERE dedupe_key=$1`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// resolveDispatchRef 把用户填写的 ref 解析为完整的 ref 和 commit。不带前缀时先当作分支，再当作 tag。
func (p *Planner) resolveDispatchRef(ctx context.Context, repo *store.Repo, ref string) (fullRef, sha string, err error) {
	fullRef, sha, err = gitread.ResolveRef(ctx, p.gh.Inst(repo.InstallationID), repo, ref)
	if gitread.IsRefError(err) {
		return "", "", &DispatchError{Field: "ref", Err: err}
	}
	return fullRef, sha, err
}

// DispatchWorkflow 是某个 ref 下的一个 workflow 文件，以及它能否手动触发。
type DispatchWorkflow struct {
	File string // 文件名，例如 ci.yml
	Name string // workflow 的 name（没有写时为空）
	// Dispatchable 表示声明了 on.workflow_dispatch（与 Dispatch 的判断相同）。
	Dispatchable bool
	// ConfigError 是配置错误的摘要：能手动触发时，触发后 run 会因配置错误直接失败。
	ConfigError string
	Inputs      []workflow.DispatchInput
}

// DispatchTarget 是手动触发的目标：解析后的 ref 和 commit，以及这个 commit 中的 workflow（按文件名排序）。
type DispatchTarget struct {
	Repo      *store.Repo
	Ref, SHA  string
	Workflows []DispatchWorkflow
}

// DispatchWorkflows 解析仓库 repo（owner/name）的 ref（空表示默认分支），列出其中 workflow 目录（workflow.Dir）下的 workflow 及能否手动触发。
// ref 找不到时返回 *DispatchError。只读取 GitHub，不创建 run。
func (p *Planner) DispatchWorkflows(ctx context.Context, repoName, ref string) (*DispatchTarget, error) {
	if p.gh == nil {
		return nil, errNoGitHubApp
	}
	repo, err := repodir.Find(ctx, p.db, p.gh, repoName)
	if err != nil {
		return nil, err
	}
	fullRef, sha, err := p.resolveDispatchRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	inst := p.gh.Inst(repo.InstallationID)
	tree, err := p.tree(ctx, inst, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("读取 git tree：%w", err)
	}
	files := workflowFiles(tree)
	t := &DispatchTarget{Repo: repo, Ref: fullRef, SHA: sha, Workflows: make([]DispatchWorkflow, 0, len(files))}
	for file, blobSHA := range files {
		data, err := p.blob(ctx, inst, repo, blobSHA)
		if err != nil {
			return nil, fmt.Errorf("读取 %s：%w", file, err)
		}
		wf, diags := workflow.Parse(file, data)
		w := DispatchWorkflow{File: file, Name: wf.Name, Dispatchable: wf.On.WorkflowDispatch != nil}
		if w.Dispatchable {
			w.Inputs = wf.On.WorkflowDispatch.Inputs
		}
		if diags.HasErrors() {
			w.ConfigError = diags.Errors().Error()
		}
		t.Workflows = append(t.Workflows, w)
	}
	slices.SortFunc(t.Workflows, func(a, b DispatchWorkflow) int { return strings.Compare(a.File, b.File) })
	return t, nil
}
