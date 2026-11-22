-- Kiln 初始 schema（PLAN #5.2、#16.4）。迁移只能向前。

CREATE TABLE installations (
  id             bigint PRIMARY KEY,
  account_id     bigint NOT NULL,
  account_login  text   NOT NULL,
  account_type   text   NOT NULL DEFAULT 'User',
  suspended      boolean NOT NULL DEFAULT false,
  deleted        boolean NOT NULL DEFAULT false,
  updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE repos (
  id              bigint PRIMARY KEY,           -- GitHub 仓库 ID；Kiln 内部一律用 ID 关联
  installation_id bigint NOT NULL,
  owner_id        bigint NOT NULL,
  owner           text   NOT NULL,
  name            text   NOT NULL,
  private         boolean NOT NULL DEFAULT true,
  default_branch  text   NOT NULL DEFAULT 'main',
  clone_url       text   NOT NULL,
  active          boolean NOT NULL DEFAULT true,
  cache_salt      text   NOT NULL DEFAULT '',
  max_concurrency int    NOT NULL DEFAULT 8,
  baseline_done   boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX repos_name ON repos (lower(owner), lower(name));

CREATE TABLE ref_heads (
  repo_id    bigint NOT NULL,
  ref        text   NOT NULL,                   -- refs/heads/x | refs/tags/x | refs/pull/N/head
  sha        text   NOT NULL,                   -- commit sha（tag 已解开）
  object_sha text   NOT NULL DEFAULT '',        -- tag ref 指向的对象（annotated tag 为 tag 对象）
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, ref)
);

CREATE TABLE events (
  delivery_id     text PRIMARY KEY,
  event_type      text NOT NULL,
  action          text,
  installation_id bigint,
  payload         jsonb NOT NULL,
  received_at     timestamptz NOT NULL DEFAULT now(),
  source          text NOT NULL DEFAULT 'webhook',
  processed_at    timestamptz,
  attempts        int  NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  result          text,
  error           text
);
CREATE INDEX events_unprocessed ON events (next_attempt_at) WHERE processed_at IS NULL;

CREATE TABLE runs (
  id                 bigserial PRIMARY KEY,
  repo_id            bigint NOT NULL REFERENCES repos(id),
  workflow           text   NOT NULL,           -- 文件名，例如 ci.yml
  workflow_name      text   NOT NULL DEFAULT '',
  event              text   NOT NULL,
  ref                text   NOT NULL,
  sha                text   NOT NULL,
  source             text   NOT NULL,
  dedupe_key         text   NOT NULL UNIQUE,
  pr_number          int,
  sender_login       text   NOT NULL DEFAULT '',
  sender_is_bot      boolean NOT NULL DEFAULT false,
  current_rerun      int    NOT NULL DEFAULT 0,
  concurrency_key    text,
  cancel_in_progress boolean NOT NULL DEFAULT false,
  state              text   NOT NULL,           -- queued | active | succeeded | failed | cancelled | skipped
  config_error       text,
  warnings           text,
  github_ctx         jsonb  NOT NULL DEFAULT '{}',
  inputs             jsonb  NOT NULL DEFAULT '{}',
  commit_time        bigint NOT NULL DEFAULT 0,
  workflow_blob_sha  text   NOT NULL DEFAULT '',
  priority           int    NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz
);
CREATE INDEX runs_repo ON runs (repo_id, id DESC);
CREATE INDEX runs_open ON runs (state) WHERE state IN ('queued', 'active');
CREATE INDEX runs_concurrency ON runs (repo_id, concurrency_key) WHERE state IN ('queued', 'active');

CREATE TABLE run_reruns (
  run_id        bigint NOT NULL REFERENCES runs(id),
  rerun         int    NOT NULL,
  state         text   NOT NULL,
  conclusion    text,
  check_run_id  bigint,
  keep_on_failure boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  started_at    timestamptz,
  finished_at   timestamptz,
  PRIMARY KEY (run_id, rerun)
);

CREATE TABLE runners (
  id            bigserial PRIMARY KEY,
  pool          text   NOT NULL DEFAULT 'default',
  name          text   NOT NULL UNIQUE,
  public_key    bytea  NOT NULL,
  platforms     text[] NOT NULL DEFAULT '{}',
  executors     text[] NOT NULL DEFAULT '{}',
  capabilities  jsonb  NOT NULL DEFAULT '{}',
  version       text   NOT NULL DEFAULT '',
  state         text   NOT NULL DEFAULT 'active',   -- active | revoked
  drain         boolean NOT NULL DEFAULT false,
  running_jobs  int    NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz
);

CREATE TABLE enrollment_tokens (
  id          bigserial PRIMARY KEY,
  token_hash  text NOT NULL UNIQUE,
  pool        text NOT NULL DEFAULT 'default',
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  runner_id   bigint
);

CREATE TABLE auth_nonces (
  runner_id  bigint NOT NULL,
  nonce      text   NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (runner_id, nonce)
);

CREATE TABLE jobs (
  id                 bigserial PRIMARY KEY,
  kind               text   NOT NULL,           -- normal | publish | image | internal
  run_id             bigint REFERENCES runs(id),
  parent_job_id      bigint,
  rerun              int    NOT NULL DEFAULT 0,
  job_key            text   NOT NULL,
  job_name           text   NOT NULL,           -- workflow 中的 job ID
  display_name       text   NOT NULL,
  matrix             jsonb,
  spec               jsonb  NOT NULL,
  platform           text   NOT NULL,
  executor_kind      text   NOT NULL,
  needs              bigint[] NOT NULL DEFAULT '{}',
  action_key         text,
  key_parts          jsonb,
  input_digest       text,
  environment        text,
  config_version     bigint,
  last_skip_reason   text,
  reason             text,                      -- 跳过、失败、等待的原因
  state              text   NOT NULL,
  epoch              bigint NOT NULL DEFAULT 0,
  attempt_count      int    NOT NULL DEFAULT 0,
  priority           int    NOT NULL DEFAULT 1,
  current_attempt_id bigint,
  lease_expires_at   timestamptz,
  started_step       boolean NOT NULL DEFAULT false,
  cancel_requested   boolean NOT NULL DEFAULT false,
  cancel_reason      text,
  result_manifest    text,
  cached_from_job_id bigint,
  reused_from_job_id bigint,
  follows_job_id     bigint,
  effects            boolean NOT NULL DEFAULT false,
  force_execute      boolean NOT NULL DEFAULT false,
  keep_on_failure    boolean NOT NULL DEFAULT false,
  check_run_id       bigint,
  waiting_since      timestamptz,
  approved_by        text,
  approved_at        timestamptz,
  queued_at          timestamptz,
  started_at         timestamptz,
  finished_at        timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, rerun, job_key)
);
CREATE INDEX jobs_pending  ON jobs (priority DESC, queued_at) WHERE state = 'pending';
CREATE INDEX jobs_leased   ON jobs (lease_expires_at) WHERE state = 'running';
CREATE INDEX jobs_inflight ON jobs (action_key) WHERE state IN ('pending', 'running');
CREATE INDEX jobs_follows  ON jobs (follows_job_id) WHERE follows_job_id IS NOT NULL;
CREATE INDEX jobs_run      ON jobs (run_id, rerun);
CREATE INDEX jobs_waiting_approval ON jobs (id) WHERE state = 'waiting_approval';

