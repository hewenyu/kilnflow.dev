-- AI Runner 一期：区分 CI Runner 与 AI Runner，保存 AI 审计的结果。
--
-- 1. Runner 的类型由注册令牌决定（Server 授权，客户端不能修改）：ci 是 kiln-runner，ai 是 kiln-ai-runner。
--    已有的令牌和 Runner 都是 ci，不需要重新注册。CI Runner 只领取普通 job，AI Runner 只领取 ai_review job。
ALTER TABLE enrollment_tokens ADD COLUMN kind text NOT NULL DEFAULT 'ci';
ALTER TABLE runners ADD COLUMN kind text NOT NULL DEFAULT 'ci';

-- 2. AI 审计的待领取 job（jobs.kind='ai_review'）。jobs_pending 也覆盖它们，这个索引让 AI Runner 的领取不必扫描 CI job。
CREATE INDEX jobs_pending_ai ON jobs (priority DESC, queued_at) WHERE state = 'pending' AND kind = 'ai_review';

-- 3. AI 审计的结果：一个 ai_review job 一行。报告（Agent 写出、Runner 校验后提交的 JSON）属于结果，
--    与 run、job、attempt 的状态一样永久保留（PLAN #10.3），不随 7 天的过程数据清理。
--    review_state 是发布到 GitHub 的 PR review 的状态：none（不需要发布）| pending | published | failed。
CREATE TABLE ai_reviews (
  job_id            bigint PRIMARY KEY REFERENCES jobs(id),
  run_id            bigint NOT NULL REFERENCES runs(id),
  repo_id           bigint NOT NULL,
  attempt_id        bigint NOT NULL,
  pr_number         int,
  head_sha          text   NOT NULL,
  base_sha          text   NOT NULL DEFAULT '',
  agent             text   NOT NULL,
  verdict           text   NOT NULL,          -- no_issues | issues_found | incomplete
  summary           text   NOT NULL DEFAULT '',
  findings          int    NOT NULL DEFAULT 0,
  report            jsonb  NOT NULL,
  review_state      text   NOT NULL DEFAULT 'none',
  github_review_id  bigint,
  review_error      text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  published_at      timestamptz
);
CREATE INDEX ai_reviews_repo ON ai_reviews (repo_id, created_at DESC);
