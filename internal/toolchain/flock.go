package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LocksDirName 是安装目录下放文件锁的子目录（<root>/.locks/<digest hex>）。锁文件是空文件，不删除：删除后另一个进程会在
// 新文件上加锁，与仍持有旧文件锁的进程互不相干。清理 runtime 时要跳过这个目录。
const LocksDirName = ".locks"

// 装好的 runtime 是工具，不是过程数据，不按过程数据的保留期删除（PLAN #12.8、C10）。
const (
	// Retention 是装好的 runtime 没有用到时的保留时间。
	Retention = 30 * 24 * time.Hour
	// DefaultMaxSize 是一个安装目录中 runtime 总量的默认上限：超过后从最久没用的删起。
	DefaultMaxSize int64 = 5 << 30
)

// lockPollInterval 是等待另一个进程释放安装锁时的查询间隔。
const lockPollInterval = 200 * time.Millisecond

// IsTemp 判断 runtime 根目录下的条目是不是安装过程的临时文件（下载到一半的发行包、解包目录）：安装结束时都会删除，
// 留下的是进程被强行结束时的残留。
func IsTemp(name string) bool { return strings.HasPrefix(name, ".") && name != LocksDirName }

// lockInstall 取得 runtime hex 的安装锁，返回释放函数；锁被持有时等待，ctx 结束时返回它的错误。每次调用各自打开锁文件：
// flock 按打开的文件互斥，所以同一进程内的调用之间和进程之间都互斥。
func lockInstall(ctx context.Context, root, hex string) (func(), error) {
	dir := filepath.Join(root, LocksDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, hex), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}
