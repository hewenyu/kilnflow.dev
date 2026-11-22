-- AI Runner 二期：AI 开发（PLAN #26.7–#26.16）。
--
-- 1. AI Runner 的角色由注册令牌决定（Server 授权，客户端不能修改）：review（审计）或 development（开发）。
--    已有的 AI 令牌和 AI Runner 都是审计（一期只有审计）；CI 的角色为空。
ALTER TABLE enrollment_tokens ADD COLUMN role text NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN role text NOT NULL DEFAULT '';
UPDATE enrollment_tokens SET role='review' WHERE kind='ai';
UPDATE runners SET role='review' WHERE kind='ai';
ALTER TABLE enrollment_tokens ADD CONSTRAINT enrollment_tokens_role CHECK
  ((kind='ci' AND role='') OR (kind='ai' AND role IN ('review','development')));
ALTER TABLE runners ADD CONSTRAINT runners_role CHECK
  ((kind='ci' AND role='') OR (kind='ai' AND role IN ('review','development')));

-- 2. AI 开发的待领取 job（jobs.kind='ai_develop'），与 jobs_pending_ai 相同的用途。
CREATE INDEX jobs_pending_ai_develop ON jobs (priority DESC, queued_at) WHERE state = 'pending' AND kind = 'ai_develop';

-- 3. 工作项（#26.8）：一批同时领取的 Issue + 一个分支 + 至多一个 PR，持续到合并或终止。同一个仓库可以有多个未结束的
--    工作项（由不同的开发机并行开发），能并行几个取决于空闲的开发机。
--    state：developing | delivering | pr_open | fixing | blocked（未结束）| merged | stopped | no_change（终态）。
CREATE TABLE dev_work_items (
  id                    bigserial PRIMARY KEY,
  repo_id               bigint      NOT NULL REFERENCES repos(id),
  state                 text        NOT NULL,
  -- 未结束的工作项 active 为真（与 state 同步写入）
  active                boolean     NOT NULL DEFAULT true,
  base_ref              text        NOT NULL,                 -- 目标分支（领取时的默认分支）
  base_sha              text        NOT NULL,                 -- 领取时目标分支的 head（选题、开发的起点）
  pr_number             int,
  head_sha              text        NOT NULL DEFAULT '',      -- 已知的正式分支 / PR head（期望的 head）
  fix_rounds            int         NOT NULL DEFAULT 0,
  max_fix_rounds        int         NOT NULL,
  agent                 text        NOT NULL,
  -- 领取它的 attempt（I3：一次执行至多领取一次）
  claim_attempt_id      bigint      NOT NULL UNIQUE,
  -- 开发过这个工作项的宿主（host fingerprint 的摘要，I8：这些宿主不能审计它的 PR）
  dev_hosts             text[]      NOT NULL DEFAULT '{}',
  plan                  text        NOT NULL DEFAULT '',
  -- 当前（或最近一次）为它执行的 job：选题 job，或者返修 job（fixing 时为空表示返修 job 还没有规划）
  job_id                bigint,
  -- 待推进的交付：attempt 分支和 commit（delivering 时非空）
  delivery_branch       text        NOT NULL DEFAULT '',
  delivery_sha          text        NOT NULL DEFAULT '',
  delivery_job_id       bigint,
  -- 已经为哪个 head 建过返修（同一个 head 不重复派单）
  fixed_head            text        NOT NULL DEFAULT '',
  hold_label            boolean     NOT NULL DEFAULT false,   -- 交付要人确认（敏感路径、提交说明里的关闭关键字等），PR 要加 kiln:hold
  -- 推进前决定要加 kiln:hold、还没有确认加上：合并队列不自动入队这个工作项的 PR（按 PR 编号或正式分支关联）
  hold_pending          boolean     NOT NULL DEFAULT false,
  requirements_changed  boolean     NOT NULL DEFAULT false,
  -- Issue 上的状态 label 已经按哪个状态同步过（与 state 不同时执行者同步一次）
  labels_state          text        NOT NULL DEFAULT '',
  reason                text        NOT NULL DEFAULT '',      -- 受阻、终止的原因，或者当前在等什么
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  finished_at           timestamptz,
  CHECK (active = (state IN ('developing','delivering','pr_open','fixing','blocked')))
);
CREATE INDEX dev_work_items_active ON dev_work_items (repo_id) WHERE active;
-- I6：一个 PR 只属于一个工作项
CREATE UNIQUE INDEX dev_work_items_pr ON dev_work_items (repo_id, pr_number) WHERE pr_number IS NOT NULL;
CREATE INDEX dev_work_items_repo ON dev_work_items (repo_id, id DESC);

