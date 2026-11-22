-- PR 的 head 已经按哪些目标分支规划完成（PLAN #28.3）：planner 在 pull_request 事件的规划全部成功后写入，只增不改。
-- 合并队列只在当前 head 按默认分支规划完成后才看验证结果。不用 ref_heads（它是 PR 的当前位置，乱序到达的旧事件会覆盖它）。
-- 不回填：升级前的数据证明不了规划完成（逐个 workflow 提交，规划中途失败时已经有部分 run；基线也会记录 ref_heads），
-- 升级前打开的 PR 推送新的 commit、或关闭后重新打开（规划成功即记录，已有的 run 按 dedupe_key 不重复）后才能合并。
CREATE TABLE pr_plans (
  repo_id    bigint NOT NULL,
  pr_number  int    NOT NULL,
  sha        text   NOT NULL,
  base_ref   text   NOT NULL,
  planned_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, pr_number, sha, base_ref)
);
