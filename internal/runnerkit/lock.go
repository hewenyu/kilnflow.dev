package runnerkit

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked 表示另一个进程持有锁（数据目录锁、本机 AI 执行节点锁）。
var ErrLocked = errors.New("已经被另一个进程锁住")

// Lock 以非阻塞方式锁住数据目录（run 运行期间持有），返回释放函数；已经被锁住时返回包含 ErrLocked 的错误。
// 手动执行的 kiln-runner gc 据此判断服务是否在运行（TryLock）。
func (h *Home) Lock() (func(), error) {
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := flockFile(h.Path(lockFileName))
	if errors.Is(err, ErrLocked) {
		return nil, fmt.Errorf("数据目录正被另一个 %s 进程使用（%s）：%w", h.Prog.Name, h.Dir, ErrLocked)
	}
	return unlock, err
}

// flockFile 以非阻塞方式锁住文件 p（不存在时创建），返回释放函数；已经被锁住时返回 ErrLocked。
func flockFile(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// FlockWait 锁住文件 p（不存在时创建，0600），已经被锁住时等待，返回释放函数。
// 用于同一数据目录上可能有多个进程读写的文件（例如 Runner 的 images.json）。
func FlockWait(p string) (func(), error) { return flockWait(p) }

// flockWait 锁住文件 p（不存在时创建），已经被锁住时等待，返回释放函数。
func flockWait(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// TryLock 供手动执行的 kiln-runner gc 使用：数据目录正被 run 使用（服务正在运行）时返回 running 为 true；
// 否则锁住数据目录并返回释放函数。清理期间要一直持有：只检查一下就释放的话，服务可能在清理中途启动并开始执行 job，
// 按容量回收和删除残留容器会删掉它正在用的缓存和容器。
func (h *Home) TryLock() (unlock func(), running bool, err error) {
	unlock, err = h.Lock()
	switch {
	case errors.Is(err, ErrLocked):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return unlock, false, nil
}
