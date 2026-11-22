package review

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// TestNewStoresDocs：New 必须保存 Docs 依赖，否则审计任务永远拿不到冻结文档（否则 p.docs 恒为 nil）。
func TestNewStoresDocs(t *testing.T) {
	d := &docsync.Service{}
	r := New(Deps{Docs: d})
	if r.docs != d {
		t.Fatal("New 没有保存 Docs 依赖")
	}
}
