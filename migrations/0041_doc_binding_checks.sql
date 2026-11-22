-- 文档绑定的最近更新者与显式只读连接验证的结果。
--
-- 只读验证由管理员在 Dashboard 上显式触发：列出可访问的文档集、确认目标文档集在列表中、列举文档并抽查 1 篇正文。
-- 结果只保存固定枚举的状态与错误码，绝不保存远端返回的 message、响应体或底层 URL 错误（可能回显请求头、token）。
-- check_doc_count 在列举没有完成时为 NULL（不默认 0：0 表示"文档集可访问、确实是空的"）。
--
-- updated_by 记录最近一次保存配置的操作者；历史行没有这个记录，保持 NULL，不回填成 created_by（不伪造来源）。
ALTER TABLE repo_doc_bindings
  ADD COLUMN updated_by                  text,
  ADD COLUMN check_started_at            timestamptz,
  ADD COLUMN checked_at                  timestamptz,
  ADD COLUMN checked_by                  text,
  -- NULL 表示"配置已保存，尚未验证"；ok / empty / failed 是验证结论。
  ADD COLUMN check_status                text,
  -- 固定枚举的错误码（不含远端原始 message / URL / token）。
  ADD COLUMN check_error_code            text NOT NULL DEFAULT '',
  ADD COLUMN check_doc_count             integer,
  ADD COLUMN check_sample_doc_id         text NOT NULL DEFAULT '',
  -- 验证时从 Outline collections.list 观测到的文档集地址与名称（未验证时为空）。
  ADD COLUMN collection_url              text NOT NULL DEFAULT '',
  ADD COLUMN observed_collection_name    text NOT NULL DEFAULT '',
  ADD CONSTRAINT repo_doc_bindings_check_status CHECK (
    check_status IS NULL OR check_status IN ('ok', 'empty', 'failed')
  ),
  ADD CONSTRAINT repo_doc_bindings_check_doc_count CHECK (
    check_doc_count IS NULL OR check_doc_count >= 0
  );
