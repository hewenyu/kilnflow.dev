package publish

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 发布阶段（PLAN #13.4），保存在 releases.stage 中，Dashboard 和 CLI 展示发布进度时读取。
const (
	stagePrepare = "prepare" // 阶段 0：准备
	stageStaging = "stage"   // 阶段 A：暂存
	stageCommit  = "commit"  // 阶段 B：提交
	stageDone    = "done"    // 已完成
)

// publishJob 是一次发布的公共上下文：发布 job、它所在的 run、源仓库和触发的 tag。
type publishJob struct {
	job     *cistore.Job
	run     *cistore.Run
	src     *store.Repo
	srcInst *gh.Inst
	tag     string
	plog    *publishLog
	// secrets 是阶段 A 解密的发布凭据（secret 名称到值），记录目标的错误前用它脱敏。
	secrets map[string]string
	// npmCount 是 npm 包的个数：npm 包的 seq 排在最前，镜像的 seq 从这里开始（imageSeq）。
	npmCount int
}

// redact 去掉错误信息中的发布凭据。
func (pj *publishJob) redact(err error) error { return redactSecrets(err, pj.secrets) }

// publishJob 执行一个发布 job（PLAN #13.4）：阶段 0 准备、阶段 A 暂存、阶段 B 提交。
// 阶段 A 中所有目标（npm、镜像、GitHub Release、HTTP 上传）都暂存成功后，阶段 B 才按顺序提交：
// 先发布 npm 包（版本不可删除重来），再设置镜像 tag，再提交 HTTP 上传，最后把 GitHub Release 正式发布。
// 每个目标的结果记录在 release_targets 中。
func (w *Worker) publishJob(ctx context.Context, j *cistore.Job, plog *publishLog) (err error) {
	pj, err := w.newPublishJob(ctx, j, plog)
	if err != nil {
		return err
	}
	r := j.Spec.Release
	gp, np, err := w.newTargets(ctx, pj)
	if err != nil {
		return err
	}
	recs := targetRecords(np, gp, r)
	if _, err := w.db.Exec(ctx, `INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (job_id) DO UPDATE SET state='running', error=NULL, updated_at=now()`, j.ID, pj.run.ID, pj.src.ID, targetSummary(recs), pj.tag); err != nil {
		return err
	}
	if err := w.initTargets(ctx, j.ID, recs); err != nil {
		return err
	}

	// ---------- 阶段 0：准备 ----------
	w.releaseStage(ctx, j.ID, stagePrepare, map[string]any{})
	work := filepath.Join(w.cfg.WorkDir, itoa(j.ID))
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(work); err != nil {
			w.log.Warn("清理发布工作目录", "dir", work, "err", err)
		}
	}()
	images, err := w.prepareImages(ctx, pj)
	if err != nil {
		return err
	}
	if np != nil {
		if err := w.packNpm(ctx, pj, np, work, assetModTime(pj.run)); err != nil {
			return err
		}
	}
	var assets []release.Asset
	if r.HasFileTarget() {
		if assets, err = w.buildAssets(ctx, pj, work); err != nil {
			return err
		}
	}
	files, err := w.fileTargets(ctx, pj, gp, recs)
	if err != nil {
		return err
	}
	m := &manifest{tag: pj.tag, commit: pj.run.SHA, runID: pj.run.ID, assets: assets}

	// ---------- 阶段 A：暂存 ----------
	w.releaseStage(ctx, j.ID, stageStaging, map[string]any{})
	if err := w.checkTag(ctx, pj); err != nil {
		return err
	}
	// 确认 tag 之后对所有发布 job（包括只有 release.github 的）再次校验发布授权：environment 存在、ref 策略、
	// 跨仓库的允许发布仓库列表。不再只在需要凭据的目标之前检查。
	if err := w.recheckAuthorization(ctx, pj); err != nil {
		return err
	}
	if len(images) > 0 || len(r.Uploads) > 0 || np != nil {
		// 凭据只在确认 tag、environment 和 ref 策略之后才解密；修改的是具名返回值 err：之后返回的任何错误都经过脱敏
		defer func() { err = pj.redact(err) }()
		if err := w.stageWithCredentials(ctx, pj, np, images); err != nil {
			return err
		}
	}
	if len(files) > 0 {
		w.releaseStage(ctx, j.ID, stageStaging, map[string]any{"assets": len(assets)})
	}
	for _, t := range stageOrder(files) {
		if err := t.stage(ctx, m); err != nil {
			w.targetFailed(ctx, pj, t.seq(), err)
			return err
		}
		w.setTarget(ctx, j.ID, t.seq(), targetStaged, nil)
	}

	// ---------- 阶段 B：提交 ----------
	// 单事务原子屏障：尚未进入阶段 B 且已请求取消时在这里中止；进入之后不再响应取消（PLAN #13.4）。
	if err := w.enterCommitStage(ctx, j.ID); err != nil {
		return err
	}
	if err := w.commitCredentialed(ctx, pj, np, images); err != nil {
		return err
	}
	for _, t := range files {
		detail, err := t.commit(ctx, m)
		if err != nil {
			w.targetFailed(ctx, pj, t.seq(), err)
			return err
		}
		state := targetCommitted
		if detail != nil && detail["draft"] == true {
			state = targetStaged // notes.from: draft：Release 保持 draft，留给人工发布
		}
		w.setTarget(ctx, j.ID, t.seq(), state, detail)
	}
	w.releaseStage(ctx, j.ID, stageDone, map[string]any{})
	return nil
}

