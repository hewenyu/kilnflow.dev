-- AI 开发的平台可以配置（PLAN #26.7）：development.platform 是单个平台或一组平台（any_of），哪台平台在其中、有空闲槽位的
-- 开发机先领到就由哪台执行。
--
-- 1. 开发冻结的工具链改为按平台（spec.platform_tools：Runner 平台 → 工具，与审计相同），领取时按节点的平台下发对应的那一份。
--    已有的开发任务只有一个平台（以前固定为 macOS arm64）：旧的 tools 归到任务的那个平台。platform_id 不再使用。
UPDATE ai_tasks SET spec = (spec - 'tools') || jsonb_build_object('platform_tools', jsonb_build_object(platforms[1], spec -> 'tools'))
  WHERE type = 'develop' AND spec ? 'tools';
UPDATE ai_tasks SET spec = spec - 'platform_id' WHERE type = 'develop' AND spec ? 'platform_id';

-- 2. ai_repo_state.dev_platforms：最近一次从默认分支读到的开发平台（与 dev_agent 一起记下）。空闲的开发机按它判断有没有
--    平台匹配的空闲槽位，不读 GitHub；NULL 表示还没读过（不按平台筛选），同步任务池时清空。
ALTER TABLE ai_repo_state ADD COLUMN dev_platforms text[];
