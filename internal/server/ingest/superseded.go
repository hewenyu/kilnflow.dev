package ingest

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// 过时的事件：事件处理失败后按退避重试，不阻塞之后收到的事件。重试时，同一个 PR（或同一个 ref 的 push）
// 可能已经有更晚收到的事件，PR 的 head、目标分支或开关状态已经变了。这时旧事件不再规划、不取消 run、也不更新 ref_heads：
// 否则旧 head 的 run 会按 cancel-in-progress 取消新 head 的 run，ref_heads 也会退回旧的位置。
//
// 新旧按 Kiln 收到事件的先后（events.received_at）判断，不按 commit 时间（force push 后不可靠），也不向 GitHub 查询
// （synchronize 刚到时 API 可能还是旧的 head，会把新事件误判为过时）。GitHub 投递顺序本身颠倒（旧事件确实最后收到）时判断不出来。

// prSnapshot 是 pull_request 事件 payload 中 PR 当时的状态。每个 pull_request 事件（包括 labeled 等）都带着它，
// 所以任何更晚收到、状态不同的事件都说明这个事件已经过时。
type prSnapshot struct {
	Number int
	State  string // open、closed
	Head   string
	Base   string
}

// prSuperseded 判断 delivery 之后是否收到过同一个 PR、状态不同的 pull_request 事件。
func (i *Ingester) prSuperseded(ctx context.Context, delivery string, repoID int64, s prSnapshot) (bool, error) {
	var stale bool
	err := i.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events e
		WHERE e.event_type='pull_request' AND e.payload->'repository'->>'id'=$2 AND e.payload->'pull_request'->>'number'=$3
		  AND e.received_at > (SELECT received_at FROM events WHERE delivery_id=$1)
		  AND (e.payload->'pull_request'->>'state', e.payload->'pull_request'->'head'->>'sha', e.payload->'pull_request'->'base'->>'ref')
		      IS DISTINCT FROM ($4::text, $5::text, $6::text))`,
		delivery, strconv.FormatInt(repoID, 10), strconv.Itoa(s.Number), s.State, s.Head, s.Base).Scan(&stale)
	return stale, err
}

// pushSuperseded 判断 delivery 之后是否收到过同一个 ref、after 不同的 push 事件。
func (i *Ingester) pushSuperseded(ctx context.Context, delivery string, repoID int64, ref, after string) (bool, error) {
	var stale bool
	err := i.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events e
		WHERE e.event_type='push' AND e.payload->'repository'->>'id'=$2 AND e.payload->>'ref'=$3
		  AND e.received_at > (SELECT received_at FROM events WHERE delivery_id=$1)
		  AND e.payload->>'after' IS DISTINCT FROM $4::text)`,
		delivery, strconv.FormatInt(repoID, 10), ref, after).Scan(&stale)
	return stale, err
}

// supersededResult 是过时事件的处理结果。
const supersededResult = "之后收到了更新的事件，这个事件已经过时，忽略"

// maxPushChain 是 pushChangesBase 默认最多向前追溯的事件数（Ingester.pushChainLimit）。
const maxPushChain = 100

// pushChangesBase 返回分支 push 事件匹配 paths 时比较的起点。过时的 push 不规划，它带来的改动要由之后的 push 一并匹配：
// 否则 c0→c1 改了 src/、c1→c2 只改了 README 时，c1 的事件过时，c2 只比较 c1...c2，源码改动没有触发任何构建。
// 所以从 before 起向前追溯同一个 ref 上更早收到、没有成功规划的 push（还在等待或重试、已按过时忽略、最终失败），
// 它的 after 等于当前的起点时把起点移到它的 before。按收到的先后严格向前，force push 来回也不会循环。
// 追溯到上限仍没有走完时返回空：改动的范围不完整，paths 按匹配处理（与新分支相同），宁可多构建也不漏掉。
func (i *Ingester) pushChangesBase(ctx context.Context, delivery string, repoID int64, ref, before string) (string, error) {
	var at time.Time
	err := i.db.QueryRow(ctx, `SELECT received_at FROM events WHERE delivery_id=$1`, delivery).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return before, nil
	}
	if err != nil {
		return "", err
	}
	for range i.pushChainLimit {
		if before == "" || before == gitref.ZeroSHA {
			return before, nil
		}
		var prev string
		err := i.db.QueryRow(ctx, `SELECT coalesce(payload->>'before',''), received_at FROM events
			WHERE event_type='push' AND payload->'repository'->>'id'=$1 AND payload->>'ref'=$2 AND payload->>'after'=$3
			  AND received_at < $4 AND (processed_at IS NULL OR result IN ($5, 'failed'))
			ORDER BY received_at DESC LIMIT 1`,
			strconv.FormatInt(repoID, 10), ref, before, at, supersededResult).Scan(&prev, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return before, nil
		}
		if err != nil {
			return "", err
		}
		before = prev
	}
	i.log.Warn("追溯过时的 push 超过上限，paths 按匹配处理", "repo", repoID, "ref", ref, "limit", i.pushChainLimit)
	return "", nil
}
