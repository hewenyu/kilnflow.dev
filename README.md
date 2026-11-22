<div align="center">

<img src="web/public/favicon.svg" width="64" height="64" alt="Kiln Logo" />

# Kiln (`kilnflow.dev`)

**用你自己的机器构建、测试、AI 审计并发布 GitHub 私有仓库的自托管 CI**

[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg?style=flat-square)](LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/hewenyu/kilnflow.dev?sort=semver&style=flat-square)](https://github.com/hewenyu/kilnflow.dev/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/dl/)
[![Node](https://img.shields.io/badge/Node-24%2B-339933?style=flat-square&logo=node.js&logoColor=white)](https://nodejs.org/)
[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-Community-1C1C1E?style=flat-square&logo=discourse&logoColor=white)](https://linux.do)

<p>
  <a href="#项目简介">项目简介</a> ·
  <a href="#核心特性">核心特性</a> ·
  <a href="#设计边界与安全说明">设计边界</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#安装方式">安装方式</a> ·
  <a href="#系统架构">系统架构</a> ·
  <a href="#文档索引">文档索引</a> ·
  <a href="#社区交流与致谢">LINUX DO 社区</a>
</p>

</div>

---

## 项目简介

**Kiln** 是一套面向独立开发者与小团队的自托管持续集成系统：由一台轻量控制面（`kiln-server` + Postgres）和任意多台构建机（`kiln-runner`）组成。workflow 直接写在仓库的 `.kiln/workflows/*.yml` 中，触发后由你自己的机器执行，状态原样回报为 GitHub Check Run，多目标发布则由 Server 集中完成；需要时还可以接入 AI 执行节点（`kiln-orca` + Orca），让 Codex / Claude Code 自动独立审计 PR，甚至从 Issue 自主设计、编码、提 PR 并走完合并发版闭环。

如果你有闲置的 **Mac mini、家庭 Linux 主机或云主机**，主要写 Go / TypeScript，不想再受限于 GitHub Actions 的分钟数与计费策略，同时又想保留 GitHub 风格的清爽体验与可追溯的状态视图——Kiln 就是为此而生。

> **Dogfooding**：Kiln 本身就是用 Kiln 构建、测试、AI 审计、合并并发布的——仓库里的 [`.kiln/`](.kiln/) 就是它自己的生产级配置。

---

## 核心特性

### 为什么选择 Kiln

- **机器完全自主**：构建在自己注册的 Runner 上运行，不消耗 GitHub Actions 分钟数；Runner 通过长轮询**主动出站**连接 Server，家里或内网构建机无需公网 IP。
- **零门槛迁移语法**：`.kiln/workflows/*.yml` 沿用 GitHub Actions 熟悉结构（`on`、`jobs`、`needs`、`if`、矩阵、并发组、`services`、`container:`），写过 Actions 就能上手。
- **输入不变就不重跑**：按 job 的全部输入计算 action key，命中分布式缓存的 job 直接成功，不占构建机、不浪费时间。
- **发布集中在 Server**：推送 `v*` tag 后由 Server 统一发布到 GitHub Release、npm、Docker Hub 或自建 HTTP 下载站；发布凭据不下发到构建机，支持从私有仓库跨仓库发布到同账号的公开仓库。
- **内置合并队列与自动发版**：每个仓库独立串行合并队列，按最新状态重判；PR 验证与审计通过后可自动入队合并、按 `.kiln/merge.yml` 自动打下一个版本 tag 并规划发布。
- **原生 AI 审计与开发闭环**：执行机上的 Orca 经连接器 `kiln-orca` 领取任务，由 Codex 或 Claude Code 完成 PR 独立审计（发为 PR review）或从标记 `kiln:ready` 的 Issue 自动开发并开 PR；严格保证**审计 Agent 与开发 Agent 相互独立**。
- **极简运维**：Runner 一次性令牌一条命令接入，支持 Dashboard 按台自升级与启动失败自动回滚；受管理的 Go / Node / pnpm / Bun runtime 按版本复用，30 天未用自动回收。
- **一切状态可追问**：唯一状态源在 Server 的 Postgres 中，内嵌 GitHub 风格 Dashboard（React + Primer）与 `kiln` CLI，随时查清每个 job 为什么排队、为什么失败。

### 功能一览

| 模块 | 提供的能力 |
|---|---|
| 触发与规划 | push / tag / PR / 手动 dispatch；矩阵、`needs`、`if`、并发组、同 key 合并、job 级超时与资源声明 |
| 执行环境 | 主机直接执行或 `container:` 容器隔离；`services`（如测试用 Postgres）；BuildKit 构建多架构镜像；macOS App 签名与公证 |
| 工具链与配置 | 受管理 runtime（Go、Node、pnpm、Bun，由 `.kiln/runtimes.lock` 锁定）；`toolcache` 持久化；账号 / 仓库 / environment 分级 secret 与变量（信封加密） |
| 缓存与回收 | 基于 action key 的分布式缓存、git bare 镜像、OCI 镜像缓存；Runner 本地容量上限与自动 GC |
| 多目标发布 | GitHub Release、npm（含各平台 Runner 升级包）、Docker Hub、HTTP 上传；asset 统一打包、支持跨仓库发布 |
| 合并与发版 | 内置合并队列（不依赖 GitHub 分支保护）、支持多目标分支独立排队、合入发版分支后自动打 tag |
| AI 执行节点 | 独立 PR 审计（PR review）；Issue 自动选题 → 设计 → 实现 → 冲突/审计返修 → 自动合并；支持外部文档（Outline）冻结快照与待决策清单同步 |
| 运维与管理 | 一次性注册令牌、Runner 按台升级与回滚、排空、缓存清单、补对账；Server 内嵌 Dashboard；完整 `kiln` CLI |

---

## 设计边界与安全说明

这些是 Kiln 明确的设计取舍与安全原则，便于你在部署前快速评估是否契合自己的场景：

- **只服务自有私有仓库**：GitHub App 仅接受白名单账号（`KILN_ALLOWED_ACCOUNTS`）的安装，**所有 fork PR 一律拒绝构建与审计**，不提供放开开关，以此保证受控信任边界。
- **凭据最小化与隔离**：
  - 所有发布 token、secret 均使用主密钥信封加密存储在 Server 侧，不下发给构建机或浏览器；
  - CI 与 AI 的 git 凭据均为 Server 按单次网络操作签发的短期单仓库 installation token，**操作结束立即吊销**，构建机与 AI 执行机无需配置个人 git 凭据或 SSH Key；
  - 仅在用户显式配置的范围内运行，不采集遥测数据，不上传本地私有信息。
- **专注 CI，不做 CD**：不设审批流或人工门禁，发布仅由 `v*` tag 触发。
- **不兼容 Marketplace Action**：不支持 `uses:`；表达式仅支持规划期可求值的上下文（`github`、`matrix`、`inputs`、`vars`、`secrets` 等）。
- **平台支持**：Runner 支持 `darwin/arm64`、`linux/arm64`、`linux/amd64`；Windows 产物通过交叉编译产出。
- **轻量存储（过程数据保留 12 小时）**：日志、产物、缓存到期自动清理，长期仅保留元数据与结果；正式产物由发布目标持久化。

---

## 快速开始

下面是从零跑通第一个 CI 的最短路径（完整参数与排错见 [docs/DEPLOY.md](docs/DEPLOY.md)，尖括号 `<…>` 替换为你的实际值）。

### 环境前提

| 组件 | 要求 |
|---|---|
| **Server** | 一台装有 Docker Compose 的 Linux 主机，具备 GitHub 可访问的 HTTPS 公网入口（反代或 Cloudflare Tunnel / Tailscale Funnel，转发至 `127.0.0.1:8080`） |
| **Runner** | 一台构建机（`darwin/arm64`、`linux/arm64` 或 `linux/amd64`），可与 Server 同机 |
| **仓库** | 一个属于你自己 GitHub 账号的私有仓库（下以 Go 仓库为例） |

### 1. 创建 GitHub App

按 [DEPLOY #3](docs/DEPLOY.md#3-创建-github-app) 创建仅限自己账号安装的 GitHub App，勾选对应权限与事件，Webhook URL 填写 `https://<你的域名>/webhook`。完成后保存 App ID、Webhook Secret 和私钥 `.pem`。

### 2. 启动 `kiln-server`

```bash
git clone https://github.com/hewenyu/kilnflow.dev.git && cd kilnflow.dev/deploy
cp server.env.example server.env && cp <下载的私钥>.pem github-app.pem
openssl rand -hex 24   # 生成 KILN_ADMIN_TOKEN
openssl rand -hex 32   # 生成 KILN_MASTER_KEY（务必备份：丢失将无法解密已存 secret）

# 编辑 server.env，填入：
# KILN_PUBLIC_URL、KILN_ADMIN_TOKEN、KILN_MASTER_KEY、
# KILN_GITHUB_APP_ID、KILN_GITHUB_WEBHOOK_SECRET、KILN_ALLOWED_ACCOUNTS（你的 GitHub 登录名）

docker compose up -d && docker compose logs -f server   # 镜像 hewenyulucky/kilnflow:latest + Postgres 17
```

看到 `kiln-server 已启动` 后，打开 `KILN_PUBLIC_URL` 用 admin token 登录 Dashboard，并将 GitHub App **安装**到你的账号、勾选要接入的仓库。其他运行方式（systemd / launchd）见 [DEPLOY #4](docs/DEPLOY.md#4-启动-kiln-server)。

### 3. 安装 CLI 并注册 Runner

```bash
# 下载 kiln 与 kiln-runner（<os>-<arch> 可选 darwin-arm64 / linux-arm64 / linux-amd64，校验和见 SHA256SUMS）
for p in kiln kiln-runner; do
  curl -fL -o "$p" "https://github.com/hewenyu/kilnflow.dev/releases/latest/download/$p-<os>-<arch>"
done
chmod +x kiln kiln-runner && sudo mv kiln kiln-runner /usr/local/bin/

# 登录控制面并签发一次性注册令牌
kiln login --server https://<你的域名>
kiln runner token

# 在构建机以普通用户执行（如需 Docker 执行，该用户需有 docker 权限）
kiln-runner enroll --server https://<你的域名> --token <注册令牌> --name <runner 名称>
kiln-runner doctor && kiln-runner service install
sudo loginctl enable-linger "$USER"        # 仅 Linux：确保无人登录时服务常驻
```

运行 `kiln runner list` 或打开 Dashboard 的 **Runner** 页即可确认在线状态。详见 [DEPLOY #5.1](docs/DEPLOY.md#51-注册-ci-runner)。

### 4. 添加第一个 Workflow

```yaml
# .kiln/workflows/ci.yml
name: ci
on: { push: { branches: [main] }, pull_request: {} }   # main 改成你的默认分支
jobs:
  test:
    runs-on: linux/amd64        # 改成你的 Runner 平台：darwin/arm64、linux/arm64 或 linux/amd64
    runtimes: [go]              # 按 .kiln/runtimes.lock 准备 Go，版本取自 go.mod
    network: true               # job 默认断网，拉取依赖时打开
    toolcache:
      - { name: gomod, env: GOMODCACHE }
      - { name: gocache, env: GOCACHE }
    steps:
      - run: go test ./...
```

```bash
kiln runtime lock    # 生成 .kiln/runtimes.lock（需联网）
kiln lint            # 本地校验 workflow
git add .kiln && git commit -m "chore: 加入 Kiln CI" && git push
```

### 5. 查看构建与进阶玩法

push 后 GitHub 提交页会出现 `ci (push)`、`ci / test (push)` Check Run；Dashboard 的 **运行** 页提供依赖图、实时日志与产物详情，也可用 `kiln runs --repo <owner>/<仓库>` / `kiln logs <job 编号> -f` 查看。如遇排队或未触发，见 [DEPLOY #9 排查](docs/DEPLOY.md#9-排查)。

| 进阶场景 | 参考文档 |
|---|---|
| 配置 secret / 变量 / environment，tag 自动发布到 Release、npm、Docker Hub | [DEPLOY #7](docs/DEPLOY.md#7-secret变量environment发布) |
| PR 加 `kiln:merge` 自动入队合并、合并后自动打 tag 发版 | [DEPLOY #8.1](docs/DEPLOY.md#81-合并队列)、[#8.2](docs/DEPLOY.md#82-合并后自动打-tag) |
| 接入 Orca + `kiln-orca` 开启 AI 独立审计与 Issue 自动开发 | [DEPLOY #5.6](docs/DEPLOY.md#56-ai-执行节点orca--kiln-orca)、[#8.3](docs/DEPLOY.md#83-ai-开发) |
| Runner 配置、按台升级、缓存管理与日常运维 | [DEPLOY #5](docs/DEPLOY.md#5-runner)、[#8](docs/DEPLOY.md#8-日常操作) |

---

## 安装方式

| 组件 | 获取方式 | 支持平台 |
|---|---|---|
| `kiln-server` | Docker Hub 镜像 `docker.io/hewenyulucky/kilnflow`；或 [GitHub Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 二进制 | `linux-arm64`、`linux-amd64`、`darwin-arm64`（镜像仅 Linux） |
| `kiln-runner` | [GitHub Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 二进制；或 npm `@yuebanlaosiji/kilnflow-runner-<os>-<arch>` | `darwin-arm64`、`linux-arm64`、`linux-amd64` |
| `kiln`（CLI） | [GitHub Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 二进制 | `darwin-arm64`、`linux-arm64`、`linux-amd64` |
| `kiln-orca`（AI 节点连接器） | npm `@yuebanlaosiji/kilnflow-orca`（需 Node.js 24+） | 跨平台 |

Server 依赖 Postgres 17。CI Runner 支持从 npm 检测新版本并在 Dashboard 按台升级（失败自动回滚）；Server 通过更新容器镜像手动升级。

---

## 系统架构

```text
                 webhook                        长轮询（仅出站）
  GitHub ───────────────────▶ kiln-server ◀──────────── kiln-runner × N   CI：主机 / Docker / BuildKit
  (App)  ◀── Check Run、Release、──┤  │   ◀──────────── kiln-orca × N     AI 审计 / 开发：Orca + Codex / Claude Code
             PR review             │  Postgres（唯一状态源）
                       Dashboard ──┘
                       kiln CLI
```

| 组件 | 职责 | 部署位置 |
|---|---|---|
| **GitHub App** | 推送 push、PR、tag、Issue 事件；承载 Check Run、Release、PR review | 你的 GitHub 账号 |
| **`kiln-server`** | 唯一控制面：事件接收、规划调度、对外写操作（发布、合并、打 tag、评论），内嵌 Dashboard | 公网可达的小 VPS（配 Postgres） |
| **`kiln-runner`** | 无状态 CI Runner：领取 job，在宿主机或 Docker 容器中执行并回传日志与产物 | 任意数量构建机 |
| **`kiln-orca`** | AI 执行节点连接器：领取审计/开发任务，交由本机 Orca 驱动 Codex / Claude Code 并交回结果 | 安装了 Orca 与 Agent 的机器 |
| **`kiln`** | 命令行工具：登录、签发令牌、查日志、重跑、配置 secret 等，与 Dashboard 共用 Admin API | 开发机 |

> CI 与 AI 在领域模型、数据表、调度与通信协议（`runner.v1` vs `airunner.v2`）上彻底隔离，仅共享认证、日志、存储、outbox 等基础设施机制。

---

## 文档索引

| 文档 | 说明 |
|---|---|
| [docs/DEPLOY.md](docs/DEPLOY.md) | 完整部署与运维手册：GitHub App、Server、Runner、AI 节点、workflow、发布、排查与升级 |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | `.kiln/workflows/*.yml` 语法规范、上下文表达式、`.kiln/runtimes.lock` 与 `kiln lint` |
| [docs/PLAN.md](docs/PLAN.md) | 设计路由索引：三层权威边界与规格目录映射 |
| [docs/UPLOAD-PROTOCOL.md](docs/UPLOAD-PROTOCOL.md) | HTTP 上传协议（`release.uploads`），供自建下载站实现服务端 |
| [kiln-orca/README.md](kiln-orca/README.md) | AI 执行节点连接器 `kiln-orca` 安装与命令说明 |
| [PRODUCT.md](PRODUCT.md) / [DESIGN.md](DESIGN.md) | Dashboard 产品定位与 Primer 设计系统契约 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献流程、提交规范与测试要求 |
| [AGENTS.md](AGENTS.md) | 项目核心决定（C1–C23）、代码地图与 AI 协作规范 |

---

## 仓库结构

```text
.
├── cmd/                    # 三个 Go 入口：kiln-server、kiln-runner、kiln
├── internal/server/        # Server 核心：ci/、ai/、mergequeue、autotag 及共用设施
├── internal/runner/        # CI Runner 执行引擎
├── internal/runnerkit/     # Runner 配置、注册、日志上传与自升级
├── internal/workflow/      # workflow 解析、表达式求值与 action key
├── kiln-orca/              # AI 执行节点连接器（TypeScript）
├── proto/ → gen/           # Protobuf 协议定义与 Go/TS 生成代码
├── web/                    # Dashboard 前端（React + @primer/react + Vite，内嵌进 kiln-server）
├── migrations/             # Postgres 自动迁移脚本
├── deploy/                 # Docker Compose 与环境变量模板
└── .kiln/                  # Kiln 自身的 CI/发布 workflow、合并配置与 AI 审计说明
```

---

## 本地开发与参与贡献

环境要求：Go（版本见 [`go.mod`](go.mod)）、Node.js 24+、pnpm，以及用于数据库测试的本机 `initdb` / `pg_ctl`（或通过 `KILN_TEST_DATABASE_URL` 指定可自由建库的 Postgres；两者均无时数据库测试会被跳过，**跳过不算通过**）。

```bash
cd web && pnpm install --frozen-lockfile && cd ..
cd kiln-orca && pnpm install --frozen-lockfile && cd ..

make lint                   # 多平台 golangci-lint + go mod tidy -diff + web / kiln-orca lint & tsc
make test                   # go vet + go test -race（含 Postgres）+ web / kiln-orca vitest
make web build              # 构建前端并编译 cmd/ 二进制到 dist/local/
make fmt                    # gofumpt + goimports
go run ./cmd/kiln lint      # 修改 .kiln/ 后校验 workflow
```

修改 `proto/` 后运行 `make generate` 同步 Go 与 TS 生成代码。

**参与贡献**（详见 [CONTRIBUTING.md](CONTRIBUTING.md)）：
- 建议先开 Issue 讨论需求或 Bug，一个 PR 只解决一件事；
- 本地需跑通 `make lint`、`make test`，提交前可参考 [.kiln/agent/review.md](.kiln/agent/review.md) 做一轮自查；
- 由于安全边界限制，**fork PR 不会触发本仓库的自动构建与审计**，请在 PR 中附上本地验证结果。

---

## 项目状态

Kiln 已处于**长期活跃自用与持续演进**阶段：CI、多目标发布、合并队列、自动打 tag、AI 独立审计与开发、Runner 按台自升级等核心链路已在自身仓库稳定运行，正式构建产物发布在 [GitHub Releases](https://github.com/hewenyu/kilnflow.dev/releases)。

后续规划方向：cosign 镜像签名、provenance 与 SBOM、`on: schedule` 定时触发、通知与监控指标、本地复现命令 `kiln run`、macOS 虚拟机执行器（tart）、弹性伸缩以及周期性 AI 审计。

---

## 社区交流与致谢

本项目在 **[LINUX DO](https://linux.do/)** 社区开源分享，欢迎各位佬友试用、交流架构设计、反馈 Bug 或提出改进建议：

- **社区交流**：欢迎前往 [LINUX DO](https://linux.do/) 社区交流讨论，一起折腾自托管基础设施与 AI Coding 工作流
- **问题反馈**：直接在 [GitHub Issues](https://github.com/hewenyu/kilnflow.dev/issues) 提交 Bug 报告或功能建议
- **安全披露**：发现安全漏洞请勿公开提交 Issue，请通过 [GitHub Security Advisory](https://github.com/hewenyu/kilnflow.dev/security/advisories/new) 私下联系

感谢 [LINUX DO](https://linux.do/) 社区真诚、友善、团结、专业的开源交流氛围。如果 Kiln 对你有帮助，欢迎点一个 **Star** 支持！

---

## 开源协议

本项目基于 [Apache-2.0 License](LICENSE) 开源。
