package publish

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/fakeupload"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// ---------- 发布到 HTTP 上传目标（用 internal/fakeupload） ----------

const (
	siteToken  = "lc_rel_s3cr3t-site-token"
	releaseEnv = "release"
)

// uploadEnv 是 HTTP 上传的测试环境：上传服务 site（token siteToken），账号级 environment release
// （ref 策略 refs/tags/v*，secret SITE_TOKEN）。
type uploadEnv struct {
	*pubEnv
	site  *fakeupload.Server
	envID int64
}

func newUploadEnv(t *testing.T) *uploadEnv {
	t.Helper()
	e := &uploadEnv{pubEnv: newPubEnv(t), site: fakeupload.New(siteToken)}
	t.Cleanup(e.site.Close)
	e.w.uploadOpts = []upload.Option{upload.WithRetry(3, 0)}
	if err := e.db.QueryRow(t.Context(), `INSERT INTO environments (owner_id, name, ref_policy) VALUES ($1, $2, '["refs/tags/v*"]') RETURNING id`,
		ownerID, releaseEnv).Scan(&e.envID); err != nil {
		t.Fatal(err)
	}
	e.setSecret(store.ScopeEnvironment, e.envID, "SITE_TOKEN", siteToken)
	return e
}

// uploadRelease 返回发布 build.dist 到 GitHub Release 和上传服务 site 的配置（新写法 release.assets）。
func (e *uploadEnv) uploadRelease(opts ...func(*workflow.Release)) workflow.Release {
	rel := workflow.Release{
		Assets:  []workflow.Asset{{From: "build.dist", Archive: "tar.gz"}},
		GitHub:  &workflow.ReleaseGitHub{},
		Uploads: []workflow.UploadTarget{{Name: "site", URL: e.site.URL, Token: "${{ secrets.SITE_TOKEN }}"}},
	}
	for _, f := range opts {
		f(&rel)
	}
	return rel
}

// uploadJob 插入引用 environment release 和 secret SITE_TOKEN 的发布 job。
func (e *uploadEnv) uploadJob(r *testRun, rel workflow.Release) int64 {
	e.t.Helper()
	return e.publishJob(r, rel, func(s *workflow.Spec) { s.Environment, s.Secrets = releaseEnv, []string{"SITE_TOKEN"} })
}

// targetRow 是 release_targets 中的一行。
type targetRow struct {
	Kind, Name, Target, State, Error string
	Detail                           map[string]any
}

