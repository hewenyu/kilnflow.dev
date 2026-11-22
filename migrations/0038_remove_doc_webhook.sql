-- 移除 Outline Webhook 接入与服务端自动差分建单。
--
-- 文档外置只保留只读绑定与工作项冻结快照：删除自动差分专用的水位线、投递去重和增量 Issue 意图表，
-- 以及绑定上的 Webhook 开关与签名密钥列。
--
-- 保留 repo_doc_bindings.generation 及序列 repo_doc_bindings_generation_seq：绑定代次在重绑时继续推进。
DROP TABLE IF EXISTS doc_delta_issues;
DROP TABLE IF EXISTS doc_webhook_deliveries;
DROP TABLE IF EXISTS doc_watermarks;

ALTER TABLE repo_doc_bindings
  DROP COLUMN IF EXISTS hook_enabled,
  DROP COLUMN IF EXISTS webhook_secret_ciphertext,
  DROP COLUMN IF EXISTS webhook_secret_wrapped_dek;
