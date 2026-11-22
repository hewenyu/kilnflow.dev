# kiln-task 子命令契约

本文是 8 个公开子命令的完整契约，与 `kiln-orca` 的实现一致。任务说明给出的 `<KILN_TOOL>` 是命令前缀（已经包含数据目录和这次执行的 attempt），下文写成：

```sh
<KILN_TOOL> <子命令> [参数…]
```

`<KILN_TASK_DIR>` 是任务目录（`info` 的 `task_dir`）。Kiln 以任务目录中的报告文件和工具的输出判定结果。

## 通用约定

- 命令从任务说明给出的前缀运行，输出写在 stdout；连接器自己的说明也写在 stdout（`kiln-orca` 的严重错误写 stderr）。
- 任务已经停止或正在结束（取消、超时、失效或 Agent 已经结束）时，命令输出「这个任务已经停止或正在结束（取消、超时、失效或 Agent 已经结束），不要再继续。」并以 `1` 退出：不要再继续，按 `done` 的说明结束。
- 连接不上连接器（例如连接器正在重启）时输出「无法连接 kiln-orca（…）：稍后重试；连接器重启期间任务会被恢复。」并以 `1` 退出：稍后运行同一条命令。
- 输出「执行失败（可以稍后重试）：…」的 `1` 是临时错误，可以稍后重试。已经决定的结果会原样重放，不会重复执行。

## 1. `info`

```sh
<KILN_TOOL> info
```

输出当前任务的 JSON（缩进 2 空格），退出码 `0`。字段：

| 字段 | 说明 |
|---|---|
| `attempt` | 这次执行的 attempt 编号（数字） |
| `repository` | 仓库（`owner/name`） |
| `kind` | `review` 或 `develop` |
| `mode` | 开发模式：`select`、`implement`、`fix`（只有开发任务有） |
| `phase` | 开发阶段：`design` 或 `implement`（只有开发任务有；领取之后是领取到的阶段） |
| `work_item` | 工作项编号（字符串；还没有领取时可能没有） |
| `task_dir` | 任务目录的绝对路径 |
| `worktree` | Orca 中这次执行的工作区路径 |
| `start_commit` | 这次执行的起点 commit |
| `deadline` | 截止时间（ISO 8601） |
| `tools` | 受管理的工具链，例如 `["go 1.27.3", "pnpm 12.14.0"]`（任务没有声明时没有这个字段） |

没有值的可选字段不会出现在 JSON 中。

## 2. `check-report`

```sh
<KILN_TOOL> check-report
```

只用于审计任务。读取 `<KILN_TASK_DIR>/report.json`（不超过 1 MiB 的普通文件）并交给 Kiln 按同一份规则校验。

退出码：

- `0`：报告有效。输出「通过：审计报告有效。现在运行 `<KILN_TOOL> done --outcome succeeded` 结束这一轮。」——**仍然要运行 `done`**，它才会向 Orca 报告结束。
- `1`：文件不存在（「还没有 …」）、报告不合格（「审计报告不合格，请修正后再运行：」加每条问题）或者临时错误。按输出修正后重跑。
- `2`：不是审计任务（「check-report 只用于审计任务」）。

## 3. `claim`

```sh
<KILN_TOOL> claim
```

只用于选题任务（`mode=select`）。读取并按 Kiln 的规则校验 `<KILN_TASK_DIR>/selection.json`，然后原子领取选中的 Issue。**按输出继续，不要自己判断领取结果。**

退出码：

- `0`：有两种正常结果，都必须按输出继续：
  - 领取成功：输出是下一步的设计或开发说明（分支、阶段、交付要求）；
  - 没有领取（没有选中 Issue、`dry_run`、或者没有可领取的候选）：输出是结束这一轮的说明，运行 `done --outcome succeeded` 结束。
- `3`：领取冲突，需要重新选题。连接器已经用新的候选覆盖 `<KILN_TASK_DIR>/candidates.json`、更新 `<KILN_TASK_DIR>/in_progress.json`，并删除了 `selection.json`。重新选题、覆盖写 `selection.json`，再运行 `claim`。
- `1`：报告不合格、还没有 `selection.json`，或者临时错误。按输出修正后重跑；已经决定的结果（领取成功、不领取、冲突）在重试时会原样重放，不会重复领取。
- `2`：不是选题任务（「claim 只用于选题任务」）。

`selection.json` 由 Kiln 的选题规则校验（`selected` 是选中的 Issue 编号，未选的候选写进 `skipped` 并写明原因），不合格时输出里有每条问题。

## 4. `deliver`

```sh
<KILN_TOOL> deliver
```

只用于开发任务（设计、实现、返修）；选题任务必须先成功领取，否则输出「还没有领取工作项：先写选题报告并运行 claim。」并以 `1` 退出。

读取 `<KILN_TASK_DIR>/delivery.json` 并交给 Kiln 校验。`status` 是 `delivered` 时还会核对工作区：

- 工作区干净（没有未提交的改动和未跟踪的文件）；
- `HEAD` 是起点（`start_commit`）的后代，相对起点有新的 commit；
- 设计阶段只改了 Markdown（`*.md`）；
- `design.documents` 中列出的文件已经在 `HEAD` 中。

退出码：

- `0`：通过。输出「通过：交付报告有效。现在运行 `<KILN_TOOL> done --outcome succeeded` 结束这一轮（不要 push，连接器会推送）。」
- `1`：还没有 `delivery.json`、报告不合格、工作区不符合要求，或者临时错误。按输出修正后重跑。
- `2`：不是开发任务（「deliver 只用于开发任务」）。

