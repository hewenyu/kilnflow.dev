-- 合并队列与自动打 tag 的原因码（Dashboard 多语言，docs/I18N.md 4.3）：领域层不接收语言，在原来的中文说明（status、reason，
-- CLI、日志、审计和评论用）之外记下稳定的原因码和原始参数，由 Dashboard 按语言显示。
--
-- merge_queue.status_code / status_params：排队中时在等什么、合并后关闭关联 issue 的进度或失败（mergequeue.Status*）。
-- merge_queue.reason_code / reason_params：出队原因、在队列之外合并（mergequeue.Reason*）。
-- auto_tags.reason_code / reason_params：自动打 tag 跳过、失败或稍后重试的原因（autotag.Reason*）。
--
-- 与对应的中文列一起写入、一起清空。旧行的原因码为空串、参数为空对象：Dashboard 原样显示中文说明，不按中文反推原因码。
ALTER TABLE merge_queue
  ADD COLUMN status_code   text  NOT NULL DEFAULT '',
  ADD COLUMN status_params jsonb NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN reason_code   text  NOT NULL DEFAULT '',
  ADD COLUMN reason_params jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE auto_tags
  ADD COLUMN reason_code   text  NOT NULL DEFAULT '',
  ADD COLUMN reason_params jsonb NOT NULL DEFAULT '{}'::jsonb;
