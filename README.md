# <img src="web/public/favicon.svg" width="34" align="top" /> Kiln

**用你自己的机器构建、测试和发布 GitHub 私有仓库的自托管 CI。**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/hewenyu/kilnflow.dev?sort=semver)](https://github.com/hewenyu/kilnflow.dev/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Node](https://img.shields.io/badge/Node-24%2B-339933?logo=node.js&logoColor=white)](https://nodejs.org/)

Kiln 是一个自托管的持续集成系统：一台控制面（`kiln-server`）+ 任意多台构建机（`kiln-runner`）。workflow 写在仓库的 `.kiln/workflows/*.yml` 里，触发后由你自己的机器执行，结果照常显示为 GitHub Check Run，发布由 Server 统一完成。需要时，还可以让 AI Agent 审计每个 PR、从 Issue 开发并开 PR。

它适合有这样需求的人：有自己的构建机（Mac mini、Linux 主机或云主机）、主要写 Go / TypeScript、想摆脱 Actions 的分钟数和计费政策、又希望构建状态集中可查。**Kiln 用 Kiln 构建、测试、审计和发布自己**——仓库里的 [`.kiln/`](.kiln/) 就是它自己的 CI 配置。

[功能](#功能) · [快速开始](#快速开始) · [部署与使用手册](docs/DEPLOY.md) · [设计文档](docs/PLAN.md) · [下载](https://github.com/hewenyu/kilnflow.dev/releases/latest) · [参与贡献](CONTRIBUTING.md)

## 为什么是 Kiln

- **机器是自己的**：构建在你自己注册的 Runner 上执行，不消耗 Actions 分钟数。Runner 主动连 Server 领取任务（只发出站连接），不需要公网入口。
- **写法熟悉**：`.kiln/workflows/*.yml` 沿用 GitHub Actions 的结构——`on`、`jobs`、`needs`、`if`、矩阵、并发组、`services`、容器执行，学过的不用重学。
- **输入没变就不重跑**：job 的 action key 由全部输入算出，命中缓存的 job 直接成功，不占 Runner、不浪费时间。
- **发布集中在 Server**：推送 tag 后由 Server 发布到 GitHub Release、npm、容器镜像仓库或 HTTP 下载站；凭据不下发到构建机，也可以从私有仓库发布到同账号的公开仓库。
- **合并即发版**：内置合并队列，PR 验证通过后由 Kiln 合并；可选合并后自动打下一个版本 tag 并发布。
- **AI 审计与开发**：执行机器上的 Orca 经连接器 `kiln-orca` 领取任务，由 Codex 或 Claude Code 审计 PR（结论发布为 PR review），或从人标记的 Issue 开发并开 PR。
- **少运维**：Runner 用一次性令牌一条命令接入；自己从 npm 查到新版本后由管理员在 Dashboard 按台升级，启动失败自动回滚；装好的 runtime 按版本复用，30 天没用才清理。
- **所有状态都能追问原因**：状态都在 Server 的 Postgres 里，Dashboard（GitHub 风格）和 `kiln` CLI 能回答每个 job 为什么排队、为什么失败。

## 功能

| 方面 | 提供的能力 |
|---|---|
| 触发与规划 | push / tag / PR / 手动 dispatch；矩阵、`needs`、`if`、并发组、同 key 合并、job 级超时与资源声明 |
| 执行 | 主机上直接执行或 `container:` 隔离执行；`services`（测试用 Postgres 等）；BuildKit 构建 Dockerfile；macOS App 签名与公证 |
| 环境 | 受管理的工具链（Go、Node、pnpm、Bun，版本由 `.kiln/runtimes.lock` 固定）；`toolcache` 持久目录；secret / 变量 / environment 分级配置，全部加密存放 |
| 缓存与复用 | 按 action key 的分布式缓存、git 镜像、OCI 镜像缓存；Runner 本地清理策略与容量上限 |
| 发布 | GitHub Release、npm（含按平台的 Runner 包）、Docker Hub、HTTP 上传；asset 统一打包，跨仓库发布 |
| 合并与发版 | 合并队列（每个仓库一个执行者，按最新状态重判）、合并后按 `.kiln/merge.yml` 自动打 tag 并规划发布 |
| AI | PR 独立审计并发布为 PR review；从 Issue 选题、设计、实现、返修，自动开 PR；审计 Agent 与开发 Agent 必须是不同的 Agent |
| 运维 | Runner 一次性令牌注册、按台升级与回滚、排空、缓存清单、补对账；Server 内嵌 Dashboard；`kiln` CLI |

## 设计边界

这些是设计上的取舍，不是待补的缺口：

- **只服务自有的私有仓库**：GitHub App 只接受白名单账号的安装，**fork 的 PR 一律不构建、不审计**，没有开关。
- **只做 CI，不做 CD**：没有审批和人工门禁，发布只由 tag 触发。
- **不运行 marketplace action**：不支持 `uses:`；表达式只支持规划时就能求值的部分（`github`、`matrix`、`inputs`、`vars`、`secrets` 等）。
- **平台**：Runner 支持 macOS arm64、Linux arm64、Linux amd64；Windows 产物用交叉编译。
- **网络自己打通**：GitHub 要能访问 Server 的 webhook 地址；Runner 要能访问 Server、GitHub 和下载源。
- **过程数据只留 12 小时**：日志、产物、缓存到期清理，长期只保留结果（状态、时间、原因、发布记录），产物由发布目标保存。

## 快速开始

从零跑通第一个 CI 的最短路径，每一步的完整说明见 [DEPLOY.md](docs/DEPLOY.md)；尖括号里的 `<…>` 换成你自己的值。

**前提**

| 需要 | 说明 |
|---|---|
| Server | 一台装 Docker Compose 的 Linux 主机，有一个 GitHub 能访问的 HTTPS 地址（反向代理或隧道，转发到 `127.0.0.1:8080`） |
| Runner | 一台构建机（macOS arm64 / Linux arm64 / Linux amd64），可以与 Server 同机 |
| 仓库 | 一个属于你自己账号的 GitHub 私有仓库，下面以 Go 项目为例 |

**1. 创建 GitHub App**——按 [DEPLOY #3](docs/DEPLOY.md#3-创建-github-app) 创建只能安装到自己账号的 App，订阅事件、勾选权限，webhook 地址填 `https://<你的域名>/webhook`。完成后你手上有 App ID、webhook secret、私钥 `.pem`。

**2. 启动 Server**

```bash
git clone https://github.com/hewenyu/kilnflow.dev.git && cd kilnflow.dev/deploy
cp server.env.example server.env && cp <下载的私钥>.pem github-app.pem
openssl rand -hex 24   # KILN_ADMIN_TOKEN
openssl rand -hex 32   # KILN_MASTER_KEY（务必备份：丢了已保存的 secret 无法解密）
# 编辑 server.env：KILN_PUBLIC_URL、KILN_ADMIN_TOKEN、KILN_MASTER_KEY、
# KILN_GITHUB_APP_ID、KILN_GITHUB_WEBHOOK_SECRET、KILN_ALLOWED_ACCOUNTS（你的 GitHub 登录名）
docker compose up -d && docker compose logs server   # 镜像 hewenyulucky/kilnflow:latest + Postgres 17
```

日志出现 `kiln-server 已启动` 后，打开 `KILN_PUBLIC_URL` 用 admin token 登录 Dashboard，再把 App **安装**到你的账号、选择要构建的仓库。其他运行方式（launchd、systemd）见 [DEPLOY #4](docs/DEPLOY.md#4-启动-kiln-server)。

**3. 安装 CLI，注册 Runner**

```bash
for p in kiln kiln-runner; do
  curl -fL -o "$p" "https://github.com/hewenyu/kilnflow.dev/releases/latest/download/$p-<os>-<arch>"
done   # <os>-<arch>：darwin-arm64 / linux-arm64 / linux-amd64，校验和在 SHA256SUMS
chmod +x kiln kiln-runner && sudo mv kiln kiln-runner /usr/local/bin/

kiln login --server https://<你的域名>     # 粘贴 admin token
kiln runner token                          # 输出一次性注册令牌 kiln-enroll-…

# 在构建机上，以普通用户执行（用 Docker 执行 job 时这个用户要能 docker info）
kiln-runner enroll --server https://<你的域名> --token <注册令牌> --name <runner 名称>
kiln-runner doctor && kiln-runner service install
sudo loginctl enable-linger "$USER"        # 仅 Linux：无人登录时继续运行
```

`kiln runner list` 或 Dashboard 的 **Runner** 页能看到它在线。详见 [DEPLOY #5.1](docs/DEPLOY.md#51-注册-ci-runner)。

**4. 加入第一个 workflow**

```yaml
# .kiln/workflows/ci.yml
name: ci
on: { push: { branches: [main] }, pull_request: {} }   # main 改成你的默认分支
jobs:
  test:
    runs-on: linux/amd64        # 改成你的 Runner 平台：darwin/arm64、linux/arm64 或 linux/amd64
    runtimes: [go]              # 按 .kiln/runtimes.lock 准备 Go，版本来自 go.mod
    network: true               # job 默认断网，下载依赖要打开
    toolcache:
      - { name: gomod, env: GOMODCACHE }
      - { name: gocache, env: GOCACHE }
    steps:
      - run: go test ./...
```

```bash
kiln runtime lock    # 生成 .kiln/runtimes.lock（需联网）
kiln lint            # 本地检查 workflow
git add .kiln && git commit -m "加入 Kiln CI" && git push
```

**5. 查看结果**：GitHub 上这个 commit 出现 `ci (push)`、`ci / test (push)` 两个 Check Run；Dashboard 的 **运行** 页有 run、job 详情、实时日志、依赖图和产物；命令行用 `kiln runs --repo <owner>/<仓库>`、`kiln logs <job 编号> -f`。push 后没有 run、job 一直排队时，按 [DEPLOY #9 排查](docs/DEPLOY.md#9-排查)。

不写 `container:` 时 job 直接在 Runner 主机上执行；容器执行、更多字段与注意事项见 [DEPLOY #6](docs/DEPLOY.md#6-第一个-workflow) 与 [docs/WORKFLOW.md](docs/WORKFLOW.md)。

**接下来**

| 想要 | 看 |
|---|---|
| 配置 secret、变量、environment，tag 时发布到 GitHub Release、npm、镜像仓库 | [DEPLOY #7](docs/DEPLOY.md#7-secret变量environment发布) |
| PR 加 label `kiln:merge` 由 Kiln 合并，合并后自动打 tag 发版 | [DEPLOY #8.1](docs/DEPLOY.md#81-合并队列)、[#8.2](docs/DEPLOY.md#82-合并后自动打-tag) |
| 让 AI 审计每个 PR、从 Issue 开发 | [DEPLOY #5.6](docs/DEPLOY.md#56-ai-执行节点orca--kiln-orca)、[#8.3](docs/DEPLOY.md#83-ai-开发) |
| 代理、Runner 配置、升级、日常操作 | [DEPLOY #5](docs/DEPLOY.md#5-runner)、[#8](docs/DEPLOY.md#8-日常操作) |

## 安装

| 组件 | 获取方式 | 平台 |
|---|---|---|
| `kiln-server` | 容器镜像 `docker.io/hewenyulucky/kilnflow`；或 [Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 中的二进制 | linux-arm64、linux-amd64、darwin-arm64（镜像仅 linux） |
| `kiln-runner` | [Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 二进制；或 npm `@yuebanlaosiji/kilnflow-runner-<os>-<arch>` | darwin-arm64、linux-arm64、linux-amd64 |
| `kiln`（CLI） | [Release](https://github.com/hewenyu/kilnflow.dev/releases/latest) 二进制 | darwin-arm64、linux-arm64、linux-amd64 |
| `kiln-orca`（AI 执行节点连接器） | npm `@yuebanlaosiji/kilnflow-orca`，Node.js 24+ | 与平台无关 |

Server 需要 Postgres 17。Runner 的升级包从 npm 拉取，由管理员在 Dashboard 按台执行，失败自动回滚；Server 由你手动换镜像升级。

## 架构

```text
                 webhook                        长轮询（只出站）
  GitHub ───────────────────▶ kiln-server ◀──────────── kiln-runner × N   CI：主机 / Docker / BuildKit
  (App)  ◀── Check Run、Release、──┤  │   ◀──────────── kiln-orca × N     AI 审计 / 开发：Orca + Codex / Claude Code
             PR review             │  Postgres（唯一的状态）
                       Dashboard ──┘
                       kiln CLI
```

| 组件 | 作用 | 运行在 |
|---|---|---|
| GitHub App | 送来 push、PR、tag、Issue 事件；承载 Check Run、Release、PR review | 你的 GitHub 账号 |
| `kiln-server` | 唯一的控制面：接收事件、规划、调度，并执行所有对外写操作（发布、合并、打 tag、评论）；内嵌 Dashboard | 一台公网可达的小 VPS，配 Postgres |
| `kiln-runner` | CI Runner：领取 job，在主机上或 Docker 容器中执行，上传日志和产物；无状态，可以有多台 | 任何构建机 |
| `kiln-orca` | AI 执行节点的连接器：从 Server 领取审计或开发任务，交给本机常驻的 Orca 执行，再把结果交回 | 装了 Orca 与 Codex / Claude Code 的机器 |
| `kiln` | CLI：登录、注册令牌、查看 run、重跑、配置 secret 等，与 Dashboard 共用同一套管理接口 | 你的电脑 |

CI 与 AI 在领域上完全分开：AI 任务不放在 run / job / attempt 里，有自己的表、调度、生命周期和协议；两边只共用代码层面的机制（认证、升级、日志、存储）。设计细节见 [docs/PLAN.md](docs/PLAN.md)（路由到 Outline 活规格）。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/DEPLOY.md](docs/DEPLOY.md) | 部署与使用手册：GitHub App、Server、Runner、AI 执行节点、workflow、发布、日常操作、排查、版本升级 |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | `.kiln/workflows/*.yml` 语法、上下文与表达式、`runtimes.lock` 与校验 |
| [docs/PLAN.md](docs/PLAN.md) | 设计路由索引：三层权威边界、Outline 活规格目录、`PLAN #x.y → K-*` 映射 |
| [docs/UPLOAD-PROTOCOL.md](docs/UPLOAD-PROTOCOL.md) | HTTP 上传协议，供第三方下载站实现服务端 |
| [kiln-orca/README.md](kiln-orca/README.md) | AI 执行节点连接器的安装与命令 |
| [PRODUCT.md](PRODUCT.md)、[DESIGN.md](DESIGN.md) | Dashboard 的产品定位与设计系统 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献流程与代码要求 |
| [AGENTS.md](AGENTS.md) | 项目约定、已确认的决定与代码地图（人和 AI 工具共用） |

## 仓库结构

| 路径 | 内容 |
|---|---|
| `cmd/` | 三个入口：`kiln-server`、`kiln-runner`、`kiln` |
| `internal/server/` | Server：`ci/`（引擎、规划、发布、调度）、`ai/`（审计、开发）、合并（`mergequeue`、`autotag`）与共用设施 |
| `internal/runner/`、`internal/runnerkit/` | CI Runner 的执行与配置、注册、升级 |
| `kiln-orca/` | AI 执行节点连接器（TypeScript） |
| `internal/workflow/`、`internal/expr/` | workflow 解析、表达式求值、action key |
| `proto/` → `gen/` | 协议定义与生成代码（`runner.v1`、`airunner.v2`、Admin API） |
| `web/` | Dashboard 前端（React + Primer，构建后内嵌进 `kiln-server`） |
| `migrations/` | Postgres 迁移，启动时自动执行 |
| `.kiln/` | Kiln 自己的 workflows、合并配置与 AI 审计 / 开发说明 |

## 开发

需要 Go（版本见 [`go.mod`](go.mod)）、Node.js 24+ 和 pnpm。Server、CLI、CI Runner 是 Go，Dashboard 前端在 `web/`，AI 连接器在 `kiln-orca/`；目录分工见 [AGENTS.md #3](AGENTS.md#3-代码地图)。

```bash
cd web && pnpm install --frozen-lockfile && cd ..
make lint    # golangci-lint（多平台）、go mod tidy -diff、前端 eslint 与 tsc
make test    # go vet、go test -race、前端 vitest
make build   # 构建 cmd/ 下的程序到 dist/local/（先 make web 才内嵌 Dashboard）
make fmt     # gofumpt + goimports
```

数据库测试需要本机的 `initdb` / `pg_ctl`，或用 `KILN_TEST_DATABASE_URL` 指向一个可以随意建库的 Postgres；两者都没有时这些测试会被跳过，**这样的通过不算数**。`kiln-orca/` 单独 `pnpm install --frozen-lockfile && pnpm lint && pnpm test`。改了 proto 要 `make generate`，改了 `.kiln/` 要 `go run ./cmd/kiln lint`。

## 参与贡献

- 先开 issue 说明问题或需求，再做改动（[CONTRIBUTING.md](CONTRIBUTING.md) 第 1 节）。
- 本地必须跑通 `make lint`、`make test`；提 PR 前先用 AI 工具按 [审计说明](.kiln/agent/review.md) 自审一遍。
- 一个 PR 只做一件事，描述里关联 issue。
- **fork 的 PR 不会被构建或审计**（Kiln 只构建自有代码），所以请把本地结果贴进 PR。
- 本仓库的 PR 由 Kiln 自己跑 CI 和 AI 审计，通过后经合并队列合并并发版。

## 项目状态

Kiln 已经用于构建、测试、审计和发布它自己：CI、发布、合并队列与自动打 tag、AI 审计与开发、Runner 按台升级都已实现，版本由合并后自动打的 tag 发布到 [Release](https://github.com/hewenyu/kilnflow.dev/releases)。项目仍在演进，命令与配置可能变动。

尚未实现的主要有：cosign 签名、provenance 与 SBOM、`on: schedule` 定时触发、通知与指标、本地复现 `kiln run`、虚拟机执行（tart）、弹性伸缩，以及 AI 的周期审计。完整里程碑与实现差异见 [docs/PLAN.md](docs/PLAN.md)。

## 安全

发现安全问题请不要公开开 issue，用 GitHub 的 [Security Advisory](https://github.com/hewenyu/kilnflow.dev/security/advisories/new) 私下报告。部署与凭据模型见 [DEPLOY.md](docs/DEPLOY.md) 与 [docs/PLAN.md](docs/PLAN.md) 的架构活规格。

## 许可

[Apache-2.0](LICENSE)
