-- CI 与 AI 领域拆分（PLAN #30）：Runner 分为共用身份和 CI / AI 各自的表，AI 任务搬出 runs / jobs / attempts，
-- 放进 AI 自己的三层表（请求 → 任务 → attempt）。迁移在 Server 启动时的一个事务中完成，只能前进。
--
-- 迁移前的 AI 数据不迁移，直接删除（用户决定，#30.11）：AI 审计、开发的 run / job / attempt，审计报告、工作项，以及 AI Runner
-- 的注册。新版 kiln-ai-runner 重新注册；执行中的 AI 任务随之结束；GitHub 上已有的 AI Check Run 不再更新，PR 推送新的 commit
-- 后重新审计。CI Runner 和 CI 的数据不受影响。

-- ============================================================================================================
-- 1. Runner：共用身份（runners）+ CI（ci_runners）+ AI（ai_runners、ai_runner_agents）；CI Runner 原样迁移，
--    AI Runner 在第 3 步删除（需要重新注册）
-- ============================================================================================================

CREATE TABLE ci_runners (
  runner_id         bigint      PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
  pool              text        NOT NULL DEFAULT 'default',
  platforms         text[]      NOT NULL DEFAULT '{}',
  executors         text[]      NOT NULL DEFAULT '{}',
  -- 主机指纹、上报的空闲资源（free_cpu、free_memory）
  capabilities      jsonb       NOT NULL DEFAULT '{}',
  capacity          jsonb,
  running_jobs      int         NOT NULL DEFAULT 0,
  cache_report      jsonb,
  cache_reported_at timestamptz
);
INSERT INTO ci_runners (runner_id, pool, platforms, executors, capabilities, capacity, running_jobs, cache_report, cache_reported_at)
SELECT id, pool, platforms, executors, capabilities - 'ai_protocol' - 'agents' - 'host_id', capacity, running_jobs, cache_report, cache_reported_at
FROM runners WHERE kind = 'ci';

CREATE TABLE ai_runners (
  runner_id         bigint      PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
  role              text        NOT NULL CHECK (role IN ('review', 'development')),
  platform          text        NOT NULL DEFAULT '',
  -- AI 协议版本（agentcfg.AIProtocol）：不是 Server 支持的版本时不派任务
  protocol          int         NOT NULL DEFAULT 0,
  host_id           text        NOT NULL DEFAULT '',
  host_fingerprint  jsonb       NOT NULL DEFAULT '{}',
  herdr_state       text        NOT NULL DEFAULT 'unknown' CHECK (herdr_state IN ('ready', 'unavailable', 'unknown')),
  herdr_reason      text        NOT NULL DEFAULT '',
  docker_state      text        NOT NULL DEFAULT 'unknown' CHECK (docker_state IN ('ready', 'unavailable', 'unknown')),
  docker_reason     text        NOT NULL DEFAULT '',
  running           int         NOT NULL DEFAULT 0,
  reported_at       timestamptz
);

-- 只有 Runner 对外提供的 Agent 才有行；installed 为 NULL 表示没有上报
CREATE TABLE ai_runner_agents (
  runner_id         bigint      NOT NULL REFERENCES ai_runners(runner_id) ON DELETE CASCADE,
  agent             text        NOT NULL,
  installed         boolean,
  version           text        NOT NULL DEFAULT '',
  state             text        NOT NULL CHECK (state IN ('available', 'unavailable')),
  reason            text        NOT NULL DEFAULT '',
  checked_at        timestamptz,
  backoff_until     timestamptz,
  PRIMARY KEY (runner_id, agent)
);

ALTER TABLE runners DROP CONSTRAINT runners_role;
ALTER TABLE runners
  DROP COLUMN pool, DROP COLUMN platforms, DROP COLUMN executors, DROP COLUMN capabilities, DROP COLUMN running_jobs,
  DROP COLUMN capacity, DROP COLUMN cache_report, DROP COLUMN cache_reported_at, DROP COLUMN role;
