package runner

import (
	"slices"
	"strings"
	"testing"
)

func TestOCIBuildkitPathAndLimits(t *testing.T) {
	o := &ociExec{name: "kiln-7", imagePath: "/usr/bin:/bin", cpu: 1.5, memory: 1 << 30, label: runnerLabelValue(testRunnerID)}
	if o.basePath() != "/usr/bin:/bin" {
		t.Fatalf("没有 buildkit 时 PATH 不变：%s", o.basePath())
	}
	o.buildkitImage = "moby/buildkit@sha256:abc"
	if !strings.HasPrefix(o.basePath(), ociBuildkitDir+":") {
		t.Fatalf("buildctl 应在 PATH 最前面：%s", o.basePath())
	}
	if got := o.limitArgs(); !slices.Equal(got, []string{"--cpus", "1.50", "--memory", "1073741824"}) {
		t.Fatalf("limitArgs = %v", got)
	}
	c, v := o.buildkitNames()
	if c != "kiln-7-buildkitd" || v != "kiln-7-buildkit" {
		t.Fatalf("names = %s %s", c, v)
	}
}