CREATE TABLE attempts (
  id                 bigserial PRIMARY KEY,
  job_id             bigint NOT NULL REFERENCES jobs(id),
  epoch              bigint NOT NULL,
  runner_id          bigint NOT NULL REFERENCES runners(id),
  acquire_request_id text NOT NULL,
  outcome            text,
  error_class        text,
  exit_code          int,
  message            text,
  current_step       int NOT NULL DEFAULT -1,
  log_size           bigint NOT NULL DEFAULT 0,
  started_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz,
  cpu_seconds        double precision,
  UNIQUE (job_id, epoch)
);
CREATE INDEX attempts_request ON attempts (runner_id, acquire_request_id);

CREATE TABLE action_cache (
  action_key      text PRIMARY KEY,
  manifest_digest text NOT NULL,
  producer_job_id bigint NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_hit_at     timestamptz,
  suspect         boolean NOT NULL DEFAULT false
);

CREATE TABLE blobs (
  digest      text PRIMARY KEY,
  size        bigint NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  last_ref_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE outbox (
  id              bigserial PRIMARY KEY,
  dedupe_key      text UNIQUE,
  kind            text NOT NULL,
  installation_id bigint,
  payload         jsonb NOT NULL DEFAULT '{}',
  state           text NOT NULL DEFAULT 'pending',   -- pending | done | dead
  attempts        int  NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error      text,
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_due ON outbox (next_attempt_at) WHERE state = 'pending';

CREATE TABLE releases (
  id                 bigserial PRIMARY KEY,
  job_id             bigint NOT NULL UNIQUE,
  run_id             bigint NOT NULL,
  source_repo_id     bigint NOT NULL,
  target_repo        text   NOT NULL,           -- owner/name
  tag                text   NOT NULL,
  state              text   NOT NULL DEFAULT 'running',   -- running | succeeded | failed
  stage              text   NOT NULL DEFAULT 'prepare',
  github_release_id  bigint,
  progress           jsonb  NOT NULL DEFAULT '{}',
  error              text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE environments (
  id            bigserial PRIMARY KEY,
  owner_id      bigint NOT NULL,
  repo_id       bigint,
  name          text   NOT NULL,
  ref_policy    jsonb  NOT NULL DEFAULT '[]',
  reviewers     text[] NOT NULL DEFAULT '{}',    -- GitHub 登录名
  wait_timer    interval,
  sync_github_deployments boolean NOT NULL DEFAULT false,
  allowed_publish_repos text[] NOT NULL DEFAULT '{}',  -- owner/name
  UNIQUE NULLS NOT DISTINCT (owner_id, repo_id, name)
);

CREATE TABLE variables (
  id          bigserial PRIMARY KEY,
  scope_kind  text   NOT NULL,                  -- owner | repo | environment
  scope_id    bigint NOT NULL,
  name        text   NOT NULL,
  value       text   NOT NULL,
  UNIQUE (scope_kind, scope_id, name)
);

CREATE TABLE secrets (
  id          bigserial PRIMARY KEY,
  scope_kind  text   NOT NULL,
  scope_id    bigint NOT NULL,
  name        text   NOT NULL,
  ciphertext  bytea  NOT NULL,
  wrapped_dek bytea  NOT NULL,
  updated_by  text   NOT NULL DEFAULT '',
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (scope_kind, scope_id, name)
);

CREATE TABLE config_versions (
  version     bigserial PRIMARY KEY,
  owner_id    bigint NOT NULL,
  change      jsonb  NOT NULL,
  changed_by  text   NOT NULL DEFAULT '',
  changed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
  id         bigserial PRIMARY KEY,
  actor      text   NOT NULL,
  action     text   NOT NULL,
  detail     jsonb  NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE kv (
  key   text PRIMARY KEY,
  value text NOT NULL
);
