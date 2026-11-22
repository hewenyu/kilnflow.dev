package admin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Runner 的规格与容量（PLAN #8.6）、缓存清单和按范围清理（PLAN #8.3）。CLI 和 Dashboard 共用。

// CacheKinds 返回允许清理的缓存类别（与 Runner 的 CacheEntry.kind 相同）。返回的切片由调用方持有。
func CacheKinds() []string {
	return []string{"toolcache", "runtime", "git", "cas", "buildkit", "work", "image", "legacy"}
}

// recentPurges 是 GetRunnerCache 返回的最近的清理请求数。
const recentPurges = 20

func (a *rpcAPI) GetRunnerCache(ctx context.Context, req *connect.Request[apiv1.GetRunnerCacheRequest]) (*connect.Response[apiv1.GetRunnerCacheResponse], error) {
	resp, err := a.s.GetRunnerCache(ctx, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) PurgeRunnerCache(ctx context.Context, req *connect.Request[apiv1.PurgeRunnerCacheRequest]) (*connect.Response[apiv1.PurgeRunnerCacheResponse], error) {
	resp, err := a.s.PurgeRunnerCache(ctx, req.Msg, rpcActor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// capacityView 把 store 中的容量转换为 API 的格式；allocated 是正在执行的 job 声明的资源之和。
func capacityView(c *store.RunnerCapacity, allocated store.Resources) *apiv1.RunnerCapacity {
	if c == nil {
		return nil
	}
	res := func(r store.Resources) *apiv1.ResourceAmount {
		return &apiv1.ResourceAmount{Cpu: r.CPU, MemoryBytes: r.Memory}
	}
	v := &apiv1.RunnerCapacity{
		Machine: res(c.Machine), Reserved: res(c.Reserved), MaxJobs: clampInt32(c.MaxJobs),
		Allocatable: res(c.Allocatable()), Allocated: res(allocated),
	}
	for k, l := range c.Limits {
		if v.ExecutorLimits == nil {
			v.ExecutorLimits = map[string]*apiv1.ResourceAmount{}
		}
		v.ExecutorLimits[k] = res(l)
	}
	return v
}

// decodeCacheReport 解析 ci_runners.cache_report；没有上报时返回空的清单。
func decodeCacheReport(raw []byte) (*runnerv1.CacheReport, error) {
	rep := &runnerv1.CacheReport{}
	if len(raw) == 0 {
		return rep, nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, rep); err != nil {
		return nil, fmt.Errorf("解析缓存清单：%w", err)
	}
	return rep, nil
}

// cacheBytes 返回清单中全部项的大小之和。
func cacheBytes(raw []byte) int64 {
	rep, err := decodeCacheReport(raw)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range rep.Entries {
		n += e.SizeBytes
	}
	return n
}

// runnerByName 按名称读取一台 CI Runner（本地缓存只属于 CI Runner）。
func (s *Service) runnerByName(ctx context.Context, name string) (*cistore.CIRunner, error) {
	list, err := cistore.ListCIRunners(ctx, s.db, store.RunnerFilter{Name: name})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("没有 Runner %s", name)
	}
	return list[0], nil
}

// repoNames 返回仓库 ID 对应的全名（owner/name）。
func (s *Service) repoNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id, owner || '/' || name FROM repos WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// cacheEntries 把清单项转换为 API 的格式，并填上仓库名。
func (s *Service) cacheEntries(ctx context.Context, list []*runnerv1.CacheEntry) ([]*apiv1.RunnerCacheEntry, error) {
	var ids []int64
	for _, e := range list {
		if e.RepoId != 0 && !slices.Contains(ids, e.RepoId) {
			ids = append(ids, e.RepoId)
		}
	}
	names, err := s.repoNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.RunnerCacheEntry, 0, len(list))
	for _, e := range list {
		out = append(out, &apiv1.RunnerCacheEntry{
			Kind: e.Kind, RepoId: e.RepoId, Repo: names[e.RepoId], Platform: e.Platform, Domain: e.Domain, Name: e.Name,
			Path: e.Path, SizeBytes: e.SizeBytes, LastUsedAt: e.LastUsedAt, ExpiresAt: e.ExpiresAt,
		})
	}
	return out, nil
}

func cacheFailures(list []*runnerv1.CacheFailure) []*apiv1.RunnerCacheFailure {
	out := make([]*apiv1.RunnerCacheFailure, 0, len(list))
	for _, f := range list {
		out = append(out, &apiv1.RunnerCacheFailure{Kind: f.Kind, Path: f.Path, Error: f.Error})
	}
	return out
}

// GetRunnerCache 返回 Runner 最近一次上报的缓存清单、上一次清理的结果和最近的清理请求。
func (s *Service) GetRunnerCache(ctx context.Context, name string) (*apiv1.GetRunnerCacheResponse, error) {
	r, err := s.runnerByName(ctx, name)
	if err != nil {
		return nil, err
	}
	rep, err := decodeCacheReport(r.CacheReport)
	if err != nil {
		return nil, err
	}
	entries, err := s.cacheEntries(ctx, rep.Entries)
	if err != nil {
		return nil, err
	}
	purges, err := s.listPurges(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetRunnerCacheResponse{
		GeneratedAt: rep.GeneratedAt, Entries: entries, DiskTotalBytes: rep.DiskTotalBytes, DiskFreeBytes: rep.DiskFreeBytes,
		GcAt: rep.GcAt, GcRemoved: rep.GcRemoved, GcRemovedBytes: rep.GcRemovedBytes, GcFailures: cacheFailures(rep.GcFailures),
		Purges: purges,
	}, nil
}

// listPurges 返回 Runner 最近的清理请求（新的在前）。
func (s *Service) listPurges(ctx context.Context, runnerID int64) ([]*apiv1.RunnerCachePurge, error) {
	rows, err := s.db.Query(ctx, `SELECT p.id, coalesce(r.owner || '/' || r.name, ''), p.kinds, p.domain, p.state, p.actor, p.created_at, p.done_at, p.result
		FROM runner_cache_purges p LEFT JOIN repos r ON r.id = p.repo_id WHERE p.runner_id=$1 ORDER BY p.id DESC LIMIT $2`, runnerID, recentPurges)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*apiv1.RunnerCachePurge, error) {
		var p apiv1.RunnerCachePurge
		var created time.Time
		var done *time.Time
		var result []byte
		if err := row.Scan(&p.Id, &p.Repo, &p.Kinds, &p.Domain, &p.State, &p.Actor, &created, &done, &result); err != nil {
			return nil, err
		}
		p.CreatedAt = created.Unix()
		if done != nil {
			p.DoneAt = done.Unix()
		}
		if len(result) > 0 {
			var res runnerv1.CachePurgeResult
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(result, &res) == nil {
				p.Removed, p.RemovedBytes, p.Failures = res.Removed, res.RemovedBytes, cacheFailures(res.Failures)
			}
		}
		return &p, nil
	})
}

