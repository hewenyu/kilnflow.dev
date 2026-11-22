-- 证明一次镜像发布用的是哪一套凭据（Dashboard 的 Docker Hub 连接据此判断"已验证"）。
-- secret 的版本号：每次写入（新建或替换）都取一个新的、全局唯一的值。只标识"哪一行的哪一次写入"，与值本身无关。
CREATE SEQUENCE secret_versions;
ALTER TABLE secrets ADD COLUMN version bigint NOT NULL DEFAULT nextval('secret_versions');
ALTER SEQUENCE secret_versions OWNED BY secrets.version;

-- 镜像发布实际使用的凭据：登录用户名（username 引用了 secret 时不记录，为 NULL），
-- 以及 password 所用 secret 的版本（password 不是恰好一个 secret 引用时不记录，为 NULL）。
-- 本迁移之前的发布没有这两项，无法证明用的是哪套凭据，一律不算验证。
ALTER TABLE image_publishes ADD COLUMN username text, ADD COLUMN password_secret_version bigint;
