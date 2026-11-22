# Kiln（kilnflow.dev）

自研的自托管 CI，替代 GitHub Actions 构建、测试和发布 Go / TS 二进制与 runtime。品牌名 **Kiln**：二进制 `kiln-server`、`kiln-runner`（CI Runner）、`kiln`（CLI），AI 执行节点的连接器 `kiln-orca`（npm 包），仓库配置目录 `.kiln/`。

本文件是 Codex、Claude Code 等 AI 工具和人共用的唯一项目说明（`CLAUDE.md` 只有一行 `@AGENTS.md`，导入本文件）。项目约定只改这里。

## 1. 先读什么

| 要做的事 | 读 |
|---|---|
| 了解设计、找 `PLAN #x.y` | [docs/PLAN.md](docs/PLAN.md)：三层权威边界、Outline `kiln` 11 篇活规格目录、`PLAN #x.y → Outline K-*` 路由映射与旧归档去向 |
| 写 workflow、查表达式与上下文 | [docs/WORKFLOW.md](docs/WORKFLOW.md)：`.kiln/workflows/*.yml` 用户语法、表达式、`.kiln/runtimes.lock` 与 `kiln lint` |
| 部署、配置、使用 | [docs/DEPLOY.md](docs/DEPLOY.md) |
| HTTP 上传协议 | [docs/UPLOAD-PROTOCOL.md](docs/UPLOAD-PROTOCOL.md) |
| 审计标准（也是写代码的标准） | [.kiln/agent/review.md](.kiln/agent/review.md) |
| AI 开发任务说明 | [.kiln/agent/development.md](.kiln/agent/development.md) |
| 贡献流程 | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Dashboard 产品与设计 | [PRODUCT.md](PRODUCT.md)、[DESIGN.md](DESIGN.md) |
| 进行中的工作 | `gh issue list` |

**文档维护**：正式设计文档在 Outline `kiln` 集合维护（AI 任务经 `kiln-orca task doc list|get` 读取冻结快照，见 [docs/PLAN.md](docs/PLAN.md) 的三层权威边界）。本仓库只保留 `AGENTS.md`（`C1–C23` 唯一生效正文）与版本伴生的手册。代码 PR **不**维护 `docs/archive/`、`PLAN #25` 或 `changelog.md`；正文只写当前方案，不写"原来……现在……"，不引用已关闭的 issue。改动部署、配置或使用方式同步 `docs/DEPLOY.md`，改动 workflow 语法或表达式同步 `docs/WORKFLOW.md`，改动 HTTP 上传协议或前端契约同步 `docs/UPLOAD-PROTOCOL.md` / `DESIGN.md`。

## 2. 已确认的决定

改动不能违反这些决定。`AGENTS.md #2` 是 `C1–C23` 根本决定的**唯一生效正文**（Outline `01 [K-GOV-01]` 只做索引，不双写）。要改先和用户确认，再更新本节；Kiln 的 AI 开发可以在 PR 的决策记录中声明 `confirmed_decision` 并写明理由，这样的 PR 会暂停（`kiln:hold`）等用户确认（C21）。

