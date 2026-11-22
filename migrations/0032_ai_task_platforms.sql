-- AI 任务可以在一组平台中的任一个上执行（PLAN #26.1、#30.7）：审计配置 review.required_platforms 的一项可以是一组可以互相
-- 替代的平台（any_of），节点上报的平台在组内即可领取，只执行一次。
--
-- 1. ai_attempts.platform：领取时节点上报的平台（实际执行的平台）。已有的 attempt 回填为任务的平台（以前每个任务只有一个）。
ALTER TABLE ai_attempts ADD COLUMN platform text NOT NULL DEFAULT '';
UPDATE ai_attempts a SET platform = t.platform FROM ai_tasks t WHERE t.id = a.task_id;

-- 2. 审计冻结的工具链改为按平台（spec.platform_tools：Runner 平台 → 工具），领取时按节点的平台下发对应的那一份。
UPDATE ai_tasks SET spec = (spec - 'tools') || jsonb_build_object('platform_tools', jsonb_build_object(platform, spec -> 'tools'))
  WHERE type = 'review' AND spec ? 'tools';

-- 3. ai_tasks.platform（一个平台）改为 platforms（可以执行的平台，至少一个）：领取时 $平台 = ANY(platforms)。
ALTER TABLE ai_tasks ADD COLUMN platforms text[];
UPDATE ai_tasks SET platforms = ARRAY[platform];
ALTER TABLE ai_tasks
  ALTER COLUMN platforms SET NOT NULL,
  ADD CONSTRAINT ai_tasks_platforms CHECK (cardinality(platforms) > 0),
  DROP COLUMN platform;
