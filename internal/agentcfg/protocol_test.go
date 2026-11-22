package agentcfg

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestAIProtocolMatchesConnector：连接器（kiln-orca）实现的 AI 协议版本与 Server 的 AIProtocol 相同。Server 只支持当前
// 版本（PLAN #33.6），两边要一起加一：只改了一边时节点都领不到任务。
func TestAIProtocolMatchesConnector(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../../kiln-orca/src/core/node.ts")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^export const protocolVersion = (\d+);$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("kiln-orca/src/core/node.ts 中没有找到 protocolVersion")
	}
	if v, _ := strconv.Atoi(string(m[1])); v != AIProtocol {
		t.Fatalf("连接器的 protocolVersion = %d，Server 的 AIProtocol = %d：两边要一起改", v, AIProtocol)
	}
}