| # | 决定 | 带来的简化 |
|---|---|---|
| C1 | Server 在公网小 VPS，Runner 在家里或云上 | webhook 直达，无需隧道；家里断电不影响接收和调度 |
| C2 | 只服务自有私有仓库：只接受自己账号或组织的安装，fork PR 一律丢弃 | 被构建的代码都可信：无需信任分级，`host` executor 可放心用，VM 隔离不是安全前提 |
| C3 | PR 构建 head commit，不构建 `refs/pull/N/merge` | Check Run 挂在 head sha；PR 与分支 push 可共用缓存 |
| C4 | 发布目标：GitHub Release、npm、容器镜像、HTTP 上传（`release.uploads`）；asset 由 `release.assets` 统一打包，各文件类目标共用；不用 SSH，不做 S3；Homebrew 以后再做 | 发布全部由 Server 执行 |
| C5 | Kiln 用 Kiln 构建、测试、打包和发布自己（含依赖 Postgres 的测试） | 需要 `services`；自身 workflow 见 PLAN #21.1 |
| C6 | Runner 与 Server、GitHub、存储、上游下载源的网络连通性由部署方保证 | 不讨论代理、镜像源、机房 |
| C7 | 主要语言 Go 和 TS（Node/Bun/pnpm），Windows 产物交叉编译 | importer 优先支持 Go、Node、pnpm、Bun |
| C8 | 只做 CI，不做 CD，不提供审批或人工门禁（由以后的 CD 模块负责） | environment 只有 ref 策略，满足即执行 |
| C9 | Environment、变量、secret 全在 Kiln 中配置（账号级、仓库级、environment 级），不用 GitHub Settings | 版本化、作用域、审计都在 Server |
| C10 | 日志、产物、缓存、镜像等过程数据只保存 12 小时（服务日志 7 天），只长期保留结果；产物由发布目标保存。Runner 装好的 runtime 是工具，不是过程数据：按版本复用，30 天未用或超过容量上限才删；AI 执行节点同样按仓库的 lockfile 准备任务用的 Go、Node、pnpm（`requirements.tools`），与 CI Runner 共用安装和回收的代码，不用宿主机上的版本（2026-11-15 用户决定） | Server 和 Runner 都不需要长期存储；runtime 每次重新下载、安装浪费时间，也会让结果依赖宿主环境 |
| C11 | 管理员与其 GitHub 账号是同一人：绑定后 GitHub 登录即管理员 | 可关闭 Dashboard 的 admin token 登录 |
| C12 | 默认事件驱动，不在启动或定时全量扫描 | 漏掉的事件靠显式补对账或可选的周期对账 |
| C13 | 只写模块的功能测试和单元测试，不写端到端测试 | 外部系统用进程内替身 |
| C14 | Dashboard 用 GitHub 风格，React + `@primer/react`，内嵌在 kiln-server | 概念、位置、图标与 GitHub Actions 一致（[DESIGN.md](DESIGN.md)） |
| C15 | AI 执行节点：`kiln-runner` 不改名（称 CI Runner）；AI 任务由常驻在执行机器上的 Orca Server 完成，Kiln 经连接器 `kiln-orca`（TypeScript，现在是常驻服务，将来作为 Orca 插件运行）把任务交给 Orca、接收结果，Orca 内部怎么分工是它自己的事；节点的角色（审计 / 开发）由注册令牌决定，可同时执行多个任务（`max_tasks`）。Agent 是 Codex 或 Claude Code（按仓库为开发、审计选择；完全权限）；独立审计按 Agent 判断：审计 AI 开发的 PR 的 Agent 不能是开发时用过的 Agent。PR 审计结果发布为 PR review。AI 开发的单次执行最长 12 小时；Server 上的过程数据 12 小时后清理；执行机器上审计和开发的 Orca 工作区（连同临时检出、分支）在结果确认、Agent 停止、终端释放之后由连接器回收，开发没有推到远端的改动不特殊处理、照样回收，日志写明（2026-11-10、2026-11-12、2026-11-16、2026-11-17 用户决定）。git 凭据由 Server 按操作签发、用后吊销（2026-11-16、2026-11-18 用户决定，设计见 Outline `08 [K-ORCA]`，路由见 [docs/PLAN.md](docs/PLAN.md)）：每个短期 installation token 只属于一次能观察到结束的网络操作——连接器的一次准备仓库或推送，Agent 经 `kiln-orca task exec` 发起的一个 git HTTP(S) 传输（由 `GIT_EXEC_PATH` 放入的传输包装持有，包装到连接器的连接关闭即吊销）——只覆盖这次访问的那一个仓库（读：任务仓库或 `requirements.repositories` 声明、与任务仓库在同一个 installation 的依赖仓库；写：只有任务仓库、只签给开发、推完 attempt 分支即吊销），操作之间不共用、不续签；Server 内存中登记操作（`op_id` 幂等、按 `op_id` 关闭、每操作一枚、关闭和 attempt 失效是终态，交出前在数据库中做最终检查），连接器崩溃或吊销失败时以 token 到期（约 1 小时）自然失效兜底；clone 的 origin 是任务仓库的真实 HTTPS 地址（不含 token），Agent 经 `task exec` 照常 `git fetch origin`；连接器的网络 git 与机器上的 git 配置隔离，执行机器上不需要 git 凭据和身份；PR 只由 Server 创建，Server 推进正式分支、写 PR 时各自签发单仓库的窄 token（`contents: write` / `pull_requests: write`），写完吊销；AI 协议 8 升级时排空在途任务。Agent 只通过 `kiln-orca task` 与 Kiln、Orca 交互，审计和开发用法相同、只有 prompt 不同；结束通知（`worker_done`）也由 `kiln-orca task done` 校验后在 Agent 的进程中发送，连接器不冒充 Agent（2026-11-17 用户决定） | AI 有自己的数据、调度、生命周期和协议，与 CI 只共用代码层面的机制（C20）；一台装了多个 Agent 的 Orca 就能全自动运行；新机器装好 Orca 和 kiln-orca、注册后即可执行 |
| C16 | Runner 升级包发布到 npm（公开），不签名；npm 的 registry、scope、token 按账号在 Kiln 中配置，workflow 只写包的短名；Runner 自己从 npm 查询最新版本、下载核对，管理员在 Runner 详情页按台升级；Runner 本地只留两个版本，由固定的启动器切换和回滚；Server 由用户手动换镜像升级 | Server 不保存升级包、不访问 npm（C10 不变）；`release.npm` 不写凭据 |
| C17 | 合并队列由 Kiln 自己实现（不用平台的 merge queue / auto-merge），写操作由 Server 调平台 API 执行；每个仓库只有一个执行者，每次按最新状态重新判断，允许合并的目标分支由 `.kiln/merge.yml` 的 `branches` 配置、各分支独立排队；现在只接 GitHub，队列只依赖平台接口 | 不需要分支保护；没有"判断"和"执行"之间的异步间隔；以后接其他平台只需实现接口 |
| C18 | 经过 PR 合入发版分支（`tag.branch`，不写时是合并那一刻的默认分支）的 commit 由 Server 按仓库规则（`.kiln/merge.yml` 的 `tag:`）打 tag，并直接规划这个 tag 的发布；合入其他允许合并的分支的合并不打 tag，同一个 commit 随后从发版分支合入时重新裁决；开启 `auto` 的仓库（`required` 须含 `ai-review`）在验证和 AI 审计通过后自动入队 | 发版只需要合并 PR；发布仍然只由 tag 触发 |
| C19 | 需要排队、互斥或按顺序执行的场景（同一对象上的多个写操作、重试、并发事件等），按场景考虑沿用合并队列的做法自己实现：状态在 Postgres 中，每个对象只有一个执行者（advisory lock），不依赖外部平台的队列，也不拆成互相独立的异步任务。"每次按最新状态重新判断"只用于幂等或带条件的写操作（合并带期望的 head、建 tag 时名字被占用会失败）；**没有幂等键的外部写操作（评论、关闭 issue）先写意图、最多尝试一次**：调用前记下"已尝试"，结果不明时不再重试，按之后读到的状态记为完成或"结果未知"交给人（2026-11-09 用户决定）。是否采用看场景，简单的单次通知仍可直接用 outbox | 可控：顺序、重试、失败原因都在一处，可在 Dashboard 中看到；不会因为响应丢失或读到滞后的副本重复评论、重复关闭 |
| C20 | CI 与 AI 在领域上彻底分开：AI 任务不放在 `jobs` / `runs` / `attempts`，有自己的表、调度、生命周期和协议（`airunner.v2`）；Runner 分为共用身份和 CI / AI 各自的表，Dashboard 分成 CI Runner 与 AI Runner 两个入口；两边只共用代码层面的机制（认证、升级、租约工具、日志、CAS、outbox）。CI 求稳，AI 按频繁演进设计（2026-11-10 用户决定） | AI 加任务类型、Agent、改协议时不牵动 CI；CI 的调度和表结构不再有 AI 分支 |
| C21 | AI 开发的设计由开发 Agent 自己完成，不要求人先写好；默认在同一次执行中先设计再开发，一个 PR 完成；Agent 判断一个 PR 完成不了时，先交付设计 PR，审计通过并合并后，Server 自动开始实现、交付实现 PR，审计通过并合并后按完成度关闭 Issue。同一个工作项，复用现有的 PR、审计、返修和合并队列；设计 PR 不关闭 Issue。有疑问的决定不等人确认：Agent 在 PR 中记录决策过程供复盘，经独立审计后照常合并；只有影响已确认的决定（根本决策）时 PR 暂停（`kiln:hold`），由人确认（2026-11-11 用户决定） | 不要求人先写实现设计；能一个 PR 完成的不多一轮 PR，完成不了的先让设计经过独立审计；不新增审计、合并系统或通用工作流 |
| C22 | 领取（Native）或预留（托管）即 `running`：同一事务中选中、`epoch+1`、创建 attempt。Native Runner 领取时签发租约、立即开始执行；托管执行在实际开始前由 attempt 的 `phase` 区分已预留、已提交、已在外部排队，期间占用配额但 `jobs.started_at` 为空、Check Run 显示 queued；Native 与托管经同一个调度核心和原子预留入口（2026-11-11 用户确认） | 不加 job 状态，所有按 `running` 计数的规则（仓库并发、max-parallel、同 key 合并）自动计入预留；同一个 job 不会被两种触发方式各领一次 |
| C23 | AI 开发同一个仓库按完整的工作项串行、跨仓库并行：仓库中有未结束的工作项（从领取到合并或终止，包括等待 CI / 审计 / 合并、返修、受阻、设计合并后的实现）时不领取新的；等待期间不占执行槽位，独立审计照常。合并冲突与审计不通过按 PR 的归属处理：Server 记录的工作项的 PR 自动返修原 PR（解决冲突时把最新 base 合并进来，正式分支只快进），新 head 重新经过 CI 和独立审计后按既有策略入队；第三方 PR 由提交者处理，不派开发任务。归属只按 Server 记录的工作项与 PR，不看作者、分支名或正文标记（2026-11-17 用户决定；设计与决策记录见 Outline `07 [K-AI-CORE]` / `06 [K-GH-REL]`，路由见 [docs/PLAN.md](docs/PLAN.md)） | Bot PR 之间不再互相冲突；人工或第三方合并引起的冲突不再让自动开发中断，也不需要人重新打标签或重试 |

