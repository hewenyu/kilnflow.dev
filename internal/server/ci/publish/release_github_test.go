package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestSplitFrom(t *testing.T) {
	for _, tc := range []struct{ in, job, output string }{
		{"build.dist", "build", "dist"},
		{"build.v2.dist", "build.v2", "dist"},
		{"build", "build", ""},
	} {
		job, output := splitFrom(tc.in)
		if job != tc.job || output != tc.output {
			t.Errorf("splitFrom(%q) = %q, %q，期望 %q, %q", tc.in, job, output, tc.job, tc.output)
		}
	}
}

func TestAssetName(t *testing.T) {
	up := &cistore.Job{Matrix: map[string]any{"os": "linux", "arch": "amd64"}}
	run := &cistore.Run{GitHubCtx: cistore.GitHubCtx{RefName: "v1.2.3"}}
	for _, tc := range []struct {
		name string
		a    workflow.Asset
		want string
	}{
		{"默认按矩阵变量名排序", workflow.Asset{}, "dist-amd64-linux"},
		{"引用 matrix 和 github", workflow.Asset{Name: "kiln-${{ github.ref_name }}-${{ matrix.os }}"}, "kiln-v1.2.3-linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := assetName(tc.a, "dist", up, run)
			if err != nil || got != tc.want {
				t.Fatalf("assetName = %q, %v，期望 %q", got, err, tc.want)
			}
		})
	}
	// 没有矩阵时只用输出名
	if got, err := assetName(workflow.Asset{}, "dist", &cistore.Job{}, run); err != nil || got != "dist" {
		t.Fatalf("assetName = %q, %v", got, err)
	}
}

// ---------- 发布到 GitHub Release（用 internal/fakegithub） ----------

// builds 在 run 中插入矩阵构建 build（os = linux、darwin），输出 dist 中有可执行文件 kiln 和 README.md。
func (e *pubEnv) builds(r *testRun) {
	e.t.Helper()
	for _, goos := range []string{"linux", "darwin"} {
		e.upstream(r, "build", "dist", map[string]any{"os": goos}, map[string]upstreamFile{
			"kiln":      {content: "kiln-binary-" + goos, exec: true},
			"README.md": {content: "readme"},
		})
	}
}

// ghRelease 返回发布 build.dist 到 GitHub Release 的配置。
func ghRelease(opts ...func(*workflow.ReleaseGitHub)) workflow.Release {
	g := &workflow.ReleaseGitHub{Assets: []workflow.Asset{{From: "build.dist"}}}
	for _, f := range opts {
		f(g)
	}
	return workflow.Release{GitHub: g}
}

// releases 返回模拟 GitHub 上仓库的全部 Release。
func (e *pubEnv) releases(repo string) []fakegithub.Release { return e.fake.Releases(repo) }

// oneRelease 返回仓库上唯一的 Release。
func (e *pubEnv) oneRelease(repo string) fakegithub.Release {
	e.t.Helper()
	rels := e.releases(repo)
	if len(rels) != 1 {
		e.t.Fatalf("%s 上应有 1 个 Release，实际 %d 个", repo, len(rels))
	}
	return rels[0]
}

func assetNames(rel fakegithub.Release) []string {
	var names []string
	for _, a := range rel.Assets {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	return names
}

// marker 是 Release 说明中标识 run 的隐藏标记。
func marker(r *testRun) string { return fmt.Sprintf("<!-- kiln-run:%d -->", r.id) }

// inst 返回访问模拟 GitHub 的 installation 客户端（测试中直接准备 Release 用，不经过故障注入）。
func (e *pubEnv) inst() *gh.Inst {
	c := &gh.Client{AppID: e.fake.AppID, Key: testRSAKey(e.t), APIURL: e.fake.URL, UploadURL: e.fake.URL}
	return c.Inst(e.fake.InstallationID)
}

// tarEntry 是 tar.gz 中的一个文件。
type tarEntry struct {
	content string
	mode    int64
	mtime   time.Time
}

func untar(t *testing.T, data []byte) map[string]tarEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("不是 gzip：%v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string]tarEntry{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = tarEntry{content: string(b), mode: h.Mode, mtime: h.ModTime}
	}
}

