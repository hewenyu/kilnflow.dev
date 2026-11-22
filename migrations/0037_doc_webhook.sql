-- Outline Webhook 接入与文档修订水位线（阶段 3）。
--
-- 只新增列/表，不改旧数据：没有开启 hook 的仓库行为和以前完全一样。
--
-- repo_doc_bindings：新增 Outline Webhook 的签名密钥（与 API token 一样信封加密，AAD 绑定 ("doc", repo_id, "webhook_secret")）。
--   hook_enabled 打开时必须先设置密钥，接收端用它对 Outline-Signature 做 HMAC-SHA256 校验。
-- generation 用序列生成：删除重建后也能拿到不会重用的值。
CREATE SEQUENCE repo_doc_bindings_generation_seq;
ALTER TABLE repo_doc_bindings
  ADD COLUMN webhook_secret_ciphertext  bytea,
  ADD COLUMN webhook_secret_wrapped_dek bytea,
  -- generation 是绑定代次：每次重绑、改地址、开关 hook 或轮换密钥都推进到序列的下一个值。文档差分在扫描开始时记下代次，
  -- 写入前核对代次未变，使「关闭再开启并重建基线」、重绑、解绑后重建都能作废上一轮的在途读取。
  ADD COLUMN generation bigint NOT NULL DEFAULT nextval('repo_doc_bindings_generation_seq');

-- doc_watermarks：补两阶段差分需要的目标修订（target_rev）、已处理内容摘要（triaged_sha）与时间戳。
--   已有的 seen_rev / triaged_rev 不变；target_rev 在差分启动时冻结，完成后清零（防跳过执行期间到达的新修订）。
ALTER TABLE doc_watermarks
  ADD COLUMN target_rev  bigint NOT NULL DEFAULT 0,
  ADD COLUMN triaged_sha text   NOT NULL DEFAULT '',
  ADD COLUMN seen_at     timestamptz,
  ADD COLUMN triaged_at  timestamptz,
  -- collection_id 记录这条水位属于哪个绑定文档集：重绑到别的文档集后，旧文档集的水位因集合不一致被忽略，
  -- 不会再按旧文档建单。
  ADD COLUMN collection_id text NOT NULL DEFAULT '';

-- doc_webhook_deliveries：Outline Webhook 投递去重（按投递 id），避免重复投递被处理多次。
CREATE TABLE doc_webhook_deliveries (
  repo_id     bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  delivery_id text   NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, delivery_id)
);
-- 保留清理按 received_at 删除过期投递记录，加索引避免全表扫描。
CREATE INDEX doc_webhook_deliveries_received_at ON doc_webhook_deliveries (received_at);

-- doc_delta_issues：文档修订差分生成的增量 Issue 的 C19 意图（外部写操作没有幂等键，先写意图、最多尝试一次）。
--   (repo_id, doc_id, revision) 唯一；state: pending | created | failed。issue_number 是创建成功后的 issue 号。
CREATE TABLE doc_delta_issues (
  repo_id        bigint  NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  doc_id         text    NOT NULL,
  revision       bigint  NOT NULL,
  title          text    NOT NULL DEFAULT '',
  url            text    NOT NULL DEFAULT '',
  content_sha256 text    NOT NULL DEFAULT '',
  state          text    NOT NULL DEFAULT 'pending',
  issue_number   integer,
  last_error     text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, doc_id, revision)
);
