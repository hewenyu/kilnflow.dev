-- CI 排队顺序（PLAN #14.2）：pending 的 job 按 (priority DESC, created_at, queued_at, id) 排队，created_at 是 job 所属那次
-- 提交（run 的首次规划或一次重跑）的时间。部分索引 jobs_pending 换成与这个顺序一致的列，候选分页（ORDER BY 与游标的行比较）、
-- 发布 job 的领取和调度解释都按它读取。只换索引，不改数据。
DROP INDEX jobs_pending;
CREATE INDEX jobs_pending ON jobs (priority DESC, created_at, queued_at, id) WHERE state = 'pending';