-- 4. 工作项包含的 Issue。digest 是领取时标题 + 正文的 sha256（快照）；resolution 是交付报告给出的完成度。
CREATE TABLE dev_work_item_issues (
  work_item_id  bigint  NOT NULL REFERENCES dev_work_items(id),
  repo_id       bigint  NOT NULL,
  issue_number  int     NOT NULL,
  title         text    NOT NULL DEFAULT '',
  digest        text    NOT NULL,
  active        boolean NOT NULL DEFAULT true,
  -- Issue 已经关闭（issues.closed 事件）：全部关闭时工作项终止
  closed        boolean NOT NULL DEFAULT false,
  resolution    text    NOT NULL DEFAULT '',   -- '' | full | partial | none
  PRIMARY KEY (work_item_id, issue_number)
);
-- I1：一个 Issue 同时至多属于一个未结束的工作项
CREATE UNIQUE INDEX dev_work_item_issues_active ON dev_work_item_issues (repo_id, issue_number) WHERE active;

-- 5. 不再进入候选的 Issue（#26.8）：内容摘要不变就不再选；重新打上 kiln:ready 时删除。
CREATE TABLE dev_issue_skips (
  repo_id       bigint      NOT NULL,
  issue_number  int         NOT NULL,
  digest        text        NOT NULL,
  reason        text        NOT NULL,
  reconsider    text        NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, issue_number)
);

-- 5a. 任务池（#26.11）：有 kiln:ready 的打开的 Issue 在 Server 中的镜像，由 issues webhook 和手动同步（全量）维护。
--     in_pool 为假的行是离开任务池的 Issue（关闭、去掉 ready）：保留 issue_updated_at，晚到的旧事件不会把它加回来。
CREATE TABLE dev_pool (
  repo_id           bigint      NOT NULL REFERENCES repos(id),
  issue_number      int         NOT NULL,
  in_pool           boolean     NOT NULL,
  title             text        NOT NULL DEFAULT '',
  body              text        NOT NULL DEFAULT '',     -- 正文前 16 KiB（交给 Agent 的快照）
  labels            text[]      NOT NULL DEFAULT '{}',
  url               text        NOT NULL DEFAULT '',
  digest            text        NOT NULL DEFAULT '',     -- 完整标题 + 正文的 sha256
  issue_updated_at  timestamptz NOT NULL,                -- GitHub 上 Issue 的 updated_at：只接受不更旧的内容
  ready_at          timestamptz NOT NULL DEFAULT now(),  -- 进入任务池的时间（候选按它排序）
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, issue_number)
);
CREATE INDEX dev_pool_ready ON dev_pool (repo_id, ready_at) WHERE in_pool;

-- 5b. 交给一次执行的候选（领取时按它核对内容摘要）：Runner 准备好后向 Server 要候选时写入，领取冲突后给出的新候选覆盖它。
CREATE TABLE dev_offers (
  attempt_id    bigint      PRIMARY KEY,
  candidates    jsonb       NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 6. 每次执行的选题报告和交付报告（结果，永久保留）。
CREATE TABLE dev_reports (
  job_id        bigint      NOT NULL REFERENCES jobs(id),
  attempt_id    bigint      NOT NULL,
  kind          text        NOT NULL,          -- selection | delivery
  work_item_id  bigint      REFERENCES dev_work_items(id),
  report        jsonb       NOT NULL,
  -- 报告的适用范围（#26.14）：Runner / Agent 版本、模型（拿不到为空，显示为"未知"）
  runner_version text       NOT NULL DEFAULT '',
  agent_version  text       NOT NULL DEFAULT '',
  model          text       NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, kind)
);
CREATE INDEX dev_reports_work_item ON dev_reports (work_item_id, created_at);

