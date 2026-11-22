-- 按 commit 读取 run 和 AI 审计请求的索引。
--
-- runs、ai_requests 是结果、永久保留，一个仓库的行数随时间增长。按 (repo_id, sha) 读取的查询（CI 验证 PRVerifications、
-- PRSettled、LatestVerification，重跑 commit 上的 run；AI 审计的 PRVerifications、返修读取审计、沿用审计、重跑 commit 上的
-- 审计）原先只能用 (repo_id, id DESC) 的索引读出这个仓库的全部行再过滤，合并队列每一轮处理都要为每个排队的 PR 读一次。
-- sha 几乎唯一，(repo_id, sha) 让这些查询只读这个 commit 的几行。只加索引，不改数据。
CREATE INDEX runs_repo_sha ON runs (repo_id, sha);
CREATE INDEX ai_requests_repo_sha ON ai_requests (repo_id, sha);
