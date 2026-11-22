-- 发布目标（PLAN #13.4、#13.9，v0.25）：发布 job 的每个发布目标（镜像、HTTP 上传、GitHub Release）一行，
-- 按提交顺序编号，记录各自的结果，Dashboard 的发布详情按目标列出。发布 job 每次开始执行（包括重启后继续）时重建。
-- HTTP 上传的 latest: auto 也据此判断：Kiln 向同一个地址发布过的正式版本。
-- 与 releases 一样属于"结果"，按保留策略永久保留（#10.3）。
CREATE TABLE release_targets (
  job_id     bigint NOT NULL,
  seq        int    NOT NULL,                   -- 提交顺序：镜像、HTTP 上传、GitHub Release
  kind       text   NOT NULL,                   -- image | upload | github
  name       text   NOT NULL,                   -- 展示名
  target     text   NOT NULL,                   -- 镜像名、上传地址、GitHub 仓库（owner/name）
  state      text   NOT NULL DEFAULT 'pending', -- pending | staged | committed | failed
  detail     jsonb  NOT NULL DEFAULT '{}',      -- 结果：Release 地址、镜像 digest 和 tag、下载地址等
  error      text,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, seq)
);
CREATE INDEX release_targets_committed ON release_targets (kind, target) WHERE state = 'committed';
