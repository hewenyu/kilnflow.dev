package scheduler

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestFleetRunnerMismatch(t *testing.T) {
	newRunner := func() *NativeRunner {
		return &NativeRunner{Name: "mac", Pool: "default", Platforms: []string{"darwin/arm64", "linux/arm64"}, Executors: []string{"host", "oci"}}
	}
	job := func(exec workflow.ExecutorKind, platform string, sp workflow.Spec) *cistore.Job {
		return &cistore.Job{Executor: exec, Platform: platform, Spec: sp}
	}
	tests := []struct {
		name  string
		setup func(f *NativeRunner)
		j     *cistore.Job
		ok    bool
	}{
		{"host 原生平台", nil, job(workflow.ExecutorHost, "darwin/arm64", workflow.Spec{}), true},
		{"host 非原生平台", nil, job(workflow.ExecutorHost, "linux/arm64", workflow.Spec{}), false},
		{"没有的平台", nil, job(workflow.ExecutorOCI, "linux/amd64", workflow.Spec{}), false},
		{"没有的 executor", func(f *NativeRunner) { f.Executors = []string{"host"} }, job(workflow.ExecutorOCI, "linux/arm64", workflow.Spec{}), false},
		{"oci 只执行 linux", nil, job(workflow.ExecutorOCI, "darwin/arm64", workflow.Spec{}), false},
		{"oci linux", nil, job(workflow.ExecutorOCI, "linux/arm64", workflow.Spec{}), true},
		{"pool 不匹配", nil, job(workflow.ExecutorHost, "darwin/arm64", workflow.Spec{Pool: "gpu"}), false},
		{"buildkit 需要 Runner 开启", nil, job(workflow.ExecutorOCI, "linux/arm64", workflow.Spec{BuildkitImage: "bk"}), false},
		{"buildkit 已开启", func(f *NativeRunner) { f.Executors = append(f.Executors, workflow.CapBuildkit) }, job(workflow.ExecutorOCI, "linux/arm64", workflow.Spec{BuildkitImage: "bk"}), true},
		{"signing 需要 Runner 支持", nil, job(workflow.ExecutorHost, "darwin/arm64", workflow.Spec{Signing: &workflow.Signing{}}), false},
		{"signing 已支持", func(f *NativeRunner) { f.Executors = append(f.Executors, workflow.CapSigning) }, job(workflow.ExecutorHost, "darwin/arm64", workflow.Spec{Signing: &workflow.Signing{}}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunner()
			if tc.setup != nil {
				tc.setup(f)
			}
			if why := f.Mismatch(RequirementsOf(tc.j)); (why == "") != tc.ok {
				t.Fatalf("mismatch = %q，期望可执行 = %v", why, tc.ok)
			}
		})
	}
}

// 有效容量：机器规格 - 预留，再与 executor 的固定上限取较小值；所有 executor 合计也不超过总量。
// 已知容量时严格按账本判断（空闲也不例外）；旧版 Runner 按上报的空闲资源，没有上报时不判断。
func TestFleetRunnerFits(t *testing.T) {
	gib := int64(1 << 30)
	reqOf := func(exec workflow.ExecutorKind, cpu float64, mem int64) Requirements {
		return RequirementsOf(&cistore.Job{Executor: exec, Spec: workflow.Spec{CPU: cpu, MemoryBytes: mem}})
	}
	arm := &NativeRunner{Cap: &store.RunnerCapacity{
		Machine: store.Resources{CPU: 4, Memory: 24 * gib}, Reserved: store.Resources{CPU: 0, Memory: 2 * gib},
		Limits: map[string]store.Resources{"host": {CPU: 2, Memory: 6 * gib}}, MaxJobs: 4,
	}}
	if got := arm.Cap.ExecutorCapacity("host"); got != (store.Resources{CPU: 2, Memory: 6 * gib}) {
		t.Errorf("host 容量 = %v", got)
	}
	if got := arm.Cap.ExecutorCapacity("oci"); got != (store.Resources{CPU: 4, Memory: 22 * gib}) {
		t.Errorf("oci 容量 = %v", got)
	}
	if arm.Fits(reqOf("host", 2, 8*gib)) {
		t.Error("host job 不能超过 cgroup 的 MemoryMax")
	}
	if !arm.Fits(reqOf("oci", 2, 8*gib)) {
		t.Error("oci job 不受 host 的上限约束")
	}
	arm.Add(reqOf("oci", 3, 8*gib))
	if arm.Fits(reqOf("host", 2, gib)) {
		t.Error("所有 executor 合计不能超过机器规格 - 预留（剩 1 CPU）")
	}
	if !arm.Fits(reqOf("host", 1, gib)) {
		t.Error("剩余 1 CPU 时应能放下 1 CPU 的 job")
	}
	small := &NativeRunner{Cap: &store.RunnerCapacity{Machine: store.Resources{CPU: 2, Memory: 2062835712}}}
	if small.Fits(reqOf("oci", 2, 4*gib)) {
		t.Error("2C2G 的机器不能领取 4GiB 的 job（即使空闲）")
	}
	if !small.Fits(reqOf("oci", 1, gib)) {
		t.Error("2C2G 的机器应能领取 1C1G 的 job")
	}
	legacy := &NativeRunner{LegacyFree: &store.Resources{CPU: 2, Memory: 2 * gib}}
	if legacy.Fits(reqOf("oci", 2, 4*gib)) || !legacy.Fits(reqOf("oci", 2, 2*gib)) {
		t.Error("旧版 Runner 按上报的空闲资源严格判断")
	}
	if !(&NativeRunner{}).Fits(reqOf("oci", 64, 512*gib)) {
		t.Error("没有上报资源的旧版 Runner 不按资源判断")
	}
}