// matchPurge 判断清单项是否在清理范围内（与 Runner 的 matchPurge 相同）。
func matchPurge(e *runnerv1.CacheEntry, repoID int64, kinds []string, domain string) bool {
	if len(kinds) > 0 && !slices.Contains(kinds, e.Kind) {
		return false
	}
	if repoID != 0 && e.RepoId != repoID {
		return false
	}
	return domain == "" || e.Domain == domain
}

// PurgeRunnerCache 按最近一次上报的清单预览清理范围；不是 dry_run 时发起清理请求并记录审计日志。
// Runner 在没有正在执行的 job 时执行（有待执行的请求时暂停领取新任务），完成后回报结果。
func (s *Service) PurgeRunnerCache(ctx context.Context, req *apiv1.PurgeRunnerCacheRequest, actor string) (*apiv1.PurgeRunnerCacheResponse, error) {
	r, err := s.runnerByName(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if r.Revoked() {
		return nil, invalid(fmt.Errorf("已吊销的 Runner %s 不能清理缓存", r.Name))
	}
	for _, k := range req.Kinds {
		if !slices.Contains(CacheKinds(), k) {
			return nil, invalid(fmt.Errorf("未知的缓存类别 %q（可选：%v）", k, CacheKinds()))
		}
	}
	var repoID int64
	if req.Repo != "" {
		repo, err := store.FindRepo(ctx, s.db, req.Repo)
		if errors.Is(err, store.ErrRepoNotFound) {
			return nil, notFound("%v", err)
		}
		if err != nil {
			return nil, err
		}
		repoID = repo.ID
	}
	rep, err := decodeCacheReport(r.CacheReport)
	if err != nil {
		return nil, err
	}
	var matched, shared []*runnerv1.CacheEntry
	var size int64
	for _, e := range rep.Entries {
		switch {
		case matchPurge(e, repoID, req.Kinds, req.Domain):
			matched = append(matched, e)
			size += e.SizeBytes
		case repoID != 0 && e.RepoId == 0:
			shared = append(shared, e)
		}
	}
	resp := &apiv1.PurgeRunnerCacheResponse{MatchedBytes: size}
	if resp.Matched, err = s.cacheEntries(ctx, matched); err != nil {
		return nil, err
	}
	if resp.RetainedShared, err = s.cacheEntries(ctx, shared); err != nil {
		return nil, err
	}
	if rep.GeneratedAt == 0 {
		resp.Notes = append(resp.Notes, "这台 Runner 还没有上报缓存清单（旧版 kiln-runner 或刚注册），预览为空；清理请求仍会在 Runner 上按范围执行")
	} else {
		resp.Notes = append(resp.Notes, fmt.Sprintf("预览依据 %s 上报的清单，执行时以 Runner 上的实际内容为准", time.Unix(rep.GeneratedAt, 0).Format(time.DateTime)))
	}
	jobs, err := cistore.RunningJobs(ctx, s.db, []int64{r.ID})
	if err != nil {
		return nil, err
	}
	if len(jobs) > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf("%s 正在执行 %d 个 job：清理在它们结束后进行，期间不领取新任务", r.Name, len(jobs)))
	}
	if repoID != 0 && len(shared) > 0 {
		resp.Notes = append(resp.Notes, "runtime、CAS、镜像等不属于单个仓库的共享缓存继续保留，按各自的到期时间清理")
	}
	if req.DryRun {
		return resp, nil
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		var repo *int64
		if repoID != 0 {
			repo = &repoID
		}
		kinds := emptyIfNil(req.Kinds)
		if err := tx.QueryRow(ctx, `INSERT INTO runner_cache_purges (runner_id, repo_id, kinds, domain, actor) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			r.ID, repo, kinds, req.Domain, actor).Scan(&resp.PurgeId); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "runner.cache_purge", map[string]any{
			"name": r.Name, "repo": req.Repo, "kinds": req.Kinds, "domain": req.Domain, "purge_id": resp.PurgeId, "matched_bytes": size,
		})
	})
	if err != nil {
		return nil, err
	}
	s.hub.All() // 唤醒正在长轮询的 Runner，让它尽快收到请求
	return resp, nil
}
