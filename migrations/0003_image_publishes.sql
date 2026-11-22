-- 已经由 Kiln 发布的镜像版本（PLAN #13.8）：设置完不可变 tag 后记录一行。
-- 浮动 tag（例如 latest）只在本版本是该镜像名所有正式版本中 semver 最大时才推进，依据就是这张表。
CREATE TABLE image_publishes (
  name        text        NOT NULL,          -- 规范化的镜像名，例如 docker.io/example/kiln-server
  version     text        NOT NULL,          -- 触发发布的 git tag，例如 v1.2.0
  digest      text        NOT NULL,          -- 镜像（manifest 或 index）的 digest
  job_id      bigint      NOT NULL,          -- 发布 job
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (name, version)
);
