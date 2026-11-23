-- Dashboard 高频查询的索引（总览每 5 秒刷新、运行列表筛选、启动构建时的 workflow 候选）。
--
-- runs_failed：总览的"最近失败"和 /runs?state=failed 都是 state='failed' 按 id 倒序取最近几条。部分索引只包含失败的
-- run，比按 state 的整列索引小得多，也直接给出需要的顺序。
CREATE INDEX runs_failed ON runs (id DESC) WHERE state = 'failed';

-- runs_actor：总览的"我发起的"和 /runs?actor= 按 lower(sender_login) 过滤、按 id 倒序取最近几条。
CREATE INDEX runs_actor ON runs (lower(sender_login), id DESC);

-- runs_repo_workflow：启动构建的候选和运行列表按仓库列出去重后的 workflow（WHERE repo_id=$1）。
CREATE INDEX runs_repo_workflow ON runs (repo_id, workflow);
