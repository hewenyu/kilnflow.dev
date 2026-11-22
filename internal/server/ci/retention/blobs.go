package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// 产物的标记-清除（PLAN #10.4）。
//
// 标记：从还没有清理的 job 的结果 manifest 和仍然有效的缓存条目出发，沿 manifest 找到所有可达的对象。
// 清除：删除不可达、并且 last_ref_at 早于保留期的对象。标记之后新产生的引用不会被误删：
//   - 新上传的对象：PrepareUpload 和上传都会刷新 last_ref_at（PrepareUpload 只有刷新成功才认为对象已存在，
//     上传先写 blobs 行再写文件），删除时在行锁下复查 last_ref_at；
//   - 命中缓存：只会命中标记时已经存在的条目（过期条目在标记之前删除），它们引用的对象都已标记；
//   - 重跑沿用结果：重跑在 run 的行锁下复查 run 是否已经清理，已清理的 run 不沿用产物。

// reachable 返回所有可达对象的 digest。
func (c *Collector) reachable(ctx context.Context) (map[string]bool, error) {
	// 在一个只读快照中读取根：job 的结果和缓存条目
	var roots []string
	err := pgx.BeginTxFunc(ctx, c.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT j.result_manifest FROM jobs j LEFT JOIN runs r ON r.id=j.run_id
			WHERE j.result_manifest IS NOT NULL AND (r.purged_at IS NULL OR j.created_at >= r.purged_at)
			UNION SELECT manifest_digest FROM action_cache`)
		if err != nil {
			return err
		}
		roots, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(roots))
	for _, d := range roots {
		if err := c.markResult(ctx, d, live); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// markResult 标记结果 manifest 及其引用的输出 manifest 和文件。manifest 已经不存在或者本身无效（损坏、无法解析）时
// 跳过：产物早已不完整，重试也不会好；否则一个损坏的 manifest 会让此后每一轮都不清除任何对象。
// 其他读取错误返回错误：此时无法确定哪些对象可达，这一轮不清除任何对象。
func (c *Collector) markResult(ctx context.Context, digest string, live map[string]bool) error {
	if live[digest] {
		return nil
	}
	live[digest] = true
	var rm cas.ResultManifest
	if err := cas.ReadJSON(ctx, c.cas, digest, &rm); err != nil {
		if c.unusable(err) {
			return nil
		}
		return fmt.Errorf("结果 manifest %s：%w", digest, err)
	}
	for _, od := range rm.Outputs {
		if live[od] {
			continue
		}
		live[od] = true
		var om cas.OutputManifest
		if err := cas.ReadJSON(ctx, c.cas, od, &om); err != nil {
			if c.unusable(err) {
				continue
			}
			return fmt.Errorf("输出 manifest %s：%w", od, err)
		}
		for _, f := range om.Files {
			if f.Digest != "" {
				live[f.Digest] = true
			}
		}
	}
	return nil
}

// unusable 判断标记时读取 manifest 的错误是否表示它已经不能用（不存在或者本身无效），可以跳过；无效时记一条日志。
func (c *Collector) unusable(err error) bool {
	switch {
	case errors.Is(err, cas.ErrNotFound):
		return true
	case errors.Is(err, cas.ErrInvalidObject):
		c.log.Warn("清理产物：跳过无效的 manifest", "err", err)
		return true
	}
	return false
}

// sweepBlobs 标记可达对象后，删除不可达且 last_ref_at 早于 cutoff 的对象，以及早于 cutoff 的上传临时文件。
func (c *Collector) sweepBlobs(ctx context.Context, cutoff time.Time, res *Result) error {
	live, err := c.reachable(ctx)
	if err != nil {
		return err
	}
	if err := c.adoptOrphans(ctx, cutoff, res); err != nil {
		return err
	}
	after := ""
	for {
		rows, err := c.db.Query(ctx, `SELECT digest FROM blobs WHERE last_ref_at < $1 AND digest > $2 ORDER BY digest LIMIT $3`, cutoff, after, blobBatch)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(batch) == 0 {
			return err
		}
		after = batch[len(batch)-1]
		for _, d := range batch {
			if live[d] {
				continue
			}
			if err := c.deleteBlob(ctx, d, cutoff, res); err != nil {
				return fmt.Errorf("删除 %s：%w", d, err)
			}
		}
	}
}

// deleteBlob 在 blobs 行锁下复查 last_ref_at，然后删除文件和行。文件删除失败时回滚，行保留，下一轮再试。
func (c *Collector) deleteBlob(ctx context.Context, digest string, cutoff time.Time, res *Result) error {
	var size int64
	deleted := false
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM blobs WHERE digest=$1 AND last_ref_at < $2 RETURNING size`, digest, cutoff).Scan(&size)
		if errors.Is(err, pgx.ErrNoRows) {
			deleted = false
			return nil // 期间被重新引用
		}
		if err != nil {
			return err
		}
		deleted = true
		return c.cas.Delete(ctx, digest)
	})
	if err == nil && deleted {
		res.Blobs++
		res.BlobBytes += size
	}
	return err
}

// adoptOrphans 处理存储中没有 blobs 记录的文件：早于 cutoff 的对象按文件时间登记到 blobs（随后按同样的规则清除），
// 早于 cutoff 的上传临时文件直接删除。没有记录的对象是写入文件后记录失败留下的。
func (c *Collector) adoptOrphans(ctx context.Context, cutoff time.Time, res *Result) error {
	var (
		digests []string
		sizes   []int64
		times   []time.Time
	)
	flush := func() error {
		if len(digests) == 0 {
			return nil
		}
		_, err := c.db.Exec(ctx, `INSERT INTO blobs (digest, size, created_at, last_ref_at)
			SELECT d, s, t, t FROM unnest($1::text[], $2::bigint[], $3::timestamptz[]) AS o(d, s, t)
			ON CONFLICT (digest) DO NOTHING`, digests, sizes, times)
		digests, sizes, times = digests[:0], sizes[:0], times[:0]
		return err
	}
	err := c.cas.Walk(ctx, func(o cas.Object) error {
		if !o.ModTime.Before(cutoff) {
			return nil
		}
		if o.Digest == "" {
			if err := os.Remove(o.Path); err == nil {
				res.Blobs++
				res.BlobBytes += o.Size
			}
			return nil
		}
		digests, sizes, times = append(digests, o.Digest), append(sizes, o.Size), append(times, o.ModTime)
		if len(digests) >= blobBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}
