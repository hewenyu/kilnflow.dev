-- 合并队列合并的 PR 关联的 issue（PLAN #28.3）：GitHub 只在用户合并时按 Closes #N 等关键字关闭 issue，
-- App 合并时不会，由队列的执行者在处理仓库时关闭（与合并结果同一事务标记为 pending，失败时下一次处理重试）。
-- 进度和失败原因写在 status 中（Dashboard 的最近合并中显示）。
ALTER TABLE merge_queue
  ADD COLUMN issues_state  text  NOT NULL DEFAULT '',   -- '' 不需要（出队、在队列之外合并、升级前合并的）| pending | done | failed（放弃）
  ADD COLUMN issues_closed int[] NOT NULL DEFAULT '{}', -- Kiln 已经关闭的 issue：之后被人重新打开的不再关闭
  ADD COLUMN issues_tries  int   NOT NULL DEFAULT 0,    -- 失败次数（限流不计）
  -- 每个 issue 第一次请求关闭之前它最新的事件 ID（{"3": 12345}）：关闭结果未知（响应丢失、记录前重启）时，
  -- 只有这之后本 App 的 closed 事件才算这次关闭成功（不受时钟偏差和其他 PR 的关闭影响）。
  ADD COLUMN issues_marks  jsonb NOT NULL DEFAULT '{}';
CREATE INDEX merge_queue_issues_pending ON merge_queue (repo_id) WHERE issues_state = 'pending';
-- action 另有取值 merging：已请求合并、结果未知（响应丢失或记录结果前重启），之后看到 PR 已合并时按队列合并的处理。