// newTargets 准备 GitHub Release 和 npm 包的目标（没有时为 nil），记下 npm 包的个数（镜像的 seq 排在它们之后）。
func (w *Worker) newTargets(ctx context.Context, pj *publishJob) (*githubPublish, *npmPublish, error) {
	var gp *githubPublish
	if pj.job.Spec.Release.GitHub != nil {
		var err error
		if gp, err = w.newGitHubPublish(ctx, pj); err != nil {
			return nil, nil, err
		}
	}
	np, err := w.prepareNpm(ctx, pj)
	if err != nil {
		return nil, nil, err
	}
	if np != nil {
		pj.npmCount = len(np.pkgs)
	}
	return gp, np, nil
}

// stageWithCredentials 解密发布凭据并暂存需要凭据的目标（npm 包、镜像）。调用方负责把返回的错误脱敏。
func (w *Worker) stageWithCredentials(ctx context.Context, pj *publishJob, np *npmPublish, images []*imagePublish) error {
	creds, npmCreds, err := w.publishCredentials(ctx, pj, np != nil)
	if err != nil {
		return err
	}
	pj.secrets = secretValues(creds)
	if npmCreds != nil {
		pj.secrets[npmTokenKey] = npmCreds.Token
	}
	return w.stageCredentialed(ctx, pj, np, npmCreds, images, creds)
}

// stageCredentialed 是阶段 A 中需要发布凭据的目标：npm 包和镜像（凭据已解密）。
func (w *Worker) stageCredentialed(ctx context.Context, pj *publishJob, np *npmPublish, npmCreds *store.NpmCredentials,
	images []*imagePublish, creds map[string]store.ResolvedSecret,
) error {
	if np != nil {
		if err := w.npmClient(np, npmCreds); err != nil {
			return err
		}
		if i, err := w.stageNpm(ctx, pj, np); err != nil {
			w.targetFailed(ctx, pj, i, err)
			return err
		}
		for _, p := range np.pkgs {
			w.setTarget(ctx, pj.job.ID, p.seq, targetStaged, nil)
		}
	}
	if len(images) > 0 {
		if i, err := w.stageImages(ctx, pj, images, creds); err != nil {
			w.targetFailed(ctx, pj, pj.imageSeq(i), err)
			return err
		}
		for i := range images {
			w.setTarget(ctx, pj.job.ID, pj.imageSeq(i), targetStaged, nil)
		}
	}
	return nil
}

// commitCredentialed 是阶段 B 的前两步：先发布 npm 包（版本不可删除重来），再设置镜像 tag。
func (w *Worker) commitCredentialed(ctx context.Context, pj *publishJob, np *npmPublish, images []*imagePublish) error {
	if np != nil {
		if i, err := w.commitNpm(ctx, pj, np); err != nil {
			w.targetFailed(ctx, pj, i, err)
			return err
		}
	}
	if i, err := w.commitImages(ctx, pj, images); err != nil {
		w.targetFailed(ctx, pj, pj.imageSeq(i), err)
		return err
	}
	return nil
}

