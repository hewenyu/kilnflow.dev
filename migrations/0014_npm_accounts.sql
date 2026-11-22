-- 账号级 npm 配置（PLAN #27.2）：release.npm 发布到 @<scope>/<name>，registry、scope 和 token 在 Kiln 中按账号配置，
-- workflow 中不写凭据。一个账号一份。token 与 secret 一样用信封加密（PLAN #16.3），只写不读；
-- version 与 secrets 共用序列，每次写入都不同，发布审计中记录用的是哪一版。
CREATE TABLE npm_accounts (
  owner_id    bigint PRIMARY KEY,
  account     text   NOT NULL,                 -- 账号登录名（展示用，设置时记录）
  scope       text   NOT NULL,                 -- 带 @，如 @acme
  registry    text   NOT NULL,                 -- 以 / 结尾，如 https://registry.npmjs.org/
  ciphertext  bytea  NOT NULL,
  wrapped_dek bytea  NOT NULL,
  version     bigint NOT NULL DEFAULT nextval('secret_versions'),
  updated_by  text   NOT NULL DEFAULT '',
  updated_at  timestamptz NOT NULL DEFAULT now()
);
