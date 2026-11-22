package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// pagedReply 返回按 page、per_page 分页的列表接口：共 total 项，第 i 项由 item(i) 生成；
// field 非空时数组包在该字段中（例如 {"repositories": [...]}）。
func pagedReply(total int, field string, item func(i int) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 || per < 1 {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"message": "缺少分页参数"})
			return
		}
		list := []any{}
		for i := (page - 1) * per; i < min(page*per, total); i++ {
			list = append(list, item(i))
		}
		if field != "" {
			writeTestJSON(w, http.StatusOK, map[string]any{"total_count": total, field: list})
			return
		}
		writeTestJSON(w, http.StatusOK, list)
	}
}

// pages 返回请求的页码（按顺序），并检查每个请求都带 per_page=100。
func pages(t *testing.T, reqs []recordedRequest) []string {
	t.Helper()
	var out []string
	for _, r := range reqs {
		if pp := r.Query.Get("per_page"); pp != "100" {
			t.Errorf("%s 的 per_page = %q，应为 100", r.key(), pp)
		}
		out = append(out, r.Query.Get("page"))
	}
	return out
}

// 分页接口逐页请求，直到某一页少于 100 条。
func TestPagination_ListReleases(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		wantPages string
	}{
		{"空列表只请求一页", 0, "1"},
		{"不足一页", 42, "1"},
		{"正好一页时再请求一页确认结束", 100, "1,2"},
		{"多页", 250, "1,2,3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/releases", pagedReply(tt.total, "", func(i int) any {
				return map[string]any{"id": i + 1, "tag_name": fmt.Sprintf("v%d", i)}
			}))
			rels, err := c.Inst(1).ListReleases(t.Context(), "o/r")
			if err != nil {
				t.Fatal(err)
			}
			if len(rels) != tt.total {
				t.Fatalf("返回 %d 项，应为 %d 项", len(rels), tt.total)
			}
			for i, r := range rels {
				if r.ID != int64(i+1) {
					t.Fatalf("第 %d 项 ID = %d，顺序不对", i, r.ID)
				}
			}
			if got := strings.Join(pages(t, f.requests()), ","); got != tt.wantPages {
				t.Errorf("请求的页 = %s，应为 %s", got, tt.wantPages)
			}
		})
	}
}

// 达到最大页数时停止，返回已读取的部分。
func TestPagination_MaxPages(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/releases", pagedReply(1_000_000, "", func(i int) any { return map[string]any{"id": i} }))
	rels, err := c.Inst(1).ListReleases(t.Context(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != maxReleasePages*perPage || len(f.requests()) != maxReleasePages {
		t.Errorf("读取 %d 项、请求 %d 次，应为 %d、%d", len(rels), len(f.requests()), maxReleasePages*perPage, maxReleasePages)
	}
}

// 路径已有查询参数时用 & 连接分页参数。
func TestPagination_ExistingQuery(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/pulls", pagedReply(3, "", func(i int) any { return map[string]any{"number": i + 1} }))
	prs, err := c.Inst(1).ListOpenPulls(t.Context(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	r := f.only(t)
	if len(prs) != 3 || r.Query.Get("state") != "open" || r.Query.Get("page") != "1" {
		t.Errorf("ListOpenPulls：%d 项，查询参数 %v", len(prs), r.Query)
	}
}

// 中途出错时返回已读取的部分和错误；响应格式不对时报错。
func TestPaginate_Errors(t *testing.T) {
	full := "[" + strings.TrimSuffix(strings.Repeat("1,", perPage), ",") + "]"
	boom := errors.New("boom")
	var paths []string
	// 第一页返回满页，第二页出错
	fetch := func(_ context.Context, path string, out any) error {
		paths = append(paths, path)
		if len(paths) == 2 {
			return boom
		}
		raw, ok := out.(*json.RawMessage)
		if !ok {
			return fmt.Errorf("out 类型 %T", out)
		}
		*raw = json.RawMessage(full)
		return nil
	}
	got, err := paginate(t.Context(), fetch, "/x?a=1", 5, unwrapList[int])
	if !errors.Is(err, boom) || len(got) != perPage {
		t.Errorf("第二页出错：返回 %d 项，err=%v", len(got), err)
	}
	if paths[0] != "/x?a=1&per_page=100&page=1" || paths[1] != "/x?a=1&per_page=100&page=2" {
		t.Errorf("请求路径：%v", paths)
	}

	tests := []struct {
		name    string
		raw     string
		unwrap  func([]byte) ([]int, error)
		want    int
		wantErr string
	}{
		{"数组", "[1,2]", unwrapList[int], 2, ""},
		{"不是数组", `{"a":1}`, unwrapList[int], 0, "解析列表"},
		{"字段中的数组", `{"items":[1,2,3]}`, unwrapField[int]("items"), 3, ""},
		{"没有该字段视为空", `{"other":[1]}`, unwrapField[int]("items"), 0, ""},
		{"不是对象", `[1]`, unwrapField[int]("items"), 0, "解析响应"},
		{"字段不是数组", `{"items":"x"}`, unwrapField[int]("items"), 0, "解析列表"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.unwrap([]byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("错误应包含 %q，实际 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil || len(got) != tt.want {
				t.Fatalf("返回 %v，err=%v，应为 %d 项", got, err, tt.want)
			}
		})
	}
}
