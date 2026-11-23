package cistore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// sqlRecorder 记录收到的 SQL 和参数并返回错误：只检查生成的语句，不访问数据库。
type sqlRecorder struct {
	sql  string
	args []any
}

func (q *sqlRecorder) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.sql, q.args = sql, args
	return nil, errors.New("停止")
}

func (q *sqlRecorder) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("RunWorkflows 不应使用 QueryRow")
}

// TestRunWorkflowsSQL：按是否有仓库给出精确的语句，不含 $1=0 OR repo_id=$1
// （OR 会让规划器放弃 repo_id 的索引扫描）。
func TestRunWorkflowsSQL(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		repoID int64
		want   string
		args   []any
	}{
		{
			name: "全部仓库",
			want: `SELECT DISTINCT workflow FROM runs ORDER BY 1 LIMIT $1`,
			args: []any{200},
		},
		{
			name:   "单个仓库",
			repoID: 42,
			want:   `SELECT DISTINCT workflow FROM runs WHERE repo_id=$1 ORDER BY 1 LIMIT $2`,
			args:   []any{int64(42), 200},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &sqlRecorder{}
			_, err := RunWorkflows(t.Context(), q, c.repoID, 200)
			if err == nil {
				t.Fatal("Query 返回错误时应返回错误")
			}
			if q.sql != c.want {
				t.Errorf("SQL = %q，应为 %q", q.sql, c.want)
			}
			if strings.Contains(q.sql, " OR ") {
				t.Errorf("SQL 不应包含 OR：%q", q.sql)
			}
			if !slices.Equal(q.args, c.args) {
				t.Errorf("参数 = %v，应为 %v", q.args, c.args)
			}
		})
	}
}
