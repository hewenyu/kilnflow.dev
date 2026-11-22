package github

import (
	"maps"
	"slices"
	"testing"
)

func TestCommentableLines(t *testing.T) {
	patch := "@@ -1,4 +1,5 @@\n a\n-b\n+B\n+C\n c\n@@ -20,2 +21,2 @@ func x() {\n-y\n+Y\n z\n\\ No newline at end of file"
	got := slices.Sorted(maps.Keys(CommentableLines(patch)))
	want := []int{1, 2, 3, 4, 21, 22}
	if !slices.Equal(got, want) {
		t.Fatalf("CommentableLines = %v，应为 %v", got, want)
	}
	if len(CommentableLines("")) != 0 || len(CommentableLines("Binary files differ")) != 0 {
		t.Error("没有 hunk 的 patch 不应有可评论的行")
	}
}
