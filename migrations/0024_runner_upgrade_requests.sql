-- Runner 按台升级（PLAN #27.11–#27.13）：Runner 自己从 npm 发现、下载升级包，Server 只记录管理员的
-- 按台升级请求并下发。删除升级来源、登记的版本、类型级目标（auto / pin / pause、最低版本、generation、自动暂停）和
-- 逐台推进：引入时 Server 和所有 Runner 都手动升级一次，不需要回退到旧 Server。

DROP TABLE runner_upgrade_source;
DROP TABLE runner_releases;
DROP TABLE runner_upgrade_targets;

ALTER TABLE runners
  DROP COLUMN upgrade_pin,
  DROP COLUMN upgrade_assigned,
  DROP COLUMN upgrade_assigned_at,
  -- upgrade_check：检查序号，管理员点"检查更新"时 + 1，随指令下发；Runner 看到变化后立即查询最新版本。
  ADD COLUMN upgrade_check bigint NOT NULL DEFAULT 0;

-- 按台升级请求（结果，永久保留）。id 下发给 Runner 作为 generation：Runner 记下失败时的 generation，更大的（管理员重试
-- 生成的新请求）之前不再安装这个版本。序列从 1000 开始，旧 state.json 中按旧目标 generation（很小的数）记下的失败不会
-- 挡住新的请求。
CREATE TABLE runner_upgrade_requests (
  id           bigserial PRIMARY KEY,
  runner_id    bigint NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  version      text   NOT NULL,             -- 目标版本（npm 版本号，不带 v），请求时 Runner 上报的 latest
  from_version text   NOT NULL DEFAULT '',  -- 请求时 Runner 的版本（不带 v，开发构建为空）
  state        text   NOT NULL DEFAULT 'pending', -- pending | succeeded | failed | cancelled
  error        text   NOT NULL DEFAULT '',
  actor        text   NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
ALTER SEQUENCE runner_upgrade_requests_id_seq RESTART WITH 1000;
-- 每台 Runner 最多一个进行中的请求（重复点击、浏览器重试返回同一个）。
CREATE UNIQUE INDEX runner_upgrade_requests_pending ON runner_upgrade_requests (runner_id) WHERE state = 'pending';
CREATE INDEX runner_upgrade_requests_runner ON runner_upgrade_requests (runner_id, id DESC);