-- 7. 要发的评论（没有幂等键，C19 至多一次，#26.12）：pending → attempted → done | unknown | failed（限流回到 pending）。
CREATE TABLE dev_comments (
  id            bigserial   PRIMARY KEY,
  work_item_id  bigint      NOT NULL REFERENCES dev_work_items(id),
  repo_id       bigint      NOT NULL,
  number        int         NOT NULL,          -- Issue 或 PR 编号
  kind          text        NOT NULL,          -- claimed | blocked | dispositions | stopped | no_change
  -- 同一个意图只写一次（例如同一轮的处置汇总）
  dedupe_key    text        NOT NULL UNIQUE,
  body          text        NOT NULL,
  state         text        NOT NULL DEFAULT 'pending',
  error         text        NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dev_comments_pending ON dev_comments (repo_id) WHERE state = 'pending';

-- 8. attempt 分支（交付或 checkpoint，#26.13）：推进后或创建 12 小时后删除。
CREATE TABLE dev_branches (
  repo_id       bigint      NOT NULL,
  branch        text        NOT NULL,
  sha           text        NOT NULL,
  work_item_id  bigint      REFERENCES dev_work_items(id),
  kind          text        NOT NULL,          -- delivery | checkpoint
  created_at    timestamptz NOT NULL DEFAULT now(),
  deleted_at    timestamptz,
  PRIMARY KEY (repo_id, branch)
);
CREATE INDEX dev_branches_live ON dev_branches (created_at) WHERE deleted_at IS NULL;

-- 9. 仓库的 AI 开发状态：
--    ai_dev_paused：暂停（kiln ai dev pause），不再领取新的 Issue，已有的工作项照常收尾；
--    ai_dev_select_seq：选题轮次（同一个仓库同时只有一台机器在选题），选题 run 的去重键带它，领取成功、没有领取或
--      选题 run 在领取前结束时加一；
--    ai_dev_examined：最近一次交给选题的任务池状态（可选的 Issue 与未结束的工作项的摘要），没有变化就不再选题；
--    ai_dev_pool_changed_at：任务池最近一次变化的时间（批量打 label 时等任务池静默一会儿再给出候选）；
--    ai_dev_synced_at：最近一次手动同步任务池的时间；
--    ai_dev_agent：最近一次从默认分支读到的开发 Agent（空闲的开发机按它判断能不能做，不读 GitHub；同步时清空）。
ALTER TABLE repos
  ADD COLUMN ai_dev_paused          boolean     NOT NULL DEFAULT false,
  ADD COLUMN ai_dev_select_seq      int         NOT NULL DEFAULT 0,
  ADD COLUMN ai_dev_examined        text        NOT NULL DEFAULT '',
  ADD COLUMN ai_dev_pool_changed_at timestamptz,
  ADD COLUMN ai_dev_synced_at       timestamptz,
  ADD COLUMN ai_dev_agent           text        NOT NULL DEFAULT '';

-- 10. 审计报告的适用范围（#26.14）。
ALTER TABLE ai_reviews
  ADD COLUMN policy_digest     text NOT NULL DEFAULT '',
  ADD COLUMN instructions_from text NOT NULL DEFAULT '',
  ADD COLUMN runner_version    text NOT NULL DEFAULT '',
  ADD COLUMN agent_version     text NOT NULL DEFAULT '',
  ADD COLUMN model             text NOT NULL DEFAULT '',
  -- 工作项的 PR 上的审计：finding 的稳定 ID 以 F<job id>-<序号> 引用
  ADD COLUMN work_item_id      bigint;
