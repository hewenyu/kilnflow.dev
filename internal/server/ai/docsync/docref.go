package docsync

import "github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"

// SameDocRefs 判断任务 spec 中的文档引用与一份快照是否逐篇一致（ID、标题、修订号、摘要、地址）。
//
// 两侧都不接受空 ID 或重复 ID：合法冻结的文档 ID 非空且唯一，[A, A] 这类畸形引用不能因为"按 ID 查到同一篇"
// 而被当成与 [A] 或 [A, B] 相同的依据（否则审计任务校验和迁移前依据固化会放过不一致的 spec）。
// 两侧都为空表示"没有文档依据"，算一致；调用方通常在空集合上有自己的语义，会先单独处理。
func SameDocRefs(refs []aispec.DocRef, snaps []Snapshot) bool {
	if len(refs) != len(snaps) {
		return false
	}
	byID := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		if s.DocID == "" {
			return false
		}
		if _, dup := byID[s.DocID]; dup {
			return false
		}
		byID[s.DocID] = s
	}
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r.ID == "" {
			return false
		}
		if _, dup := seen[r.ID]; dup {
			return false
		}
		seen[r.ID] = struct{}{}
		s, ok := byID[r.ID]
		if !ok || s.Title != r.Title || s.Revision != r.Revision || s.SHA256 != r.SHA256 || s.URL != r.URL {
			return false
		}
	}
	return true
}
