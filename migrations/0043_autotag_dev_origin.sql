-- AI 开发 PR 的永久归属：Server 创建或核验过的正式 PR 记在 dev_pull_origins，自动打 tag 据此判断发布
-- 是否来自可信的 AI 开发实现工作项。归属与工作项的阶段、轮次、PR 编号无关，工作项换了阶段、轮次或者结束时都不删除：
-- 已经合并的实现 PR 仍然需要能认出它的来源。
--
-- (repo_id, pr_number) 唯一：一个 PR 只有一条归属；(work_item_id, phase, impl_round) 唯一：一个工作项的每个阶段、轮次
-- 只对应一个正式 PR。返修推进同一个 PR 时只更新交付引用，不改写身份列（work_item、阶段、轮次、分支）。
CREATE TABLE dev_pull_origins (
  repo_id          bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pr_number        int         NOT NULL,
  work_item_id     bigint      NOT NULL REFERENCES dev_work_items(id) ON DELETE CASCADE,
  phase            text        NOT NULL CHECK (phase IN ('design', 'implement')),
  impl_round       int         NOT NULL CHECK (impl_round >= 0),
  head_ref         text        NOT NULL CHECK (head_ref <> ''),
  base_ref         text        NOT NULL CHECK (base_ref <> ''),
  -- 平台返回的 PR 作者；由 Server 创建时为空（创建本身就是可信来源）
  creator_login    text        NOT NULL DEFAULT '',
  -- 交付在哪个任务、哪个 commit；返修推进同一个 PR 时更新
  delivery_task_id bigint,
  delivery_sha     text        NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, pr_number),
  UNIQUE (work_item_id, phase, impl_round)
);
CREATE INDEX dev_pull_origins_work_item ON dev_pull_origins (work_item_id);

-- 待核验的 PR 创建归属：创建 PR 的响应丢失（工作项 pr_number 还是空的），PR 却先被合并、合并事件先到时，
-- 合并事件按正式分支找到工作项，趁换阶段、换轮次改写 phase、impl_round、base_ref、交付引用之前冻结一条待核验记录。
-- aidev 的仓库执行者持有仓库锁时经平台读取这个 PR 核验（不是 fork、head/base 与冻结的记录一致、作者是本 App），
-- 通过后在同一事务中补记 dev_pull_origins 并删除这条记录；不通过（平台明确没有这个 PR，或者核验失败）只删除记录。
-- 绝不按分支直接授权写 dev_pull_origins。每个 PR 至多一条。
CREATE TABLE dev_pull_origin_checks (
  repo_id          bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pr_number        int         NOT NULL,
  work_item_id     bigint      NOT NULL REFERENCES dev_work_items(id) ON DELETE CASCADE,
  phase            text        NOT NULL CHECK (phase IN ('design', 'implement')),
  impl_round       int         NOT NULL CHECK (impl_round >= 0),
  head_ref         text        NOT NULL CHECK (head_ref <> ''),
  base_ref         text        NOT NULL CHECK (base_ref <> ''),
  delivery_task_id bigint,
  delivery_sha     text        NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, pr_number)
);

-- 自动打 tag 的记录补充发布来源的判定依据（PLAN #29.4）：PR 的分支、真实的发布发起者是否是机器人、以及由自动打 tag
-- 的执行者认定的 AI 开发工作项（审计关联，不加跨领域外键）。旧行保留默认值，不回填、不重发已经合并的 PR。
ALTER TABLE auto_tags
  ADD COLUMN work_item_id  bigint,
  ADD COLUMN head_ref      text    NOT NULL DEFAULT '',
  ADD COLUMN base_ref      text    NOT NULL DEFAULT '',
  ADD COLUMN sender_is_bot boolean NOT NULL DEFAULT false;