// newPublishJob 读取发布 job 所在的 run 和源仓库，确认它由 tag push 触发。
func (w *Worker) newPublishJob(ctx context.Context, j *cistore.Job, plog *publishLog) (*publishJob, error) {
	if w.gh == nil {
		return nil, errNoGitHubApp
	}
	if j.Spec.Release == nil || !j.Spec.Release.HasTarget() {
		return nil, errors.New("发布 job 没有发布目标")
	}
	run, err := cistore.GetRun(ctx, w.db, j.RunID, false)
	if err != nil {
		return nil, err
	}
	src, err := store.GetRepo(ctx, w.db, run.RepoID)
	if err != nil {
		return nil, err
	}
	tag, ok := strings.CutPrefix(run.Ref, gitref.TagPrefix)
	if !ok {
		return nil, errors.New("发布只能由 tag push 触发")
	}
	return &publishJob{job: j, run: run, src: src, srcInst: w.gh.Inst(src.InstallationID), tag: tag, plog: plog}, nil
}

// releaseStage 记录发布进度（只用于展示，失败时记录告警）。
func (w *Worker) releaseStage(ctx context.Context, jobID int64, stage string, progress map[string]any) {
	if _, err := w.db.Exec(ctx, `UPDATE releases SET stage=$2, progress=progress || $3::jsonb, updated_at=now() WHERE job_id=$1`,
		jobID, stage, store.MustJSON(progress)); err != nil {
		w.log.Warn("记录发布进度", "job", jobID, "stage", stage, "err", err)
	}
}

// splitFrom 把 "<job>.<output>" 拆成 job 名和输出名。
func splitFrom(from string) (job, output string) {
	job, output, _ = strings.CutLast(from, ".")
	return job, output
}

// upstreamJobs 返回 from（"<job>.<output>"）引用的、与发布 job 同一次运行的上游 job（矩阵的全部变体，按 ID 排序）和输出名。
func (w *Worker) upstreamJobs(ctx context.Context, j *cistore.Job, from string) ([]*cistore.Job, string, error) {
	jobName, output := splitFrom(from)
	ups, err := cistore.JobsNamed(ctx, w.db, j.RunID, j.Rerun, jobName)
	if err != nil {
		return nil, "", err
	}
	if len(ups) == 0 {
		return nil, "", fmt.Errorf("找不到上游 job %s", jobName)
	}
	return ups, output, nil
}

// upstreamOutput 读取上游 job 一个输出的 manifest，上游必须已经成功。错误信息不含上游的名称，由调用方补充。
func (w *Worker) upstreamOutput(ctx context.Context, up *cistore.Job, output string) (*cas.OutputManifest, error) {
	if up.State != store.JobSucceeded || up.ResultManifest == nil {
		return nil, fmt.Errorf("上游没有成功（%s）", up.State)
	}
	md, err := w.engine.OutputManifest(ctx, *up.ResultManifest, output)
	if err != nil {
		return nil, err
	}
	var om cas.OutputManifest
	if err := cas.ReadJSON(ctx, w.cas, md, &om); err != nil {
		return nil, err
	}
	return &om, nil
}

// checkTag 是阶段 A 的第一步：确认源仓库的 tag 仍然指向本次构建的 commit，已删除或已移动则停止。
func (w *Worker) checkTag(ctx context.Context, pj *publishJob) error {
	commit, _, err := pj.srcInst.ResolveTag(ctx, pj.src.FullName(), pj.tag)
	if err != nil {
		return fmt.Errorf("读取 tag %s：%w", pj.tag, err)
	}
	if commit != pj.run.SHA {
		return fmt.Errorf("tag %s 已经移动到 %s，不再指向本次构建的 %s，停止发布", pj.tag, shorten.Hash(commit), shorten.Hash(pj.run.SHA))
	}
	return nil
}

// redactedError 是把 secret 的值替换成 *** 后的错误，保留原错误链（errors.Is 仍然可用）。
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactSecrets 去掉错误信息中出现的 secret 值（规则见 internal/redact：先替换较长的值，太短的值不替换）。
func redactSecrets(err error, secrets map[string]string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := redact.String(msg, slices.Collect(maps.Values(secrets))...)
	if out == msg {
		return err
	}
	return &redactedError{msg: out, err: err}
}
