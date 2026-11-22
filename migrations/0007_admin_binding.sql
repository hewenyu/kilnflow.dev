-- 管理员绑定 GitHub 账号（PLAN #16.5，修订记录 v0.20）：Kiln 的管理员和他的 GitHub 账号是同一个人。
-- 管理员先用 admin token 登录 Dashboard，绑定自己的 GitHub 账号，之后用 GitHub 登录即为管理员；
-- 确认 GitHub 登录可用后可以关闭 Dashboard 的 admin token 登录（CLI 仍用 admin token，操作者记为绑定的账号）。
-- 只有一行：没有这一行表示还没有绑定，此时 admin token 登录总是开启。
CREATE TABLE admin_binding (
  singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  github_id    bigint      NOT NULL,
  github_login text        NOT NULL,
  -- token_login：Dashboard 是否还允许用 admin token 登录
  token_login  boolean     NOT NULL DEFAULT true,
  bound_by     text        NOT NULL,
  bound_at     timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
