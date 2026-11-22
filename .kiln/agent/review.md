这是 Kiln 仓库自己的审计说明，补充 AI Runner 的通用要求。目标是**深度审计**：不只看能不能跑，还要看改动是否站得住、是否符合这个项目的约定。宁可少报，也不要报没有证据的问题。

## 1. 先确认能在本地跑起来（必做）

PR 作者应当在本地跑通这些命令，审计要实际复核一遍。测试用的 Postgres 已经通过 `KILN_TEST_DATABASE_URL` 注入，数据库测试不应被跳过。

```bash
go mod tidy -diff
make lint        # golangci-lint（go tool，版本由 tools/golangci/go.mod 锁定，按 Makefile 的 TARGETS 每个平台各查一次）+ go mod tidy -diff + 前端 eslint/tsc
make test        # go vet + go test -race + 前端 vitest
make build
go run ./cmd/kiln lint   # 校验 .kiln/workflows
```

- 前端依赖用 `cd web && pnpm install --frozen-lockfile` 安装。
- 本仓库声明了受管理的工具链（`.kiln/agent/config.yml` 的 `requirements.tools: [go, node, pnpm]`）：本说明中的每条 `go`、`make`、`pnpm` 命令（包括依赖安装和下面的变异验证）都在前面加上任务说明给出的 `kiln-orca task … exec --` 执行，用它准备好的 Go、Node、pnpm（版本与 `.kiln/runtimes.lock` 一致）；不要用本机 PATH 中的版本，也不要自己安装、下载别的版本或写包装脚本。golangci-lint 是 `go tool`（版本由 `tools/golangci/go.mod` 锁定），经 exec 运行 `make lint` 即可，不要另外安装。工具链本身的问题写进 unverified，不作为 finding。
- 任何一条失败，并且失败由本次改动引起：作为 finding 报告（severity high，evidence reproduced），在 proof 中写清命令、退出码和关键输出。
- 失败与本次改动无关（master 上同样失败、网络或环境问题）：不作为 finding。最终解决了的写进 env_notes，没能解决、导致相关验证没有完成的写进 unverified，并说明原因。
- 每条命令的结果写进 checks（kind=suite，每项一行，例如"make test：Go race 测试和 195 个前端测试通过"）。
- 所有运行过的命令都记入 commands_run，并用 outcome 区分最终失败（failed）、已解决的环境失败（env_resolved）、预期失败的变异验证（expected_failure）、没有匹配的搜索（no_match）和探索操作（explore）。

## 2. 审计维度

按下面的顺序看，前面的比后面的重要。每个维度先读相关代码，再下结论。

1. **正确性**：并发与事务（同一事务中领取、epoch+1、签发租约；状态变更必须带 epoch 条件）、状态机的每个转移、错误是否被吞掉、边界条件（空、零、超长、重复、乱序）、资源泄漏（goroutine、连接、文件、临时目录、容器）、context 取消与超时。
2. **安全**：认证与授权边界（Runner、CLI、Dashboard、webhook 验签）、secret 是否可能进入日志或报告、外部输入（webhook、workflow、Agent 报告、上传的文件）是否都被校验、路径穿越、fork PR 是否仍被丢弃。
3. **内聚与耦合**：
   - 包按领域归类，由 `internal/archtest` 强制检查：**库**（`lib`，不含领域判断的客户端、协议、工具）、**共用**（`shared`，Runner 身份、outbox、日志、仓库等共用机制）、**CI**（`ci`）、**AI**（`aiDom`）、**合并**（`merge`）、**组装与展示**（`assembly`：`internal/server`、`ingest`、`admin`、`web`、`cmd/kiln`、`cmd/kiln-server`）。
   - 依赖只允许向下：`lib` 只依赖 `lib`；`shared` 只依赖 `shared`/`lib`；`ci`、`aiDom`、`merge` 三大业务领域各自只依赖本领域加 `shared`/`lib`，**互不直接导入**——跨领域只通过接口在组装层接线，也不跨领域共享表，或直查、外键引用对方专有的表（表归属同样由 `internal/archtest` 检查）。出现反向依赖，或为绕开边界引入全局变量、回调注册，都是问题。
   - 新增包或新表必须先在 `internal/archtest` 归类，没有归类的包会让依赖检查失败。
   - 组件在构造时显式注入依赖，不新增包级可变状态和 `init()` 中的副作用。
   - 一个函数或类型只做一件事；新增的逻辑放在职责对应的包里，而不是哪里方便放哪里；同样的逻辑不要在两处各写一份。
   - 新的对外接口（proto、Admin API、CLI 参数、配置字段）是否必要，命名和语义是否与已有的一致。
