# Orca CLI 输出夹具

连接器 `OrcaCli`（`src/core/orca.ts`）的契约测试（`test/orca-contract.test.ts`）用这些夹具：每个文件是一条 `orca … --json`
命令在 stdout 上的完整输出。Orca 改了输出格式时，用新版本重新采集、替换夹具，测试会指出哪里的解析要改。

- 采集的版本：**Orca 1.4.258**（macOS，`/Applications/Orca.app/Contents/Resources/bin/orca`），2026-11-14。
- 2026-11-20 补采（**Orca 1.4.263**，同一台机器）：`check-escalation-chaterr.json`、
  `check-escalation-stop.json`。已在 Orca 1.4.263 观察到 Agent 运行中断的 `escalation` 上带通用字段
  `turnOutcome`（`failure`）、`failureKind`（Agent 私有，例如 `notSignedIn`）、`error`、`detail`，系统停止通知上带
  `stopReason`（`needs-input`、`exited`、`lost`、`finished`）。样本取自本机
  `orca orchestration inbox --limit 500 --full --json` 中真实的 `msg_chaterr_*`、`msg_stop_*` 消息（Agent 报错、一轮结束、
  停在需要人回答的提示上），payload 原样搬进 `check --full` 的形状。没有这些字段时（例如 1.4.258 的夹具）连接器安全回退到
  现有等待逻辑：没有 `turnOutcome` / `stopReason` 的 `escalation` 仍是普通求助，照现有逻辑自动回复。
- 采集方式：用编译后的 `OrcaCli`、`Mailbox` 对真实 Orca 跑冒烟（3 个 `claude` worker：立即 `worker_done`、先 `ask` 再
  `worker_done`、启动后立即 `worker-stop`），记录每次 `orca` 调用的 stdout；另外手工执行了几条只读命令和不启动 Agent 的
  邮箱实验（自己 `terminal create` + `run-create`，`orchestration send --to run:<id>` 给自己发消息）。
- 2026-11-14 补采（同一版本）：`worker-release-*`、`worker-start-failed.json`，1 个 `claude` worker（`sleep` 后
  `worker_done`）和一次不启动 Agent 的失败启动（`--base-branch` 指向不存在的 commit）。
- 2026-11-14 补采（同一版本）：`worker-show-not-found.json`、`worker-stop-not-found.json`、`check-run-not-found.json`，
  用不存在的 dispatch id、run id、终端句柄调用（模拟 Orca 的编排状态被重置之后的回答）；不启动 Agent，没有执行
  `orchestration reset`，不影响正在使用的编排状态。同时确认 `request-show` 查不存在的请求仍是 `request-show-absent.json` 的形状。
- 2026-11-16 补采（同一版本）：`worktree-list.json`、`worktree-rm*.json`、`terminal-list.json`。在临时 git 仓库上模拟连接器
  的目录（`repos/1/target` 是 clone，`orca repo add` 注册），`orca worktree create --name kiln-a9155-e1 --comment
  'kiln:attempt=9155;epoch=1;task=1;kind=review' --setup skip --no-parent` 建工作区，再在其中 `git worktree add
  tasks/9155/head <sha>`（审计 Agent 的检出）。仓库的 `externalWorktreeVisibility` 是默认的 `hide` 时 `worktree list` 不列出
  这个检出，`show` 时列出（夹具是 `show` 时采的）；两种情况下 `worktree rm --worktree id:<repoId>::<检出路径>` 都能删除它。
  工作区不干净时不带 `--force` 的 `worktree rm` 以 1 退出、`runtime_error`（`worktree-rm-dirty.json`），带 `--force` 删除成功；
  删除不带 `--delete-branch` 时本地分支（`kiln/kiln-a9155-e1`，Orca 按 git 用户名加前缀）保留。`orca worktree create`
  会在新工作区中开一个终端（`terminal-list.json` 中的 `Terminal 1`），`worktree rm` 会把工作区中活着的终端一起关掉。
  `worker-list.json` 取自本机 `orca orchestration worker-list --terminal-state retained --limit 100 --json --full` 的回执（两个
  worker：一个 structured 模式、还在运行的 Agent，它不在这个工作区的 `terminal list` 中；一个已经结束、终端保留的）；Orca
  推断不出调用方时 `scope.source` 是 `all`（连接器不依赖它，见下面的补采）；翻页在 `page` 中，最后一页 `hasMore` 为 false、
  `nextCursor` 为 null。