**`deliver` 只做交付前校验，不会立刻 push。** 连接器在 `done` 之后把这次执行的 commit 推到 attempt 分支。

## 5. `ci [--sha <commit>]`

```sh
<KILN_TOOL> ci
<KILN_TOOL> ci --sha <commit>
```

查询 PR 当前 head（不写 `--sha` 时）或指定 commit 的 Kiln CI 验证状态和失败的 job。审计任务和开发任务都可以用：审计 PR 时查这个 PR，开发任务查自己交付的 PR；没有 PR 时（例如开发还没推送）回退查询任务的 `sha`。

**退出码 `0` 只表示查询成功，不代表 CI 通过。** 必须读输出：

- 第一行是 `commit <sha>：<state>`，有失败的 job 时后面带 `（N 个失败的 job）`。`state` 是 Kiln 的验证结论，取值只有 `none`（还没有验证）、`pending`、`succeeded`、`failed`、`cancelled`。
- 每个失败的 job 是一段 `## 失败：<名称>（<分类>）：<原因>`，带地址；分类以 `infra.` 开头时是基础设施的问题，不需要改代码。
- 日志是最后 16 KiB（每个 job）。日志已过保留期时写明 `（日志已过保留期）`，不输出日志段；日志为空且没有过期时同样不输出日志段。
- 输出总量约 128 KiB，放不下时明确写出哪些 job 的日志没有列出、还有几个 job 没有列出。

退出码 `1`：临时错误，稍后重试。

## 6. `doc list` / `doc get <doc_id>`

```sh
<KILN_TOOL> doc list
<KILN_TOOL> doc get <doc_id>
```

只读地读取本任务冻结的外部设计文档快照（`doc` 不带子命令时等同于 `doc list`）。

- `doc list`：输出 JSON 数组，每篇是 `{ "id", "title", "revision", "sha256", "url" }`；没有文档时输出「这个任务没有外部设计文档快照。」
- `doc get <doc_id>`：输出 `# <标题>（修订 <N>，<地址>）`，空行，然后是 Markdown 正文。

退出码 `0` 成功；`1` 临时错误或没有这篇文档。快照是不可变的（修订号固定），之后文档更新不影响这次任务。不要用本机的 Outline 凭据自己访问。

## 7. `exec -- <命令> [参数…]`

```sh
<KILN_TOOL> exec -- <命令> [参数…]
```

在当前工作目录中运行命令（`--` 必须写，之后的参数原样传给命令）。连接器为它准备：

- 受管理的工具链（任务声明了 `requirements.tools` 时）：这些工具在 `PATH` 最前面，并设置它们需要的环境变量（例如 Go 的 `GOROOT`、`GOTOOLCHAIN=local`）。
- git 凭据：命令中的网络 git HTTP(S) 传输按操作拿到只读凭据，只覆盖任务仓库和任务声明的依赖仓库，传输结束即吊销；推送会被拒绝。

退出码是命令自己的退出码；命令被信号结束时是 `128 + 信号编号`。找不到命令或无法启动它（例如没有执行权限）时退出码 `127`（同 shell 的 command not found）。用法错误（例如缺少 `--`）时退出码 `2`，不会运行任何命令。连不上连接器或者准备环境（工具链、git 的传输包装）失败时退出码 `1`，**命令没有运行**；不要改用本机安装的工具。

不要修改 git 的 remote、credential、`url.*.insteadOf` 设置，不要自己配置凭据，也不要安装或查找这些工具的其他版本。依赖（`go mod download`、`pnpm install --frozen-lockfile` 等）照常经 `exec` 安装。

## 8. `done --outcome succeeded|failed [--summary "…"]`

```sh
<KILN_TOOL> done --outcome succeeded
<KILN_TOOL> done --outcome failed --summary "<原因，一句话>"
```

结束这一轮。

- `--outcome succeeded`：按任务当前的类型和阶段做与 `check-report` / `claim` / `deliver` 相同的校验；不通过时输出问题并以 `1` 退出，**不会发送**。选题任务还没有领取时，要有确定的结论（`claim` 已经回复了不领取）。
- `--outcome failed`：必须带 `--summary` 写明为什么做不完（否则是用法错误，退出码 `2`）。不校验报告。
- `--outcome` 缺失或不是这两个值：用法错误，退出码 `2`。

校验通过后，`done` 在**你的进程中**（继承 `ORCA_*` 环境变量，不带 `--from`，用固定的幂等键）向 Orca 发送 `worker_done`：不要清理 `ORCA_*` 环境变量，不要在另开的终端中运行。

输出和退出码：

- `0`：输出「已向 Orca 报告结束（completed / failed）」或者「这个任务之前已经报告过结束（Orca：settled）」。**现在结束这一轮，不要再运行任何命令。**
- `1`：
  - 校验不通过：按输出的问题修正后重跑；
  - Orca 不接受这次报告的发送者（身份问题）：按输出修正能在本会话中修正的部分，然后重跑同一条命令；做不到时结束这一轮；
  - Orca 没能确认这次报告（结果未知、暂时不可用）：稍后运行同一条命令，同一个请求会重放，不会重复报告；
  - 执行不了 Orca 的命令行：这是节点的问题，已经报告给 Kiln；稍后重跑，还是不行就结束这一轮；
  - Orca 拒绝了这次报告：不是你能修正的问题，已经报告给 Kiln；结束这一轮。

结束只用 `done`。不要用 `orca orchestration send` 自己发送 `worker_done`，也不要自己去找 Orca 的命令行。
