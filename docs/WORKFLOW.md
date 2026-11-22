# Kiln workflow 手册

`.kiln/workflows/*.yml` 的写法沿用 GitHub Actions，但有若干有意的差异（不运行 `uses:`、表达式只支持规划时可求值的部分、PR 构建 head commit、工作区不含 `.git`）。本文是写给 workflow 作者的手册：语法、上下文与表达式、`.kiln/runtimes.lock` 和校验。设计背景见 [PLAN.md](PLAN.md) 路由到的 Outline `03 [K-CI-WF]`；部署与日常使用见 [DEPLOY.md](DEPLOY.md)；`release.uploads` 的服务端协议见 [UPLOAD-PROTOCOL.md](UPLOAD-PROTOCOL.md)。

## 1. 设计原则

- **位置**：`.kiln/workflows/*.yml`（或 `.yaml`），一个文件一个 workflow；不放 `.github/workflows/`（会被 Actions 执行）。文件名 `ai-review.yml` / `.yaml` 保留给内建的 AI 审计，用了报错。
- **语法沿用 Actions**（差异见 #5），主要不同：
  1. 不运行 `uses:`，改用声明式字段，缓存全自动。
  2. 表达式只支持规划时可求值的部分（#7），执行前即可确定 job 定义和 action key。
  3. PR 构建 head commit，不是合并提交（`AGENTS.md` C3）。
  4. 工作区没有 `.git`、不注入 `GITHUB_*`，用 `${{ github.* }}` 显式写（#7）。

## 2. 目录结构

```
.kiln/
  workflows/
    ci.yml         # push / PR：lint、测试、构建
    release.yml    # tag：构建并发布
  runtimes.lock    # `kiln runtime lock` 生成（#8）
  runtimes.toml    # 可选：monorepo 中指定版本声明的位置
  merge.yml        # 可选：合并队列、合并后自动打 tag
  agent/config.yml # 可选：AI 审计
```

## 3. 示例：`ci.yml`（Go + TS）

只有 deps job 联网，构建 job 用 `fetch` 取依赖目录离线构建。Kiln 自己的 workflow（含容器镜像 `docker.yml`）见仓库的 `.kiln/workflows/`。

```yaml
name: ci
on: { push: { branches: [main] }, pull_request: { branches: [main] } }

jobs:
  go-deps:
    runs-on: linux/arm64
    container: docker.io/library/debian@sha256:…
    runtimes: [go]
    inputs: [go.mod, go.sum]
    network: true
    env: { GOMODCACHE: "${{ kiln.workspace }}/.gomodcache" }
    steps:
      - run: go mod download
    outputs: { modcache: .gomodcache/ }

  build:
    needs: [go-deps]
    strategy:
      matrix:
        include:
          - { goos: darwin, goarch: arm64 }
          - { goos: linux, goarch: amd64 }
    runs-on: linux/arm64  # 交叉编译，一台出全部产物
    container: docker.io/library/debian@sha256:…
    runtimes: [go]
    inputs: [go.mod, go.sum, cmd/, internal/]
    fetch: [{ from: go-deps.modcache, to: .gomodcache/ }]
    env:
      GOMODCACHE: ${{ kiln.workspace }}/.gomodcache
      GOPROXY: "off"
      GOOS: ${{ matrix.goos }}
      GOARCH: ${{ matrix.goarch }}
    steps:
      - run: go build -trimpath -buildvcs=false -ldflags="-buildid=" -o dist/myapp ./cmd/myapp
    outputs: { bin: dist/myapp }

  # TS 同理：node-deps 联网 pnpm fetch --store-dir .pnpm-store 并输出它，
  # 构建 job fetch 后 pnpm install --offline --frozen-lockfile
```

## 4. 示例：`release.yml`

发布到 GitHub Release 和 npm，只由 tag push 触发。要命中 ci 的缓存，`go-deps` 和 workflow 级 `env` 须与 ci.yml 逐字相同（不能跨 workflow 引用 job；action key 含合并后的 `env`）。

