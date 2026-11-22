package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvDir 设置后，日志写到这个目录中按天切分的文件，不再写到 stderr（服务方式运行时使用）。
const EnvDir = "KILN_LOG_DIR"

// FileRetention 是按天切分的日志文件的保留时间：只保留最近 7 天的文件（含当天）。
const FileRetention = 7 * 24 * time.Hour

// dayLayout 是日志文件名中的日期格式。
const dayLayout = "2006-01-02"

// DailyFile 把日志写到 <dir>/<name>-<YYYY-MM-DD>.log（本地日期）。日期变化时切换到新文件，
// 并删除日期早于保留期的旧文件。可以并发写入。
type DailyFile struct {
	dir, name string
	keep      time.Duration
	now       func() time.Time

	mu     sync.Mutex
	day    string
	f      *os.File
	closed bool // Close 之后不再写入，也不再因为日期变化打开新文件
}

var _ io.WriteCloser = (*DailyFile)(nil)

// OpenDailyFile 创建目录、打开当天的日志文件并清理过期的文件。keep 为 0 时使用 FileRetention。
func OpenDailyFile(dir, name string, keep time.Duration) (*DailyFile, error) {
	return openDailyFile(dir, name, keep, time.Now)
}

func openDailyFile(dir, name string, keep time.Duration, now func() time.Time) (*DailyFile, error) {
	if keep <= 0 {
		keep = FileRetention
	}
	//nolint:gosec // G703：目录来自运维人员设置的 KILN_LOG_DIR 或 Runner 的数据目录
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	d := &DailyFile{dir: dir, name: name, keep: keep, now: now}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.rotate(now()); err != nil {
		return nil, err
	}
	return d, nil
}

// Write 写入当天的文件；日期变化后先切换文件。切换失败时继续写原来的文件。
func (d *DailyFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if now := d.now(); now.Format(dayLayout) != d.day {
		if err := d.rotate(now); err != nil && d.f == nil {
			return 0, err
		}
	}
	if d.f == nil {
		return 0, os.ErrClosed
	}
	return d.f.Write(p)
}

// Close 关闭当前的文件。
func (d *DailyFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// rotate 打开 now 所在日期的文件，关闭原来的文件并清理过期的文件；调用方持有锁。
func (d *DailyFile) rotate(now time.Time) error {
	day := now.Format(dayLayout)
	f, err := os.OpenFile(filepath.Join(d.dir, d.name+"-"+day+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if d.f != nil {
		_ = d.f.Close() // 只写打开的日志文件，内容已经写入，关闭失败不影响之后的日志
	}
	d.f, d.day = f, day
	d.prune(now)
	return nil
}

// prune 删除日期早于保留期的日志文件（只处理 <name>-<日期>.log 形式的文件）；尽力而为。
func (d *DailyFile) prune(now time.Time) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	// 保留最近 keep 天（含当天）：日期不早于 cutoff 的文件保留
	cutoff := now.Add(-d.keep + 24*time.Hour).Format(dayLayout)
	for _, e := range entries {
		day, ok := strings.CutPrefix(e.Name(), d.name+"-")
		if !ok {
			continue
		}
		day, ok = strings.CutSuffix(day, ".log")
		if !ok {
			continue
		}
		if _, err := time.Parse(dayLayout, day); err != nil || day >= cutoff {
			continue
		}
		_ = os.Remove(filepath.Join(d.dir, e.Name()))
	}
}

// Setup 按环境变量创建进程的 Logger：设置了 KILN_LOG_DIR 时写到该目录中按天切分的 <name>-<日期>.log，
// 保留 7 天；否则写到 stderr。返回的 close 在进程退出前调用。
func Setup(stderr io.Writer, name string) (log *slog.Logger, closeFn func() error, err error) {
	w := stderr
	closeFn = func() error { return nil }
	if dir := os.Getenv(EnvDir); dir != "" {
		f, err := OpenDailyFile(dir, name, FileRetention)
		if err != nil {
			return nil, nil, fmt.Errorf("%s=%q：%w", EnvDir, dir, err)
		}
		w, closeFn = f, f.Close
	}
	log, err = FromEnv(w)
	if err != nil {
		return nil, nil, errors.Join(err, closeFn())
	}
	return log, closeFn, nil
}