// checkChecksums 检查校验和文件覆盖除它之外的全部 asset，并与 asset 的内容一致。
func (e *pubEnv) checkChecksums(rel fakegithub.Release, name string) {
	e.t.Helper()
	sums := map[string]string{}
	for l := range strings.SplitSeq(strings.TrimSpace(string(e.fake.AssetData(rel.ID, name))), "\n") {
		sum, file, ok := strings.Cut(l, "  ")
		if !ok {
			e.t.Fatalf("%s 的行格式不对：%q", name, l)
		}
		sums[file] = sum
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			continue
		}
		got := sha256.Sum256(e.fake.AssetData(rel.ID, a.Name))
		if sums[a.Name] != hex.EncodeToString(got[:]) {
			e.t.Fatalf("%s 中 %s 的校验和 %q 与内容不符", name, a.Name, sums[a.Name])
		}
		delete(sums, a.Name)
	}
	if len(sums) != 0 {
		e.t.Fatalf("%s 中有多余的条目：%v", name, sums)
	}
}

// 完整的发布：打包矩阵的各变体、生成 SHA256SUMS、先 draft 再正式发布；重跑时识别出本 run 已经发布过，不重复操作。
func TestWorker_publishGitHubRelease(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())
	e.publish()

	e.wantJob(id, store.JobSucceeded, "")
	if s := e.runState(r); s != string(store.RunSucceeded) {
		t.Fatalf("run 状态 %s，期望 succeeded", s)
	}
	rel := e.oneRelease(srcRepo)
	if rel.Draft || rel.Prerelease || rel.TagName != "v1.2.0" || rel.TargetCommitish != r.sha || rel.MakeLatest != "true" {
		t.Fatalf("Release 状态不对：draft=%v prerelease=%v tag=%s target=%s latest=%q", rel.Draft, rel.Prerelease, rel.TagName, rel.TargetCommitish, rel.MakeLatest)
	}
	if !strings.Contains(rel.Body, marker(r)) || !strings.Contains(rel.Body, "generated") {
		t.Fatalf("说明应由 GitHub 生成并带 run 标记：%q", rel.Body)
	}
	if got, want := assetNames(rel), []string{"SHA256SUMS", "dist-darwin.tar.gz", "dist-linux.tar.gz"}; !slices.Equal(got, want) {
		t.Fatalf("asset %v，期望 %v", got, want)
	}
	e.checkChecksums(rel, "SHA256SUMS")
	files := untar(t, e.fake.AssetData(rel.ID, "dist-linux.tar.gz"))
	commitTime := time.Unix(mustInt(t, git(t, e.dir, "show", "-s", "--format=%ct", r.sha)), 0)
	if k := files["kiln"]; k.content != "kiln-binary-linux" || k.mode&0o111 == 0 || !k.mtime.Equal(commitTime) {
		t.Fatalf("包内 kiln：%+v，期望内容来自 linux 变体、可执行、mtime 为 commit 时间 %v", k, commitTime)
	}
	if rd := files["README.md"]; rd.mode&0o111 != 0 {
		t.Fatalf("普通文件不应有可执行位：%o", rd.mode)
	}
	row := e.releaseRow(id)
	if row.State != "succeeded" || row.Stage != stageDone || row.ReleaseID == nil || *row.ReleaseID != rel.ID ||
		row.Target != srcRepo || row.Progress["html_url"] != rel.HTMLURL {
		t.Fatalf("发布进度：%+v", row)
	}
	if l := e.log(id); !strings.Contains(l, "创建 draft Release") || !strings.Contains(l, "发布完成") {
		t.Fatalf("日志：\n%s", l)
	}
	requests := map[string]int{"POST /releases": 1, "POST /assets": 3, "PATCH": 1, "DELETE": 0}
	e.wantReleaseRequests(requests)

	// 重跑整个 run：新的发布 job 按标记找回本 run 的 Release，发现已经正式发布，不新建、不上传、不修改
	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	if l := e.log(again); !strings.Contains(l, "已经由本 run 正式发布") {
		t.Fatalf("重跑应识别出已经发布：\n%s", l)
	}
	e.wantReleaseRequests(requests)
	if len(e.releases(srcRepo)) != 1 {
		t.Fatal("重跑不应新建 Release")
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantReleaseRequests 按类别检查模拟 GitHub 收到的 Release 请求数。
func (e *pubEnv) wantReleaseRequests(want map[string]int) {
	e.t.Helper()
	got := map[string]int{}
	for _, r := range e.fake.Requests() {
		method, path, _ := strings.Cut(r, " ")
		switch {
		case method == http.MethodPost && strings.HasSuffix(path, "/releases"):
			got["POST /releases"]++
		case method == http.MethodPost && strings.HasSuffix(path, "/assets"):
			got["POST /assets"]++
		case method == http.MethodPatch && strings.Contains(path, "/releases/"):
			got["PATCH"]++
		case method == http.MethodDelete && strings.Contains(path, "/releases/"):
			got["DELETE"]++
		}
	}
	for k, n := range want {
		if got[k] != n {
			e.t.Errorf("模拟 GitHub 收到 %s 请求 %d 次，期望 %d 次", k, got[k], n)
		}
	}
}

// 发布中途 Server 退出（上传第二个 asset 时）：发布 job 保持 running，进度停在暂存阶段；
// 重启后 ResetClaims 让它继续执行，按保存的 Release ID 找回 draft，已上传的 asset 跳过，只补传剩下的。
func TestWorker_publishGitHubRelease_resumeAfterRestart(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())

	ctx := e.ctxCanceledOn(isUpload("dist-linux.tar.gz"))
	if e.w.publishOnce(ctx) {
		t.Fatal("Server 退出时 publishOnce 应返回 false")
	}
	if state, _ := e.jobResult(id); state != store.JobRunning {
		t.Fatalf("中断的发布 job 应保持 running：%s", state)
	}
	row := e.releaseRow(id)
	if row.State != "running" || row.Stage != stageStaging || row.ReleaseID == nil {
		t.Fatalf("进度应停在暂存阶段并已保存 Release ID：%+v", row)
	}
	if rel := e.oneRelease(srcRepo); !rel.Draft || len(rel.Assets) != 1 {
		t.Fatalf("中断时应只有一个 draft 和一个已上传的 asset：draft=%v assets=%v", rel.Draft, assetNames(rel))
	}

	// 重启：还没有清除执行标记时不会重复领取
	e.faults.set(nil)
	if e.w.publishOnce(t.Context()) {
		t.Fatal("执行标记还在时不应再次领取")
	}
	if err := e.w.ResetClaims(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.publish()
	e.wantJob(id, store.JobSucceeded, "")
	rel := e.oneRelease(srcRepo)
	if rel.Draft || len(rel.Assets) != 3 {
		t.Fatalf("继续后应正式发布并有 3 个 asset：draft=%v assets=%v", rel.Draft, assetNames(rel))
	}
	e.checkChecksums(rel, "SHA256SUMS")
	if l := e.log(id); !strings.Contains(l, "asset dist-darwin.tar.gz 已存在且一致，跳过") {
		t.Fatalf("应跳过已上传的 asset：\n%s", l)
	}
	e.wantReleaseRequests(map[string]int{"POST /releases": 1, "POST /assets": 3, "PATCH": 1, "DELETE": 0})
}

// 上传失败：发布 job 失败、run 失败，draft 留在 GitHub 上（用户看不到）；重跑后按标记找回 draft 继续，不重复创建。
func TestWorker_publishGitHubRelease_retryAfterFailure(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())
	e.faults.failOnce(isUpload("dist-linux.tar.gz"))
	e.publish()

	e.wantJob(id, store.JobFailed, "上传 dist-linux.tar.gz")
	if s := e.runState(r); s != string(store.RunFailed) {
		t.Fatalf("发布失败后 run 应为 failed：%s", s)
	}
	row := e.releaseRow(id)
	if row.State != "failed" || row.Stage != stageStaging || !strings.Contains(row.Error, "injected failure") {
		t.Fatalf("发布进度应记录失败：%+v", row)
	}
	if rel := e.oneRelease(srcRepo); !rel.Draft {
		t.Fatal("失败时 Release 应仍是 draft")
	}
	if l := e.log(id); !strings.Contains(l, "发布失败") {
		t.Fatalf("日志应记录失败：\n%s", l)
	}

	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	if l := e.log(again); !strings.Contains(l, "找回本 run 的 Release") || !strings.Contains(l, "已存在且一致，跳过") {
		t.Fatalf("重跑应找回 draft 并跳过已上传的 asset：\n%s", l)
	}
	rel := e.oneRelease(srcRepo)
	if rel.Draft || len(rel.Assets) != 3 {
		t.Fatalf("重跑后应正式发布并有 3 个 asset：draft=%v assets=%v", rel.Draft, assetNames(rel))
	}
	if row := e.releaseRow(again); row.ReleaseID == nil || *row.ReleaseID != rel.ID {
		t.Fatalf("新的发布 job 应记录找回的 Release ID：%+v", row)
	}
	if s := e.runState(r); s != string(store.RunSucceeded) {
		t.Fatalf("重跑成功后 run 应为 succeeded：%s", s)
	}
}

