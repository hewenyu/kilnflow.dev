-- AI 开发：自主设计，一个 PR 完成不了时先交付设计 PR（PLAN #31）。

-- 1. 工作项的阶段与轮次（#31.3）：
--    phase：design（先设计时的设计阶段）| implement；已有的工作项按原规则领取，为 implement；
--    design_pr、design_sha：已合并的设计 PR 和合并得到的 commit；
--    impl_round：实现的轮次（部分完成的实现 PR 合并后加一，下一轮用新的分支 kiln/dev-<id>-r<n>）；
--    round_base：本阶段、本轮开始时的 fix_rounds（fix_rounds 单调递增，返修上限按 fix_rounds - round_base 计算）；
--    round_ran：本轮是否执行过任务（开始新的轮次时为假，实现任务关联到工作项时为真；retry 据此选择起点）；
--    head_task_id：交付在正式分支上的任务（推进时记下；合并时是实际合并的那次交付），之后的轮次按它的报告取交接；
--    design_docs：已合并的设计文档（设计 PR 或部分完成的实现 PR 合并时，按合并的那次交付报告记下）。
ALTER TABLE dev_work_items
  ADD COLUMN phase       text    NOT NULL DEFAULT 'implement' CHECK (phase IN ('design', 'implement')),
  ADD COLUMN design_pr   int,
  ADD COLUMN design_sha  text    NOT NULL DEFAULT '',
  ADD COLUMN impl_round  int     NOT NULL DEFAULT 1,
  ADD COLUMN round_base  int     NOT NULL DEFAULT 0,
  ADD COLUMN round_ran   boolean NOT NULL DEFAULT true,
  ADD COLUMN head_task_id bigint REFERENCES ai_tasks(id),
  ADD COLUMN design_docs text[]  NOT NULL DEFAULT '{}';

-- 已有的工作项回填 head_task_id（推进时记下交付的任务；之前没有这一列）：已经开了 PR 的，没有待推进的交付时是最近一次
-- 交付的任务（delivery_task_id 推进后不清空）；有待推进的交付（delivering）时是它之前最近一次已交付（delivered）的报告
-- 所属的任务。
UPDATE dev_work_items SET head_task_id = delivery_task_id WHERE pr_number IS NOT NULL AND delivery_sha = '';
UPDATE dev_work_items w SET head_task_id = (
  SELECT r.task_id FROM dev_reports r
  WHERE r.work_item_id = w.id AND r.kind = 'delivery' AND r.report->>'status' = 'delivered' AND r.task_id IS DISTINCT FROM w.delivery_task_id
  ORDER BY r.created_at DESC LIMIT 1)
WHERE pr_number IS NOT NULL AND delivery_sha <> '';

-- 2. 实现任务（新的轮次开始后由执行者规划，#31.3）
ALTER TABLE ai_dev_requests DROP CONSTRAINT ai_dev_requests_mode_check;
ALTER TABLE ai_dev_requests ADD CONSTRAINT ai_dev_requests_mode_check CHECK (mode IN ('select', 'implement', 'fix'));

-- 3. attempt 分支所属的阶段和轮次：retry 只用本阶段、本轮的 checkpoint（#31.3）
ALTER TABLE dev_branches
  ADD COLUMN phase      text NOT NULL DEFAULT 'implement',
  ADD COLUMN impl_round int  NOT NULL DEFAULT 1;

-- 4. 依赖等待（#31.6）：waiting_on 是还没关闭的依赖 Issue，全部关闭后删除这条跳过记录；waiting_checked 为假时执行者
--    读一次平台上依赖的状态（登记之后、手动同步时）。
ALTER TABLE dev_issue_skips
  ADD COLUMN waiting_on      int[]   NOT NULL DEFAULT '{}',
  ADD COLUMN waiting_checked boolean NOT NULL DEFAULT true;
CREATE INDEX dev_issue_skips_unchecked ON dev_issue_skips (repo_id) WHERE NOT waiting_checked;

-- 5. 选题时按旧规则（要求一个 PR 能完成、取舍不明不选）写的跳过记录作废，按新规则重新评估（#31.7）。任务池只收带
--    kiln:ready 的 Issue：被人去掉 kiln:ready 的不会因此回来。
DELETE FROM dev_issue_skips WHERE reason LIKE '选题时未选%';