ALTER TABLE runners ADD CONSTRAINT runners_kind CHECK (kind IN ('ci', 'ai'));

-- ============================================================================================================
-- 2. AI 的仓库状态（原 repos.ai_dev_*）
-- ============================================================================================================

CREATE TABLE ai_repo_state (
  repo_id               bigint      PRIMARY KEY REFERENCES repos(id),
  dev_paused            boolean     NOT NULL DEFAULT false,
  dev_select_seq        int         NOT NULL DEFAULT 0,
  dev_examined          text        NOT NULL DEFAULT '',
  dev_pool_changed_at   timestamptz,
  dev_synced_at         timestamptz,
  dev_agent             text        NOT NULL DEFAULT ''
);
INSERT INTO ai_repo_state (repo_id, dev_paused, dev_select_seq, dev_examined, dev_pool_changed_at, dev_synced_at, dev_agent)
SELECT id, ai_dev_paused, ai_dev_select_seq, ai_dev_examined, ai_dev_pool_changed_at, ai_dev_synced_at, ai_dev_agent FROM repos
WHERE ai_dev_paused OR ai_dev_select_seq <> 0 OR ai_dev_examined <> '' OR ai_dev_pool_changed_at IS NOT NULL
   OR ai_dev_synced_at IS NOT NULL OR ai_dev_agent <> '';
ALTER TABLE repos
  DROP COLUMN ai_dev_paused, DROP COLUMN ai_dev_select_seq, DROP COLUMN ai_dev_examined, DROP COLUMN ai_dev_pool_changed_at,
  DROP COLUMN ai_dev_synced_at, DROP COLUMN ai_dev_agent;

-- ============================================================================================================
-- 3. 删除迁移前的 AI 记录
-- ============================================================================================================

-- 引用 AI 的 outbox 记录（Check Run、PR review）
DELETE FROM outbox o WHERE o.kind = 'ai.pr_review'
   OR (o.kind = 'checkrun.job' AND EXISTS (SELECT 1 FROM jobs j WHERE j.id = (o.payload->>'job_id')::bigint AND j.kind IN ('ai_review', 'ai_develop')))
   OR (o.kind = 'checkrun.run' AND EXISTS (SELECT 1 FROM runs r WHERE r.id = (o.payload->>'run_id')::bigint AND r.workflow IN ('ai-review', 'ai-develop')));

-- 审计报告和 AI 开发的工作项（引用旧的 job、attempt）；任务池（dev_pool）和跳过记录保留
TRUNCATE ai_reviews, dev_reports, dev_offers, dev_comments, dev_branches, dev_work_item_issues, dev_work_items;

DELETE FROM attempts a USING jobs j WHERE j.id = a.job_id AND j.kind IN ('ai_review', 'ai_develop');
DELETE FROM jobs WHERE kind IN ('ai_review', 'ai_develop');
DELETE FROM run_reruns rr USING runs r WHERE r.id = rr.run_id AND r.workflow IN ('ai-review', 'ai-develop');
DELETE FROM runs WHERE workflow IN ('ai-review', 'ai-develop');
-- 旧的 AI Runner（runner.v1）：删除注册，新版 kiln-ai-runner 重新注册（升级请求随之删除）
DELETE FROM runners WHERE kind = 'ai';
DROP INDEX jobs_pending_ai;
DROP INDEX jobs_pending_ai_develop;

-- ============================================================================================================
-- 4. AI 任务：请求 → 任务 → attempt
-- ============================================================================================================

