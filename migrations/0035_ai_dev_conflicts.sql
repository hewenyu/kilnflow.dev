-- AI 开发的合并冲突返修（PLAN #26.11、#28.3）。
--
-- dev_work_items：
--   conflict_head、conflict_base 是待处理的冲突（合并队列交回时写入：在哪个 PR head 上、与目标分支的哪个 commit 冲突），
--   执行者开始返修或判断冲突过期后清空；
--   fixed_conflict 是已经为哪个 "head:base" 开过冲突返修（同一个冲突只返修一次；同一个 head 面对新的 base 是新的冲突）；
--   round_conflicts 是本阶段、本轮做过的冲突返修数（与审计 / CI 返修分开计预算，上限同为 max_fix_rounds；新的轮次归零）。
-- merge_queue：
--   removal 是自动出队的类别：conflict_dev（冲突，已交给 AI 开发返修）、conflict_author（冲突，等待提交者解决），
--   其他出队为空；conflict_base 是交回 AI 开发时目标分支的 head。
-- 只加列（默认值为空），不改已有的行：升级前的工作项和队列项照常处理。
ALTER TABLE dev_work_items
    ADD COLUMN conflict_head text NOT NULL DEFAULT '',
    ADD COLUMN conflict_base text NOT NULL DEFAULT '',
    ADD COLUMN fixed_conflict text NOT NULL DEFAULT '',
    ADD COLUMN round_conflicts integer NOT NULL DEFAULT 0;

ALTER TABLE merge_queue
    ADD COLUMN removal text NOT NULL DEFAULT '',
    ADD COLUMN conflict_base text NOT NULL DEFAULT '';