4. **常量与变量**：
   - 魔法数字和字符串（超时、上限、重试次数、状态名、label、路径）要有命名常量，并放在使用它的包里；同一个值不要在多处重复定义。
   - 协议和数据库中的值（错误原因、状态、枚举）只能有一个定义来源（例如 `internal/rpcerr`、`gen/`）。
   - 变量的作用域尽量小；不用包级 `var` 保存运行时状态；零值是否可用、是否需要显式初始化。
5. **最佳实践**：Go 代码对照 `.claude/skills/golang-*`（错误处理、并发、context、数据库、命名、安全、测试），TS 代码对照 `.claude/skills/typescript-best-practices`。只读与改动相关的 skill，指出具体违反的条目。
6. **测试**：新行为必须有单元测试或功能测试；需要 Postgres 的用 `internal/pgtest`，GitHub 用 `internal/fakegithub`，镜像仓库用 `internal/oci/ocitest`，HTTP 上传用 `internal/fakeupload`，前端用 vitest。**不接受端到端测试**。测试是否有效按第 3 节逐项检查，这是深度审计的重点。
7. **设计与文档**：设计依据分两层。
   - **Git 仓库（`R_repo`）**：`AGENTS.md` 中的已确认决定（`C1–C23`）与代码约定，以及版本伴生的契约手册——部署与使用 `docs/DEPLOY.md`、HTTP 上传 `docs/UPLOAD-PROTOCOL.md`、workflow 用户语法手册 `docs/WORKFLOW.md`、前端 `DESIGN.md`。
   - **外部活规格**：任务提供的冻结 Outline 正式活规格快照，用 `kiln-orca task doc list` 列出、`kiln-orca task doc get <doc_id>` 按需读取正文；旧 `PLAN #x.y` 的映射见 `docs/PLAN.md`。

   外部正式设计文档由维护者在 Outline 维护，**代码 PR 不要求同步修改 `docs/PLAN.md`（路由索引）或 Outline 活规格**；但改动部署、配置或使用方式须同步 `docs/DEPLOY.md`，改动 workflow 语法或表达式须同步 `docs/WORKFLOW.md`，改动 HTTP 上传协议或前端契约须同步对应手册。不能违反 `AGENTS.md` 中"已确认的决定"（例如：不支持 `uses:`、不做审批门禁、不提供 fork PR 开关、workflow 不放 `.github/`、不写端到端测试）。AI 开发的 PR 在描述的"决策记录"中声明要改某个已确认的决定（`confirmed_decision`，PR 带 `kiln:hold` 等用户确认）时，不因为"与已确认的决定不同"机械否决，按第 8 节判断理由；没有声明却违反的照常报告。

## 3. 测试有效性（深度检查）

"有测试"不等于"测住了"。对本次新增或修改的每个测试（手动全面审计时是核心包的测试），都要确认它在代码出错时真的会失败。

**变异验证（必做）**：对本次改动中最关键的逻辑（修复的 bug、新的分支、状态转移、校验条件），在临时副本中把它还原或改坏，确认对应的测试会失败：

```bash
mkdir -p "$TMP/mut" && git archive HEAD | tar -x -C "$TMP/mut"   # $TMP 用 prompt 给出的临时目录，不改动仓库
cd "$TMP/mut" && <还原修复 / 反转条件 / 删掉一行校验> && <task exec 命令> go test -race -run '<相关测试>' ./<包>/
```

- 改坏后测试仍然通过：这个测试是无效的，作为 finding 报告（severity medium；如果它是这个修复唯一的测试，则为 high），写清改了哪一行、跑了什么、结果如何，evidence 为 reproduced。
- 每个 PR 至少变异 1–3 处；每处变异写进 checks（kind=mutation，改坏后测试失败为 passed），命令写进 commands_run（outcome 为 expected_failure，note 写明"变异：……，预期失败，实际……"）。
- PR 声称修复的每个问题，验证通过后写进 checks（kind=change），不要写成 finding。

**被跳过的测试**：用 `<task exec 命令> go test -v -run '<相关测试>' ./<包>/ 2>&1 | grep -E -- '--- SKIP'` 确认相关测试没有被跳过。数据库测试在本次审计中应当真的连上了 `KILN_TEST_DATABASE_URL`；新加的 `t.Skip` 必须有正当理由。

**常见的无效测试**，发现即报告：

