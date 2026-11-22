package aidev

import "testing"

// spareSlots：排队任务只占用它能执行的平台上的槽位，占用数取最大匹配；返回在此之后还能为 want 的任务匹配的槽位数。
func TestSpareSlots(t *testing.T) {
	const mac, linux, arm = "darwin/arm64", "linux/amd64", "linux/arm64"
	q := func(n int, platforms ...string) pendingTasks { return pendingTasks{platforms: platforms, n: n} }
	cases := []struct {
		name    string
		free    map[string]int
		pending []pendingTasks
		want    []string
		spare   int
	}{
		{"没有空闲槽位", map[string]int{}, nil, nil, 0},
		{"没有排队任务：任何平台", map[string]int{mac: 2, linux: 1}, nil, nil, 3},
		{"没有排队任务：只数 want 中的平台", map[string]int{mac: 2, linux: 1}, nil, []string{mac}, 2},
		{"槽位为 0 的平台不算", map[string]int{mac: 0, linux: 1}, nil, []string{mac}, 0},
		{"同平台的排队任务扣减", map[string]int{mac: 2}, []pendingTasks{q(1, mac)}, []string{mac}, 1},
		{"排队任务多于槽位时为 0，不为负", map[string]int{mac: 3}, []pendingTasks{q(5, mac)}, []string{mac}, 0},
		{"只能在没有槽位的平台上执行的排队任务不占组内 Linux 的槽位", map[string]int{linux: 1}, []pendingTasks{q(1, mac)}, []string{mac, linux}, 1},
		{"Linux 专属的排队任务不占 macOS 的槽位", map[string]int{mac: 1}, []pendingTasks{q(1, linux)}, []string{mac}, 1},
		{"没有节点的平台上的排队任务不扣减（平台不限）", map[string]int{mac: 2}, []pendingTasks{q(4, arm)}, nil, 2},
		{"一组平台的排队任务能放到 want 之外时不占 want", map[string]int{mac: 1, linux: 1}, []pendingTasks{q(1, mac, linux)}, []string{mac}, 1},
		{"一组平台的排队任务只能用 want 中的槽位时扣减", map[string]int{mac: 1}, []pendingTasks{q(1, mac, linux)}, []string{mac}, 0},
		{"两个排队任务正好占满组内两个平台", map[string]int{mac: 1, linux: 1}, []pendingTasks{q(1, mac, linux), q(1, mac)}, []string{mac, linux}, 0},
		// 按到达顺序贪心会把一组平台的任务先放到 macOS、macOS 专属的任务就放不下（只扣 1）；最大匹配两个都放下，Linux 上没有剩余
		{"按最大匹配扣减，不受排队任务的顺序影响", map[string]int{mac: 1, linux: 1}, []pendingTasks{q(1, mac, linux), q(1, mac)}, []string{linux}, 0},
		{"排队任务占不满时剩余的槽位可用", map[string]int{mac: 1, linux: 2}, []pendingTasks{q(1, mac, linux), q(1, mac)}, []string{linux}, 1},
		{"组外平台的空闲槽位不算进 want", map[string]int{mac: 1, arm: 3}, []pendingTasks{q(1, mac)}, []string{mac, linux}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spareSlots(c.free, c.pending, c.want); got != c.spare {
				t.Fatalf("spareSlots(%v, %v, %v) = %d，应为 %d", c.free, c.pending, c.want, got, c.spare)
			}
		})
	}
}
