-- Runner 自升级的控制面（PLAN #27.6）。只增加表和列：migration 之后可以回到旧 Server（#27.8）。

-- 升级来源（只有一行）：只登记这个仓库（按 ID，改名不影响）tag 发布 job 中、包名符合模板的 npm 包。
CREATE TABLE runner_upgrade_source (
  id         smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  repo_id    bigint NOT NULL,
  package    text   NOT NULL,             -- 例如 @acme/{program}-{os}-{arch}
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- 登记的升级包（结果，永久保留，只存元数据不存包）。integrity 和 size 由 Server 用自己打出的 tarball 计算。
-- 发布 job 提交 npm 包时写入（staged），发布 job 成功结束时在同一事务中改为 available；只有 available 的会被下发。
CREATE TABLE runner_releases (
  id         bigserial PRIMARY KEY,
  program    text   NOT NULL,             -- kiln-runner | kiln-ai-runner
  version    text   NOT NULL,             -- npm 版本号（不带 v）
  os         text   NOT NULL,
  arch       text   NOT NULL,
  package    text   NOT NULL,
  tarball    text   NOT NULL,
  integrity  text   NOT NULL,
  size       bigint NOT NULL,
  file       text   NOT NULL,             -- 包内程序的路径（package/<program>）
  job_id     bigint NOT NULL,             -- 最近一次提交它的发布 job
  state      text   NOT NULL DEFAULT 'staged',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (program, version, os, arch)
);
CREATE INDEX runner_releases_job ON runner_releases (job_id) WHERE state = 'staged';

-- 每种 Runner 的升级目标。generation 在管理员每次重试、改模式或版本时 + 1；halted_reason 非空时（auto 模式下
-- 有 Runner 回滚）不下发。
CREATE TABLE runner_upgrade_targets (
  kind          text PRIMARY KEY,         -- ci | ai
  mode          text   NOT NULL DEFAULT 'auto',
  version       text   NOT NULL DEFAULT '',
  min_version   text   NOT NULL DEFAULT '',
  generation    bigint NOT NULL DEFAULT 1,
  halted_reason text   NOT NULL DEFAULT '',
  updated_at    timestamptz NOT NULL DEFAULT now()
);
INSERT INTO runner_upgrade_targets (kind) VALUES ('ci'), ('ai');

-- upgrade_pin：单台指定的版本（空表示跟随目标）；upgrade_assigned：逐台推进中分配给它、还没有确认或失败的版本；
-- upgrade：Runner 上报的升级状态（kiln.runner.v1.UpgradeStatus 的 protojson，旧 Runner 为 NULL）。
ALTER TABLE runners
  ADD COLUMN upgrade_pin         text NOT NULL DEFAULT '',
  ADD COLUMN upgrade_assigned    text NOT NULL DEFAULT '',
  ADD COLUMN upgrade_assigned_at timestamptz,
  ADD COLUMN upgrade             jsonb;
