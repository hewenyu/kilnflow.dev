-- 统一 CI 调度核心（PLAN #32）：attempt 可以属于 Native Runner，也可以属于托管池；托管执行在实际开始前用 phase 区分阶段。
--   managed_pool：托管池的 Key（Native 为 NULL）；托管池的表由之后的 Provider 契约加上，届时再加外键。
--   phase：reserved（已预留，没有发出任何外部调用）| submitting（已写下派发意图）| queued（外部已接受，还没开始执行）
--          | running（正在执行；Native 领取即是这个阶段）| unknown（提交或观察的结果未知，等待对账）。
-- 已有的行取默认值（Native、running），满足新约束。
ALTER TABLE attempts
  ADD COLUMN managed_pool text,
  ADD COLUMN phase text NOT NULL DEFAULT 'running'
    CHECK (phase IN ('reserved', 'submitting', 'queued', 'running', 'unknown')),
  DROP CONSTRAINT attempts_runner_known,
  -- 恰好属于一种资源：托管池，或者 Native Runner（删除 Runner 后记着它的名称）
  ADD CONSTRAINT attempts_executor_known CHECK ((managed_pool IS NULL) = (runner_id IS NOT NULL OR runner_name IS NOT NULL)),
  -- Native attempt 只能是 running（Native 没有预留阶段：领取即开始执行）
  ADD CONSTRAINT attempts_native_running CHECK (managed_pool IS NOT NULL OR phase = 'running');