-- 一次审计（所有平台）或一次开发执行；请求级 Check Run 的对象。
-- state：queued（并发组中排队）| active | succeeded | failed | cancelled | skipped
CREATE TABLE ai_requests (
  id            bigserial   PRIMARY KEY,
  type          text        NOT NULL CHECK (type IN ('review', 'develop')),
  repo_id       bigint      NOT NULL REFERENCES repos(id),
  event         text        NOT NULL,
  ref           text        NOT NULL,
  sha           text        NOT NULL,
  trigger       text        NOT NULL DEFAULT '',
  source        text        NOT NULL DEFAULT '',
  sender        text        NOT NULL DEFAULT '',
  -- 只写在首次请求上；重跑的请求为空
  dedupe_key    text        UNIQUE,
  -- 请求族：首次请求指向自己，重跑（不论重跑几次）都指向首次请求
  family_id     bigint      NOT NULL REFERENCES ai_requests(id),
  -- 触发重跑的 webhook delivery（GitHub 上的 Re-run）；Dashboard、CLI 的重跑为空
  delivery_id   text,
  -- 审计 'review:pr:<N>'（取代），返修 'fix:work-item:<id>'（排队）；同一仓库内有效
  group_key     text,
  rerun_of      bigint      REFERENCES ai_requests(id),
  state         text        NOT NULL CHECK (state IN ('queued', 'active', 'succeeded', 'failed', 'cancelled', 'skipped')),
  config_error  text,
  warnings      text,
  check_run_id  bigint,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  finished_at   timestamptz,
  purged_at     timestamptz,
  UNIQUE (family_id, delivery_id)
);
CREATE INDEX ai_requests_repo ON ai_requests (repo_id, id DESC);
CREATE INDEX ai_requests_open ON ai_requests (state) WHERE state IN ('queued', 'active');
CREATE INDEX ai_requests_group ON ai_requests (repo_id, group_key) WHERE state IN ('queued', 'active');
CREATE INDEX ai_requests_unpurged ON ai_requests (finished_at) WHERE finished_at IS NOT NULL AND purged_at IS NULL;

-- 审计请求的明细：PR（手动审计为空）、分支、base、配置与审计规则、所属工作项、沿用的来源
CREATE TABLE ai_review_requests (
  request_id    bigint      PRIMARY KEY REFERENCES ai_requests(id),
  pr_number     int,
  head_ref      text        NOT NULL DEFAULT '',
  base_ref      text        NOT NULL DEFAULT '',
  base_sha      text        NOT NULL DEFAULT '',
  config_sha    text        NOT NULL DEFAULT '',
  policy_digest text        NOT NULL DEFAULT '',
  work_item_id  bigint      REFERENCES dev_work_items(id),
  reused_from   bigint      REFERENCES ai_requests(id)
);
CREATE INDEX ai_review_requests_pr ON ai_review_requests (pr_number) WHERE pr_number IS NOT NULL;

-- 开发请求的明细：选题或返修、返修的工作项和轮次
CREATE TABLE ai_dev_requests (
  request_id    bigint      PRIMARY KEY REFERENCES ai_requests(id),
  mode          text        NOT NULL CHECK (mode IN ('select', 'fix')),
  work_item_id  bigint      REFERENCES dev_work_items(id),
  round         int         NOT NULL DEFAULT 0
);

-- 一个平台上的一次执行；领取、租约、任务级 Check Run 的对象。
-- state：waiting（请求在并发组中排队）| pending | running | succeeded | failed | timed_out | cancelled | skipped
CREATE TABLE ai_tasks (
  id                 bigserial   PRIMARY KEY,
  request_id         bigint      NOT NULL REFERENCES ai_requests(id),
  key                text        NOT NULL,
  type               text        NOT NULL,
  platform           text        NOT NULL,
  agent              text        NOT NULL,
  display_name       text        NOT NULL,
  -- 冻结的执行输入（任务类型的内容，例如审计的 PR、base、说明）；所需协议版本由任务类型计算，不存
  spec               jsonb       NOT NULL,
  state              text        NOT NULL CHECK (state IN ('waiting', 'pending', 'running', 'succeeded', 'failed', 'timed_out', 'cancelled', 'skipped')),
  reason             text,
  last_skip_reason   text,
  priority           int         NOT NULL DEFAULT 1,
  timeout_minutes    int         NOT NULL,
  epoch              bigint      NOT NULL DEFAULT 0,
  attempt_count      int         NOT NULL DEFAULT 0,
  current_attempt_id bigint,
  lease_expires_at   timestamptz,
  cancel_requested   boolean     NOT NULL DEFAULT false,
  cancel_reason      text,
  -- 沿用（重跑只重跑失败的、合并队列更新后）时指向结果的来源
  reused_from        bigint      REFERENCES ai_tasks(id),
  check_run_id       bigint,
  created_at         timestamptz NOT NULL DEFAULT now(),
  queued_at          timestamptz,
  started_at         timestamptz,
  finished_at        timestamptz,
  UNIQUE (request_id, key)
);
CREATE INDEX ai_tasks_pending ON ai_tasks (priority DESC, queued_at) WHERE state = 'pending';
CREATE INDEX ai_tasks_leased ON ai_tasks (lease_expires_at) WHERE state = 'running';

