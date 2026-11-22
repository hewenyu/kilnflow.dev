package logging

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// 按本地日期切分：日期变化时写到新文件，并删除早于保留期的文件（只删除 <name>-<日期>.log）。
func TestDailyFileRotatesAndPrunes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	day := time.Date(2026, 11, 1, 23, 59, 0, 0, time.Local)
	now := day
	for _, name := range []string{"svc-2026-10-20.log", "svc-2026-10-26.log", "svc-2026-10-25.log", "other-2026-10-01.log", "svc-x.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := openDailyFile(dir, "svc", 7*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	now = day.Add(2 * time.Minute) // 第二天
	if _, err := f.Write([]byte("b\n")); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("svc-2026-11-01.log"); got != "a\n" {
		t.Errorf("11-01 = %q", got)
	}
	if got := read("svc-2026-11-02.log"); got != "b\n" {
		t.Errorf("11-02 = %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// 11-02 时保留 10-27 及之后的 7 天
	want := []string{"other-2026-10-01.log", "svc-2026-11-01.log", "svc-2026-11-02.log", "svc-x.log"}
	if !slices.Equal(names, want) {
		t.Errorf("目录中的文件 = %v，应为 %v", names, want)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("c")); err == nil {
		t.Error("关闭后写入应返回错误")
	}
	// 关闭后日期变化也不能重新打开文件（否则文件描述符泄漏，日志还会继续写）
	now = day.Add(24 * time.Hour)
	if _, err := f.Write([]byte("d")); err == nil {
		t.Error("关闭后跨天写入应返回错误")
	}
	if _, err := os.Stat(filepath.Join(dir, "svc-2026-11-03.log")); err == nil {
		t.Error("关闭后不应创建新的日志文件")
	}
}

// 设置了 KILN_LOG_DIR 时日志写到按天切分的文件，不写 stderr。
func TestSetupLogDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvFormat, "")
	var stderr strings.Builder
	log, closeLog, err := Setup(&stderr, "kiln-test")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("你好")
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "kiln-test-"+time.Now().Format(dayLayout)+".log"))
	if err != nil || !strings.Contains(string(b), "你好") {
		t.Errorf("日志文件：%q %v", b, err)
	}
	if stderr.Len() != 0 {
		t.Errorf("不应写到 stderr：%q", stderr.String())
	}

	t.Setenv(EnvDir, "")
	log, closeLog, err = Setup(&stderr, "kiln-test")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("stderr")
	_ = closeLog()
	if !strings.Contains(stderr.String(), "stderr") {
		t.Errorf("没有设置 KILN_LOG_DIR 时写到 stderr：%q", stderr.String())
	}

	t.Setenv(EnvDir, dir)
	t.Setenv(EnvFormat, "xml")
	if _, _, err := Setup(&stderr, "kiln-test"); err == nil {
		t.Error("格式无效时应返回错误")
	}
}
