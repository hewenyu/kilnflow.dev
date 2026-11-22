-- 多目标分支（PLAN #28.5、#29.4）：合并队列与自动打 tag 不再写死仓库默认分支。
--
-- merge_queue.base_ref：排队项最近观测到的目标分支。用于展示、按目标分支计算排位、action=updating 期间 PR 改投目标分支的
-- 检测，以及审计沿用核对。执行者每次调度仍以平台上的 PR 目标分支（Pull.BaseRef）为唯一权威，读到后回填这里。旧行为空串
-- （按 PR 的实际目标分支回填）。
ALTER TABLE merge_queue ADD COLUMN base_ref text NOT NULL DEFAULT '';

-- merge_queue.action_base：发起 action=updating 时那次更新的目标分支。期间 PR 改投分支时，接受更新产生的新 head 必须核对它
-- （dev 更新出来的合并提交不能当成 main 的更新接受），审计沿用（UpdatedFrom）也只按它判断来源。旧行为空串（视为未知，
-- 按 base_ref 兼容判断）。
ALTER TABLE merge_queue ADD COLUMN action_base text NOT NULL DEFAULT '';

-- auto_tags.default_branch_at_merge：合并那一刻的默认分支快照。默认分支改过名时，迟到的 webhook 仍按合并当时的默认分支
-- 裁决（省略 tag.branch 时它是发版分支）。旧行为空串（按仓库现在的默认分支）。
ALTER TABLE auto_tags ADD COLUMN default_branch_at_merge text NOT NULL DEFAULT '';

-- auto_tags.skip_code：结构化的跳过原因码。branch_not_release 表示合并进的不是发版分支（不自动打 tag，也不在页面上刷屏）；
-- 其他跳过、正在处理或失败时为空。Retry 和同一 commit 的重新记录会清回空串。
ALTER TABLE auto_tags ADD COLUMN skip_code text NOT NULL DEFAULT '';

-- auto_tag_candidates：同一个 commit 还在 pending 时从另一个分支合入（例如先记录 dev、还没处理就从 master 合入同一个
-- commit，或者两个 webhook 乱序到达）留下的待裁决候选。auto_tags 每个 (repo_id, sha) 只有一行，pending 时不能直接被
-- 后来的记录覆盖（否则乱序晚到的 dev 会反过来覆盖 master）；执行者离开 pending 之前按候选里能发版的那一条重新裁决，
-- 之后清空这个 commit 的候选，绝不残留。旧数据没有候选。
CREATE TABLE auto_tag_candidates (
  id                      bigserial   PRIMARY KEY,
  repo_id                 bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  sha                     text        NOT NULL,
  pr_number               int         NOT NULL,
  labels                  text[]      NOT NULL DEFAULT '{}',
  merged_by               text        NOT NULL DEFAULT '',
  sender_is_bot           boolean     NOT NULL DEFAULT false,
  head_ref                text        NOT NULL DEFAULT '',
  base_ref                text        NOT NULL DEFAULT '',
  default_branch_at_merge text        NOT NULL DEFAULT '',
  source                  text        NOT NULL,
  created_at              timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repo_id, sha, base_ref)
);
CREATE INDEX auto_tag_candidates_repo_sha_idx ON auto_tag_candidates (repo_id, sha, id);