```yaml
name: release
on: { push: { tags: ["v*"] } }
jobs:
  # go-deps 同 ci.yml；build 只多 -X main.version=${{ github.ref_name }}（因此与分支构建分开缓存）
  publish:  # 发布 job：无 steps，由 Server 执行
    needs: [build]
    environment: release  # 须已在 Kiln 中创建
    release:
      assets:  # 只打包一次，各文件类目标共用；上游每个矩阵变体一个 asset
        - from: build.bin
          archive: none  # tar.gz（默认）| zip | none（输出须恰好一个文件）
          name: myapp-${{ matrix.goos }}-${{ matrix.goarch }}  # matrix 指上游变体
      github: {}  # 本仓库的 Release；校验和文件默认 SHA256SUMS
      npm:  # registry、scope、token 按账号配置（kiln npm set）
        - from: build.bin
          name: myapp-${{ matrix.goos }}-${{ matrix.goarch }}  # 发布为 @<scope>/myapp-darwin-arm64 等
          access: public
```

## 5. 与 GitHub Actions 的对照

| Actions | Kiln |
|---|---|
| `on: push` | `branches`、`branches-ignore`、`tags`、`tags-ignore`、`paths`、`paths-ignore`；`paths` 只对分支 push 生效 |
| `on: pull_request` | `branches`（匹配 base）、`branches-ignore`、`paths`、`paths-ignore`、`types`（只支持 `opened`、`synchronize`、`reopened`、`ready_for_review`，默认前三个）；忽略 fork PR |
| `on: workflow_dispatch` | `inputs` 支持 `string`、`boolean`、`choice`（须有 `options`）；Dashboard 或 `kiln dispatch REPO WORKFLOW [--ref] [-i k=v]` 触发 |
| `on: schedule` | 只解析，**尚未实现**（设计：按 UTC 在默认分支最新 commit 上运行，走缓存） |
| `runs-on: [self-hosted, macOS, ARM64]` | `runs-on: darwin/arm64`：`darwin` / `linux` 加 `arm64` / `amd64`（可再加 `/gnu`、`/musl`），可用表达式；另可加 `pool:` |
| `container:` | `<镜像>` 或 `{image: …}`，须 digest 固定，用 oci executor（没写用 host）；只支持 `image`（`env` 写在 job 级） |
| — | `vm:`：Tart 镜像（OCI 引用，带 digest），tart executor；**尚未实现** |
| `strategy.matrix`（`include`、`exclude`、`fail-fast`、`max-parallel`）、`env`、`defaults.run`、`needs`、`timeout-minutes`、`environment`（名称或 `{name, url}`） | 相同。不支持 job 级 `continue-on-error` |
| `steps[]` 的 `id`、`name`、`if`、`run`、`shell`、`working-directory`、`env`、`continue-on-error` | 相同（`run` 必填）；`if` 为 false 的 step 规划时去掉；不支持 `timeout-minutes`。`shell` 默认 `bash`（`--noprofile --norc -eo pipefail`），另有 `sh`、`python` 或含 `{0}` 的自定义命令 |
| `concurrency` | workflow 级相同，`cancel-in-progress` 默认 `false`；job 级**未实现** |
| `services:` | 只支持 `image`、`env`，另加 `command`（覆盖默认命令）；按服务名访问，无需 `ports` |
| `if:` | 只能引用 `github`、`matrix`、`inputs`、`vars`，规划时求值，false 则 job 为 skipped |
| `uses:` | `checkout` → `inputs`；`setup-*` → `runtimes`；`cache` → 不需要（依赖用 deps job）；`upload-artifact` / `download-artifact` → `outputs` / `fetch`；导入签名证书 → `signing`；其他不支持 |
| `environment:` | 在 Kiln 中配置，不存在则规划报错。名称可用表达式（`github`、`inputs`、`matrix`、账号级和仓库级 `vars`），求出后再按它合并 job 的 `vars`；矩阵先于此展开，名称含表达式时矩阵里的 `vars` 只有账号级、仓库级 |
| `vars.X` | Kiln 中配置，计入 action key；未定义为空串 |
| `secrets.X` | Kiln 中配置，不计入 action key（以名称占位）。**只能用在** `env`（workflow / job / step）、`signing`、`release.images[].username/password`、`release.uploads[].token`；其中 password、token 和 `signing` 的 `p12`、密码、`key` **必须**引用 secret。`release.npm` 不写 token |
| `permissions:`、`GITHUB_TOKEN`、`run-name` | 不提供（job 不访问 GitHub，发布由 Server 做） |