CREATE TABLE ai_attempts (
  id                 bigserial   PRIMARY KEY,
  task_id            bigint      NOT NULL REFERENCES ai_tasks(id),
  epoch              bigint      NOT NULL,
  runner_id          bigint      REFERENCES runners(id) ON DELETE SET NULL,
  runner_name        text,
  host_id            text        NOT NULL DEFAULT '',
  acquire_request_id text        NOT NULL,
  outcome            text,
  error_class        text,
  message            text,
  progress           jsonb,
  log_size           bigint      NOT NULL DEFAULT 0,
  runner_version     text        NOT NULL DEFAULT '',
  agent_version      text        NOT NULL DEFAULT '',
  agent_model        text        NOT NULL DEFAULT '',
  started_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz,
  UNIQUE (task_id, epoch),
  CONSTRAINT ai_attempts_runner_known CHECK (runner_id IS NOT NULL OR runner_name IS NOT NULL)
);
CREATE INDEX ai_attempts_request ON ai_attempts (runner_id, acquire_request_id);

ALTER TABLE ai_tasks ADD CONSTRAINT ai_tasks_current_attempt FOREIGN KEY (current_attempt_id) REFERENCES ai_attempts(id);

-- ---------- 引用 AI 任务的表改指 AI 表 ----------

ALTER TABLE ai_reviews DROP CONSTRAINT ai_reviews_job_id_fkey, DROP CONSTRAINT ai_reviews_run_id_fkey;
ALTER TABLE ai_reviews DROP COLUMN run_id;
ALTER TABLE ai_reviews RENAME COLUMN job_id TO task_id;
ALTER TABLE ai_reviews
  ADD CONSTRAINT ai_reviews_task_id_fkey FOREIGN KEY (task_id) REFERENCES ai_tasks(id),
  ADD CONSTRAINT ai_reviews_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES ai_attempts(id),
  ADD CONSTRAINT ai_reviews_work_item_id_fkey FOREIGN KEY (work_item_id) REFERENCES dev_work_items(id);

ALTER TABLE dev_reports DROP CONSTRAINT dev_reports_job_id_fkey;
ALTER TABLE dev_reports RENAME COLUMN job_id TO task_id;
ALTER TABLE dev_reports
  ADD CONSTRAINT dev_reports_task_id_fkey FOREIGN KEY (task_id) REFERENCES ai_tasks(id),
  ADD CONSTRAINT dev_reports_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES ai_attempts(id);

ALTER TABLE dev_work_items RENAME COLUMN job_id TO task_id;
ALTER TABLE dev_work_items RENAME COLUMN delivery_job_id TO delivery_task_id;
ALTER TABLE dev_work_items
  ADD CONSTRAINT dev_work_items_task_id_fkey FOREIGN KEY (task_id) REFERENCES ai_tasks(id),
  ADD CONSTRAINT dev_work_items_delivery_task_id_fkey FOREIGN KEY (delivery_task_id) REFERENCES ai_tasks(id),
  ADD CONSTRAINT dev_work_items_claim_attempt_id_fkey FOREIGN KEY (claim_attempt_id) REFERENCES ai_attempts(id);

ALTER TABLE dev_offers ADD CONSTRAINT dev_offers_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES ai_attempts(id) ON DELETE CASCADE;
