package runnerkit

import (
	"errors"
	"testing"
)

// kiln-runner gc 在清理期间一直持有数据目录的锁：期间服务（Run 中的 Home.Lock）无法启动；
// 释放后可以启动。服务正在运行时 TryLock 报告 running，不加锁。
func TestTryLock(t *testing.T) {
	t.Parallel()
	h := &Home{Dir: t.TempDir(), Prog: testProgram}
	unlock, running, err := h.TryLock()
	if err != nil || running || unlock == nil {
		t.Fatalf("TryLock = %v %v %v", unlock != nil, running, err)
	}
	if _, err := h.Lock(); !errors.Is(err, ErrLocked) {
		t.Errorf("gc 持有锁时服务不应能启动：%v", err)
	}
	unlock()

	unlockSvc, err := h.Lock()
	if err != nil {
		t.Fatalf("gc 释放锁后服务应能启动：%v", err)
	}
	defer unlockSvc()
	unlock, running, err = h.TryLock()
	if err != nil || !running || unlock != nil {
		t.Errorf("服务运行时 TryLock = %v %v %v，期望 running", unlock != nil, running, err)
	}
}
