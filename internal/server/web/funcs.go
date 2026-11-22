package web

import "fmt"

// 页面上的文字说明（状态、事件、时间等）由前端格式化（web/src/lib/format.ts）；这里只保留服务端生成提示时用到的。

// rerunLabel 返回第 n 次运行的名称：0 是"首次运行"，其余是"第 n 次重跑"。
func rerunLabel(n int) string {
	if n == 0 {
		return "首次运行"
	}
	return fmt.Sprintf("第 %d 次重跑", n)
}