## 3. 代码地图

| 位置 | 内容 |
|---|---|
| `cmd/` | `kiln-server`、`kiln-runner`、`kiln` 三个入口 |
| `internal/server/` | Server 组装层（跨领域接口在这里接线）；按领域分为 `ci/`、`ai/`、合并（`mergequeue`、`autotag`、`mqevents`）和共用（`store`、`outbox`、`runnerauth`、`gitcred` 等） |
| `internal/server/ci/` | CI 领域：`engine`（状态机与执行）、`planner`（规划）、`publish`（发布）、`runnerapi`、`cistore`、`scheduler`、`cicheck`、`retention` |
| `internal/server/ai/` | AI 领域：`review`（独立审计）、`aidev`（开发工作项与返修）、`aiprompt`、`aispec`、`docsync`（外部文档冻结快照）、`decisioninbox`（待决策清单同步）等 |
| `internal/docprovider` | 外部文档提供方（Outline）：只读快照拉取与待决策清单写入 |
| `internal/runner/` | CI Runner |
| `kiln-orca/` | AI 执行节点的连接器（TypeScript）：`src/core/` 协议、领取、执行状态机、恢复、交付校验，`src/host/` 服务、配置、本机工具；`pnpm lint && pnpm test` |
| `internal/runnerkit` | CI Runner 用的配置、注册、认证、日志上传、升级、服务安装 |
| `internal/archtest` | 领域边界检查（依赖、表归属）；新包、新表要先在这里归类 |
| `internal/workflow`、`expr`、`actionkey` | workflow 解析、表达式、action key |
| `internal/github`、`forge` | GitHub 客户端；平台接口（合并队列只依赖它） |
| `internal/agentcfg`、`aireport`、`devreport`、`mergecfg` | `.kiln/agent/config.yml`、AI 审计报告、AI 开发的选题和交付报告、`.kiln/merge.yml` |
| `internal/npm`、`oci`、`upload`、`release` | 发布协议与打包 |
| `internal/upgrade`、`toolchain` | Runner 自升级共用部分；runtime |
| `proto/` → `gen/` | CI Runner（`runner.v1`）、AI 执行节点（`airunner.v2`，Go 与 `kiln-orca/src/gen` 的 TS）、Admin API（`make generate`） |
| `migrations/` | Postgres 迁移，启动时自动执行 |
| `web/` | Dashboard 前端（Vite + TS + pnpm） |
| `.kiln/` | Kiln 自己的 workflows、`merge.yml`、`agent/`（审计、开发的配置和说明） |

