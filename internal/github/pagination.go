package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// perPage 是分页接口每页的条数（GitHub 允许的最大值）。
const perPage = 100

// paginate 依次请求 path 的每一页（每页 100 条），直到返回少于 100 条或达到 maxPages。
func paginate[T any](ctx context.Context, fetch func(ctx context.Context, path string, out any) error, path string, maxPages int, unwrap func([]byte) ([]T, error)) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		var raw json.RawMessage
		if err := fetch(ctx, fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, perPage, page), &raw); err != nil {
			return all, err
		}
		items, err := unwrap(raw)
		if err != nil {
			return all, err
		}
		all = append(all, items...)
		if len(items) < perPage {
			break
		}
	}
	return all, nil
}

// unwrapList 解码直接是数组的响应。
func unwrapList[T any](raw []byte) ([]T, error) {
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析列表：%w", err)
	}
	return out, nil
}

// unwrapField 解码数组包在某个字段中的响应，例如 {"repositories": [...]}。
func unwrapField[T any](field string) func([]byte) ([]T, error) {
	return func(raw []byte) ([]T, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("解析响应：%w", err)
		}
		v, ok := m[field]
		if !ok {
			return nil, nil
		}
		return unwrapList[T](v)
	}
}
