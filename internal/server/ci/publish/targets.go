package publish

import (
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 发布目标（PLAN #13.4）：每个目标在 release_targets 中一行，按提交顺序编号：npm 包、镜像、HTTP 上传、GitHub Release。

// 目标的种类（release_targets.kind）。
const (
	kindNpm    = "npm"
	kindImage  = "image"
	kindUpload = "upload"
	kindGitHub = "github"
)

// 目标的状态（release_targets.state）。
const (
	targetPending   = "pending"   // 还没有暂存
	targetStaged    = "staged"    // 已暂存，对外不可见（GitHub 为 draft）
	targetCommitted = "committed" // 已提交，对外可见
	targetFailed    = "failed"
)

// targetRec 是一个发布目标在 release_targets 中的记录。
type targetRec struct {
	seq    int
	kind   string
	name   string // 展示名
	target string // 镜像名、上传地址、GitHub 仓库
}

// targetRecords 按提交顺序列出发布 job 的全部目标：npm 包、镜像、HTTP 上传、GitHub Release。
// npm 包的 seq 就是它在 np.pkgs 中的下标，镜像的 seq 是 npm 包数加上它在 release.images 中的下标（imageSeq）。
func targetRecords(np *npmPublish, gp *githubPublish, r *workflow.Release) []targetRec {
	var recs []targetRec
	add := func(kind, name, target string) {
		recs = append(recs, targetRec{seq: len(recs), kind: kind, name: name, target: target})
	}
	if np != nil {
		for _, p := range np.pkgs {
			add(kindNpm, p.name, p.name)
		}
	}
	for _, t := range r.Images {
		add(kindImage, t.Name, t.Name)
	}
	for _, u := range r.Uploads {
		add(kindUpload, cmp.Or(u.Name, u.URL), u.URL)
	}
	if gp != nil {
		add(kindGitHub, gp.target.FullName(), gp.target.FullName())
	}
	return recs
}

// imageSeq 返回 release.images[i] 的 seq（排在 npm 包之后）；i < 0（不属于某个镜像的错误）时返回 -1。
func (pj *publishJob) imageSeq(i int) int {
	if i < 0 {
		return -1
	}
	return pj.npmCount + i
}

// manifest 是文件类目标在阶段 A、B 中使用的发布内容：tag、构建的 commit、run 和全部 asset（含校验和文件）。
type manifest struct {
	tag    string
	commit string
	runID  int64
	assets []release.Asset
}

// fileTarget 是文件类发布目标（GitHub Release、HTTP 上传）：asset 在阶段 0 只打包一次，各目标发布同一份字节。
// 所有目标都 stage 成功之后才依次 commit；任何一个 stage 失败，所有目标都不提交。
// stage 和 commit 都必须幂等：重跑时已经完成的部分跳过，只补剩下的。
type fileTarget interface {
	seq() int
	// stage 是阶段 A：检查并上传 asset，对外不可见。
	stage(ctx context.Context, m *manifest) error
	// commit 是阶段 B：原子地对外发布。返回的 detail 记录到 release_targets.detail。
	commit(ctx context.Context, m *manifest) (map[string]any, error)
}

// fileTargets 是阶段 0 中文件类目标的准备：按提交顺序（HTTP 上传、GitHub Release）创建，GitHub 还要取得 Release 说明。
func (w *Worker) fileTargets(ctx context.Context, pj *publishJob, gp *githubPublish, recs []targetRec) ([]fileTarget, error) {
	var out []fileTarget
	uploads := pj.job.Spec.Release.Uploads
	for _, r := range recs {
		switch r.kind {
		case kindUpload:
			t := uploads[0]
			uploads = uploads[1:]
			t.Name = r.name
			out = append(out, &uploadTarget{w: w, pj: pj, t: t, n: r.seq})
		case kindGitHub:
			notes, err := w.releaseNotes(ctx, gp)
			if err != nil {
				return nil, err
			}
			out = append(out, &githubTarget{w: w, p: gp, n: r.seq, notes: notes})
		}
	}
	return out, nil
}

// stageOrder 返回阶段 A 的暂存顺序：GitHub Release 最先（它的检查最多：已有正式 Release、源仓库名检查），
// 然后是 HTTP 上传。阶段 B 的提交顺序相反，GitHub Release 最后正式发布：用户在 GitHub 上看到新版本时，其他地方已经可以下载。
func stageOrder(files []fileTarget) []fileTarget {
	out := make([]fileTarget, 0, len(files))
	for _, first := range []bool{true, false} {
		for _, t := range files {
			if _, gh := t.(*githubTarget); gh == first {
				out = append(out, t)
			}
		}
	}
	return out
}

// targetSummary 返回 releases.target_repo 中记录的发布目标（逗号分隔）：GitHub 仓库、HTTP 上传的名称、镜像名、npm 包名。
func targetSummary(recs []targetRec) string {
	var out []string
	for _, kind := range []string{kindGitHub, kindUpload, kindImage, kindNpm} {
		for _, r := range recs {
			if r.kind == kind {
				out = append(out, r.name)
			}
		}
	}
	return strings.Join(out, ", ")
}

// initTargets 在发布 job 开始执行时（包括重启后继续）重建它的目标记录，全部为 pending。
func (w *Worker) initTargets(ctx context.Context, jobID int64, recs []targetRec) error {
	return w.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM release_targets WHERE job_id=$1`, jobID); err != nil {
			return err
		}
		for _, r := range recs {
			if _, err := tx.Exec(ctx, `INSERT INTO release_targets (job_id, seq, kind, name, target) VALUES ($1,$2,$3,$4,$5)`,
				jobID, r.seq, r.kind, r.name, r.target); err != nil {
				return err
			}
		}
		return nil
	})
}

// setTarget 记录目标的状态和结果（detail 合并到已有的结果中）。只用于展示和 latest 的判断，失败时记录告警。
func (w *Worker) setTarget(ctx context.Context, jobID int64, seq int, state string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if _, err := w.db.Exec(ctx, `UPDATE release_targets SET state=$3, detail=detail || $4::jsonb, error=NULL, updated_at=now() WHERE job_id=$1 AND seq=$2`,
		jobID, seq, state, store.MustJSON(detail)); err != nil {
		w.log.Warn("记录发布目标", "job", jobID, "seq", seq, "err", err)
	}
}

// targetFailed 记录目标失败的原因（凭据已脱敏）。取消不算目标失败，保持原来的状态。
func (w *Worker) targetFailed(ctx context.Context, pj *publishJob, seq int, err error) {
	if seq < 0 || errors.Is(err, errPublishCancelled) || ctx.Err() != nil {
		return
	}
	if _, xerr := w.db.Exec(ctx, `UPDATE release_targets SET state=$3, error=$4, updated_at=now() WHERE job_id=$1 AND seq=$2`,
		pj.job.ID, seq, targetFailed, pj.redact(err).Error()); xerr != nil {
		w.log.Warn("记录发布目标", "job", pj.job.ID, "seq", seq, "err", xerr)
	}
}
