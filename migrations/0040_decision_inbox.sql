-- AI 开发的决策疑难点单向同步到 Outline「待拍板」清单。
--
-- repo_decision_docs：每个仓库最多一份「待决策清单」文档的配置。它和只读文档绑定（repo_doc_bindings）是两回事：
--   - 只读绑定冻结设计文档快照，Kiln 不修改外部文档；
--   - 这里的配置允许 Kiln 用独立、受限的写 token 往一篇专用的「待拍板」文档里追加条目，单向同步 AI 开发记录的决策疑难点。
-- token 与 secret / npm / 文档绑定一样用信封加密，AAD 绑定到仓库和 "decision_token"，不能挪到别的仓库或别的用途。
-- generation 是配置代次：新建由序列生成，重设时推进（token 轮换也推进，但不清空队列）；观测的篇幅与告警字段用于提醒管理员
-- 轮转到新文档（Outline 有单篇文档的篇幅上限）。
CREATE SEQUENCE repo_decision_docs_generation_seq;
CREATE TABLE repo_decision_docs (
  repo_id          bigint PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  provider         text   NOT NULL DEFAULT 'outline' CHECK (provider = 'outline'),
  base_url         text   NOT NULL CHECK (base_url <> ''),
  doc_id           uuid   NOT NULL,
  collection_id    text   NOT NULL CHECK (collection_id <> ''),
  token_ciphertext bytea  NOT NULL CHECK (octet_length(token_ciphertext) > 0),
  token_wrapped_dek bytea NOT NULL CHECK (octet_length(token_wrapped_dek) > 0),
  generation       bigint NOT NULL DEFAULT nextval('repo_decision_docs_generation_seq') CHECK (generation > 0),
  created_by       text   NOT NULL,
  updated_by       text   NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  -- observed_units 是最近一次按 UTF-16 code unit 量到的文档篇幅；warning_code 非空表示需要管理员轮转到新文档。
  observed_units   bigint CHECK (observed_units >= 0),
  observed_at      timestamptz,
  warning_code     text   NOT NULL DEFAULT '',
  warned_at        timestamptz
);
ALTER SEQUENCE repo_decision_docs_generation_seq OWNED BY repo_decision_docs.generation;

-- dev_decision_syncs：要同步到「待拍板」清单的每个条目（一次交付报告里的每条决策记录，以及一条受阻摘要）。
--
-- C19：没有幂等键的外部写操作先写意图、最多尝试一次。状态机：
--   pending   —— 意图已登记，等待第一次写入（或冲突 / 限流后的重试）；
--   attempted —— 已经（或可能已经）发出过一次写请求，只允许只读查证，绝不再写；
--   delivered —— 已确认写入（写入成功，或只读查证在正文里找到精确 marker）；
--   unknown   —— 写请求结果不明（5xx、超时、响应丢失）且只读查证没有找到 marker，或无法查证（配置轮转 / 权限丢失）；
--   cancelled —— 不会写入（配置清除 / 轮转 / 永久拒绝 / 篇幅超限 / 文档状态不符）。
-- delivered、cancelled 是终态；unknown 永不自动回到 pending，只能人工对账。
--
-- target_* 是入队时冻结的投递目标：之后配置轮换不影响已有条目的判定；payload 是入队时冻结的报告内容，payload_md 是第一次
-- 尝试前冻结的 Markdown 正文（之后即使冲突回退 pending 也复用，不重新渲染）。
-- marker 是不变的行内代码唯一标识，用来在文档里精确查证条目是否已经写入。
CREATE TABLE dev_decision_syncs (
  id                   bigserial PRIMARY KEY,
  repo_id              bigint  NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  work_item_id         bigint  NOT NULL REFERENCES dev_work_items(id),
  task_id              bigint  NOT NULL REFERENCES ai_tasks(id),
  entry_idx            integer NOT NULL CHECK (entry_idx >= 0),
  target_provider      text    NOT NULL CHECK (target_provider = 'outline'),
  target_base_url      text    NOT NULL CHECK (target_base_url <> ''),
  target_doc_id        uuid    NOT NULL,
  target_collection_id text    NOT NULL CHECK (target_collection_id <> ''),
  target_generation    bigint  NOT NULL CHECK (target_generation > 0),
  marker               text    NOT NULL UNIQUE,
  entry_kind           text    NOT NULL CHECK (entry_kind IN ('decision', 'blocked')),
  category             text    CHECK (category IN ('confirmed_decision', 'acceptance', 'policy', 'other')),
  -- confirmed_decision 是 category 的生成列：审计、Dashboard 可以直接按它筛选需要人确认（kiln:hold）的决策。
  confirmed_decision   boolean GENERATED ALWAYS AS (category IS NOT DISTINCT FROM 'confirmed_decision') STORED,
  report_sha256        text    NOT NULL CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
  payload              jsonb   NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  payload_md           text,
  state                text    NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending', 'attempted', 'delivered', 'unknown', 'cancelled')),
  attempts             integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at        timestamptz DEFAULT now(),
  last_error_code      text    NOT NULL DEFAULT '',
  last_error           text    NOT NULL DEFAULT '',
  attempted_revision   bigint  CHECK (attempted_revision >= 0),
  delivered_revision   bigint  CHECK (delivered_revision >= 0),
  attempted_at         timestamptz,
  delivered_at         timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  resolved_by          text,
  resolved_at          timestamptz,
  resolution_note      text,
  UNIQUE (repo_id, task_id, entry_idx),
  -- marker 必须完全由这些列拼出来，不能手写成别的值（防止伪造或碰撞）。
  CHECK (marker = 'kiln-dec:' || repo_id::text || ':wi' || work_item_id::text || ':t' || task_id::text || ':' || entry_idx::text),
  -- 决策条目一定要有类别；受阻摘要一定没有类别。
  CHECK ((entry_kind = 'decision' AND category IS NOT NULL) OR (entry_kind = 'blocked' AND category IS NULL)),
  -- 尝试过（attempts > 0）的条目一定有冻结的正文和尝试时间；没尝试过的条目两者都可以为空。
  CHECK (attempts = 0 OR (payload_md IS NOT NULL AND attempted_at IS NOT NULL)),
  -- attempted 状态必须有尝试次数和尝试时的修订号。
  CHECK (state <> 'attempted' OR (attempts > 0 AND attempted_revision IS NOT NULL)),
  -- delivered 状态必须记下送达时间。
  CHECK (state <> 'delivered' OR delivered_at IS NOT NULL),
  -- 只有 pending / attempted 有下次处理时间；终态（delivered / unknown / cancelled）一律为空。
  CHECK ((state IN ('pending', 'attempted')) = (next_retry_at IS NOT NULL))
);

-- 处理队列按到期时间扫描；同一仓库的处理按 id 顺序。终态条目不在部分索引里。
CREATE INDEX dev_decision_syncs_due ON dev_decision_syncs (next_retry_at, repo_id, id)
  WHERE state IN ('pending', 'attempted');
CREATE INDEX dev_decision_syncs_repo_state ON dev_decision_syncs (repo_id, state, id);
