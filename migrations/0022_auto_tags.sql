-- 自动打 tag（PLAN #29.4）：经过 PR 合入默认分支的 commit 由 Server 按 .kiln/merge.yml 的 tag: 打 tag，
-- 并直接规划这个 tag 的发布。每个 commit 一行（结果，永久保留）：(repo_id, sha) 唯一，一个 commit 最多打一个 tag（T1）。
-- 来源：合并队列合并时与合并结果同一事务写入（source=queue），手动合并由 pull_request.closed webhook 写入（source=webhook）。
-- 状态：pending → tagging（已写下 tag 名、正在创建）→ tagged（tag 已存在且指向这个 commit）→ planned（已规划发布），
-- 或 skipped / failed。由仓库的执行者（advisory lock kiln:autotag:<repo id>）按 id 顺序逐个处理。
CREATE TABLE auto_tags (
  id          bigserial PRIMARY KEY,
  repo_id     bigint NOT NULL,
  sha         text   NOT NULL,
  pr_number   int    NOT NULL,
  labels      text[] NOT NULL DEFAULT '{}',  -- 合并那一刻 PR 的 label（决定版本号）
  merged_by   text   NOT NULL DEFAULT '',    -- 规划发布时的发送者（GitHub login）
  source      text   NOT NULL,               -- queue | webhook
  state       text   NOT NULL DEFAULT 'pending',
  tag         text   NOT NULL DEFAULT '',
  reason      text   NOT NULL DEFAULT '',
  tries       int    NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repo_id, sha)
);
CREATE INDEX auto_tags_open ON auto_tags (repo_id, id) WHERE state IN ('pending', 'tagging', 'tagged');
CREATE INDEX auto_tags_recent ON auto_tags (repo_id, id DESC);
-- 同一个仓库中一个 tag 名最多给一个 commit（T1）。只约束进行中和成功的记录：failed / skipped 的记录保留尝试过的名字
-- 用于显示，但那个名字可能并没有建出来，不能挡住之后的合并（已经建出来的 tag 在平台的列表中，计算版本号时会跳过）。
CREATE UNIQUE INDEX auto_tags_tag ON auto_tags (repo_id, tag) WHERE tag <> '' AND state IN ('tagging', 'tagged', 'planned');

-- 自动入队的候选（PLAN #29.5）：PR 的 head 有 run 结束、去掉 kiln:hold 时记下，合并队列的执行者处理时检查条件，
-- 满足就入队；处理后删除。
CREATE TABLE merge_auto_candidates (
  repo_id    bigint NOT NULL,
  pr_number  int    NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, pr_number)
);

-- 合并队列请求合并时与合并意图（action=merging）一起记下当时 PR 的 label（auto_tags.labels 的来源）：合并的响应丢失、
-- 下一次处理才看到已合并时用它，而不是重新读到的 label（期间可能被人整理过）。NULL 表示没有记下。
ALTER TABLE merge_queue ADD COLUMN merge_labels text[];
