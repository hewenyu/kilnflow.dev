package cistore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AttemptProgresses 返回这些 attempt 最近上报的进度（没有上报的不在结果中）。
func AttemptProgresses(ctx context.Context, q store.Querier, ids []int64) (map[int64]*store.AttemptProgress, error) {
	out := map[int64]*store.AttemptProgress{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, progress FROM attempts WHERE id = ANY($1) AND progress IS NOT NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		var p store.AttemptProgress
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("attempt %d 的进度无法解析：%w", id, err)
		}
		out[id] = &p
	}
	return out, rows.Err()
}
