-- 保留策略（PLAN #10.3、#10.4）：run 结束超过保留期（默认 7 天）后，Server 清理它的过程数据——
-- 日志、产物（CAS 对象）、缓存条目，只保留结果（run、job、attempt 的状态、时间、原因、退出码）。
-- purged_at 是最近一次清理的时间：在它之前创建的 job 的日志和产物都已经清理（Dashboard 据此说明）。
-- 重跑后 finished_at 晚于 purged_at，新的一次运行结束超过保留期后再清理一次。
ALTER TABLE runs ADD COLUMN purged_at timestamptz;
-- 找出待清理的 run：已结束、最近一次运行还没有清理，按结束时间
CREATE INDEX runs_unpurged ON runs (finished_at)
  WHERE finished_at IS NOT NULL AND (purged_at IS NULL OR purged_at < finished_at);
-- 清理已处理的 webhook 事件
CREATE INDEX events_processed ON events (processed_at) WHERE processed_at IS NOT NULL;