- 2026-11-16 补采（同一版本）：`run-list.json`（`orca orchestration run-list --limit 2 --json --full`，有下一页）、`run-list-last.json`
  （同上，最后一页，`nextCursor` 为 null，只留两个 Run）、`worker-list-bound.json`、`worker-list-run.json`。采集时去掉了调用
  进程的全部 `ORCA_*` 环境变量、当前目录在 `/tmp`，整个 Orca 中只有本机连接器的邮箱终端：Orca CLI 把调用方推断为邮箱，
  `run-current` 返回邮箱的 Run，不带 `--run` 的 `worker-list --terminal-state retained` 只列出这个 Run（`scope.source=bound`，
  `worker-list-bound.json`，没有 worker）；带 `--run <另一个 Run>` 时 `scope.source=flag`、`scope.run` 是请求的 Run
  （`worker-list-run.json`：三个 worker，一个是还在运行的 structured Agent，两个已经结束、终端保留）。另外在临时目录中
  `git worktree add --detach` 再删除目录、只留登记，`orca worktree rm --worktree id:<r>::<路径> --json`（不带 `--force`）返回
  与 `worktree-rm.json` 相同的 `removed: true`，git 的登记一起去掉（没有另存夹具）。
- 2026-11-17 补采（同一版本）：不带 `--terminal-state` 的翻页。`worker-list-all-p1.json`、`-p2.json`、`-p3.json`
  是同一个 Run（`run_000000000601`）的 `orca orchestration worker-list --run <run> --limit 3 [--cursor <c>] --json --full`
  （为了翻页用了 `--limit 3`，连接器用 100）：七个 dispatch，三个终端保留（其中一个是还在运行的 structured Agent）、四个没有
  终端（`terminalState` 为 null、`resource` 为 null）且已经结束；每页的 `page.total` 都是 7，`counts` 只计 `terminalState` 不为
  null 的。游标是 base64 的 JSON（`{"version":2,"snapshot":{"databaseId":…},"after":{…,"databaseId":…}}`，第一页记下快照的行号，
  之后按上一页最后一行的行号翻），脱敏时解码、按同样的规则换掉其中的 id 再编码。`worker-list-all-released.json` 是另一个 Run
  （`run_000000000602`）的一页：十个 dispatch，终端已经释放（`released`），或者没有终端、已经结束。`worker-list-cursor-expired.json`
  是游标所在的行不存在时的回执（构造一个指向不存在的行的游标，只读）：Orca 内部的 `worker_list_cursor_expired` 在 CLI 回执中是
  `runtime_error`，以 1 退出。工作区路径换成 `/Users/kiln/orca/workspaces/target/kiln-a<n>-e1`。
- 2026-11-17 补采（邮箱终端的存活，同一版本）：在临时 git 仓库上（`orca repo add` 注册，路径脱敏为邮箱仓库
  `/Users/kiln/Library/Application Support/kiln-orca/kiln-orca`）自己 `terminal create` 测试终端：`terminal-show-exited.json`
  是在终端中 `exit` 之后的 `terminal show`，`terminal-close.json` 是关闭进程还在的终端（对已经关闭的再关回答相同），
  `terminal-close-stale.json` 是关闭不存在的句柄，`terminal-list-mailbox.json` 是这个仓库中两个标题为"kiln-orca 邮箱"、一个
  标题为 `zsh` 的终端（`terminal list --worktree path:<仓库>`）。`terminal-show-orphaned.json` 是本机生产节点上连接器泄漏的
  一个邮箱终端（没有界面窗格、进程还在：`orphaned:true`、`connected:true`、带 `recordedPaneKey`）的 `terminal show`，只读采集。
