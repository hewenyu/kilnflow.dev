<!-- 提交前请读 CONTRIBUTING.md。没有本地验证结果、没有自审的 PR 会被直接关闭。 -->

## 改了什么、为什么

<!-- 一个 PR 只做一件事。关联 issue：Fixes #123 -->

## 本地验证

<!-- 贴出命令输出的最后几行和退出码。数据库测试被跳过（没有 initdb 也没有 KILN_TEST_DATABASE_URL）不算通过。 -->

- [ ] `go mod tidy -diff`
- [ ] `make lint`
- [ ] `make test`（数据库测试没有被跳过）
- [ ] `make build`
- [ ] `go run ./cmd/kiln lint`（改了 `.kiln/workflows` 时）
- [ ] Dashboard 改动附上前后截图（改了 `web/` 时）

```text
（输出）
```

## AI 自审

<!-- 按 .kiln/agent/review.md 让 AI 审过一遍改动。写明用的工具、修掉了哪些问题；没修的问题写明理由。 -->

- [ ] 已按 `.kiln/agent/review.md` 自审并处理了发现的问题

## 文档

- [ ] 改变了部署、配置、workflow 语法、上传协议或前端契约时，已同步对应仓库手册（`docs/DEPLOY.md`、`docs/WORKFLOW.md`、`docs/UPLOAD-PROTOCOL.md`、`DESIGN.md`）
- [ ] 新行为有单元测试或功能测试（不写端到端测试）
- [ ] 测试有效：临时还原修复 / 改坏关键逻辑后，测试会失败（写明试了哪处）
