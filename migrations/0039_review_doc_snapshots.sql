-- 独立 PR 审计与手动全面审计的文档冻结快照。
--
-- 工作项快照（work_item_doc_snapshots / work_item_doc_freezes）以工作项为单位冻结一次；独立 PR 和手动审计没有工作项，
-- 因此新增按请求冻结的文档快照集 doc_snapshot_sets / doc_snapshot_docs，供审计请求直接引用。两边的正文都是过程数据，
-- 保留期后清空正文、保留元数据（doc_id、标题、修订号、摘要、来源与清理时间）。
--
-- doc_snapshot_sets：一次冻结的整批文档（含空集）。binding_generation 是冻结时仓库绑定代次，manifest_digest 是整批文档
--   的确定性摘要（含来源与每篇文档的元数据），用于重跑与沿用时的依据核对。content_purged_at 非空表示正文已清理。
CREATE TABLE doc_snapshot_sets (
  id                 bigserial PRIMARY KEY,
  repo_id            bigint  NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  provider           text    NOT NULL,
  base_url           text    NOT NULL,
  collection_id      text    NOT NULL,
  collection_name    text    NOT NULL DEFAULT '',
  binding_generation bigint  NOT NULL CHECK (binding_generation > 0),
  doc_count          integer NOT NULL CHECK (doc_count >= 0),
  manifest_digest    text    NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
  frozen_at          timestamptz NOT NULL DEFAULT now(),
  content_purged_at  timestamptz
);

-- doc_snapshot_docs：快照集中的每篇文档。正文在保留期后清空（content=''），摘要列保留用于核对。
CREATE TABLE doc_snapshot_docs (
  snapshot_set_id bigint NOT NULL REFERENCES doc_snapshot_sets(id) ON DELETE CASCADE,
  doc_id          text   NOT NULL CHECK (doc_id <> ''),
  title           text   NOT NULL,
  revision        bigint NOT NULL,
  content_sha256  text   NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
  content         text   NOT NULL,
  url             text   NOT NULL,
  PRIMARY KEY (snapshot_set_id, doc_id)
);

-- 保留清理按"未清理 + 冻结时间"扫描，部分索引避免扫描已清理的历史集。
CREATE INDEX doc_snapshot_sets_unpurged ON doc_snapshot_sets (frozen_at, id)
  WHERE content_purged_at IS NULL;

-- ai_review_requests：记录审计请求固定的文档依据。docs_source 取值：
--   snapshot_set（独立审计冻结的快照集）、work_item（工作项快照）、none（没有可用的文档依据，含绑定空集合的观察结果）、
--   legacy（迁移前的请求，未记录依据）、unplanned（配置错误、没有进入冻结的请求族）。
-- 外键使用默认 NO ACTION/RESTRICT：清理不得擦除历史依据（保留元数据），引用关系必须保持可核对。
ALTER TABLE ai_review_requests
  ADD COLUMN doc_snapshot_set_id  bigint REFERENCES doc_snapshot_sets(id),
  ADD COLUMN docs_source          text   NOT NULL DEFAULT 'legacy',
  ADD COLUMN docs_manifest_digest text   NOT NULL DEFAULT '',
  ADD CONSTRAINT ai_review_docs_source CHECK (
    (docs_source = 'snapshot_set' AND doc_snapshot_set_id IS NOT NULL AND work_item_id IS NULL)
    OR (docs_source = 'work_item' AND work_item_id IS NOT NULL AND doc_snapshot_set_id IS NULL)
    OR (docs_source IN ('none', 'legacy', 'unplanned') AND doc_snapshot_set_id IS NULL)
  ),
  -- legacy / unplanned 是迁移前或没有进入冻结的请求，摘要为空；其余来源一律要有合法的 64 位 hex 摘要。
  ADD CONSTRAINT ai_review_docs_digest CHECK (
    docs_source IN ('legacy', 'unplanned') OR docs_manifest_digest ~ '^[0-9a-f]{64}$'
  );

CREATE INDEX ai_review_requests_doc_set ON ai_review_requests (doc_snapshot_set_id)
  WHERE doc_snapshot_set_id IS NOT NULL;
CREATE INDEX ai_review_requests_doc_work_item ON ai_review_requests (work_item_id)
  WHERE work_item_id IS NOT NULL;

-- work_item_doc_freezes：补上冻结时的来源（provider、base_url、collection_id、collection_name、绑定代次）、整批摘要和
-- 正文清理时间。迁移前写入的旧行这些列为 NULL，不做回填（不伪造来源），摘要按历史元数据确定性计算。
ALTER TABLE work_item_doc_freezes
  ADD COLUMN provider           text,
  ADD COLUMN base_url           text,
  ADD COLUMN collection_id      text,
  ADD COLUMN collection_name    text,
  ADD COLUMN binding_generation bigint,
  ADD COLUMN manifest_digest    text,
  ADD COLUMN content_purged_at  timestamptz;