- 脱敏：本机路径、用户名、主机名换成 `/Users/kiln/…`、`kiln@host`；UUID 换成 `00000000-0000-4000-8000-…`；`ctx_`、`task_`、
  `run_`、`msg_`、`delivery_`、`wtr_` 开头的 id 换成按出现顺序编号的占位值（同一个值在所有夹具中是同一个占位）；commit sha
  （`head`、`baseRef`、`baseBranch` 等字段）换成 `0123456789abcdef…` 这类明显不是真实提交的占位值；`repo list`
  只留冒烟用的两个仓库。其余字段原样保留。

| 文件 | 命令 |
|---|---|
| `status.json` | `orca status --json` |
| `repo-list.json`、`repo-add.json` | `orca repo list --json`、`orca repo add --path <p> --json` |
| `terminal-create.json` | `orca terminal create --worktree path:<p> --title <t> --json` |
| `terminal-show.json` | `orca terminal show --terminal <h> --json`（存活） |
| `terminal-show-closed.json` | 同上，终端已被 `terminal close` 关闭（仍查得到：`orphaned`、`connected:false`） |
| `terminal-show-stale.json` | 同上，句柄不存在（`terminal_handle_stale`） |
| `terminal-show-orphaned.json` | 同上，没有界面窗格、进程还在（`orphaned:true`、`connected:true`） |
| `terminal-show-exited.json` | 同上，终端中的 shell 退出了（`connected:false`、`exitCause.kind: unknown`） |
| `terminal-close.json`、`terminal-close-stale.json` | `orca terminal close --terminal <h> --json`（关闭；句柄不存在：`terminal_handle_stale`，以 1 退出） |
| `terminal-list-mailbox.json` | `orca terminal list --worktree path:<邮箱仓库> --limit 10000 --json`（两个邮箱终端、一个别的标题的终端） |
| `run-create.json` | `orca orchestration run-create --from <h> --objective <o> --json --full` |
| `run-create-concise.json` | 同上，不带 `--full`（精简视图，没有 `run.id`） |
| `run-use.json`、`run-use-not-found.json` | `orca orchestration run-use --id <run> --from <h> --json --full`（成功；Run 不存在） |
| `worker-start.json` | `orca orchestration worker-start --from <h> --run <run> --retry-request <uuid> --spec … --worktree new-top-level --repo id:<r> --name … --base-branch <sha> --comment … --agent claude --json --full` |
| `request-show-completed.json`、`request-show-absent.json` | `orca orchestration request-show --request <uuid> --json --full`（worker-start 的回执；没有记录） |
| `worker-show-running.json`、`-completed`、`-released`、`-stopped` | `orca orchestration worker-show --dispatch <d> --json --full`：运行中；发了 worker_done；`worker-release` 之后；`worker-stop` 之后 |
| `worker-show-concise.json` | 同上，不带 `--full`（精简视图，没有 `projection`） |
| `check-worker-done.json`、`check-question.json` | `orca orchestration check --terminal <h> --run <run> --wait --types worker_done,escalation,question --timeout-ms <n> --retry-request <uuid> --json --full` |
| `check-rejected.json` | 同上，批中有 Orca 拒绝的 worker_done（`payload._orcaLifecycleRejection`）和被拒绝后改为 status 的 escalation |
| `check-escalation-chaterr.json` | 同上，Orca 报告 Agent 运行中断（`turnOutcome: failure`、`failureKind`、`error`、`detail`，Orca 1.4.263） |
| `check-escalation-stop.json` | 同上，系统停止通知：`stopReason: finished`（这一轮结束）和 `stopReason: needs-input`（停在需要人回答的提示上，Orca 1.4.263） |
| `check-concise.json` | 同上，不带 `--full`（`payload` 是对象、`from` 代替 `from_handle`、没有 `deliveryId`） |
| `check-timeout.json` | 同上，等待超时、没有消息（`timedOut`） |
| `check-interrupted.json` | 重放一次调用方中途退出的 check（`cancelled`、`connectionLost`，没有交出消息） |
| `check-fenced.json` | 用不是 Run 绑定的终端读取（`consumer_fenced`） |
| `check-run-not-found.json` | 同上，Run 不存在（`run_not_found`，以 1 退出；用不存在的终端句柄采集，回答的是 Run 不存在） |
| `worker-show-not-found.json` | `orca orchestration worker-show --dispatch <d> --json --full`，dispatch 不存在（`dispatch_not_found`，以 1 退出） |
| `worker-stop.json`、`worker-stop-settled.json` | `orca orchestration worker-stop --dispatch <d> --json --full`（停止；已经结束时再停） |
| `worker-stop-not-found.json` | 同上，dispatch 不存在（`dispatch_not_found`，以 1 退出） |
| `worker-release.json` | `orca orchestration worker-release --dispatch <d> --json --full` |
| `worker-release-already.json` | 同上，已经释放过再释放（`already_released`） |
| `worker-release-retained.json` | 同上，dispatch 没有归 Orca 管的终端（启动失败的 dispatch：`retained`、`no_owned_resource`） |
| `worker-release-inactive.json` | 同上，worker 还没有结束（`dispatch_inactive`，以 1 退出） |
| `worker-release-not-found.json` | 同上，dispatch 不存在（`dispatch_not_found`，以 1 退出） |
| `worker-start-failed.json` | `worker-start`（同上）在建 worktree 时失败（`state: failed`，带 `dispatchId`，以 1 退出） |
| `reply.json`、`reply-duplicate.json` | `orca orchestration reply --from <h> --id <msg> --body <b> --json --full`（第一次；同样的回答再发一次） |
| `worktree-list.json` | `orca worktree list --repo id:<r> --limit 10000 --json`：仓库本身、Agent 的检出（`tasks/…/head`，没有备注）、连接器建的工作区（带备注） |
| `worktree-rm.json` | `orca worktree rm --worktree id:<r>::<路径> [--force] --json`（删除） |
| `worktree-rm-dirty.json` | 同上，不带 `--force`、工作区不干净（`runtime_error`，"Failed to delete worktree at …" 后面是 `git status --porcelain`，以 1 退出） |
| `worktree-rm-not-found.json` | 同上，已经删除后再删（`selector_not_found`，以 1 退出） |
| `worker-list.json` | `orca orchestration worker-list --terminal-state <状态> --limit 100 --json --full`，推断不出调用方（`scope.source=all`）：连接器不用这种查询（按状态分开查询不是同一时刻的，见 `worker-list-all-*`），契约测试用它确认作用域不对时报错 |
| `worker-list-bound.json` | `orca orchestration worker-list --terminal-state retained --limit 100 --json --full`，调用方被推断为邮箱终端（`scope.source=bound`，只列出邮箱的 Run） |
| `worker-list-run.json` | `orca orchestration worker-list --run <run> --terminal-state retained --limit 100 --json --full`（`scope.source=flag`） |
| `worker-list-all-p1.json`、`-p2.json`、`-p3.json` | `orca orchestration worker-list --run <run> --limit 3 [--cursor <c>] --json --full`（不按状态过滤，三页；连接器的回收用这种查询） |
| `worker-list-all-released.json` | 同上，另一个 Run 的一页（终端已经释放的、没有终端且已经结束的） |
| `worker-list-cursor-expired.json` | 同上，游标所在的行已经不在（`runtime_error`，"The worker inventory changed destructively while paging"，以 1 退出） |
| `run-list.json`、`run-list-last.json` | `orca orchestration run-list --limit <n> [--cursor <c>] --json --full`（有下一页；最后一页） |
| `terminal-list.json` | `orca terminal list --worktree id:<r>::<路径> --limit 10000 --json`（工作区中两个活着的终端） |
