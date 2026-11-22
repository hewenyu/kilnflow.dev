-- 由 GitHub 重跑请求（check_run / check_suite rerequested）创建的 rerun 记录对应的 webhook delivery。
-- 同一个 delivery 对同一个 run 最多创建一次 rerun：事件中途被限流、恢复后重试时，已经处理过的 run 不再重跑，
-- 即使它在冷却期间已经结束。Dashboard 和 CLI 的重跑没有 delivery，为 NULL，不受约束。
ALTER TABLE run_reruns ADD COLUMN delivery_id text;
CREATE UNIQUE INDEX run_reruns_delivery ON run_reruns (run_id, delivery_id) WHERE delivery_id IS NOT NULL;
