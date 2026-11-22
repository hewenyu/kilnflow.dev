package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// runs 列出最近的 run。
func (a *app) runs(ctx context.Context, args []string) error {
	fs := newFlags("runs")
	repo := fs.String("repo", "", "仓库")
	n := int32Flag(fs, "n", 30, "数量")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListRuns(ctx, req(&apiv1.ListRunsRequest{Repo: *repo, Limit: *n}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Runs))
	for _, r := range resp.Msg.Runs {
		rows = append(rows, []string{"#" + strconv.FormatInt(r.Id, 10), r.State, r.Repo, r.Workflow, r.Event, r.Ref, shorten.Hash(r.Sha), ago(r.CreatedAt)})
	}
	a.table("RUN\t状态\t仓库\tworkflow\t事件\tref\tcommit\t创建", rows)
	return nil
}

// showRun 输出 run 详情和 job 列表。
func (a *app) showRun(ctx context.Context, args []string) error {
	if err := need(args, 1, "run RUN_ID"); err != nil {
		return err
	}
	runID, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.GetRun(ctx, req(&apiv1.GetRunRequest{Id: runID}))
	if err != nil {
		return err
	}
	r := resp.Msg.Run
	fmt.Fprintf(a.stdout, "run #%d  %s  %s  %s  %s %s@%s  rerun %d  %s/runs/%d\n",
		r.Id, r.State, r.Repo, r.Workflow, r.Event, r.Ref, shorten.Hash(r.Sha), r.Rerun, a.server, r.Id)
	if r.Error != "" {
		fmt.Fprintln(a.stdout, "\n配置错误：\n"+r.Error)
	}
	rows := make([][]string, 0, len(resp.Msg.Jobs))
	for _, j := range resp.Msg.Jobs {
		st := j.State
		if j.CachedFromJobId != 0 {
			st += fmt.Sprintf("（缓存 #%d）", j.CachedFromJobId)
		}
		dur := ""
		if j.StartedAt != 0 {
			end := j.FinishedAt
			if end == 0 {
				end = time.Now().Unix()
			}
			dur = (time.Duration(end-j.StartedAt) * time.Second).String()
		}
		rows = append(rows, []string{strconv.FormatInt(j.Id, 10), j.DisplayName, st, j.Platform, j.Runner, dur, jobNote(j)})
	}
	fmt.Fprintln(a.stdout)
	a.table("JOB\t名称\t状态\t平台\tRunner\t耗时\t说明", rows)
	return nil
}

// logs 输出 job 日志；-f 时持续输出直到 job 结束或被中断。
func (a *app) logs(ctx context.Context, args []string) error {
	fs := newFlags("logs")
	follow := fs.Bool("f", false, "持续输出直到结束")
	attempt := fs.Int64("attempt", 0, "attempt ID，默认最新")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "logs JOB_ID [-f]"); err != nil {
		return err
	}
	jobID, err := parseID(pos[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	var off int64
	for {
		resp, err := cl.GetJobLog(ctx, req(&apiv1.GetJobLogRequest{JobId: jobID, AttemptId: *attempt, Offset: off}))
		if err != nil {
			return err // 包括用户按 Ctrl-C 中断 -f，由 exitCode 映射为 130
		}
		if _, err := a.stdout.Write(resp.Msg.Data); err != nil {
			return err
		}
		off = resp.Msg.NextOffset
		if len(resp.Msg.Data) > 0 {
			continue
		}
		if !*follow || resp.Msg.Finished {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (a *app) cancel(ctx context.Context, args []string) error {
	if err := need(args, 1, "cancel RUN_ID"); err != nil {
		return err
	}
	runID, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.CancelRun(ctx, req(&apiv1.CancelRunRequest{Id: runID})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已请求取消")
	return nil
}

func (a *app) rerun(ctx context.Context, args []string) error {
	const use = "rerun RUN_ID [--mode failed|all|all_no_cache]"
	fs := newFlags("rerun")
	mode := fs.String("mode", "failed", "failed | all | all_no_cache")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, use); err != nil {
		return err
	}
	runID, err := parseID(pos[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.RerunRun(ctx, req(&apiv1.RerunRunRequest{Id: runID, Mode: *mode}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已创建 rerun %d\n", resp.Msg.Rerun)
	return nil
}

func (a *app) rerunJob(ctx context.Context, args []string) error {
	if err := need(args, 1, "rerun-job JOB_ID"); err != nil {
		return err
	}
	jobID, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.RerunJob(ctx, req(&apiv1.RerunJobRequest{JobId: jobID}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已创建 rerun %d\n", resp.Msg.Rerun)
	return nil
}

func (a *app) dispatch(ctx context.Context, args []string) error {
	fs := newFlags("dispatch")
	ref := fs.String("ref", "", "分支或 tag，默认为默认分支")
	var inputs multi
	fs.Var(&inputs, "i", "输入 key=value（可重复）")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "dispatch REPO WORKFLOW [--ref REF] [-i key=value]"); err != nil {
		return err
	}
	in := make(map[string]string, len(inputs))
	for _, kv := range inputs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return usageErrorf("输入应写成 key=value：%s", kv)
		}
		in[k] = v
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.Dispatch(ctx, req(&apiv1.DispatchRequest{Repo: pos[0], Workflow: pos[1], Ref: *ref, Inputs: in}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已创建 run #%d\n", resp.Msg.RunId)
	return nil
}

// jobNote 返回 job 列表中的"说明"：排队中的 job 显示等待原因（例如资源不够时需要多少、各 Runner 能提供多少），
// 其他显示原因。
func jobNote(j *apiv1.Job) string {
	if j.WaitReason != "" {
		return truncate("等待："+j.WaitReason, 160)
	}
	return truncate(j.Reason, 60)
}
