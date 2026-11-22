-- 移除审批（PLAN 修订记录 v0.18）：Kiln 只做 CI，不提供审批 / 人工门禁，审批和 CD 由未来的 CD 模块负责。
-- environment 只保留 ref 策略、变量、secret 和允许发布到的仓库；job 状态不再有 waiting_approval。
--
-- 1. 仍在等待审批（或等待计时）的 job 直接放行，与原来审批通过后一样进入 pending：
--    发布 job 由 Server 执行，其余由 Runner 领取。这些 job 在规划时已经通过 environment 的 ref 策略校验，
--    发布凭据解密前 Server 还会再校验一次 ref 策略。jobs.state 是 text，没有枚举类型或 CHECK 约束需要修改。
UPDATE jobs SET state = 'pending', queued_at = now(),
  reason = CASE WHEN kind = 'publish' THEN '等待 Server 执行发布' ELSE '等待 Runner 领取' END
  WHERE state = 'waiting_approval';

-- 2. 删除只为审批服务的索引和字段。
DROP INDEX IF EXISTS jobs_waiting_approval;
ALTER TABLE jobs DROP COLUMN waiting_since, DROP COLUMN approved_by, DROP COLUMN approved_at;
ALTER TABLE environments DROP COLUMN reviewers, DROP COLUMN wait_timer;
