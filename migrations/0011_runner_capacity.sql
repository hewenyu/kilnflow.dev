-- Runner 的静态规格与容量（PLAN #8.6）和本地缓存清单（PLAN #8.3）。
-- capacity：Runner 每次领取时上报的机器规格、预留、各 executor 的固定上限和 max_jobs（store.RunnerCapacity 的 JSON），
--   NULL 表示旧版 Runner 没有上报，调度时按它上报的空闲资源（capabilities.free_*）判断。
-- cache_report：Runner 最近一次上报的缓存清单和上一次清理的结果（kiln.runner.v1.CacheReport 的 protojson）。
ALTER TABLE runners
  ADD COLUMN capacity          jsonb,
  ADD COLUMN cache_report      jsonb,
  ADD COLUMN cache_reported_at timestamptz;

-- 缓存清理请求（kiln runner cache purge / Dashboard）：Runner 在没有正在执行的 job 时执行，回报结果后为 done。
-- 与审计日志一样属于"结果"，按保留策略永久保留（#10.3）。
CREATE TABLE runner_cache_purges (
  id         bigserial PRIMARY KEY,
  runner_id  bigint NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  repo_id    bigint,                              -- 只清理这个仓库的 toolcache、git 镜像和工作区；NULL 表示不限
  kinds      text[] NOT NULL DEFAULT '{}',        -- 类别（toolcache、git、runtime…），空表示全部
  domain     text   NOT NULL DEFAULT '',          -- toolcache 的执行身份，空表示全部
  state      text   NOT NULL DEFAULT 'pending',   -- pending | done
  actor      text   NOT NULL DEFAULT '',
  result     jsonb,                               -- 删除的数量、字节数和失败的对象
  created_at timestamptz NOT NULL DEFAULT now(),
  done_at    timestamptz
);
CREATE INDEX runner_cache_purges_pending ON runner_cache_purges (runner_id) WHERE state = 'pending';