// 正式发布后强制重跑，本次的 asset 与已发布的不同（不确定的构建、asset 被改动等）：已发布的 Release 不覆盖，
// 也不能静默按成功处理，发布失败并说明哪些文件不同。内容一致时的重跑见 TestWorker_publishGitHubRelease。
func TestWorker_publishGitHubRelease_publishedConflict(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())
	e.publish()
	e.wantJob(id, store.JobSucceeded, "")
	rel := e.oneRelease(srcRepo)

	// 已发布的 Release 上：dist-linux.tar.gz 换成别的内容，多出一个 notes.txt
	for _, a := range rel.Assets {
		if a.Name == "dist-linux.tar.gz" {
			if err := e.inst().DeleteReleaseAsset(t.Context(), srcRepo, a.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, data := range map[string]string{"dist-linux.tar.gz": "other build", "notes.txt": "x"} {
		if _, err := e.inst().UploadReleaseAsset(t.Context(), srcRepo, rel.ID, name, strings.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	before := e.fake.Requests()

	again := e.rerun(r)
	e.publish()
	e.wantJob(again, store.JobFailed, "已经以不同的内容发布")
	l := e.log(again)
	for _, want := range []string{"不同：dist-linux.tar.gz", "多出：notes.txt", "请发布新的版本"} {
		if !strings.Contains(l, want) {
			t.Errorf("日志缺少 %q：\n%s", want, l)
		}
	}
	for _, req := range e.fake.Requests()[len(before):] {
		if method, _, _ := strings.Cut(req, " "); method != http.MethodGet {
			t.Errorf("已发布的 Release 不应被改动：%s", req)
		}
	}
}

// draft 中已有同名但内容不一致的 asset（例如上一次上传到一半）：先删除再重新上传。
func TestWorker_publishGitHubRelease_replacesMismatchedAsset(t *testing.T) {
	t.Parallel()
	e := newPubEnv(t)
	r := e.tagRun("v1.2.0")
	e.builds(r)
	id := e.publishJob(r, ghRelease())
	draft := true
	pre, err := e.inst().CreateRelease(t.Context(), srcRepo, &gh.Release{TagName: "v1.2.0", Body: marker(r), Draft: &draft})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst().UploadReleaseAsset(t.Context(), srcRepo, pre.ID, "dist-linux.tar.gz", strings.NewReader("partial"), 7); err != nil {
		t.Fatal(err)
	}
	e.publish()

	e.wantJob(id, store.JobSucceeded, "")
	rel := e.oneRelease(srcRepo)
	if rel.ID != pre.ID || len(rel.Assets) != 3 {
		t.Fatalf("应沿用已有的 draft #%d 并有 3 个 asset：#%d %v", pre.ID, rel.ID, assetNames(rel))
	}
	e.checkChecksums(rel, "SHA256SUMS")
	e.wantReleaseRequests(map[string]int{"POST /releases": 1, "DELETE": 1})
}

// release.github 的各个选项：prerelease、latest、notes.from、checksums、archive、asset 名。
func TestWorker_publishGitHubRelease_options(t *testing.T) {
	t.Parallel()
	type check func(t *testing.T, e *pubEnv, r *testRun, rel fakegithub.Release)
	tests := []struct {
		name  string
		tag   string
		g     func(*workflow.ReleaseGitHub)
		setup func(e *pubEnv)
		check check
	}{
		{
			name: "预发布 tag 自动标为预发布、不是 latest", tag: "v1.3.0-rc.1",
			check: func(t *testing.T, _ *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if !rel.Prerelease || rel.MakeLatest != "false" {
					t.Fatalf("prerelease=%v latest=%q", rel.Prerelease, rel.MakeLatest)
				}
			},
		},
		{
			name: "prerelease: true", tag: "v1.2.0", g: func(g *workflow.ReleaseGitHub) { g.Prerelease = "true" },
			check: func(t *testing.T, _ *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if !rel.Prerelease || rel.MakeLatest != "false" {
					t.Fatalf("prerelease=%v latest=%q", rel.Prerelease, rel.MakeLatest)
				}
			},
		},
		{
			name: "已有更高的正式版本时不是 latest", tag: "v1.2.0", setup: publishedRelease("v2.0.0"),
			check: func(t *testing.T, _ *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if rel.Prerelease || rel.MakeLatest != "false" {
					t.Fatalf("prerelease=%v latest=%q", rel.Prerelease, rel.MakeLatest)
				}
			},
		},
		{
			name: "latest: true 强制设为 latest", tag: "v1.2.0", setup: publishedRelease("v2.0.0"),
			g: func(g *workflow.ReleaseGitHub) { g.Latest = "true" },
			check: func(t *testing.T, _ *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if rel.MakeLatest != "true" {
					t.Fatalf("latest=%q", rel.MakeLatest)
				}
			},
		},
		{
			name: "notes.from 文件取对应版本的段落", tag: "v1.2.0", g: func(g *workflow.ReleaseGitHub) { g.NotesFrom = "CHANGELOG.md" },
			check: func(t *testing.T, _ *pubEnv, r *testRun, rel fakegithub.Release) {
				t.Helper()
				if !strings.Contains(rel.Body, "- 新功能") || strings.Contains(rel.Body, "旧的修复") || !strings.Contains(rel.Body, marker(r)) {
					t.Fatalf("说明：%q", rel.Body)
				}
			},
		},
		{
			name: "notes.from: draft 保持 draft", tag: "v1.2.0", g: func(g *workflow.ReleaseGitHub) { g.NotesFrom = "draft" },
			check: func(t *testing.T, e *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if !rel.Draft || len(rel.Assets) != 3 {
					t.Fatalf("应保持 draft 并上传全部 asset：draft=%v %v", rel.Draft, assetNames(rel))
				}
				e.wantReleaseRequests(map[string]int{"PATCH": 0})
			},
		},
		{
			name: "zip 且不生成校验和", tag: "v1.2.0",
			g: func(g *workflow.ReleaseGitHub) { g.Checksums, g.Assets[0].Archive = workflow.ChecksumsNone, "zip" },
			check: func(t *testing.T, _ *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				if got := assetNames(rel); !slices.Equal(got, []string{"dist-darwin.zip", "dist-linux.zip"}) {
					t.Fatalf("asset %v", got)
				}
			},
		},
		{
			name: "自定义 asset 名和校验和文件名", tag: "v1.2.0",
			g: func(g *workflow.ReleaseGitHub) {
				g.Checksums, g.Assets[0].Name = "SUMS.txt", "kiln-${{ github.ref_name }}-${{ matrix.os }}"
			},
			check: func(t *testing.T, e *pubEnv, _ *testRun, rel fakegithub.Release) {
				t.Helper()
				want := []string{"SUMS.txt", "kiln-v1.2.0-darwin.tar.gz", "kiln-v1.2.0-linux.tar.gz"}
				if got := assetNames(rel); !slices.Equal(got, want) {
					t.Fatalf("asset %v，期望 %v", got, want)
				}
				e.checkChecksums(rel, "SUMS.txt")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPubEnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			r := e.tagRun(tc.tag)
			e.builds(r)
			opts := []func(*workflow.ReleaseGitHub){}
			if tc.g != nil {
				opts = append(opts, tc.g)
			}
			id := e.publishJob(r, ghRelease(opts...))
			e.publish()
			e.wantJob(id, store.JobSucceeded, "")
			var rel fakegithub.Release
			for _, x := range e.releases(srcRepo) {
				if x.TagName == tc.tag {
					rel = x
				}
			}
			tc.check(t, e, r, rel)
		})
	}
}

// publishedRelease 在源仓库上准备一个其他 tag 的正式 Release。
func publishedRelease(tag string) func(e *pubEnv) {
	return func(e *pubEnv) {
		e.t.Helper()
		draft := false
		if _, err := e.inst().CreateRelease(e.t.Context(), srcRepo, &gh.Release{TagName: tag, Draft: &draft}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// 跨仓库发布（PLAN #13.6）：从私有源码仓库发布到同账号的公开仓库。Release 建在目标仓库上，tag 指向目标仓库的分支；
// 发布到公开仓库时检查 asset 和说明中没有源仓库名。
func TestWorker_publishGitHubRelease_crossRepository(t *testing.T) {
	t.Parallel()
	leak := "# Changelog\n\n## v1.2.0\n\n- 见 tester/myapp-src#12\n"
	tests := []struct {
		name    string
		opts    []func(*envOpts)
		g       func(*workflow.ReleaseGitHub)
		leakBin bool   // 构建产物中含源仓库名
		wantErr string // 空表示应成功
		target  string // 成功时 Release 的 target_commitish
	}{
		{name: "发布到公开仓库", target: "main"},
		{name: "指定目标分支", g: func(g *workflow.ReleaseGitHub) { g.Target = "release" }, target: "release"},
		{name: "说明中有源仓库名", opts: []func(*envOpts){withFiles(map[string]string{"CHANGELOG.md": leak})}, wantErr: "Release 说明"},
		{name: "asset 中有源仓库名", leakBin: true, wantErr: "tester/myapp-src"},
		{
			name: "允许出现源仓库名", opts: []func(*envOpts){withFiles(map[string]string{"CHANGELOG.md": leak})},
			g: func(g *workflow.ReleaseGitHub) { g.AllowSourceName = true }, target: "main",
		},
		{name: "目标是私有仓库时不检查", leakBin: true, opts: []func(*envOpts){func(o *envOpts) { o.targetPrivate = true }}, target: "main"},
		{name: "跨仓库必须写 notes.from", g: func(g *workflow.ReleaseGitHub) { g.NotesFrom = "" }, wantErr: "跨仓库发布必须显式填写 notes.from"},
		{name: "目标仓库不在安装范围内", opts: []func(*envOpts){func(o *envOpts) { o.noTargetInKiln = true }}, wantErr: "不在 App 的安装范围内"},
		{name: "目标仓库属于另一个 installation", opts: []func(*envOpts){func(o *envOpts) { o.targetOtherInstallation = true }}, wantErr: "不在同一个 installation 下"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPubEnv(t, tc.opts...)
			// 跨仓库发布必须声明 environment，且目标仓库要在它的允许发布仓库列表中（规划时检查，发布时复核）
			if _, err := e.db.Exec(e.t.Context(), `INSERT INTO environments (owner_id, repo_id, name, ref_policy, allowed_publish_repos)
				VALUES ($1, $2, 'release', '["refs/tags/v*"]', $3)`, ownerID, srcRepoID, []string{targetRepo}); err != nil {
				e.t.Fatal(err)
			}
			r := e.tagRun("v1.2.0")
			content := "kiln-binary"
			if tc.leakBin {
				content = "built from github.com/tester/myapp-src"
			}
			e.upstream(r, "build", "dist", nil, map[string]upstreamFile{"kiln": {content: content, exec: true}})
			id := e.publishJob(r, ghRelease(func(g *workflow.ReleaseGitHub) {
				g.Repository, g.NotesFrom = targetRepo, "CHANGELOG.md"
				if tc.g != nil {
					tc.g(g)
				}
			}), func(s *workflow.Spec) { s.Environment = "release" })
			e.publish()
			if tc.wantErr != "" {
				e.wantJob(id, store.JobFailed, tc.wantErr)
				if n := len(e.releases(targetRepo)); n != 0 {
					t.Fatalf("失败时目标仓库上不应有 Release，实际 %d 个", n)
				}
				return
			}
			e.wantJob(id, store.JobSucceeded, "")
			if n := len(e.releases(srcRepo)); n != 0 {
				t.Fatalf("源仓库上不应有 Release，实际 %d 个", n)
			}
			rel := e.oneRelease(targetRepo)
			if rel.Draft || rel.TagName != "v1.2.0" || rel.TargetCommitish != tc.target || !strings.Contains(rel.Body, marker(r)) {
				t.Fatalf("目标仓库的 Release：draft=%v tag=%s target=%s body=%q", rel.Draft, rel.TagName, rel.TargetCommitish, rel.Body)
			}
			if got := assetNames(rel); !slices.Equal(got, []string{"SHA256SUMS", "dist.tar.gz"}) {
				t.Fatalf("asset %v", got)
			}
			if row := e.releaseRow(id); row.Target != targetRepo {
				t.Fatalf("发布目标应记录为 %s：%q", targetRepo, row.Target)
			}
		})
	}
}

func withFiles(files map[string]string) func(*envOpts) {
	return func(o *envOpts) { o.files = files }
}

// 发布前的检查失败时，发布 job 失败、run 失败，GitHub 上不产生任何 Release。
func TestWorker_publish_failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    []func(*envOpts)
		run     func(e *pubEnv) *testRun
		ups     func(e *pubEnv, r *testRun)
		rel     workflow.Release
		setup   func(e *pubEnv, r *testRun)
		wantErr string
	}{
		{
			name: "上游没有成功", rel: ghRelease(),
			ups: func(e *pubEnv, r *testRun) {
				id := e.upstream(r, "build", "dist", nil, map[string]upstreamFile{"kiln": {content: "x"}})
				e.exec(`UPDATE jobs SET state='failed' WHERE id=$1`, id)
			},
			wantErr: "上游没有成功（failed）",
		},
		{name: "找不到上游", rel: ghRelease(), ups: func(*pubEnv, *testRun) {}, wantErr: "找不到上游 job build"},
		{
			name: "上游没有这个输出", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Assets[0].From = "build.other" }),
			wantErr: "没有名为 other 的输出",
		},
		{
			name: "tag 已经移动", rel: ghRelease(),
			run: func(e *pubEnv) *testRun {
				e.tagRun("v1.2.0")
				return e.addRun("refs/tags/v1.2.0", strings.Repeat("a", 40), 0)
			},
			wantErr: "已经移动",
		},
		{
			name: "不是 tag 触发", rel: ghRelease(),
			run:     func(e *pubEnv) *testRun { return e.addRun("refs/heads/main", git(e.t, e.dir, "rev-parse", "HEAD"), 0) },
			wantErr: "发布只能由 tag push 触发",
		},
		{name: "没有配置 GitHub App", opts: []func(*envOpts){func(o *envOpts) { o.noGitHub = true }}, rel: ghRelease(), wantErr: "没有配置 GitHub App"},
		{name: "没有发布目标", rel: workflow.Release{}, wantErr: "没有发布目标"},
		{
			name: "tag 已有其他 run 正式发布的 Release", rel: ghRelease(),
			setup:   func(e *pubEnv, _ *testRun) { publishedRelease("v1.2.0")(e) },
			wantErr: "已有正式发布的 Release",
		},
		{
			name: "说明文件中没有这个版本", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.NotesFrom = "CHANGELOG.md" }),
			opts:    []func(*envOpts){withFiles(map[string]string{"CHANGELOG.md": "## v0.1.0\n"})},
			wantErr: "没有以 v1.2.0 开头的二级标题",
		},
		{
			name: "asset 文件名重复", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Assets[0].Name = "same" }),
			wantErr: "asset 文件名重复",
		},
		{
			// github.ref 含 /：不能用作文件名，打包前就报错
			name: "asset 名含路径分隔符", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Assets[0].Name = "kiln-${{ github.ref }}-${{ matrix.os }}" }),
			wantErr: "不能含路径分隔符",
		},
		{
			name: "archive: none 要求恰好一个文件", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Assets[0].Archive = "none" }),
			wantErr: "打包",
		},
		{
			// 以前校验和文件按 filepath.Join 写入，名字中的 .. 会把它写到发布工作目录之外
			name: "校验和文件名含路径分隔符", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Checksums = "x/../../../escaped" }),
			wantErr: "校验和文件名",
		},
		{
			name: "校验和文件名与 asset 重名", rel: ghRelease(func(g *workflow.ReleaseGitHub) { g.Checksums = "dist-linux.tar.gz" }),
			wantErr: "与 asset 重名",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPubEnv(t, tc.opts...)
			var r *testRun
			if tc.run != nil {
				r = tc.run(e)
			} else {
				r = e.tagRun("v1.2.0")
			}
			if tc.ups != nil {
				tc.ups(e, r)
			} else {
				e.builds(r)
			}
			if tc.setup != nil {
				tc.setup(e, r)
			}
			before := len(e.releases(srcRepo))
			id := e.publishJob(r, tc.rel)
			e.publish()
			e.wantJob(id, store.JobFailed, tc.wantErr)
			if s := e.runState(r); s != string(store.RunFailed) {
				t.Fatalf("run 状态 %s，期望 failed", s)
			}
			if n := len(e.releases(srcRepo)); n != before {
				t.Fatalf("失败的发布不应创建 Release：%d → %d", before, n)
			}
			if l := e.log(id); !strings.Contains(l, "发布失败") {
				t.Fatalf("日志应记录失败原因：\n%s", l)
			}
		})
	}
}

