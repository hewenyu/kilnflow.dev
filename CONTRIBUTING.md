# 参与贡献

提 PR 前请读完本页：**没有本地跑通、也没自己审过一遍的 PR 会被直接关闭。**

## 1. 先有 issue

- bug：写清复现步骤、预期与实际行为、版本（`kiln version`）、相关日志。
- 新功能或设计改动：先开 issue 讨论，确认方向再写代码。与 [AGENTS.md](AGENTS.md)（Claude Code 经 `CLAUDE.md` 的 `@AGENTS.md` 导入）"已确认的决定"或 [docs/PLAN.md](docs/PLAN.md) 冲突的 PR 不会合并（如支持 `uses:`、审批门禁、fork PR 构建开关、把 workflow 放进 `.github/`）。
- 一个 PR 只做一件事，描述中关联 issue（`Fixes #123`）。

## 2. 本地必须跑通

环境：Go 1.27、Node 24+、pnpm；数据库测试需要本机 `initdb` / `pg_ctl`，或 `KILN_TEST_DATABASE_URL` 指向可随意建库的 Postgres。**都没有时数据库测试会被跳过，这样的"通过"不算数。**

```bash
make lint                # golangci-lint（go tool，版本由 tools/golangci/go.mod 锁定，按 Makefile 的 TARGETS 每个平台各查一次）、go mod tidy -diff、前端 eslint/tsc
make test                # go vet、go test -race、前端 vitest
make build
go run ./cmd/kiln lint   # 改了 .kiln/workflows 或 .kiln/merge.yml 时
```

把结果（至少最后几行和退出码）贴进 PR 描述；改了 Dashboard（`web/`）的附前后截图。

## 3. 提交前先让 AI 审一遍

PR 会由 Kiln 的 AI 审计（AI 执行节点上的 Agent）按 [`.kiln/agent/review.md`](.kiln/agent/review.md) 深度审计；提 PR 前先用自己的 AI 工具（Codex、Claude Code 等）按同一份说明审一遍，例如：

```text
按 .kiln/agent/review.md 审计 `git diff master...HEAD` 的改动：先跑通第 1 节的命令，再按第 2 节的维度（正确性、安全、内聚与耦合、常量与变量、最佳实践、测试、设计文档）逐项检查，按第 3 节对关键逻辑做变异验证，确认测试不是无效的；列出问题和证据，能修的直接修掉，修不了或拿不准的写明原因。
```

修掉发现的问题；剩下的、你认为不成立的，把问题和理由写进 PR 描述。

## 4. 代码要求

- 遵循 [`.claude/skills/`](.claude/skills/) 中的最佳实践（各 AI 工具按路径读取，人也照此执行）：Go 用 `golang-*`，TS 用 `typescript-best-practices`。
- Server 子包按领域划分（共用、CI、AI、合并），领域之间不直接依赖，跨领域只经接口在组装层接线（边界见 [AGENTS.md](AGENTS.md) 与 [docs/PLAN.md](docs/PLAN.md) 路由到的 Outline `02 [K-ARCH-01]`），由 `internal/archtest` 检查；依赖在构造时注入，不新增包级可变状态。子包说明见 [AGENTS.md #3](AGENTS.md#3-代码地图)。
- 不写魔法数字和字符串，用命名常量；同一个值只定义一次。
- 只写单元测试和功能测试，**不写端到端测试**。测试要有效：断言结果本身（不只是 `err == nil`），覆盖失败路径；提交前临时还原你的修复，确认测试会失败（AI 审计也做变异验证，会报告改坏代码仍通过的测试）。替身：Postgres `internal/pgtest`、GitHub `internal/fakegithub`、镜像仓库 `internal/oci/ocitest`、HTTP 上传 `internal/fakeupload`、npm registry `internal/fakenpm`，前端 vitest。
- 行为或设计变了就同步更新对应的版本伴生手册（workflow 语法见 [docs/WORKFLOW.md](docs/WORKFLOW.md)，部署或使用方式见 [docs/DEPLOY.md](docs/DEPLOY.md)，上传协议见 [docs/UPLOAD-PROTOCOL.md](docs/UPLOAD-PROTOCOL.md)，前端契约见 [DESIGN.md](DESIGN.md)）。正式设计文档由维护者在 Outline 维护，代码 PR 不修改 [docs/PLAN.md](docs/PLAN.md) 或 Outline 活规格。
- 文档和注释用中文。

## 5. PR 之后

- 本仓库分支的 PR 会跑 Kiln CI（`ci`、`release`、`docker` 的构建）和 AI 审计（`ai-review`），审计结论发布为 PR review，行内评论指向具体行。
- 通过后由合并队列合并（[`.kiln/merge.yml`](.kiln/merge.yml)：验证和 AI 审计通过后自动入队，不想自动合并就加 label `kiln:hold`）；合并后 Kiln 自动打版本 tag 并发布，PR 加 `release:minor` / `release:major` 升次版本 / 主版本，`release:skip` 不发版。
- **fork 的 PR 不构建、不审计**（Kiln 只构建自有代码，PLAN #16.1），所以第 2、3 节的本地结果尤其重要：维护者先看你贴的结果，值得才迁到本仓库分支跑 CI 和 AI 审计。
- 对 finding 有异议就在对应评论下回复理由；不要只回"已修复"，要说明改了什么。
- 推送新 commit 会取消旧审计并重新审计。