## 4. 开发与验证

```bash
make lint        # golangci-lint（go tool，版本由 tools/golangci/go.mod 锁定，按 Makefile 的 TARGETS 每个平台各查一次）+ go mod tidy -diff + 前端 eslint / tsc
make test        # go vet + go test -race + 前端 vitest
make build
make generate    # 改了 proto 时
make fmt         # gofumpt + goimports
go run ./cmd/kiln lint   # 改了 .kiln/ 时
```

- 环境：Go（版本见 `go.mod`）、Node 24+、pnpm（`cd web && pnpm install --frozen-lockfile`）。数据库测试需要 `KILN_TEST_DATABASE_URL` 或本机 `initdb` / `pg_ctl`，**都没有时会被跳过，这样的通过不算数**。
- 只写单元测试和模块功能测试，**不写端到端测试**。替身：Postgres `internal/pgtest`、GitHub `internal/fakegithub`、镜像仓库 `internal/oci/ocitest`、HTTP 上传 `internal/fakeupload`、npm `internal/fakenpm`，前端 vitest 组件测试。测试必须在代码改坏时失败（审计会做变异验证）。
- 最佳实践：Go 读 `.claude/skills/golang-*/SKILL.md`，TS 读 `.claude/skills/typescript-best-practices/SKILL.md`，只读与改动相关的。来源记在 `skills-lock.json`，用 `npx skills update -p` 更新。
- Server 组件在构造时注入依赖，不新增包级可变状态；不写魔法数字；同一个值、同一条规则只定义一次。

## 5. 协作约定

- 用中文沟通；文档、注释、提交信息用中文。
- 交互会话中，提交或推送前先征求用户同意；不在 `master` 上直接提交，先建分支。
- PR 描述关联 issue（`Fixes #N` 只用于完整解决的 issue）。本仓库 PR 跑 `ci` 和 `ai-review`，结论发为 PR review；`.kiln/merge.yml` 开了 `auto: true`，验证通过后自动入队、squash 合并并打下一个补丁版本 tag（`release:minor` / `release:major` / `release:skip` 改变版本递增，`kiln:hold` 不自动入队）。
- 回复审计 finding 时说明改了什么或不采纳的理由，不要只写"已修复"。
- 被要求做审计或审查时：只产出报告、PR review 或 issue，不改代码、不留下临时测试文件，除非用户明确要求修复。
