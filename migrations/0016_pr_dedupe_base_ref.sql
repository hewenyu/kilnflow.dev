-- PR run 的 dedupe_key 加上目标分支（PLAN #28.3，planner.dedupeKey）：把已有的 pull_request run 改成新格式，
-- 升级后同一 PR / head / 目标分支 / workflow 的事件（重投递、ready_for_review 等）仍命中原来的 run，
-- 不会重复规划并通过并发组取消原来的验证。只改旧格式（<repo>:<workflow>:<event>:<ref>:<sha>）的 key；
-- AI 审计、手动触发的 key 格式不同，不受影响。可以重复执行。
UPDATE runs
   SET dedupe_key = dedupe_key || ':' || (github_ctx->>'base_ref')
 WHERE event = 'pull_request'
   AND github_ctx->>'base_ref' IS NOT NULL
   AND dedupe_key = repo_id || ':' || workflow || ':' || event || ':' || ref || ':' || sha;
