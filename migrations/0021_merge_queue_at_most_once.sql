-- 合并队列中没有幂等键的外部写操作改为"先写意图、最多尝试一次"（PLAN C19、#29.8）。
-- 出队评论：调用前记下 comment_state='attempted'；结果不明（响应丢失）时不再评论。
-- 关闭关联的 issue：合并后第一次处理时读一次关联的 issue，把当时打开的冻结到 issues 中（issue 号 → 状态），之后不再重新读取：
--   pending → commenting（已尝试留言）→ closing（已尝试关闭）→ closed，或 unknown（关闭的结果不明、之后读到仍然打开：
--   不再关闭，交给人处理）。明确被拒时退回上一个状态，稍后重试。
-- issues_marks（关闭之前记下的事件 ID）不再需要。升级时正在关闭的项：issues 为 NULL，下一次处理时冻结，已经关闭过的
-- （issues_closed）不放进去；旧版本已经请求过关闭、结果不明的（issues_marks 中有、issues_closed 中没有）记到
-- issues_uncertain：冻结时如果仍然打开就直接按 closing 处理（读到仍然打开 → unknown，交给人），不再留言、不再关闭
-- （用户可能已经重新打开了它）。
ALTER TABLE merge_queue
  ADD COLUMN comment_state text NOT NULL DEFAULT '',  -- '' | attempted
  ADD COLUMN issues jsonb,
  ADD COLUMN issues_uncertain int[] NOT NULL DEFAULT '{}';
UPDATE merge_queue SET issues_uncertain = ARRAY(
    SELECT k::int FROM jsonb_object_keys(issues_marks) k WHERE NOT (k::int = ANY(issues_closed)) ORDER BY 1)
  WHERE issues_state = 'pending' AND issues_marks <> '{}'::jsonb;
ALTER TABLE merge_queue DROP COLUMN issues_marks;
