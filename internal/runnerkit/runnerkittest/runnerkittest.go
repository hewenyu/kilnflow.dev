// Package runnerkittest 是两种 Runner 的测试共用的替身：自升级布局中的假程序。只在测试中使用。
package runnerkittest

import (
	"fmt"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// FakeProgram 返回一个假的程序 prog：version 输出 "<prog> v<version>"，其他命令把版本和 KILN_LAUNCHER
// 追加到 $KILN_TEST_MARK，然后以 code 退出。
func FakeProgram(prog, version string, code int) []byte {
	return fmt.Appendf(nil, `#!/bin/sh
if [ "$1" = version ]; then echo "%s v%s"; exit 0; fi
echo "%s $* launcher=$KILN_LAUNCHER" >> "$KILN_TEST_MARK"
exit %d
`, prog, version, version, code)
}

// InstallLayout 在数据目录 dir 中建立程序 prog 的升级布局：启动器和 versions 中的各版本都是假程序，
// state.json 为 st。
func InstallLayout(t *testing.T, dir, prog string, st *runnerkit.BootState, versions ...string) {
	t.Helper()
	for _, v := range versions {
		if err := runnerkit.WriteFileAtomic(runnerkit.ProgramPath(dir, prog, v), FakeProgram(prog, v, 0), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := runnerkit.WriteFileAtomic(runnerkit.LauncherPath(dir, prog), FakeProgram(prog, st.Current, 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runnerkit.SaveBootState(dir, prog, st); err != nil {
		t.Fatal(err)
	}
}
