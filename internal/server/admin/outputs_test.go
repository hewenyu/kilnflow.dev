package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
)

// putBlob 把 data 写入 CAS，返回 digest。
func (e *testEnv) putBlob(data []byte) string {
	e.t.Helper()
	d := cas.Digest(data)
	if _, err := e.cas.Put(e.ctx, d, bytes.NewReader(data)); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// putJSON 把 v 的 JSON 写入 CAS，返回 digest。
func (e *testEnv) putJSON(v any) string {
	e.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.putBlob(b)
}

// dropBlob 从 CAS 中删除 digest（模拟存储丢失）。
func (e *testEnv) dropBlob(digest string) {
	e.t.Helper()
	p, err := e.cas.Path(digest)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		e.t.Fatal(err)
	}
}

// outputFixture 是一个有两个输出的 job 结果：bin 有一个普通文件、一个符号链接和一个内容已丢失的文件；docs 的 manifest 已丢失。
type outputFixture struct {
	result, bin, docs string
	app, lost         cas.FileEntry
}

func (e *testEnv) addOutputs() *outputFixture {
	e.t.Helper()
	f := &outputFixture{}
	appData, lostData := []byte("#!/bin/sh\necho app\n"), []byte("lost")
	f.app = cas.FileEntry{Path: "bin/app", Digest: e.putBlob(appData), Size: int64(len(appData)), Exec: true}
	f.lost = cas.FileEntry{Path: "a/lost.txt", Digest: e.putBlob(lostData), Size: int64(len(lostData))}
	e.dropBlob(f.lost.Digest)
	link := cas.FileEntry{Path: "bin/latest", Symlink: "app"}
	f.bin = e.putJSON(cas.OutputManifest{Schema: cas.OutputSchema, Files: []cas.FileEntry{f.app, link, f.lost}})
	f.docs = e.putJSON(cas.OutputManifest{Schema: cas.OutputSchema})
	e.dropBlob(f.docs)
	f.result = e.putJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{"docs": f.docs, "bin": f.bin}})
	return f
}

func TestService_JobOutputs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := e.addOutputs()
	run := e.addRun(runOpts{})

	// 没有结果的 job
	none, err := e.svc.JobOutputs(e.ctx, &cistore.Job{})
	if err != nil || none.Manifest != "" || len(none.Outputs) != 0 {
		t.Errorf("没有结果时 = %+v %v", none, err)
	}

	job, err := cistore.GetJob(e.ctx, e.db, e.addJob(jobOpts{runID: run, manifest: f.result}), false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.JobOutputs(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest != f.result || res.Missing || len(res.Outputs) != 2 {
		t.Fatalf("JobOutputs = %+v", res)
	}
	// 输出按名称排序，文件按路径排序
	bin, docs := res.Outputs[0], res.Outputs[1]
	if bin.Name != "bin" || bin.Digest != f.bin || bin.Missing || bin.FileCount != 3 || bin.Hidden() != 0 || bin.Single() {
		t.Errorf("bin = %+v", bin)
	}
	if want := f.app.Size + f.lost.Size; bin.Size != want {
		t.Errorf("bin 的总大小 = %d，应为 %d（符号链接不计大小）", bin.Size, want)
	}
	var paths []string
	for _, file := range bin.Files {
		paths = append(paths, fmt.Sprintf("%s:%v", file.Path, file.Missing))
	}
	if want := []string{"a/lost.txt:true", "bin/app:false", "bin/latest:false"}; fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("bin 的文件 = %v，应为 %v（内容已丢失的标出）", paths, want)
	}
	if docs.Name != "docs" || !docs.Missing || len(docs.Files) != 0 {
		t.Errorf("docs 的 manifest 已丢失：%+v", docs)
	}

	// 结果 manifest 本身丢失
	e.dropBlob(f.result)
	res, err = e.svc.JobOutputs(e.ctx, job)
	if err != nil || !res.Missing || len(res.Outputs) != 0 {
		t.Errorf("结果 manifest 丢失时 = %+v %v", res, err)
	}
}

func TestService_JobOutputsListLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	files := make([]cas.FileEntry, maxListedOutputFiles+5)
	for i := range files {
		files[i] = cas.FileEntry{Path: fmt.Sprintf("f%04d", len(files)-i), Digest: cas.Digest(nil), Size: 2}
	}
	e.putBlob(nil)
	out := e.putJSON(cas.OutputManifest{Schema: cas.OutputSchema, Files: files})
	single := e.putJSON(cas.OutputManifest{Schema: cas.OutputSchema, Files: files[:1]})
	result := e.putJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{"many": out, "one": single}})
	j, err := cistore.GetJob(e.ctx, e.db, e.addJob(jobOpts{runID: e.addRun(runOpts{}), manifest: result}), false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.JobOutputs(e.ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	many, one := res.Outputs[0], res.Outputs[1]
	if many.FileCount != len(files) || len(many.Files) != maxListedOutputFiles || many.Hidden() != 5 || many.Size != int64(2*len(files)) {
		t.Errorf("超过上限时：共 %d 个、列出 %d 个、隐藏 %d 个、总大小 %d", many.FileCount, len(many.Files), many.Hidden(), many.Size)
	}
	if many.Files[0].Path != "f0001" {
		t.Errorf("应列出按路径排序的前 %d 个：第一个为 %s", maxListedOutputFiles, many.Files[0].Path)
	}
	if !one.Single() {
		t.Errorf("恰好一个普通文件的输出应可以直接下载")
	}
}

func TestService_JobOutputFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := e.addOutputs()
	run := e.addRun(runOpts{})
	job := e.addJob(jobOpts{runID: run, manifest: f.result})
	noResult := e.addJob(jobOpts{runID: run, key: "none"})

	got, err := e.svc.JobOutputFile(e.ctx, job, "bin", "bin/app")
	if err != nil {
		t.Fatal(err)
	}
	if *got != f.app {
		t.Errorf("JobOutputFile = %+v，应为 %+v", got, f.app)
	}

	for _, c := range []struct {
		name         string
		job          int64
		output, path string
		code         connect.Code
		gone         bool
	}{
		{"job 不存在", job + 100, "bin", "bin/app", connect.CodeNotFound, false},
		{"job 没有产物", noResult, "bin", "bin/app", connect.CodeNotFound, false},
		{"没有这个输出", job, "nope", "bin/app", connect.CodeNotFound, false},
		{"输出中没有这个文件", job, "bin", "nope", connect.CodeNotFound, false},
		{"符号链接不能下载", job, "bin", "bin/latest", connect.CodeNotFound, false},
		{"文件内容已丢失", job, "bin", "a/lost.txt", 0, true},
		{"输出的 manifest 已丢失", job, "docs", "x", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.JobOutputFile(e.ctx, c.job, c.output, c.path)
			if c.gone {
				if !errors.Is(err, ErrOutputGone) {
					t.Fatalf("应返回 ErrOutputGone：%v", err)
				}
				return
			}
			wantCode(t, c.name, err, c.code)
		})
	}

	e.dropBlob(f.result)
	if _, err := e.svc.JobOutputFile(e.ctx, job, "bin", "bin/app"); !errors.Is(err, ErrOutputGone) {
		t.Errorf("结果 manifest 丢失时应返回 ErrOutputGone：%v", err)
	}
}
