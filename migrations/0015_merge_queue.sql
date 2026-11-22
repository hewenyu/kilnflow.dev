-- 合并队列（PLAN #28）：PR 加 label kiln:merge 即入队，每个仓库按入队顺序一次处理一个。
-- 一个 PR 每次入队一行（结果，永久保留）；同一个 PR 同时只有一行 queued。只有队列的执行者（internal/server/mergequeue）
-- 修改 queued 行；入队（ingest、Dashboard、CLI）只插入新行。
CREATE TABLE merge_queue (
  id           bigserial PRIMARY KEY,
  repo_id      bigint NOT NULL,
  pr_number    int    NOT NULL,
  state        text   NOT NULL DEFAULT 'queued',   -- queued | merged | removed
  head_sha     text   NOT NULL DEFAULT '',          -- 队列确认过的 head（入队时为空，第一次处理时记录）
  enqueued_by  text   NOT NULL DEFAULT '',          -- github:<login> 或 Dashboard / CLI 的操作者
  -- 正在进行的动作：已请求更新分支（从 action_head 更新，action_at 请求时间，action_tries 第几次请求；
  -- 请求暂时失败、结果未知时 action_at 为空，下一次处理立即重新请求，不计次数）。
  -- 新 head 到达后 action 回到 none，action_head 保留（规划新 head 的 AI 审计时沿用 action_head 的审计）
  -- removing：正在自动出队（reason 是原因，去掉 label、评论失败后重试时继续完成，不会被当成手动去掉 label）。
  action       text   NOT NULL DEFAULT 'none',      -- none | updating | removing
  action_head  text   NOT NULL DEFAULT '',
  action_at    timestamptz,
  action_tries int    NOT NULL DEFAULT 0,
  status       text   NOT NULL DEFAULT '',          -- 当前在等什么（只用于显示）
  reason       text   NOT NULL DEFAULT '',          -- 出队原因
  merge_sha    text   NOT NULL DEFAULT '',
  enqueued_at  timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
CREATE UNIQUE INDEX merge_queue_queued ON merge_queue (repo_id, pr_number) WHERE state = 'queued';
CREATE INDEX merge_queue_finished ON merge_queue (repo_id, finished_at DESC, id DESC) WHERE state <> 'queued';

-- PR ref 的 head 是按哪个目标分支规划过的（PLAN #28.3）：合并队列只在 head 按默认分支规划过之后才看验证结果。
-- 空表示没有规划过（第一次对账只记录的基线、分支和 tag）；已有的 PR 按这个 head 最近一次 pull_request run 的目标分支补上。
ALTER TABLE ref_heads ADD COLUMN base_ref text NOT NULL DEFAULT '';
UPDATE ref_heads h SET base_ref = r.base_ref
  FROM (SELECT DISTINCT ON (repo_id, ref, sha) repo_id, ref, sha, github_ctx->>'base_ref' AS base_ref
          FROM runs WHERE event = 'pull_request' AND github_ctx->>'base_ref' IS NOT NULL
         ORDER BY repo_id, ref, sha, id DESC) r
 WHERE h.repo_id = r.repo_id AND h.ref = r.ref AND h.sha = r.sha AND h.ref LIKE 'refs/pull/%';
