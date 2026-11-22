package publish

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestCheckCancelIgnoresStageB：checkCancel 只在尚未进入阶段 B 时响应 cancel_requested；commit_started_at 非空之后
// 即使 job 上还留着取消请求，也不中断（重启续做要完成剩余目标，PLAN #13.4）。
func TestCheckCancelIgnoresStageB(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	job := e.publishJob(r, ghRelease())
	e.exec(`INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag)
		VALUES ($1,$2,$3,'tester/myapp','v1.2.0')`, job, r.id, srcRepoID)
	e.exec(`UPDATE jobs SET cancel_requested=true WHERE id=$1`, job)
	if err := e.w.checkCancel(t.Context(), job); !errors.Is(err, errPublishCancelled) {
		t.Fatalf("阶段 A 应返回已取消：%v", err)
	}
	e.exec(`UPDATE releases SET commit_started_at=now() WHERE job_id=$1`, job)
	if err := e.w.checkCancel(t.Context(), job); err != nil {
		t.Fatalf("进入阶段 B 后不应再响应取消：%v", err)
	}
}

// TestCancelDuringCommitThenRestart：发布进入阶段 B（npm 已提交、GitHub Release 仍是 draft）后请求取消，然后进程中断
// （context 取消，不结束 job）。重启后续做时必须完成剩余目标、最终成功，不能因为之前记录的取消请求停在跨目标不一致的中途
// （PLAN #13.4）。阶段 A 的取消仍然有效（见 TestWorker_publishCancelled）。
func TestCancelDuringCommitThenRestart(t *testing.T) {
	t.Parallel()
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	e.upstream(r, "bins", "bin", map[string]any{"os": "linux", "arch": "arm64"}, map[string]upstreamFile{
		"kiln": {content: "kiln-binary", exec: true},
	})
	rel := ghRelease()
	rel.Npm = []workflow.NpmTarget{{
		From: "bins.bin", Name: "kilnflow-${{ matrix.os }}-${{ matrix.arch }}", Files: []string{"kiln"},
		Access: workflow.NpmAccessPublic, AllowSourceName: true,
	}}
	job := e.publishJob(r, rel)
	const pkg = npmScope + "/kilnflow-linux-arm64"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cancelErr error
	e.faults.set(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPatch || !strings.Contains(req.URL.Path, "/releases/") {
			return nil, nil
		}
		e.faults.set(nil) // 只处理正式发布 Release 的第一个请求
		// 此时 npm 已经提交、GitHub Release 仍是 draft：通过真实事务请求取消，然后中断当前发布。
		bg := context.WithoutCancel(req.Context())
		cancelErr = e.db.Tx(bg, func(tx pgx.Tx) error {
			return e.eng.CancelRun(bg, tx, r.id, store.CancelByUser)
		})
		cancel()
		return nil, context.Canceled
	})
	e.w.publishOnce(ctx)
	if cancelErr != nil {
		t.Fatalf("提交阶段请求取消：%v", cancelErr)
	}

	// 中断前：npm 已发布，阶段 B 已记录开始提交的时间。
	if v := e.reg.Versions(pkg); len(v) != 1 {
		t.Fatalf("中断前 npm 包应已经发布：%v", v)
	}
	var started *time.Time
	if err := e.db.QueryRow(t.Context(), `SELECT commit_started_at FROM releases WHERE job_id=$1`, job).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started == nil {
		t.Fatal("进入阶段 B 后应记录 commit_started_at")
	}
	if rel := e.oneRelease(srcRepo); !rel.Draft {
		t.Fatalf("中断时 GitHub Release 应仍是 draft：%+v", rel)
	}
	var state store.JobState
	var cancelRequested bool
	if err := e.db.QueryRow(t.Context(), `SELECT state, cancel_requested FROM jobs WHERE id=$1`, job).Scan(&state, &cancelRequested); err != nil {
		t.Fatal(err)
	}
	if state != store.JobRunning {
		t.Fatalf("中断后发布 job 应仍是 running，实际 %s", state)
	}
	if cancelRequested {
		t.Fatal("进入阶段 B 后不应再记录取消请求")
	}

	// 重启：清掉执行标记后重新执行，必须完成 GitHub Release 的正式发布并成功。
	if err := e.w.ResetClaims(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	if rel := e.oneRelease(srcRepo); rel.Draft {
		t.Fatalf("重启续做后应正式发布 GitHub Release：%+v", rel)
	}
	if v := e.reg.Versions(pkg); len(v) != 1 {
		t.Fatalf("重启续做不应重复发布 npm 包：%v", v)
	}
}
