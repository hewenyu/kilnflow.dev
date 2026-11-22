package cistore

import (
	"context"
	"errors"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 数据库出错时各查询返回错误，不会误判为"不存在"。
func TestQueriesReportDatabaseErrors(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cases := map[string]func() error{
		"ListCIRunners": func() error {
			_, err := ListCIRunners(ctx, db, store.RunnerFilter{})
			return err
		},
		"RunningJobs": func() error {
			_, err := RunningJobs(ctx, db, []int64{1})
			return err
		},
		"RunnerAllocations": func() error {
			_, err := RunnerAllocations(ctx, db, []int64{1})
			return err
		},
		"AttemptProgresses": func() error {
			_, err := AttemptProgresses(ctx, db, []int64{1})
			return err
		},
		"QueryRuns": func() error {
			_, _, err := QueryRuns(ctx, db, &RunFilter{Limit: 1})
			return err
		},
		"RunWorkflows": func() error {
			_, err := RunWorkflows(ctx, db, 0, 1)
			return err
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := f(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s 应返回 ctx 的错误：%v", name, err)
			}
		})
	}
}