## 6. Kiln 特有的字段

路径相对仓库根（`inputs`）或工作区根（`outputs`、`fetch.to`）；glob 为 doublestar 语法（`**` 跨目录），区分大小写；`/` 结尾表示整个目录（递归）。

| 字段 | 默认 | 规则 |
|---|---|---|
| `runtimes` | 无 | runtime 名列表，按 `runtimes.lock` 解析为该平台的 digest（#8） |
| `inputs` | 整个仓库，不含 `.kiln/workflows/` | 计入输入摘要并检出；`!` 开头为排除，最后匹配者生效；**未列出的文件执行时不可见**；符号链接按目标路径文本计入、不跟随；子模块按 gitlink 的 sha 计入、不检出 |
| `outputs` | 无 | 命名输出（路径或 glob），执行后上传 CAS 并生成 manifest；只保留内容和可执行位；匹配为空或有指向输出目录外的符号链接时失败（`user` 类） |
| `fetch` | 无 | `from: <job>.<output>`、`to: <路径>`；上游须在 `needs` 中，有矩阵时**必须**用 `matrix: {变量: 值}`（可用表达式）选出恰好一个变体 |
| `network` | `false` | oci 用 `--network none`（有 services 时为不能访问外网的内部网络）。host **不强制**：合并 `env` 后把 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`（含小写）指向不可达地址、`NO_PROXY` 只留本机，挡不住直连 socket；要强制断网用 oci。`true` 只表示允许联网 |
| `reproducible` | `false` | 只是标记，供**尚未实现**的抽样校验和 `release.verify-reproducible` 用 |
| `cache` | `true` | `false` 时每次执行、不写缓存；`effects` job、发布 job 强制 `false` |
| `resources` | `{cpu: 1, memory: 1Gi}` | 按 Runner 有效容量装箱，超过所有兼容 Runner 则一直等；oci 用 `--cpus`、`--memory` 强制，host 不限制 |
| `toolcache` | 无 | `[{name, env}]`：每个 `(Runner, 执行身份, 仓库, 平台, name)` 一个持久目录，`env` 指向它。`name` 是安全的单段标识符（字母或数字开头，只含字母、数字、点、下划线和连字符，最长 64 字符，不含 `..`、路径分隔符和冒号）；`env` 是合法环境变量名（`[A-Za-z_][A-Za-z0-9_]*`，不以 `__` 开头），不能覆写 `PATH`、`HOME`、`TMPDIR`、`SHELL`、`IFS`、`ENV`、`BASH_ENV`、`LD_PRELOAD`、`LD_LIBRARY_PATH` 及 `LD_*`、`DYLD_*`、`GIT_*`、`KILN_*`、`__*` 前缀。**不计入 action key**，只用于会自校验缓存、支持并发的工具（如 `GOCACHE`） |
| `services` | 无 | `{<服务名>: {image, env, command}}`，只用于带 `container:` 的 job；服务名与 `toolcache.name` 同一规则（字母或数字开头，只含字母、数字、点、下划线和连字符，最长 64 字符，不含 `..`）；`image`（含表达式展开后的结果）必须固定为 `@sha256:<64 位小写十六进制>`；按服务名访问；有 HEALTHCHECK 时等 healthy 再跑第一个 step（最多 2 分钟，超时或容器退出按 `infra.prepare` 失败，附服务最后几行日志）；`env` 不能引用 secret；镜像 digest、env、command 计入 action key |
| `repositories` | 无 | 额外只读访问的仓库（`owner/name`，须同一 installation），拉私有依赖用；计入 action key |
| `pool` | 无（任意） | 只在该 pool 的 Runner 上执行 |
| `host` | `default` | host profile 名，计入 action key；v0 不按指纹匹配 Runner |
| `effects` | `false` | 声明有外部副作用：强制 `cache: false`，不命中缓存、也不与 action key 相同的 job 合并（各自执行）；step 开始后遇到基础设施错误或租约过期不自动重试，job 直接失败，确认外部状态后手动重跑。Kiln 只保证不自动重试，外部副作用是否幂等仍须由 workflow 自己保证 |
| `release` | 无 | 发布 job，无 steps，Server 执行（配置见 #6.1） |
| `image` | 无 | 镜像组装 job，无 steps；**尚未实现** |
| `buildkit` | `false` | 用 BuildKit 构建 Dockerfile，只用于带 `container:` 的 job，Runner 须开启 |
| `signing` | 无 | macOS 签名与公证：`certificates`、`provisioning-profiles`、`notary` 至少写一项；只用于 `darwin/*` 的 host job，不能与 `container`、`vm` 同时使用（#6.2） |
| `timeout-minutes` | 60 | 正整数，`1..1440`；不写时取 `60`，`0`、负数或超过 1440 报错。超时按 `timeout` 失败，不重试 |

`strategy.max-parallel` 是正整数，`1..256`（与单个 run 的 job 上限 `MaxJobs` 相同），超出范围报错。

`release.npm[].name` 是不含 scope 的短名，规则与 `internal/npm` 一致：最长 214 字节，只含小写字母、数字、`-`、`.`、`_`，不以 `.` 或 `_` 开头，不能是 npm 保留名（`node_modules`、`favicon.ico`）；发布为 `@<账号级 scope>/<name>`。

job 只能是 `steps`（在 Runner 上执行）、`release`、`image` 之一，后两种不能写 `runs-on`、`container`、`runtimes`。

### 6.1 发布配置 `release`

发布 job 由 Server 执行、不缓存，只由 tag push 触发，至少要有一个目标（`github`、`uploads`、`npm`、`images`）。`from`、`assets`、`name` 等引用上游 job 的输出，上游须在 `needs` 中。

**文件类目标共用的 asset**：

| 字段 | 默认 | 规则 |
|---|---|---|
| `release.assets[]` | 无 | `github`、`uploads` 共用的 asset，Server 只打包一次、各目标发布相同字节（按 tag commit 时间确定性打包）。`from`（必填，`<job>.<output>`；上游带矩阵时每个变体一个 asset）、`archive`（`tar.gz`、`zip`、`none`；`none` 时输出须恰好一个文件）、`name`（不含扩展名，可引用 `matrix`（指上游变体）和 `github`；不写时为 `<输出名>[-<矩阵取值，按变量名排序>]`；须是合法文件名，不能重复） |
| `release.checksums` | `SHA256SUMS` | 校验和文件名；写 `none` 关闭；不能与 asset 重名 |

`release.assets` 必须至少有 `github` 或 `uploads` 使用者；只写 `uploads` 而没有 `assets` 报错。`release.github.assets` / `release.github.checksums` 是这两项的等价旧写法，与 `release.assets` / `release.checksums` 只能写一处（推荐后者）。

**`release.github`**（文件类目标）：

| 字段 | 默认 | 规则 |
|---|---|---|
| `repository` | 本仓库 | 发布到其他仓库（`owner/name`）：必须声明 `environment`，目标须在该 environment 的允许发布仓库列表中，App 已安装且与源仓库同一 installation；跨仓库必须显式写 `notes.from` |
| `target` | 目标仓库的默认分支 | 跨仓库时 Release 的 target commitish；本仓库发布用本次 run 的 commit |
| `prerelease` / `latest` | `auto` | `auto`、`true`、`false`。`auto` 的 prerelease：tag 是带预发布段的 semver；`auto` 的 latest：不是预发布，且是目标仓库所有正式 Release 中 semver 最大的版本 |
| `allow-source-name` | `false` | 源仓库是私有、目标是公开仓库时默认做源仓库名检查（asset 名和说明中不能出现源仓库全名）；`true` 跳过 |
| `notes.from` | `github` | 不写或 `github`：GitHub 自动生成说明（跨仓库时必须显式写出）；`draft`：保持 draft，人工编辑后手动发布；其他值按仓库文件路径读取 tag 对应 commit 的文件，取以 tag 开头的二级标题（如 `## v1.2.3`）那一节，没有则失败；`commits` **尚未实现** |
| `sign` / `provenance` / `sbom` | 无 | `sign: cosign`、`provenance`、`sbom: true` **尚未实现**；`sign` / `provenance` 写 `none` 或不写表示关闭，`sbom` 为布尔值（`false` 或不写表示关闭） |

单个 asset 上限 2GiB。

**`release.npm[]`**：每项发布为 `@<账号级 scope>/<name>`；registry、scope、token 在 Kiln 中按账号配置（`kiln npm set`），workflow 中写 `registry` / `token` 会报错。

| 字段 | 默认 | 规则 |
|---|---|---|
| `from` | 必填 | 上游输出，可以是构建 job 用 `pnpm pack` 等打好的 `.tgz` |
| `name` | 必填 | 不含 scope 的短名（短名规则见 #6 正文），可引用 `matrix`、`github`；同一发布中各变体不能重名 |
| `access` | 必填 | `public` 或 `restricted`；只决定新包的可见性，已有包沿用原来的可见性 |
| `dist-tag` | `auto` | `auto` 或合法 dist-tag。`auto`：预发布 → `next`；正式版本且不低于该包所有已发布正式版本 → `latest`；更早的正式版本 → `latest-<主版本号>` |
| `allow-source-name` | `false` | 同 GitHub Release：源仓库私有、包公开时做源仓库名检查 |
| `files` | 全部 | 放进包里的文件 glob（相对输出路径）；输出根目录的 `package.json` 只有在被 `files` 选中时才作为基础（不作为普通文件），使用 `files` 筛选时如需保留原有 `bin` / `dependencies` 等元数据须显式包含 `package.json` |
| `package` | 无 | 合并进生成的 `package.json`（同名字段以它为准）；不能写 `name`、`version`（由 Kiln 按 `name` 和 tag 写入）；字符串可引用 `matrix`、`github` |

输出恰好是一个 `.tgz` 时原样发布，包内 `package.json` 的 `name`、`version` 须与配置和 tag 一致，此时不能写 `files`、`package`；`private: true` 的包拒绝发布。tag 须是合法的 semver（可选 `v` 前缀、允许预发布段如 `1.2.3-rc.1`，禁止构建元数据 `+…`），npm 版本是去掉可选 `v` 前缀后的 tag。

**`release.images[]`**：推送容器镜像；上游输出须是 OCI image layout（如 `buildctl --output type=oci,tar=false`），上游带矩阵时合并成一个多平台 index。

| 字段 | 默认 | 规则 |
|---|---|---|
| `from` / `name` | 必填 | 上游输出 / OCI 镜像名（如 `docker.io/owner/app`） |
| `tags` | 必填 | 不可变 tag 列表，如 `["${{ github.ref_name }}"]`；已指向其他 digest 时失败 |
| `floating-tags` | 无 | 浮动 tag（如 `latest`）；不能与不可变 tag 重名；只在本版本是正式 semver、且是 Kiln 向该镜像名发布过的最大正式版本时移动 |
| `username` / `password` | 必填 | registry 凭据；`password` **必须**引用 secret（如 `${{ secrets.DOCKERHUB_TOKEN }}`），不能明文写 |
| `sign` | `none` | `none` 或 `cosign`；`cosign` **尚未实现** |

**`release.uploads[]`**：按 [UPLOAD-PROTOCOL.md](UPLOAD-PROTOCOL.md) 把 `release.assets` 发布到第三方服务。

| 字段 | 默认 | 规则 |
|---|---|---|
| `url` | 必填 | 上传服务地址；不能重复 |
| `token` | 必填 | **必须**引用 secret |
| `name` | URL 的主机名 | 日志和发布结果中的目标名 |
| `latest` / `prerelease` | `auto` | `auto`、`true`、`false`；规则同 GitHub Release，latest 比较的是 Kiln 向同一地址发布过的正式版本 |

`release.verify-reproducible` 与 `release.runtimes` 能解析但**尚未实现**：用到时该发布 job 失败（与 `vm`、`image` 相同）。

### 6.2 macOS 签名与公证 `signing`

Runner 在执行 step 前建立 job 专用的临时 keychain 并导入下列内容，`KILN_KEYCHAIN` 是它的路径，job 结束后删除。只用于 `darwin/*` 的 host job（不能与 `container`、`vm` 同时使用），`certificates`、`provisioning-profiles`、`notary` 至少写一项。

| 字段 | 规则 |
|---|---|
| `certificates[]` | `p12`（必填，base64 编码的 `.p12`，含证书链和私钥）、`password`（证书密码） |
| `provisioning-profiles` | base64 编码的 provisioning profile 内容列表，不能有空值；Runner 解码安装后通过环境变量 `KILN_PROVISIONING_PROFILES` 传入路径 |
| `notary.profile` | 必填，step 中 `xcrun notarytool --keychain-profile <名字>` 使用；以字母或数字开头，只含字母、数字、`.`、`_`、`-`，最长 64 字符 |
| `notary` 凭据 | 两组二选一：`apple-id` + `team-id` + `password`（App 专用密码），或 `key` + `key-id`（团队 key 再加 `issuer`） |

`p12`、证书的 `password`、`notary.password`、`notary.key` **必须**引用 secret（如 `${{ secrets.MACOS_CERTIFICATE }}`），不能直接写在 workflow 中；`notary` 的其他字段和 `provisioning-profiles` 可以引用 `vars`、`github`、`matrix`，也可以引用 `secrets`。

## 7. 上下文与插值

| 上下文 | 可用字段 | 进入 action key |
|---|---|---|
| `github` | 见下表（`event_name` 同 Actions）；不支持 `github.event.*` | 是（插值后的文本） |
| `matrix` | 矩阵变量；引用不存在的变量报错 | 是 |
| `inputs` | `workflow_dispatch` 的输入（`boolean` 为布尔值，没填取默认值）；其他事件为空串 | 是 |
| `vars` | 规划时读取，优先级 environment → 仓库 → 账号 | 是 |
| `kiln` | `workspace`（工作区路径，执行时替换）；`commit_time`（`github.sha` 的 committer 时间，Unix 秒）。不能用在 `if`、job 的 `name`、`environment` | `workspace` 以占位符、`commit_time` 以值进入 |
| `platform` | `os`、`arch`，只能用在 `image` job 的 `files[].select` | 是 |
| `secrets` | secret 名称，只能出现在 #5 规定的位置 | 否；Runner 运行时或 Server 发布时注入 |

引用不支持的上下文或字段报错。**表达式**是 Actions 的子集，求值规则同 Actions（字符串比较不区分大小写，`contains` / `startsWith` / `endsWith` 遇到对象返回 false，`join` 的参数不是数组时基本类型返回它自己的字符串形式、对象返回空字符串，`toJSON` 不转义 `<>&`，单个表达式最长 21000 字节，嵌套（括号、`!`、下标、函数参数）最多 100 层）：字面量（单引号字符串、数字、`true`、`false`、`null`）、`a.b`、`a['b']`、比较与逻辑运算符、括号；函数 `contains`、`startsWith`、`endsWith`、`format`、`join`、`toJSON`、`fromJSON`。表达式结果（`format`、`join`、`toJSON` 的构造结果和插值后的字符串）上限为 **1 MiB**（1048576 字节），嵌套调用也无法放大超过它，超过时规划报错。不支持 `hashFiles` 和状态函数（`success()`、`failure()`、`always()`、`cancelled()`）。

| `github.*` | 分支 push | tag push | pull_request | workflow_dispatch |
|---|---|---|---|---|
| `sha` | 分支新 commit | tag 解析到的 commit（annotated tag 先解开） | PR head | 所选 ref 当前 commit |
| `ref` | `refs/heads/<分支>` | `refs/tags/<tag>` | `refs/pull/<编号>/head` | 所选分支或 tag 的完整 ref |
| `ref_name` | `<分支>` | `<tag>` | `<编号>/head` | 所选分支或 tag 名 |
| `ref_type` | `branch` | `tag` | `branch` | `branch` 或 `tag` |
| `head_ref` / `base_ref` | 空 | 空 | PR 源 / 目标分支名 | 空 |
| `repository` / `repository_owner` / `actor` | `owner/name` / `owner` / 触发者登录名 | 同左 | 同左 | 同左 |

- **版本号影响缓存（有意）**：只有引用 `github.sha`、`github.ref_name` 的 job 的 action key 随 commit / tag 变化（所以不注入 `GITHUB_SHA`、不提供 `.git`）；带版本号的构建宜单独成 job。
- **`SOURCE_DATE_EPOCH`** 默认常量 `315532800`（1980-01-01 UTC，zip 最早时间），不影响缓存和复现；要真实时间写 `SOURCE_DATE_EPOCH: "${{ kiln.commit_time }}"`（进入 action key）。Server 发布打包固定用 tag commit 的时间。
- **PR 构建 head commit（C3）**：结果不含与目标分支合并的效果，目标分支更新时 PR 不自动重建；合并队列会先更新落后的 PR 再验证。

## 8. `.kiln/runtimes.lock`

```toml
# 由 `kiln runtime lock` 根据 go.mod、package.json 等文件生成，请勿手工修改
[go]
version = "1.24.4"
"darwin/arm64" = "sha256:9f1c…"
"linux/arm64" = "sha256:47ab…"

[pnpm]
version = "9.12.0"
"any" = "sha256:…"  # 平台无关，运行在 node 之上
```

- 平台集合和 digest 的来源见实现（平台不区分 `/gnu`、`/musl`，平台无关的只有 `any`）；digest 变化时只重建依赖它的 job。
- job 用到的 runtime 在 lockfile 中没有、没有该平台条目，或仓库没有 lockfile 时，规划报错。

## 9. 校验与报错

- `kiln lint [文件...]` 与 Server 同一份代码，按分支 push、tag push、PR 展开（`vars` 为空）并检查 lockfile；不给文件时还查 `.kiln/merge.yml`。
- Server 规划出错时不建 job，run 直接失败，只建结论为 `failure` 的汇总 Check Run（没有 `name` 时用去掉 `.yml` 的文件名），出错行标 annotation。
- **报错**（整个 workflow 规划失败）：
  - YAML 无法解析、多个文档、别名循环或合并键 `<<`、未知字段、重复的键；`needs` 引用不存在的 job 或有环。
  - `fetch`、`release`、`image` 引用的 job 或输出不存在或不在 `needs` 中；`fetch` 没选出唯一变体。
  - `runs-on` 不是已知平台；`container`、`vm`、`services[].image`、`image.base` 未用 digest 固定；lockfile 缺条目（#8）。
  - 不支持的表达式、函数、上下文字段，或 `uses:` 等字段（提示替代写法）；`secrets` 位置不对，或必须引用 secret 的字段没有引用。
  - `services`、`buildkit`、`signing` 用在不允许的 job 上（#6；`signing` 不能带 `container` / `vm`）。
  - environment 不存在；跨仓库发布时没有 environment、目标不在其允许列表、App 未安装，或没写 `notes.from`。
  - 矩阵展开后单个 run 超过 256 个 job（同 Actions），或 job 显示名重复。
  - `timeout-minutes` 不在 `1..1440`、`strategy.max-parallel` 不在 `1..256`；`toolcache` 的 `name` / `env`、`services` 的服务名不是安全的标识符或会覆写保留变量；`release.npm[].name` 不符合短名规则；表达式超过 21000 字节、嵌套超过 100 层或结果超过 1 MiB。
- **只让单个 job 失败**：用到未实现的功能；ref 策略不允许当前 ref；发布 job 不是由 tag push 触发，或没有任何目标。`if` 为 false 的 job 先判 skipped。
- **执行时才检查**：`repositories` 不在同一 installation（签发凭据时拒绝）。
- **警告**（照常规划，写进汇总 Check Run）：引用 `secrets` 却没设 `cache: false`；`inputs` 某项匹配为空。