- 没有断言，或者只断言 `err == nil`，没有检查结果本身；只检查长度、不检查内容。
- 断言的是 mock / 替身自己返回的值（测了替身而不是被测代码），或者期望值是用被测代码本身算出来的（同义反复）。
- 表驱动测试中的用例从未执行（循环变量、`t.Run` 名称重复、`continue` 跳过）；子测试里用 `t.Fatal` 却在 goroutine 中调用。
- 错误被忽略（`_ = f()`）导致失败路径永远不会被测到；只测成功路径，修复针对的失败路径没有用例。
- 依赖 `time.Sleep`、固定端口、执行顺序或全局状态，会时好时坏；并发代码没有用 `-race` 跑过。
- 前端测试只断言"渲染出来了"，没有断言交互后的状态和文案；快照测试替代了行为断言。
- 测试名称和注释说测了 A，实际测的是 B。

## 4. 不报什么

- 格式、import 顺序、行宽这类 `make lint` 已经管住的问题。
- 只是个人偏好、没有实际影响的写法差异。
- 已经在对应 Outline 活规格（冻结快照）或 `AGENTS.md` 中明确记录为现状边界的行为。

内聚耦合、常量、命名不清导致误用的风险，属于设计问题，不算"代码风格"，应当报告（通常是 medium 或 low）。

## 5. 严重程度

- **high**：会导致错误结果、数据丢失、状态卡死、安全问题，或者 `make lint` / `make test` / `make build` 因本次改动失败。
- **medium**：特定条件下才出错；缺少关键测试或测试无效（变异后仍然通过）；违反依赖方向或项目已确认的决定；必需同步的仓库手册（如 `docs/DEPLOY.md`、workflow 用户语法手册）没有同步。
- **low**：可维护性问题：魔法数字、重复逻辑、职责不清、命名误导。

## 6. 本次改动之外的已有问题

审计中顺带发现、但与本次改动无关的已有问题，不作为 finding（不要因此让这个 PR 失败）。写进 preexisting：path、line、title 写问题，detail 写证据，便于之后单独建 issue 处理。

## 7. 手动全面审计

没有 PR、由 `kiln ai review` 手动触发时，审计整个仓库而不是某个 diff：按第 2 节的维度逐个过一遍，并按第 3 节抽查核心包（runnerapi、engine、store、publish、runner）的测试是否有效，优先看 `internal/server/`（runnerapi、engine、store、outbox、publish）、`internal/runner/` 和认证相关代码。此时第 6 节不适用，所有问题都作为 finding；同一类问题合并成一条，列出所有位置。

## 8. 设计 PR

AI 开发的设计 PR（prompt 说明按设计审计时）只改 Markdown，不做变异验证，不因为"还没实现"而否决；第 1 节的命令照常运行。对照依据是 `AGENTS.md`（已确认决定 `C1–C23` 与代码约定）与任务冻结的 Outline 活规格快照（旧 `PLAN #x.y` 的章节路由见 `docs/PLAN.md`），设计正文不再要求写在 `docs/PLAN.md`，重点看：

- **问题与证据**：要解决的问题有没有代码、故障或使用场景作为依据，引用（文件、行号、issue、日志）是否属实；没有依据、只为尚未出现的场景预留的部分是过度设计。
- **与已有设计的关系**：是否与冻结的 Outline 活规格、`AGENTS.md` 的已确认决定，以及仓库既有机制（outbox、执行者、`kiln:hold`、按事件工作）矛盾而没有说明；能用已有机制解决的，是否另起了一套。
- **已确认的决定**：没有在决策记录中声明、却违反 `AGENTS.md` 中已确认的决定的，作为 finding（high）。已声明的（`confirmed_decision`）按证据、影响和替代方案判断理由是否站得住，写进 checks；是否合入由用户确认（`kiln:hold`）。
- **能否实现**：状态、事务、并发、迁移和兼容、失败与恢复是否说清；不变量是否写明怎么保证。
- **验收能否独立证明**：验证方法是否只用单元测试和模块功能测试（不写端到端测试），是否写明改坏哪里测试会失败；需要真实环境的是否列为外部输入。
- **文档约定**：正文只写当前方案，不引入双写或死链；外部正式设计文档由维护者在 Outline 维护，PR 不需要同步 `docs/PLAN.md`、`PLAN #25` 或 `changelog`；本次改动触及部署、workflow、上传协议或前端契约时，同步对应的仓库伴生手册。

一个 PR 完成的 AI 开发 PR，同一个 PR 中的设计也按上面的要点审；先设计、设计已合并的实现 PR，另外看是否符合已合并的设计，偏离是否在描述的"与已合并设计的出入"中说明且站得住。
