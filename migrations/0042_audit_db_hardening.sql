-- 数据库审计加固（2026-11-20）：environment 名称的唯一性改为大小写不敏感，并补充缺失的索引。
--
-- 1) environment 名称不区分大小写（与 ResolveScope、LookupEnvironment 一致）：原来的唯一约束
--    environments_owner_id_repo_id_name_key 区分大小写，并发的 "Prod" / "prod" 能在 upsert 的 UPDATE 都落空后
--    各自 INSERT 成功，留下两个查找时无法区分的 environment。这里先合并仅大小写不同的历史行，再删除旧约束，
--    换成 (owner_id, repo_id, lower(name)) 的唯一索引，让数据库严格原子地合并（upsert 改为按它 ON CONFLICT）。
--
--    合并规则：同一 (owner_id, repo_id, lower(name)) 保留 id 最小的一个，把挂在其它的变量挪过去（重名的以保留行为准），
--    再删除其余行。落败 environment 上若有 secret，因为密文的 AAD 绑定 scope_id、迁移中无法重加密，这里明确报错中止。
--    这是历史脏数据的修复，正常情况下没有可合并的行，这段什么也不做。

CREATE TEMP TABLE _env_merge ON COMMIT DROP AS
SELECT id AS loser_id,
       min(id) OVER (PARTITION BY owner_id, repo_id, lower(name)) AS keep_id
FROM environments;

DELETE FROM _env_merge WHERE loser_id = keep_id;

-- secret 的密文 AAD 包含 scope_id，纯 SQL 迁移没有 MasterKey，无法在挪动 scope_id 时重新加密。
-- 与其静默删掉或留下解不开的密文，不如明确中止：让用户先清理或重设这些 environment 的 secret 再升级。
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM secrets s
    JOIN _env_merge m ON s.scope_kind = 'environment' AND s.scope_id = m.loser_id
  ) THEN
    RAISE EXCEPTION '发现仅大小写不同的重复 environment 上挂有 secret（密文与 environment 绑定，无法在迁移中重加密），请先删除或重设这些 environment 的 secret 再升级';
  END IF;
END $$;

-- 变量可以安全合并：按合并目标一次性删除全部重名行（保留行为准；多个落败行之间重名的也一并删除，
-- 避免移动时撞唯一约束），再统一把落败行的 scope_id 改成保留行。
DELETE FROM variables v
USING (
  SELECT id, row_number() OVER (
    PARTITION BY coalesce(m.keep_id, v.scope_id), v.name
    ORDER BY CASE WHEN v.scope_id = coalesce(m.keep_id, v.scope_id) THEN 0 ELSE 1 END, v.scope_id, v.id
  ) AS rn
  FROM variables v
  LEFT JOIN _env_merge m ON v.scope_kind = 'environment' AND v.scope_id = m.loser_id
  WHERE v.scope_kind = 'environment'
) dup
WHERE v.id = dup.id AND dup.rn > 1;

UPDATE variables v SET scope_id = m.keep_id FROM _env_merge m
WHERE m.loser_id <> m.keep_id AND v.scope_kind = 'environment' AND v.scope_id = m.loser_id;

DELETE FROM environments e USING _env_merge m WHERE e.id = m.loser_id AND m.loser_id <> m.keep_id;

ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_owner_id_repo_id_name_key;
CREATE UNIQUE INDEX environments_scope_name_ci ON environments (owner_id, repo_id, lower(name)) NULLS NOT DISTINCT;

-- 2) 补充缺失的索引。
--    按创建时间清理、统计 run 时用。
CREATE INDEX runs_created_at_state ON runs (created_at, state);
--    按工作项查开发请求（选题关联、文档依据）时用。
CREATE INDEX ai_dev_requests_work_item ON ai_dev_requests (work_item_id) WHERE work_item_id IS NOT NULL;
--    sweep 找出 label 状态还没同步的仓库时用。
CREATE INDEX dev_work_items_labels_dirty ON dev_work_items (repo_id) WHERE labels_state <> state;
--    按工作项读评论、分支时用。
CREATE INDEX dev_comments_work_item ON dev_comments (work_item_id, id);
CREATE INDEX dev_branches_work_item ON dev_branches (work_item_id, created_at);
--    待发 / 发送中的评论（sweep 按仓库找出它们）：部分索引覆盖两种状态，替换原来只看 pending 的 dev_comments_pending。
DROP INDEX IF EXISTS dev_comments_pending;
CREATE INDEX dev_comments_open ON dev_comments (repo_id, id) WHERE state IN ('pending', 'attempted');

-- 3) 发布进入阶段 B（提交）后不可取消（PLAN #13.4）：commit_started_at 单调记录开始提交的时间，重跑、重启续做时都不重置
--    （重跑会新建 job 和对应的 releases 行，新行的这一列为 NULL）。发布被取消时只有它为空才响应 cancel_requested，
--    避免阶段 B 提交中途崩溃重启后，中断的发布因为之前记录的取消请求而停在中途、留下跨目标不一致的结果。
ALTER TABLE releases ADD COLUMN commit_started_at TIMESTAMPTZ;

--    升级前已经进入阶段 B 的发布（stage 已是 commit/done，或已经有目标提交成功）要回填，否则升级后续做会因为
--    commit_started_at 为空被当成阶段 A，可能被之前记录的取消请求错误地中止。用 updated_at 作为开始提交的近似时间。
UPDATE releases r
SET commit_started_at = COALESCE(r.updated_at, now())
WHERE r.commit_started_at IS NULL
  AND (r.stage IN ('commit', 'done')
       OR EXISTS (SELECT 1 FROM release_targets t WHERE t.job_id = r.job_id AND t.state = 'committed'));