func (e *pubEnv) targets(jobID int64) []targetRow {
	e.t.Helper()
	rows, err := e.db.Query(e.t.Context(), `SELECT kind, name, target, state, coalesce(error, ''), detail FROM release_targets WHERE job_id=$1 ORDER BY seq`, jobID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []targetRow
	for rows.Next() {
		var r targetRow
		var detail []byte
		if err := rows.Scan(&r.Kind, &r.Name, &r.Target, &r.State, &r.Error, &detail); err != nil {
			e.t.Fatal(err)
		}
		if err := json.Unmarshal(detail, &r.Detail); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// wantTargetStates 检查各目标的状态（按提交顺序）。
func (e *pubEnv) wantTargetStates(jobID int64, want ...string) []targetRow {
	e.t.Helper()
	rows := e.targets(jobID)
	var got []string
	for _, r := range rows {
		got = append(got, r.Kind+":"+r.State)
	}
	if !slices.Equal(got, want) {
		e.t.Fatalf("发布目标的状态 %v，期望 %v\n%+v\n日志：\n%s", got, want, rows, e.log(jobID))
	}
	return rows
}

// countRequests 返回上传服务收到的、方法为 method 的请求数。
func (e *uploadEnv) countRequests(method string) int {
	n := 0
	for _, r := range e.site.Requests() {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

// wantSameFiles 检查上传服务发布的 tag 与 GitHub Release 的 asset 是同一批文件、同样的字节。
func (e *uploadEnv) wantSameFiles(rel fakegithub.Release, tag string) {
	e.t.Helper()
	got := e.site.Published(tag)
	names := slices.Sorted(maps.Keys(got))
	if !slices.Equal(names, assetNames(rel)) {
		e.t.Fatalf("上传服务发布的文件 %v，GitHub Release 的 asset %v，应相同", names, assetNames(rel))
	}
	for name, data := range got {
		if !bytes.Equal(data, e.fake.AssetData(rel.ID, name)) {
			e.t.Fatalf("%s 在两个目标上的内容不同", name)
		}
	}
}

// wantHeaders 检查上传服务收到的每个请求都带 Bearer token 和 kiln 的 User-Agent。
func (e *uploadEnv) wantHeaders() {
	e.t.Helper()
	for _, h := range e.site.AuthorizationHeaders() {
		if h != "Bearer "+siteToken {
			e.t.Fatalf("Authorization = %q", h)
		}
	}
	for _, ua := range e.site.UserAgents() {
		if !strings.HasPrefix(ua, "kiln/") {
			e.t.Fatalf("User-Agent = %q", ua)
		}
	}
}

// 同时发布到 GitHub Release 和上传服务：两边是同一份字节（含 SHA256SUMS）；先提交上传、最后正式发布 Release；
// 每个目标的结果记在 release_targets 中；重跑时识别出都已发布，不重复上传和提交。
func TestWorker_publishUploads(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.uploadJob(r, e.uploadRelease())
	e.publish()

	e.wantJob(id, store.JobSucceeded, "")
	rel := e.oneRelease(srcRepo)
	if rel.Draft {
		t.Fatal("Release 应正式发布")
	}
	e.wantSameFiles(rel, "v1.2.0")
	if e.site.Latest() != "v1.2.0" {
		t.Fatalf("正式版本应设为 latest：%q", e.site.Latest())
	}
	if commit, runID, pre := e.site.PublishInfo("v1.2.0"); commit != r.sha || runID != r.id || pre {
		t.Fatalf("提交的 commit、run_id、prerelease：%s %d %v", commit, runID, pre)
	}
	rows := e.wantTargetStates(id, "upload:committed", "github:committed")
	if rows[0].Name != "site" || rows[0].Target != e.site.URL || rows[0].Detail["latest"] != true || rows[0].Detail["assets"] == nil {
		t.Fatalf("上传目标的记录：%+v", rows[0])
	}
	if rows[1].Target != srcRepo || rows[1].Detail["html_url"] != rel.HTMLURL {
		t.Fatalf("GitHub 目标的记录：%+v", rows[1])
	}
	if row := e.releaseRow(id); row.Stage != stageDone || row.Target != srcRepo+", site" {
		t.Fatalf("发布进度：%+v", row)
	}
	l := e.log(id)
	if strings.Index(l, "已发布到 site") > strings.Index(l, "已发布 "+rel.HTMLURL) {
		t.Fatalf("应先提交上传、最后正式发布 Release：\n%s", l)
	}
	if strings.Contains(l, siteToken) {
		t.Fatal("日志中出现了 token")
	}
	e.wantHeaders()
	var audits int
	if err := e.db.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor=$1 AND action IN ('publish.credentials', 'publish.upload')`, auditActor).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("应审计凭据的使用和上传的发布：%d %v", audits, err)
	}
	puts, posts := e.countRequests(http.MethodPut), e.countRequests(http.MethodPost)
	if puts != 3 || posts != 1 {
		t.Fatalf("应上传 3 个文件、提交 1 次：PUT %d POST %d", puts, posts)
	}

	// 重跑：两个目标都已发布，不再上传和提交
	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	rows = e.wantTargetStates(again, "upload:committed", "github:committed")
	if rows[0].Detail["previous"] != true || !strings.Contains(e.log(again), "已经发布过 v1.2.0，内容一致，跳过") {
		t.Fatalf("重跑应识别出已经发布：%+v\n%s", rows[0], e.log(again))
	}
	if e.countRequests(http.MethodPut) != puts || e.countRequests(http.MethodPost) != posts {
		t.Fatal("重跑不应再上传或提交")
	}
}

// 上传目标暂存失败（token 错误）：所有目标都不提交，GitHub Release 停在 draft；错误信息不含 token。
// 改正 secret 后重跑：GitHub 找回 draft 继续，上传补完，两边都发布。
func TestWorker_publishUploads_stageFailure(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	const wrong = "lc_rel_wr0ng-token-value"
	e.setSecret(store.ScopeEnvironment, e.envID, "SITE_TOKEN", wrong)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.uploadJob(r, e.uploadRelease())
	e.publish()

	e.wantJob(id, store.JobFailed, "上传目标 site")
	if rel := e.oneRelease(srcRepo); !rel.Draft || len(rel.Assets) != 3 {
		t.Fatalf("上传失败时 GitHub Release 应停在 draft（已上传 asset）：draft=%v %v", rel.Draft, assetNames(rel))
	}
	if e.site.State("v1.2.0") != "" || e.countRequests(http.MethodPost) != 0 {
		t.Fatal("上传服务不应有任何暂存或提交")
	}
	if n := e.countRequests(http.MethodGet); n != 1 {
		t.Fatalf("401 不应重试：GET %d 次", n)
	}
	rows := e.wantTargetStates(id, "upload:failed", "github:staged")
	_, reason := e.jobResult(id)
	if !strings.Contains(rows[0].Error, "HTTP 401") || strings.Contains(reason+rows[0].Error+e.log(id)+e.releaseRow(id).Error, wrong) {
		t.Fatalf("错误应说明 401 且不含 token：%q / %q", rows[0].Error, reason)
	}

	e.setSecret(store.ScopeEnvironment, e.envID, "SITE_TOKEN", siteToken)
	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	e.wantTargetStates(again, "upload:committed", "github:committed")
	if rel := e.oneRelease(srcRepo); rel.Draft {
		t.Fatal("重跑后 Release 应正式发布")
	}
	if e.site.State("v1.2.0") != upload.StatePublished {
		t.Fatal("重跑后上传服务应已发布")
	}
}

// GitHub Release 提交失败，而上传目标已经提交：发布 job 失败；重跑时上传目标识别出已发布（不再提交），只补 GitHub。
func TestWorker_publishUploads_commitFailure(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.uploadJob(r, e.uploadRelease())
	e.faults.failOnce(func(req *http.Request) bool {
		return req.Method == http.MethodPatch && strings.Contains(req.URL.Path, "/releases/")
	})
	e.publish()

	e.wantJob(id, store.JobFailed, "发布 Release")
	e.wantTargetStates(id, "upload:committed", "github:failed")
	if e.site.State("v1.2.0") != upload.StatePublished || !e.oneRelease(srcRepo).Draft {
		t.Fatal("上传目标应已发布，Release 仍是 draft")
	}
	posts := e.countRequests(http.MethodPost)

	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	e.wantTargetStates(again, "upload:committed", "github:committed")
	if e.oneRelease(srcRepo).Draft {
		t.Fatal("重跑后 Release 应正式发布")
	}
	if e.countRequests(http.MethodPost) != posts || e.countRequests(http.MethodPut) != 3 {
		t.Fatal("重跑不应再上传或提交到上传服务")
	}
}

// 上传服务上该 tag 已经以不同内容发布：阶段 A 失败，不提交任何目标（GitHub 停在 draft）。
func TestWorker_publishUploads_publishedConflict(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	e.site.Preset("v1.2.0", map[string][]byte{"other.bin": []byte("x")})
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.uploadJob(r, e.uploadRelease())
	e.publish()

	e.wantJob(id, store.JobFailed, "的 v1.2.0 已经以不同的内容发布")
	e.wantTargetStates(id, "upload:failed", "github:staged")
	if !e.oneRelease(srcRepo).Draft || e.countRequests(http.MethodPut) != 0 {
		t.Fatal("冲突时不应上传，也不应正式发布 Release")
	}
}

// 中断后继续：已暂存的文件跳过；5xx 按退避重试后成功。只发布到上传服务（没有 GitHub）时不创建 Release。
func TestWorker_publishUploads_resumeAndRetry(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	// 第一次：第一个文件暂存后，第二个文件的 PUT 持续 503，重试用完后失败
	id := e.uploadJob(r, e.uploadRelease(func(rel *workflow.Release) { rel.GitHub = nil }))
	e.site.FailAfter(http.MethodPut, 1, http.StatusServiceUnavailable, 3)
	e.publish()
	e.wantJob(id, store.JobFailed, "HTTP 503")
	e.wantTargetStates(id, "upload:failed")
	if staged := e.site.Staged("v1.2.0"); len(staged) != 1 {
		t.Fatalf("失败前应已暂存 1 个文件：%v", staged)
	}
	puts := e.countRequests(http.MethodPut)

	// 重跑：先查询，已暂存的跳过；503 两次后成功
	again := e.rerun(r)
	e.site.FailNext(http.MethodPut, http.StatusServiceUnavailable, 2)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	e.wantTargetStates(again, "upload:committed")
	if !strings.Contains(e.log(again), "已暂存且一致，跳过") {
		t.Fatalf("应跳过已暂存的文件：\n%s", e.log(again))
	}
	// 剩下 2 个文件，其中一个多试了 2 次
	if n := e.countRequests(http.MethodPut) - puts; n != 4 {
		t.Fatalf("重跑应发送 4 次 PUT（2 个文件 + 2 次重试），实际 %d", n)
	}
	if len(e.releases(srcRepo)) != 0 {
		t.Fatal("没有 release.github 时不应创建 Release")
	}
}

// latest: auto 只在正式版本、且不低于向同一地址发布过的所有正式版本时为 true；预发布版本的 prerelease 为 true。
func TestWorker_publishUploads_latest(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	release := func(tag string, opts ...func(*workflow.Release)) int64 {
		t.Helper()
		r := e.tagRun(tag)
		e.builds(r)
		id := e.uploadJob(r, e.uploadRelease(append([]func(*workflow.Release){func(rel *workflow.Release) { rel.GitHub = nil }}, opts...)...))
		e.publish()
		e.wantJob(id, store.JobSucceeded, "")
		return id
	}
	release("v1.2.0")
	if e.site.Latest() != "v1.2.0" {
		t.Fatalf("latest = %q", e.site.Latest())
	}
	release("v1.1.5")
	release("v1.3.0-rc.1")
	if e.site.Latest() != "v1.2.0" {
		t.Fatalf("旧版本的补丁和预发布版本不应成为 latest：%q", e.site.Latest())
	}
	if _, _, pre := e.site.PublishInfo("v1.3.0-rc.1"); !pre {
		t.Fatal("预发布版本的 prerelease 应为 true")
	}
	release("v1.3.0")
	if e.site.Latest() != "v1.3.0" {
		t.Fatalf("更高的正式版本应成为 latest：%q", e.site.Latest())
	}
	release("v0.9.0", func(rel *workflow.Release) { rel.Uploads[0].Latest = workflow.ChoiceTrue })
	if e.site.Latest() != "v0.9.0" {
		t.Fatalf("latest: true 时总是设为 latest：%q", e.site.Latest())
	}
}

// environment 不允许、secret 没有配置时：在访问上传服务之前失败。
func TestWorker_publishUploads_credentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		setup      func(e *uploadEnv)
	}{
		{"ref 策略不允许", "ref 策略不允许 refs/tags/v1.2.0", func(e *uploadEnv) { e.exec(`UPDATE environments SET ref_policy='["refs/tags/release-*"]'`) }},
		{"secret 没有配置", "secret SITE_TOKEN 没有配置", func(e *uploadEnv) { e.exec(`DELETE FROM secrets`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newUploadEnv(t)
			tc.setup(e)
			r := e.tagRun("v1.2.0")
			e.builds(r)
			id := e.uploadJob(r, e.uploadRelease())
			e.publish()
			e.wantJob(id, store.JobFailed, tc.want)
			if n := len(e.site.Requests()); n != 0 {
				t.Fatalf("凭据检查失败时不应访问上传服务：%d 个请求", n)
			}
		})
	}
}

// 服务端的清单中有重复的名字时，不能用重复项凑够数量（以前 [a, a] 与 [a, b] 被当成相同）；没有返回 sha256 时按不同处理。
func TestUploadedDiff(t *testing.T) {
	a := release.Asset{Name: "a", Size: 1, SHA256: "aa"}
	b := release.Asset{Name: "b", Size: 2, SHA256: "bb"}
	ua := upload.Asset{Name: "a", Size: 1, SHA256: "aa"}
	ub := upload.Asset{Name: "b", Size: 2, SHA256: "bb"}
	for _, tc := range []struct {
		name      string
		published []upload.Asset
		assets    []release.Asset
		want      bool
	}{
		{"相同（顺序不同）", []upload.Asset{ub, ua}, []release.Asset{a, b}, true},
		{"数量不同", []upload.Asset{ua}, []release.Asset{a, b}, false},
		{"摘要不同", []upload.Asset{ua, {Name: "b", Size: 2, SHA256: "xx"}}, []release.Asset{a, b}, false},
		{"重复的名字", []upload.Asset{ua, ua}, []release.Asset{a, b}, false},
		{"没有返回 sha256", []upload.Asset{ua, {Name: "b", Size: 2}}, []release.Asset{a, b}, false},
		{"都为空", nil, nil, true},
	} {
		if got := uploadedDiff(tc.published, tc.assets).same(); got != tc.want {
			t.Errorf("%s：same = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}