// 阶段 A 的授权复核对所有发布 job 都执行，包括只有 release.github、不需要凭据的：规划之后删除 environment、
// 收紧 ref_policy、移除允许发布仓库都会在发布时被拒绝，不会建出 Release（B1 纵深防御）。
func TestWorker_publishGitHubRelease_rechecksAuthorization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  string
		wantErr string
	}{
		{"删除 environment", `DELETE FROM environments WHERE owner_id=$1 AND repo_id=$2 AND name='release'`, "environment release 不存在"},
		{"收紧 ref 策略", `UPDATE environments SET ref_policy='["refs/tags/release-*"]' WHERE owner_id=$1 AND repo_id=$2 AND name='release'`, "ref 策略不允许"},
		{"移除允许发布仓库", `UPDATE environments SET allowed_publish_repos='{}' WHERE owner_id=$1 AND repo_id=$2 AND name='release'`, "不在 environment release 的允许发布仓库列表中"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPubEnv(t)
			e.exec(`INSERT INTO environments (owner_id, repo_id, name, ref_policy, allowed_publish_repos)
				VALUES ($1, $2, 'release', '["refs/tags/v*"]', $3)`, ownerID, srcRepoID, []string{targetRepo})
			r := e.tagRun("v1.2.0")
			e.upstream(r, "build", "dist", nil, nil)
			id := e.publishJob(r, ghRelease(func(g *workflow.ReleaseGitHub) {
				g.Repository, g.NotesFrom = targetRepo, "CHANGELOG.md"
			}), func(s *workflow.Spec) { s.Environment = "release" })
			// 规划之后授权被改掉：只在需要凭据的目标之前检查是不够的，github 目标也要拦住
			e.exec(tc.mutate, ownerID, srcRepoID)
			e.publish()
			e.wantJob(id, store.JobFailed, tc.wantErr)
			if n := len(e.releases(targetRepo)); n != 0 {
				t.Fatalf("被拒绝时目标仓库上不应有 Release，实际 %d 个", n)
			}
		})
	}
}
