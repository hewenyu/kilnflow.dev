# Kiln 设计路由索引

> 品牌名 Kiln（窑，烧制产物的地方），仓库名 kilnflow.dev。本文件是**路由索引**，不是设计正文：正式设计文档在 Outline `kiln` 集合维护，`PLAN #x.y` 与 `docs/design/*` 的旧引用按下文映射到 Outline 活规格。仓库里 `AGENTS.md #2` 是 `C1–C23` 根本决定的唯一生效正文。

## 一、三层权威边界（SSoT，绝不双写）

**1. Git 仓库（`R_repo`，与代码版本强绑定的规则与契约）**

- [AGENTS.md](../AGENTS.md)：`C1–C23` 根本决定的**唯一生效正文**与项目约定。
- `.kiln/agent/*`：审计标准（[review.md](../.kiln/agent/review.md)）与 AI 开发任务说明（[development.md](../.kiln/agent/development.md)）。
- [README.md](../README.md)、[CONTRIBUTING.md](../CONTRIBUTING.md)：项目入口与贡献流程。
- [docs/DEPLOY.md](DEPLOY.md)：部署、配置与使用手册。
- [docs/WORKFLOW.md](WORKFLOW.md)：`.kiln/workflows/*.yml` 用户语法、表达式与校验。
- [docs/UPLOAD-PROTOCOL.md](UPLOAD-PROTOCOL.md)：HTTP 上传协议（`release.uploads`）。
- [DESIGN.md](../DESIGN.md)、[PRODUCT.md](../PRODUCT.md)：前端 Primer Token 与组件实现契约、产品定位入口。
- 本路由索引 `docs/PLAN.md`。

**2. Outline 绑定集合 `kiln`（Kiln 只读 Viewer 可访问）**

正式设计活规格（见下节目录），Kiln 的 AI 任务在规划时把它冻结成不可变快照，Agent 经 `kiln-orca task doc list|get` 只读消费。`01 [K-GOV-01]` 只索引 `AGENTS.md` 的 `C1–C23`，不双写正文。

**3. Outline 未绑定工作区集合（Kiln 只读 Viewer 无权访问）**

存放维护者的「待决策清单（Decision Inbox）」以及尚未落地的远期提案（如 `PLAN #15 / #17 / #19` 等）与草稿。Kiln 的 Server 只把 AI 开发疑难点单向同步到指定的待决策文档，不读取其正文。

## 二、Outline `kiln` 活规格目录

| 编号 | 范围 |
|---|---|
| `00 [K-INDEX]` | 总览、三层权威边界、旧 `PLAN #x.y` 映射 |
| `01 [K-GOV-01]` | 根本约束索引与变更提案（只索引 `AGENTS.md` `C1–C23`，不双写正文） |
| `02 [K-ARCH-01]` | 系统架构、C20 领域边界、数据归属、安全与凭据模型 |
| `03 [K-CI-WF]` | Workflow 语义、表达式求值与构建规划 |
| `04 [K-CI-EXEC]` | CI Job 生命周期、统一调度（C22）、租约 / epoch 与 Runner 执行器 |
| `05 [K-CI-STORE]` | 存储、CAS 缓存、action key、Runtime（含复用，C10）与 CI Runner 自升级 |
| `06 [K-GH-REL]` | GitHub 集成、多目标发布、合并队列（C17）与自动打 tag / 自动入队（C18）、外部写操作至多一次（C19） |
| `07 [K-AI-CORE]` | AI 任务领域、独立审计、开发工作项与返修（C21、C23） |
| `08 [K-ORCA]` | Orca 执行节点、`kiln-orca`、受管理工具链与按操作 git 凭据（C15） |
| `09 [K-DOC]` | 外部文档绑定、只读冻结快照与待决策清单同步 |
| `10 [K-PRODUCT]` | 产品定位、用户场景与 Dashboard 信息架构 |
| `11 [K-RFC-*]` | 已批准的在途演进 RFC（完成后并入对应活规格并归档） |

## 三、`PLAN #x.y → Outline K-*` 兼容映射

代码注释、提交说明和历史文档中的 `PLAN #x.y` 按下表查找。旧 `PLAN.md` 与 `docs/archive/` 的章节号不变。

