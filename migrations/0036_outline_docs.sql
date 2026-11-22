-- 仓库与文档系统（Outline）的绑定、文档水位和工作项文档快照。
--
-- 全部是新增表，不改已有的表和数据：升级前没有绑定的仓库行为和以前一样，绑定后才会同步文档。
--
-- repo_doc_bindings：每个仓库最多一份文档绑定。token 与 secret / npm 一样用信封加密（PLAN #16.3），
--   token_ciphertext / token_wrapped_dek 是密文，AAD 绑定到仓库和 "token"，不能挪到别的仓库使用。
-- doc_watermarks：每个仓库、每篇文档记录已经看到（seen_rev）和已经处理过（triaged_rev）的修订号，用于增量同步。
-- work_item_doc_snapshots：工作项关联的文档快照（标题、修订号、内容摘要和正文），随工作项一起删除。
CREATE TABLE repo_doc_bindings (
  repo_id           bigint PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  provider          text NOT NULL DEFAULT 'outline',
  base_url          text NOT NULL,
  collection_id     text NOT NULL,
  collection_name   text NOT NULL DEFAULT '',
  token_ciphertext  bytea NOT NULL,
  token_wrapped_dek bytea NOT NULL,
  hook_enabled      boolean NOT NULL DEFAULT false,
  created_by        text NOT NULL DEFAULT '',
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE doc_watermarks (
  repo_id     bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  doc_id      text NOT NULL,
  seen_rev    bigint NOT NULL DEFAULT 0,
  triaged_rev bigint NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, doc_id)
);

CREATE TABLE work_item_doc_snapshots (
  work_item_id   bigint NOT NULL REFERENCES dev_work_items(id) ON DELETE CASCADE,
  doc_id         text NOT NULL,
  title          text NOT NULL DEFAULT '',
  revision       bigint NOT NULL DEFAULT 0,
  content_sha256 text NOT NULL DEFAULT '',
  content        text NOT NULL DEFAULT '',
  url            text NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (work_item_id, doc_id)
);

-- work_item_doc_freezes：工作项是否已经完成过一次文档冻结（含空集合）。冻结以工作项为单位原子完成，
-- 记录完成标记之后不再重新拉取，保证开发与审计读到同一份快照；doc_count 便于排查。随工作项级联删除。
CREATE TABLE work_item_doc_freezes (
  work_item_id bigint PRIMARY KEY REFERENCES dev_work_items(id) ON DELETE CASCADE,
  doc_count    integer NOT NULL DEFAULT 0,
  frozen_at    timestamptz NOT NULL DEFAULT now()
);
