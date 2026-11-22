-- run 详情页的活动（谁启动、取消、重跑了这个 run）按 detail 中的 run_id 查询审计日志。
CREATE INDEX audit_log_run_idx ON audit_log (((detail->>'run_id')::bigint)) WHERE detail ? 'run_id';
