-- AI 执行节点（PLAN #33）：退役 kiln-ai-runner + Herdr，改为执行机器上的 Orca Server + kiln-orca 连接器（协议
-- kiln.airunner.v2）。AI 的历史结果保留。
--
-- 1. ai_runners：去掉 Herdr 和主机指纹，增加并发上限和 Orca Server 的状态。节点上报的 host_id 保留（只用于展示，
--    不再用于独立审计的排除：审计按 Agent 判断，PLAN #33.7）。已注册的旧 AI Runner 需要用新令牌注册连接器（PLAN #33.1 O7），
--    这里不删历史，也不自动吊销它们：它们不会上报 v2 的状态，因此不会被派任务。
ALTER TABLE ai_runners
  DROP COLUMN herdr_state,
  DROP COLUMN herdr_reason,
  DROP COLUMN host_fingerprint,
  ADD COLUMN capacity int NOT NULL DEFAULT 1,
  ADD COLUMN orca_version text NOT NULL DEFAULT '',
  ADD COLUMN orca_runtime_id text NOT NULL DEFAULT '',
  ADD COLUMN orca_state text NOT NULL DEFAULT 'unknown' CHECK (orca_state IN ('ready', 'unavailable', 'unknown')),
  ADD COLUMN orca_reason text NOT NULL DEFAULT '';

-- 2. ai_attempts：attempt 在 Orca 中的执行关联（由连接器在心跳和 Complete 中上报）和结果摘要（相同摘要的重试返回
--    replayed，不同摘要返回 ALREADY_EXISTS，PLAN #33.6）。
ALTER TABLE ai_attempts
  ADD COLUMN orca jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN result_digest text;

-- 3. dev_work_items：独立审计按 Agent（PLAN #33.7）。开发过的机器列表换成开发过的 Agent 列表，回填为工作项所有实际参与过
--    开发的 Agent（去重）：首次领取时的开发 Agent（agent 列）在前，其余按首次参与的时间（第一个 attempt 的开始时间、id）。
--    参与 = 为这个工作项执行过任务，与新代码的写入（选题在领取 Issue 时，实现、返修在领取任务的事务中）对应：
--    - 选题任务：领取这个工作项的 attempt（claim_attempt_id）所属任务的 Agent；
--    - 实现、返修任务：ai_dev_requests.work_item_id 关联的任务中有 attempt 的。只规划、从未被领取的任务没有 attempt，
--      不算；领取时生成不了下发内容（error_class='spec'）的 attempt 没有交给 Agent，也不算（这时也不调用 Claimed）。
--    执行中途换过 development.agent 的工作项因此保留每一个开发过它的 Agent，不会让其中之一审计自己的 PR。Agent 为空的
--    不记。只读工作项、AI 任务和 attempt 的索引列（ai_tasks.request_id、ai_attempts.task_id），一条语句完成。
ALTER TABLE dev_work_items ADD COLUMN dev_agents text[] NOT NULL DEFAULT '{}';
UPDATE dev_work_items w SET dev_agents = d.agents
FROM (
  SELECT work_item_id, array_agg(agent ORDER BY first_rank, first_at, first_attempt) AS agents
  FROM (
    SELECT work_item_id, agent, min(rank) AS first_rank, min(started_at) AS first_at, min(attempt_id) AS first_attempt
    FROM (
      SELECT id AS work_item_id, agent, 0 AS rank, NULL::timestamptz AS started_at, NULL::bigint AS attempt_id FROM dev_work_items
      UNION ALL
      SELECT w2.id, t.agent, 1, a.started_at, a.id
      FROM dev_work_items w2 JOIN ai_attempts a ON a.id = w2.claim_attempt_id JOIN ai_tasks t ON t.id = a.task_id
      UNION ALL
      SELECT r.work_item_id, t.agent, 1, a.started_at, a.id
      FROM ai_dev_requests r JOIN ai_tasks t ON t.request_id = r.request_id JOIN ai_attempts a ON a.task_id = t.id
      WHERE r.work_item_id IS NOT NULL AND a.error_class IS DISTINCT FROM 'spec'
    ) participated
    WHERE agent <> ''
    GROUP BY work_item_id, agent
  ) per_agent
  GROUP BY work_item_id
) d
WHERE w.id = d.work_item_id;
ALTER TABLE dev_work_items DROP COLUMN dev_hosts;
