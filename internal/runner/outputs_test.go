package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectOutputSymlinkEscape(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"), "x")
	mustWrite(t, filepath.Join(ws, "dist", "ok"), "ok")
	// 中间目录是指向工作区之外的符号链接
	mustSymlink(t, outside, filepath.Join(ws, "leak"))

	for _, spec := range []string{"leak/secret", "leak/*"} {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			_, err := collectOutput(ws, spec)
			if _, ok := errors.AsType[*userError](err); !ok {
				t.Errorf("期望 userError，实际 %v", err)
			}
		})
	}
	c, err := collectOutput(ws, "dist")
	if err != nil || len(c.Manifest.Files) != 1 {
		t.Fatalf("dist：%v %+v", err, c)
	}
	// 工作区内部的目录符号链接是允许的
	mustSymlink(t, filepath.Join(ws, "dist"), filepath.Join(ws, "alias"))
	if _, err := collectOutput(ws, "alias/ok"); err != nil {
		t.Fatalf("alias/ok：%v", err)
	}
}

func TestCollectOutputForms(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "out", "a.txt"), "a")
	mustWrite(t, filepath.Join(ws, "out", "sub", "b.bin"), "b")
	mustSymlink(t, "a.txt", filepath.Join(ws, "out", "link"))
	mustSymlink(t, "../../etc/passwd", filepath.Join(ws, "out", "bad"))

	cases := []struct {
		spec  string
		paths []string
		err   bool
	}{
		{spec: "out/a.txt", paths: []string{"a.txt"}},
		{spec: "out/**/*.bin", paths: []string{"sub/b.bin"}},
		{spec: "out/missing", err: true},
		{spec: "out/*.none", err: true},
		{spec: "", err: true},
		{spec: "out/link", paths: []string{"link"}}, // 单个符号链接，目标在同一目录中
		{spec: "out", err: true},                    // 含有指向外面的 bad
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			t.Parallel()
			got, err := collectOutput(ws, c.spec)
			if c.err {
				if _, ok := errors.AsType[*userError](err); !ok {
					t.Fatalf("期望 userError，实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, f := range got.Manifest.Files {
				paths = append(paths, f.Path)
			}
			if len(paths) != len(c.paths) || paths[0] != c.paths[0] {
				t.Fatalf("paths = %v，期望 %v", paths, c.paths)
			}
		})
	}
}

// 链式符号链接：中间链接 sub -> ../.. 把后续组件带出输出目录，字面解析（filepath.Clean）看不出来。
func TestCollectOutputChainedSymlinkEscape(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "out", "ok"), "ok")
	if err := os.MkdirAll(filepath.Join(ws, "out", "d1", "d2"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "../..", filepath.Join(ws, "out", "d1", "d2", "sub"))
	mustSymlink(t, "d1/d2/sub/../../../outside/target.txt", filepath.Join(ws, "out", "escape"))
	if _, err := collectOutput(ws, "out/escape"); err == nil {
		t.Fatal("链式符号链接逃逸应报错")
	} else if _, ok := errors.AsType[*userError](err); !ok {
		t.Fatalf("期望 userError，实际 %v", err)
	}

	// 经由中间符号链接但仍留在输出目录内的目标是允许的
	mustSymlink(t, ".", filepath.Join(ws, "out", "up"))
	mustSymlink(t, "up/ok", filepath.Join(ws, "out", "alias"))
	if _, err := collectOutput(ws, "out/alias"); err != nil {
		t.Fatalf("输出目录内的链式符号链接应允许：%v", err)
	}
}

func TestCheckWorkDir(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws, "f"), "")
	outside := t.TempDir()
	mustSymlink(t, outside, filepath.Join(ws, "leak"))
	if err := checkWorkDir(ws, "sub"); err != nil {
		t.Fatal(err)
	}
	for _, wd := range []string{"missing", "f", "..", "../tmp", "/tmp", "sub/../..", "leak"} {
		t.Run(wd, func(t *testing.T) {
			t.Parallel()
			if _, ok := errors.AsType[*userError](checkWorkDir(ws, wd)); !ok {
				t.Errorf("期望 userError")
			}
		})
	}
}