| PLAN 章节 | 内容 | Outline 活规格 / 去处 |
|---|---|---|
| #0 | 摘要 | `00 [K-INDEX]` |
| #1（#1.1–#1.5） | 背景、目标与范围、已确认的决定、假设 | `00 [K-INDEX]`；#1.4 的 `C1–C23` 正文在 [AGENTS.md #2](../AGENTS.md)，`01 [K-GOV-01]` 只索引 |
| #2、#3 | 术语、总体架构 | `02 [K-ARCH-01]` |
| #4 | 构建配置规范（workflow 写法、特有字段、表达式、`runtimes.lock`、校验） | `03 [K-CI-WF]`；用户手册见 [docs/WORKFLOW.md](WORKFLOW.md) |
| #5 | 领域模型与数据库 | `02 [K-ARCH-01]` / `05 [K-CI-STORE]` |
| #6 | Job 生命周期（状态机、租约与 epoch、重试、取消、重跑、`effects`） | `04 [K-CI-EXEC]` |
| #7 | Server 详细设计（Ingest、Reconciler、Planner、Publisher） | `04 [K-CI-EXEC]` / `02 [K-ARCH-01]` |
| #8 | Runner 详细设计（Executor、本地目录与缓存、签名、progress） | `04 [K-CI-EXEC]` |
| #9 | 通信协议（`runner.v1`） | `04 [K-CI-EXEC]` |
| #10、#11 | 存储与保留、缓存与 action key | `05 [K-CI-STORE]` |
| #12（含 #12.8） | Runtime 体系与复用 | `05 [K-CI-STORE]` |
| #13.1–#13.3、#13.5 | GitHub 集成（App、token、Check Run、速率） | `06 [K-GH-REL]` |
| #13.4、#13.6–#13.9 | 发布（流程、跨仓库、npm、镜像、HTTP 上传） | `06 [K-GH-REL]`；上传协议见 [docs/UPLOAD-PROTOCOL.md](UPLOAD-PROTOCOL.md) |
| #14 | 调度算法 | `04 [K-CI-EXEC]` |
| #15 | Provider 与弹性伸缩（未实现） | 未实现远期提案；关联 `04 [K-CI-EXEC]` |
| #16 | 安全模型（信任边界、secret、environment、Dashboard 访问控制） | `02 [K-ARCH-01]` |
| #17 | 可复现性与供应链（大部分未实现） | 未实现远期提案；关联 `05 [K-CI-STORE]` |
| #18 | 可观测性与 Dashboard | `10 [K-PRODUCT]` |
| #19 | 测试与验证策略 | `00 [K-INDEX]` / 未实现远期提案 |
| #20、#21（#21.1 除外） | 部署与运维、代码结构 | `02 [K-ARCH-01]`；代码地图见 [AGENTS.md #3](../AGENTS.md) |
| #21.1 | Kiln 自身的构建、测试、打包与发布 workflow | `06 [K-GH-REL]`（发布流程）；workflow 见仓库 `.kiln/workflows/` |
| #22、#23、#24 | 里程碑、风险、待决问题 | `00 [K-INDEX]` / 待决策清单 |
| #25 | v0 实现与设计的差异、代码约定 | 已消化进各域活规格（`02 [K-ARCH-01]` 与对应域）；不再保留平行"偏差总页" |
| #26 | AI 审计与开发（业务规则） | `07 [K-AI-CORE]` |
| #27 | npm 发布（见 #13.7）与 Runner 自升级 | `05 [K-CI-STORE]` |
| #28 | 合并队列 | `06 [K-GH-REL]` |
| #29 | 自动打 tag 与自动入队、外部写操作至多一次 | `06 [K-GH-REL]` |
| #30 | CI 与 AI 领域拆分 | `02 [K-ARCH-01]` |
| #31 | AI 开发：自主设计，一个 PR 完成不了时先交付设计 PR | `07 [K-AI-CORE]` |
| #32 | 统一 CI 调度核心（Native 与托管执行共用的匹配与预留） | `04 [K-CI-EXEC]` |
| #33 | AI 执行节点（Orca Server + `kiln-orca`、`airunner.v2`、工具链、工作区回收、git 凭据） | `08 [K-ORCA]` |
| 附录 A | 参考资料 | `00 [K-INDEX]` |

原 `docs/design/` 的历史设计稿去向：

| 旧文件 | Outline 活规格 |
|---|---|
| `outline-docs.md` | `09 [K-DOC]` |
| `git-credentials-per-operation.md` | `02 [K-ARCH-01]`（凭据模型）/ `08 [K-ORCA]`（按操作签发实现） |
| `ai-dev-serial-and-conflict-fix.md` | `07 [K-AI-CORE]`（工作项串行与返修）/ `06 [K-GH-REL]`（PR 归属与合并） |

## 四、旧归档

`docs/archive/`、`docs/design/` 已从仓库删除，不保留取回方式；代码注释、提交说明里的旧路径按上一节映射到 Outline 活规格，不新增对它们的引用。
