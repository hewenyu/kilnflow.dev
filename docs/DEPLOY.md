# Kiln 部署与使用

一台机器跑 kiln-server 和 Postgres，Runner 可以多台（也可同机）。GitHub 要能访问 Server 的 `/webhook`，Runner 要能访问 Server，网络自己打通（Cloudflare Tunnel、Tailscale Funnel 等）。

## 1. 获取程序

每个程序一个文件，文件名不带版本号，都在 Release 中：`kiln`（CLI）、`kiln-runner` 有 darwin-arm64、linux-arm64、linux-amd64，`kiln-runner` 也有 npm 包 `@yuebanlaosiji/kilnflow-runner-<os>-<arch>`；`kiln-server`（Linux、macOS）另有镜像 `hewenyulucky/kilnflow`（`linux/arm64`、`linux/amd64`）。AI 执行节点的连接器 `kiln-orca` 只发布为 npm 包 `@yuebanlaosiji/kilnflow-orca`（Node.js 24+，与平台无关，见 #5.6）。

```bash
# Release：<程序名>-<os>-<arch>，校验和在 SHA256SUMS
curl -fL -o kiln-runner https://github.com/hewenyu/kilnflow.dev/releases/latest/download/kiln-runner-darwin-arm64
chmod +x kiln-runner && sudo mv kiln-runner /usr/local/bin/
# npm：包中只有这一个程序，在 package/ 下
npm pack @yuebanlaosiji/kilnflow-runner-linux-amd64
tar -xzf yuebanlaosiji-kilnflow-runner-linux-amd64-*.tgz package/kiln-runner
```

从源码构建（Go 1.27、Node.js 24+、pnpm），在仓库根目录：

```bash
make dist  # 先构建前端，再输出 dist/release/<程序名>-<os>-<arch> 和 SHA256SUMS
cd kiln-orca && pnpm install --frozen-lockfile && pnpm pack  # 连接器的 npm 包
make web build  # 只构建本机，输出 dist/local/；不先 make web 时 Dashboard 只显示提示页
```

## 2. Postgres

Postgres 14+，Server 启动时自动迁移。Mac 上：

```bash
brew install postgresql@16
brew services start postgresql@16
createdb kiln
# 连接串：postgres://$(whoami)@127.0.0.1:5432/kiln?sslmode=disable
```

也可以用 `deploy/compose.yml`（#4.2）。

## 3. 创建 GitHub App

Settings → Developer settings → GitHub Apps → New GitHub App：

| 项 | 填写 |
|---|---|
| GitHub App name | 随意，如 `kiln-myname` |
| Homepage URL | Server 地址，如 `https://kiln.example.com` |
| Callback URL | `https://kiln.example.com/auth/github/callback`（用 GitHub 登录 Dashboard 时才要） |
| Webhook | 勾选 Active；URL `https://kiln.example.com/webhook`；secret 用 `openssl rand -hex 20` 生成，即 `KILN_GITHUB_WEBHOOK_SECRET` |
| Subscribe to events | **Push**、**Pull request**、**Check run**、**Check suite**、**Repository**、**Issues**（AI 开发用） |
| Where can this GitHub App be installed? | **Only on this account** |

| Repository permissions | 用途 |
|---|---|
| **Contents: Read and write** | 读代码、发 Release、合并 PR、自动打 tag、AI 开发推进分支 |
| **Checks: Read and write** | Check Run |
| **Issues: Read and write** | 合并后关闭 `Closes #N` 关联的 issue 并留言；AI 开发读 Issue、换 label、评论 |
| **Metadata: Read-only** | 必选 |
| **Pull requests: Read and write** | 读 PR、发 AI 审计 review、更新分支、加去 label、评论、AI 开发开 PR |

创建后记下 **App ID**（`KILN_GITHUB_APP_ID`）；**Generate a private key**，`.pem` 即 `KILN_GITHUB_PRIVATE_KEY_FILE`；**Install App** 装到你的账号并选择仓库（跨仓库发布的目标仓库也要选）；用 GitHub 登录 Dashboard 时生成 Client secret、记下 Client ID。已有的 App 加权限后，要在 Settings → Applications → Installed GitHub Apps 中批准才生效。

## 4. 启动 kiln-server

### 4.1 配置

```bash
mkdir -p ~/kiln-server/data && cd ~/kiln-server
cp /path/to/repo/deploy/server.env.example server.env && chmod 600 server.env
cp ~/Downloads/<app>.private-key.pem github-app.pem && chmod 600 github-app.pem
openssl rand -hex 24  # → KILN_ADMIN_TOKEN
openssl rand -hex 32  # → KILN_MASTER_KEY（务必备份：丢失后已保存的 secret 无法解密）
```

`server.env` 必填 `KILN_DATABASE_URL`、`KILN_ADMIN_TOKEN`（至少 16 个字符）、`KILN_MASTER_KEY`（后两项可改用 `KILN_ADMIN_TOKEN_FILE`、`KILN_MASTER_KEY_FILE`）；接 GitHub 还要 `KILN_GITHUB_APP_ID`、`KILN_GITHUB_PRIVATE_KEY_FILE`、`KILN_GITHUB_WEBHOOK_SECRET`、`KILN_ALLOWED_ACCOUNTS`（你的 GitHub 登录名，其他账号的安装会被自动卸载）。全部变量见 `kiln-server --help` 和 `deploy/server.env.example`，常用的：

| 变量 | 说明 |
|---|---|
| `KILN_PUBLIC_URL` | 外部地址 `https://主机[:端口]`（webhook、Runner 注册命令、OAuth 回调用） |
| `KILN_LISTEN`、`KILN_DATA_DIR` | 默认 `:8080`、`./kiln-data`（CAS 和日志） |
| `KILN_GITHUB_CLIENT_ID`、`KILN_GITHUB_CLIENT_SECRET` | Dashboard 的 GitHub 登录，两项同时设置 |
| `KILN_ADMINS`、`KILN_USERS` | 能用 GitHub 登录的管理员、成员（成员只能查看、手动触发、取消和重跑） |

环境中已有的变量优先于 `--env-file`。

```bash
./kiln-server --env-file server.env  # 前台试运行
```

看到 `GitHub App slug=…` 和 `kiln-server 已启动` 即可。打开 `KILN_PUBLIC_URL` 用 admin token 登录（开了 GitHub 登录时在"管理员备用入口"）。

**管理员绑定 GitHub 账号**（PLAN #16.5）：**设置 → 登录与身份** →"绑定 GitHub 账号"，该账号即管理员；确认能用 GitHub 登录后，同一页"关闭 admin token 登录"（CLI 不受影响）。`kiln admin token-login on` 恢复；`kiln admin unbind` 解绑（同时恢复）。

### 4.2 作为服务运行

| 方式 | 做法 | 日志 |
|---|---|---|
| macOS | `deploy/macos/dev.kiln.server.plist` 中的 `YOU` 换成用户名，放到 `~/Library/LaunchAgents/`，`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.kiln.server.plist` | `~/kiln-server/logs/kiln-server-<日期>.log`，保留 7 天；`kiln-server.out.log` 只有启动失败和崩溃 |
| Linux | `deploy/linux/kiln-server.service`（读 `/etc/kiln/server.env`，用户 `kiln`） | journald；要只保留 7 天，设 `KILN_LOG_DIR=/var/log/kiln`（`kiln` 可写）或调 `MaxRetentionSec` |
| docker compose | 在 `deploy/` 下放 `server.env`、`github-app.pem`，`docker compose up -d`（镜像 `hewenyulucky/kilnflow:latest`，没有镜像时加 `--build`） | `docker compose logs server`，每个容器最多 5 个 20MB 文件 |

Server 只监听 HTTP，HTTPS 交给反向代理或隧道。升级即换程序或镜像后重启。

### 4.3 CLI

```bash
kiln login --server https://kiln.example.com  # 粘贴 admin token
kiln repo list
kiln admin  # 绑定的 GitHub 账号、admin token 登录是否开启
kiln help
```

环境变量 `KILN_SERVER`、`KILN_ADMIN_TOKEN` 覆盖保存的配置。退出码：0 成功，1 运行错误，2 用法错误，130 被中断。

### 4.4 仓库、补对账与保留

Server 不主动请求 GitHub：仓库有事件后才出现在"设置 → 仓库"，要提前配置就点"刷新仓库"（`kiln repo sync`）。漏掉的 webhook（投递失败、停机、一次推送超过 3 个 tag）点"补对账"或 `kiln repo reconcile owner/name`（第一次只记基线）。周期对账默认关闭，两项都设才开启：

```bash
KILN_RECONCILE_INTERVAL=15m  # 至少 1m
KILN_RECONCILE_REPOS=owner/repo  # owner/name 逗号分隔；* 表示全部
```

**保留**（PLAN #10.3）：日志、产物在 run 结束 12 小时后删除（之后要看日志只能重跑），缓存条目在创建或最后命中 12 小时后删除；每小时清理，记在"设置 → 活动"的 `retention.collect`。`KILN_RETENTION=12h`（也可写 `1d`、`7d`，至少 `1h`）。

## 5. Runner

### 5.1 注册 CI Runner

生成一次性注册令牌（或 Dashboard 的"注册新的 Runner"）：

```bash
kiln runner token  # 可选 --pool P、--ttl 分钟
```

在构建机上以能执行 `docker info` 的普通用户运行（不要复制 admin token 或 App 私钥）：

```bash
kiln-runner enroll --server https://kiln.example.com --token kiln-enroll-… --name mini-01  # 不写 --name 时用主机名
kiln-runner doctor
kiln-runner service install  # 复制为启动器和当前版本，安装并启动开机自启的服务
```

| | macOS | Linux |
|---|---|---|
| 服务 | LaunchAgent `dev.kiln.runner`（`~/Library/LaunchAgents/dev.kiln.runner.plist`） | systemd 用户服务 `kiln-runner`（`~/.config/systemd/user/kiln-runner.service`） |
| 入口 | `~/kiln/bin/launcher/kiln-runner launch` | 同左 |
| 重启 | `launchctl kickstart -k gui/$(id -u)/dev.kiln.runner` | `systemctl --user restart kiln-runner` |
| 日志 | `~/kiln/logs/runner-<日期>.log`，保留 7 天；`runner.out.log` 只有启动失败和崩溃 | 同左；启动失败和崩溃在 `journalctl --user -u kiln-runner` |

Linux 还要执行一次 `sudo loginctl enable-linger "$USER"`（无人登录时继续运行）。服务的 `PATH` 取自执行 `service install` 的 shell。其他命令：`run`（前台）、`service uninstall`、`version`。

**什么时候会被自动拉起**：服务（launchd `KeepAlive` 的 `SuccessfulExit=false`、systemd `Restart=on-failure`）只在 Runner 以非 0 退出或被信号杀死时重启。Runner 收到 SIGTERM / SIGINT（`kill <pid>`、`systemctl --user stop`、Ctrl-C）时等正在执行的任务结束后以 0 退出，**不会**被拉起；被吊销后以 0 退出，同样不会被反复拉起；出错退出（例如连不上 Server、试用的新版本没有通过确认）、崩溃、`kill -9` 后会被拉起（systemd 等 5 秒；launchd 按它的节流，两次启动至少间隔 10 秒）。手工停止后用上表的“重启”命令再启动。服务定义只在 `service install` 时写入，旧版本装的服务文件不会自动更新：升级到新版本后重新执行一次 `kiln-runner service install` 即可（会重启服务，旧进程等正在执行的任务结束后退出）。数据目录的路径中有 `%`、`$`、空格等字符时服务定义照样正确；systemd 不接受可执行文件的路径中有引号、反斜杠、控制字符，Linux 上这时 `service install` 报错且不改动任何东西，换一个数据目录（`KILN_RUNNER_DIR`）再安装。

**oci executor**：`docker info` 可用即启用（数据目录要共享给 docker 虚拟机，见 #9）。Apple Silicon 要 `linux/amd64` 时设 `oci_platforms`（需 Rosetta / QEMU）。

### 5.2 config.toml

数据目录 `~/kiln`（`KILN_RUNNER_DIR` 可改），配置在 `~/kiln/config.toml`，改后重启服务：

```toml
server = "https://kiln.example.com"
name = "mini-01"
max_jobs = 4  # 同时执行的 job 数，默认 4
reserve_cpu = 2  # 给宿主机预留，默认 2 核、"4Gi"；小机器写 0 和 "0"
reserve_memory = "4Gi"
# extra_path = ["/opt/homebrew/bin"]  # 加到 host job 的 PATH（默认只有 runtime 和 /usr/bin:/bin:/usr/sbin:/sbin）；不计入缓存 key
# oci_platforms = ["linux/arm64", "linux/amd64"]
# buildkit = true  # 接受 buildkit: true 的 job
# toolcache_max_size = "10Gi"  # 单个 toolcache 目录上限，超过后整个删除
# runtime_max_size = "5Gi"  # 装好的 runtime（rt/）总量上限，超过后从最久没用的删起
# cache_max_size = "50Gi"  # 本地缓存总量上限，默认不限；超过时先删缓存，最后才删 runtime
# disk_high_watermark = 80  # 超过时回收到 disk_low_watermark（默认 65）以下；100 关闭
```

其他字段：`docker`（命令路径；`"-"` 关闭 oci executor）、`cleanup_image`（清理 root 写入文件的镜像，要能 pull）、`keep_workspaces = true`（调试用）、`upgrade = false`（本机关闭自升级）。

job 的 `resources`（默认 1 CPU / 1GiB）超出可分配量（规格减预留）时一直等待（PLAN #14.1），`kiln runner list` 看已分配 / 可分配。2 核 / 约 2GiB 的机器把 `reserve_cpu`、`reserve_memory` 设为 0；同机有其他服务时建议 `max_jobs = 1`。

### 5.3 清理与本地缓存

服务空闲时每小时按保留期、容量上限和磁盘水位清理（PLAN #8.3）：缓存、git 镜像、CAS、镜像和失败现场 12 小时；装好的 runtime（`rt/`）是工具，30 天没有用到才删，总量超过 `runtime_max_size` 时从最久没用的删起，为 `cache_max_size` 或磁盘水位回收时排在缓存之后（PLAN #12.8）。job 日志中 runtime 一行写明"已安装，直接使用"或"下载并安装，用时 …"。

| 要做的事 | 命令 |
|---|---|
| 本机清理 / 查看 | `kiln-runner gc [--json]`（服务运行中只按保留期清理；有对象删不掉时退出码 1）、`kiln-runner cache [--json]` |
| 远程查看 | `kiln runner cache NAME`，或 `/runners/cache/<名称>` |
| 远程清理 | `kiln runner cache purge NAME [--repo R] [--kind K] [--domain D] --dry-run` 预览，去掉 `--dry-run` 执行（Runner 没有 job 时执行） |

### 5.4 只能经代理上网的 Runner

step **不继承** Runner 进程的 `HTTP_PROXY` 等变量：Runner 能下载，step 中 `go mod download` 却报 `lookup proxy.golang.org …: network is unreachable`。各环节分别配置（PLAN #8.5）：

| 环节 | 配置 |
|---|---|
| Runner 自己（Server、git、runtime） | Linux `systemctl --user edit kiln-runner` 加 `Environment=HTTPS_PROXY=…`；macOS 在 `dev.kiln.runner.plist` 的 `EnvironmentVariables` 中加（`service install` 会覆盖） |
| host job 的 step | `[proxy.host]` |
| oci job 容器、BuildKit 拉 `FROM` | `[proxy.oci]` |
| docker 拉 job / service 镜像 | Docker daemon 的代理，Linux：`sudo systemctl edit docker` 加 `[Service]` 和 `Environment="HTTPS_PROXY=http://127.0.0.1:3128" "NO_PROXY=localhost,127.0.0.1"`，`sudo systemctl restart docker`，`docker pull busybox:1.37` 验证 |
| Dockerfile 的 `RUN` | buildctl 加 `--opt build-arg:HTTPS_PROXY=$HTTPS_PROXY` |

- CI Runner 拉取源码的 git 与宿主机的 Git 配置、模板和 netrc 隔离，不读 `~/.gitconfig` 中的代理或 CA；代理和 CA 只能经 Runner 进程的环境变量（`http_proxy` / `https_proxy` / `no_proxy`、`SSL_CERT_FILE` / `SSL_CERT_DIR` / `GIT_SSL_CAINFO` / `GIT_SSL_CAPATH`）配置，也就是上表第一行给 Runner 设置的那一份。

```toml
[proxy.host]
http_proxy = "http://127.0.0.1:3128"
https_proxy = "http://127.0.0.1:3128"
no_proxy = "localhost,127.0.0.1,.corp.example.com"

[proxy.oci]  # 必须是容器可达的地址，不接受本机地址（启动时报错）
http_proxy = "http://172.17.0.1:3128"
https_proxy = "http://172.17.0.1:3128"
no_proxy = "localhost,127.0.0.1"
```

- `[proxy.oci]` 用局域网代理，或让代理监听 docker 网桥网关（Linux 默认 `172.17.0.1`，见 `docker network inspect bridge`；有 `services` 的 job 网关不同，最好监听 `0.0.0.0` 并用防火墙只放行 docker 网段）。
- 只注入 `network: true` 的 job，`env` 可覆盖。带认证写 `http://用户名:密码@proxy.corp.example.com:8080`，`config.toml` 权限设 `600`。SOCKS 写 `socks5h://`，不要写 `socks5://`。
- 验证：`kiln-runner doctor --network`；容器内 `docker run --rm -e HTTPS_PROXY=http://172.17.0.1:3128 curlimages/curl -sSI https://proxy.golang.org`。

### 5.5 升级、暂停与删除

**升级 CI Runner**（PLAN #27）：Runner 每小时从 npm 查询自己包的最新正式版本，管理员在 Runner 详情页（`/runners/detail/<名称>`）按台升级，`/settings/runner-upgrade` 看全部版本、改 registry。CLI：

```bash
kiln runner upgrade  # registry、各 Runner 的版本、最新版本、升级状态和原因
kiln runner upgrade check [mini-01]  # 立即查询（不写名称表示全部）
kiln runner upgrade start mini-01 [0.1.45] # 写版本时必须仍是最新版本；start 可省略
kiln runner upgrade cancel mini-01  # 已开始切换时不能取消
kiln runner upgrade registry https://npm.example.com/  # npm 镜像（只有地址，不要 token）；default 恢复官方
```

Runner 等手上的 job 结束后切换，新版本连续 3 次启动都没连上 Server 时自动回到旧版本（`~/kiln/bin/versions/`）。只有经启动器运行的服务能切换，启动器本身只在 `service install` 时更新。

> [!IMPORTANT]
> **CI 先升级 Server，再升级 Runner**。按操作签发源码凭据（PLAN #8.4）之后的 Runner 只用 `FetchSecrets` / `IssueGitToken` / `RevokeGitToken`，不再调用旧的 `FetchCredentials`；遇到旧 Server 时每个 job 以 `infra.credentials` 失败，原因是"Server 版本过旧，不支持按操作签发的源码凭据，请先升级 Server"（不退化到旧方式）。Server 升级后再回滚到旧版本时，step 中受管的 git 访问同样以这个原因失败（写在 git 的输出中）。旧 Runner 在新 Server 下照常工作。

**升级 AI 执行节点**：在节点上用 npm 升级连接器后重启服务。Orca 中正在执行的 Agent 不受影响，连接器重启后恢复这些任务：

```bash
npm install -g @yuebanlaosiji/kilnflow-orca@latest && kiln-orca service restart
```

kiln-orca 的 npm 包带着同版本的本平台 `kiln-runner`（可选依赖，准备受管理的工具链用，PLAN #33.12），不要加 `--omit=optional`；`kiln-orca doctor` 的 kiln-runner 一项显示它的位置和版本。

Server 只支持当前的 AI 协议：Server 升级到新协议后，协议较旧的节点不再领取（Dashboard 显示"需要升级"），升级连接器后照常领取，期间新的 AI 任务等待。

> [!IMPORTANT]
> **AI 协议 8（git 凭据按操作签发、用后吊销，PLAN #33.13）：升级要先排空。** 这是"升级连接器不影响在途任务"的一次例外：协议 8 的 Server 拒绝旧连接器的凭据请求（没有 `op_id`），新的任务说明要求 Agent `git fetch origin`，旧连接器做不到；在途任务跨不过这次升级。步骤：
>
> 1. 在 Server 上排空每台 AI 节点：`kiln runner drain NAME`（或 Dashboard）。全部节点的 drain 都成功之后，再等 Dashboard 中没有执行中的 AI 任务（开发最长 12 小时；急时可以停止任务）。
> 2. 升级 Server。
> 3. 每台节点升级 kiln-orca（上面的命令，`kiln-orca service restart`），再 `kiln runner undrain NAME`（或 Dashboard）。
> 4. 回滚同样先按第 1 步在 Server 上排空，再换回旧 Server 和旧连接器。

> [!IMPORTANT]
> **AI 协议 9（外部设计文档 `kiln-orca task doc`）：** 连接器新增 `kiln-orca task doc list|get`。Server 只把任务派给上报协议 9 的连接器；协议 8 的旧节点显示"需要升级"、不再领取新任务，在途任务是在升级前按旧 prompt 派发的、照常完成；在途的选题领取由 Server 按节点协议跳过文档指令（协议 8 的节点不会收到 `doc get`），所以**不需要排空**。升级顺序：先升级 Server，再 `npm` 升级每台节点的 kiln-orca 并 `kiln-orca service restart`。
>
> 排空只能在 Server 上做：Server 的 drain 提交之后不会再派出任务，节点本机的暂停接单只在下一次领取之前生效，已经在等待中的领取（最长 30 秒）仍可能领到任务。
>
> Server 的 GitHub App 不需要新权限（交付写 PR 用 Pull requests 的写权限，App 已有）。升级后已有 clone 的 origin 在下一次准备仓库时改回真实地址。

> [!IMPORTANT]
> **AI 协议 10（kiln-task skill）：** 连接器启动时就把随包分发的 `kiln-task` skill 同步到 Agent 的目录，并在每次启动 Agent 前再次确认（位置、原子安装和冲突处理见 #5.6），任务说明只保留任务本身的规则并指向它。Server 只把任务派给上报协议 10 的连接器；协议 9 的旧节点显示"需要升级"、不再领取新任务。执行机器上没有 Agent 的 skill 目录的写权限（或同名目录不是连接器装的）时节点上报未就绪、不领取新任务，Dashboard 和日志写明路径和原因，处理见 #5.6。
>
> **推荐先排空再升级。** 已在执行中的协议 9 任务、以及它领取时动态返回的 `Claim` 说明都保持兼容（`TaskDocMinProtocol=9`，动态说明不含 `{{KILN_SKILL}}`，只有启动 Agent 的 prompt 才有）。但如果在 Server 切换的瞬间，某台协议 9 节点的 `Acquire` 响应丢失、请求被重放，新 Server 会按协议 10 重建这次任务的 prompt，其中带有 `{{KILN_SKILL}}`，而旧连接器不认识这个占位符。这个窗口很小，但为了不让在途任务受影响，稳妥的顺序是：`kiln runner drain NAME` 排空每台 AI 节点 → 等 Dashboard 中没有执行中的 AI 任务 → 升级 Server → 每台节点 `npm install -g @yuebanlaosiji/kilnflow-orca@latest && kiln-orca service restart` → `kiln runner undrain NAME`。

> [!NOTE]
> **AI 协议 7（Agent 用 `kiln-orca task done` 结束，PLAN #33.5）**：Server 升级到这一版后，所有 AI 执行节点都要升级 kiln-orca（上面的命令）才能领取，期间新的 AI 任务等待。任务说明要求 Agent 用 `kiln-orca task done` 结束这一轮，由它在 Agent 的进程中向 Orca 报告结束，Agent 不再自己找 `orca` 命令。节点的配置不用改：Linux 节点没有写 `orca` 时默认用 `orca-ide`。升级后节点上保留的开发工作区会被回收（见下面的"升级说明"）。

> **AI 协议 6（git 凭据由 Server 签发，PLAN #33.13）**：Server 升级到这一版后，所有 AI 执行节点都要升级 kiln-orca（上面的命令）才能领取。升级后节点不再用机器上的 git 凭据；Server 的 GitHub App 不需要新权限（沿用 CI 已有的 Contents 读写）。仓库的 Agent 构建要拉同账号的私有依赖时，在 `.kiln/agent/config.yml` 中声明 `requirements.repositories`（见 #5.6 的"git 凭据"）。（协议 8 起凭据按操作签发，origin 是真实地址，见上。）

**暂停、吊销、删除**（Dashboard 也可）：`kiln runner drain NAME` / `undrain NAME` 暂停 / 恢复接单；`kiln runner revoke NAME` 吊销；`kiln runner delete NAME` 删除已吊销的（没有未结束的任务时；名称可重新注册）。构建机上 `kiln-runner drain` / `undrain` 本机停止 / 恢复领取（Dashboard 不显示）。

### 5.6 AI 执行节点（Orca + kiln-orca）

AI 审计、选题、设计、开发、返修由执行机器上常驻的 Orca 完成；连接器 `kiln-orca` 从 Kiln 领取任务、在 Orca 中启动 Agent、把结果交回 Kiln（PLAN #33）。Orca 内部怎么分工（拆分、子 Agent）是它自己的事。节点的角色（审计 / 开发）由注册令牌决定、注册后不能改；一个节点可以同时执行多个任务（`max_tasks`）。审计 AI 开发的 PR 时，审计用的 Agent 不能是开发时用过的（例如 Claude Code 开发、Codex 审计），所以一台装了 Codex 和 Claude Code 的机器就能全自动运行：在这台机器上注册一个审计节点和一个开发节点（各用自己的数据目录），也可以分到两台机器。可与 `kiln-runner` 同机。

前提（`kiln-orca doctor` 逐项检查）：Node.js 24+；Orca 桌面版已打开，版本 **1.4.258 或更高**（连接器支持的最低版本；macOS 上已用真实 Orca 验证连接器在 Orca 侧的流程，PLAN #33.4），Orca 的命令行默认 macOS 用 `/Applications/Orca.app/Contents/Resources/bin/orca`（不存在时用 `PATH` 中的 `orca`），Linux 用 `PATH` 中的 `orca-ide`（Linux 上裸的 `orca` 通常是屏幕阅读器）；装在别处时在 `config.json` 的 `orca` 中写绝对路径，或确认它在 `service install` 时的 `PATH` 中；执行不了时 `doctor` 的 Orca 一项失败，运行中的节点按 Orca 未就绪处理、不领取任务。Agent 不直接用 `orca` 命令：它用 `kiln-orca task done` 结束这一轮，由这个命令在 Agent 的进程中向 Orca 报告结束——优先用 Orca 给 Agent 的会话设置的 `ORCA_CLI_COMMAND`（Orca 命令行的绝对路径），没有时用这里的 `orca`（只是名字时按连接器的 `PATH` 查找）；两个都执行不了时 Agent 会被告知这是节点的问题，连接器日志中记 error（"Agent 的 kiln-orca task done 执行不了 Orca 命令行"，带命令和来源），这时在 `config.json` 的 `orca` 中写绝对路径。**平台**：审计节点可以是 macOS arm64、Linux amd64、Linux arm64（Linux 可以配置，实机验证进行中；仓库在 `review.required_platforms` 中写了这个平台，任务才会派给它，见下面的仓库配置）；开发节点同样可以是这三个平台（仓库的 `development.platform` 中写了这个平台才派给它，不写时只派给 macOS arm64，见 #8.3）。Orca 版本过低时 `doctor` 的 Orca 一项失败（"Orca 版本 X 低于连接器支持的最低版本 1.4.258，请升级 Orca"），运行中的节点上报 Orca 未就绪、不领取任务（Dashboard 的 AI 执行节点页显示同样的原因），升级 Orca 后一分钟内自动恢复领取，不用重启连接器；仓库指定的 Agent（`codex`、`claude`）已安装并登录；装有 git（**不需要** git 凭据、SSH key 和 git 身份：clone、fetch、推 attempt 分支用 Kiln Server 按操作签发、用后吊销的短期 GitHub App token，Agent 的 git 经 `kiln-orca task exec` 拿到只读 token，见下面的"git 凭据"）；可选 Docker Compose（提供 `pg`、`redis`）：仓库在 `requirements.services` 中声明了测试服务时，任务只派给 Docker / Compose 就绪的节点，没有这样的节点时任务等待，等待原因写明缺 Docker；没有声明测试服务的任务不需要 Docker。

```bash
npm install -g @yuebanlaosiji/kilnflow-orca
kiln runner token --kind ai  # 审计；开发用 --kind ai --role development。或 Dashboard 注册页选择 Runner 类型
kiln-orca enroll --server https://kiln.example.com --token kiln-enroll-… --name mini-ai-01
kiln-orca doctor             # 注册与 Server、Orca、Agent、kiln-runner（可选）、git、Docker（可选）、journal
kiln-orca service install    # 用户服务（macOS LaunchAgent，Linux systemd --user），打印服务标识和定义文件

# 同一台机器再注册一个开发节点：每条命令都加 --home <另一个数据目录>
kiln-orca enroll --home ~/kiln-orca-dev --server https://kiln.example.com --token kiln-enroll-… --name mini-dev-01
kiln-orca doctor --home ~/kiln-orca-dev && kiln-orca service install --home ~/kiln-orca-dev
```

一个数据目录是一个节点，也是一个服务：服务标识只由数据目录决定（例如 `dev.kiln.orca.3f2a9c1d0b4e`、`kiln-orca-3f2a9c1d0b4e.service`），同一台机器上的节点互不覆盖；同一个数据目录重新注册、改名后仍是同一个服务（`service restart` 即可，名称只显示在服务说明和 `service status` 中）。实际的服务标识、定义文件、节点现在的名称、重启 / 查看状态的 launchctl / systemctl 命令和服务日志位置以 `kiln-orca service status [--home …]` 的输出为准，不要自己拼。服务定义中记下了所属的数据目录，`service install` 遇到不属于这个数据目录的同名定义时报错，不覆盖、不停止它；遇到同一个数据目录以前装的其它服务定义时停止、删除它们（打印删除了哪些）。数据目录按规范路径识别（经符号链接指定的是同一个）；要移动数据目录，先在原来的位置 `kiln-orca service uninstall`，移动后再 `service install --home <新位置>`。`service uninstall` 只删除服务定义，数据目录（注册信息、journal、日志）保留。数据目录、Node 或 npm 的安装路径中有 `%`、`$`、空格等字符时服务定义照样正确；systemd 不接受 node 的路径中有引号、反斜杠、控制字符，这时 `service install` 报错。

其他命令：`run`（前台运行）、`service uninstall|restart|status`、`drain on|off`（本机停止 / 恢复领取，正在执行的任务继续）、`version`。数据目录 macOS `~/Library/Application Support/kiln-orca`、Linux `~/.local/share/kiln-orca`（`KILN_ORCA_HOME` 或 `--home` 可改）。Linux 上要在登出后继续运行时执行 `loginctl enable-linger`。

**kiln-task skill**（AI 协议 10 起）：Agent 用的命令（`kiln-orca task info|check-report|claim|deliver|ci|doc|exec|done`）的用法、退出码和输出格式写在随包分发的 skill 中，任务说明只保留任务本身的规则并指向它。连接器在启动 Agent 之前把它同步到 Agent 加载 skill 的目录：Codex 是 `~/.agents/skills/kiln-task`，Claude Code 是 `$CLAUDE_CONFIG_DIR/skills/kiln-task`（没有设置 `CLAUDE_CONFIG_DIR` 时 `~/.claude/skills/kiln-task`）。连接器只覆盖自己安装的目录（其中有 `.kiln-orca-skill.json`，`managedBy: kiln-orca`）：用户自建的同名目录、符号链接、普通文件一律不覆盖，这时节点上报未就绪、不领取新任务，日志和 Dashboard 的 AI 执行节点页写明路径和原因，移开或改名后一分钟内自动恢复。已经是最新内容时不写文件；升级连接器后自动更新，同机并存协议更高的连接器时保留它的版本、不降级覆盖。`kiln-orca doctor` 只读地检查每个 Agent 的 skill（尚未安装时说明会自动安装、已安装的版本、冲突和怎么处理），不创建目录。skill 中要求 Agent 只用这些公开子命令，不用仓库中或被审 PR 中的同名文件替代它（那些是被审计的内容）。

`config.json`（在数据目录中）的字段都可省略：`clone_root`（仓库 clone 的根目录，绝对路径，默认数据目录中的 `repos/`；只在没有执行中、待回收的任务时改，改后旧目录中的 clone 不再被使用和回收，可以手工删除；`doctor` 显示当前值）、`kiln_runner`（kiln-runner 的绝对路径，默认用随 kiln-orca 安装的本平台的包，见下面的受管理的工具链）、`max_tasks`（同时执行的任务数，默认 2）、`agents`（对外提供的 Agent，默认 `["codex", "claude-code"]`；不在列表中的不检测、不上报、不调度）、`orca`（Orca CLI 的路径）、`docker`、`postgres_image` / `redis_image`（默认 `postgres:17` / `redis:7`，须能匿名拉取）、`postgres_port` / `redis_port`（`127.0.0.1` 上，默认 `55432` / `56379`，被占用时换空闲端口）。改了配置后 `kiln-orca service restart`。日志在数据目录的 `logs/kiln-orca-<日期>.log`（按天轮转，保留 7 天）：服务运行（`run --service`）时正常日志只写这里；`kiln-orca run` 前台运行时同时打印到终端。macOS 上 launchd 记录的 `logs/service.err.log` / `service.out.log` 只有启动失败、崩溃和写日志文件失败时的输出，连接器启动时和之后每天把其中的内容移进对应日期的按日文件并清空它们（不会无限增长，超过 7 天的随按日文件删除）；Linux 上这些输出在 `journalctl --user -u <单元名>` 中。`journal/` 写不进去（磁盘满、只读）时节点不领取新任务，日志中有"journal 写不进去"，腾出空间后自动恢复。`journal/` 中有读不了的记录（文件权限不对、内容损坏、版本不认识等）时，可读的任务照常执行，但节点不领取新任务、不回收测试服务，每条读不了的记录按占用一份容量计；日志、`kiln-orca doctor` 和 Dashboard 的 AI 执行节点页（Orca 未就绪的原因）中写明文件和原因。连接器不会删除这些文件，处理方法见 #9；处理后一分钟内自动恢复，不用重启。仓库的 clone 在 `repos/`（或 `clone_root`），Orca 的 worktree 建在它们之上，之后的任务复用。审计和开发的工作区都是一次性的（每一轮开发都从远端的分支重新开始）：结果确认提交（或任务失效）、Agent 停止、终端释放之后，连接器经 Orca 删除这次任务的工作区（`kiln-a<attempt>-e<epoch>`）、Agent 在任务目录中的检出（例如审计的 `tasks/<attempt>/head`）和工作区的本地分支，删除后核对 Orca、git 和磁盘上都没有了（工作区中有构建产物、变异验证的改动时带 `--force`；工作区中有人打开的终端、Orca 中还有工作区在这里且终端没有释放的 worker 时等）。开发的交付和 checkpoint 在回收之前已经推到 attempt 分支并确认；没有推到远端的改动（推送失败，租约失效、节点被吊销时不推 checkpoint）不特殊处理，随工作区删除，日志中写明删掉了什么（"回收开发的工作区：删除了没有推送的改动"，带 attempt、分支、HEAD、commit 数和未提交的文件；都已推送时是 info "其中的改动都已经推到远端"）。节点上不保留开发的工作区：要看开发的过程，看 attempt 分支、PR 和 Kiln 中的日志。回收没完成（例如 Orca 暂时连不上）时记在 `journal/` 中，节点每分钟重试，不影响结果的提交、不占容量，超过 12 小时放弃并在日志中记 error 和处理方法（#9）。审计的结论、finding，开发的交付报告和日志在 Kiln 中；Orca 中 Agent 的历史会话记录是否保留由 Orca 决定。Agent 的终端在任务结束时由连接器释放（Orca 暂时连不上时节点每分钟重试，不影响结果的提交、不占容量）。`kiln-orca doctor` 的 journal 一项写明待释放终端、待回收工作区的条数。留下的已结束的工作区（例如提交结果失败时留作诊断的）由连接器每小时有界地回收（1 小时没有活动之后，每轮最多 10 个）。**升级说明**：之前的版本保留开发的工作区，升级后它们（备注是 `kiln:attempt=…;epoch=…;task=…;kind=develop`）也被当作历史残留回收，其中没有推到远端的改动一起删除（日志写明分支、HEAD 和未提交的文件）；要留的，升级前自己复制出来或推到别的分支。更早（kiln-orca 0.3.6 及之前）的工作区的备注中没有任务类型，连接器不能确认它们的来历，一律跳过（日志中有"跳过旧版本建的工作区"）；人工清理见 #9。测试服务的 docker 用数据目录 `docker/` 下的配置：连接器根据 `DOCKER_CONFIG` / `~/.docker/config.json` 生成（沿用 context，不带其中的凭据），生成不了（例如这个文件不是有效的 JSON）时 Docker 按不可用上报、原因写明哪一步和哪个文件，节点照常执行不需要测试服务的任务，修好后一分钟内自动恢复，不用重启；连接器不会退回用个人配置执行 docker；任务结束时删除测试服务的容器，删除失败时连接器在重启后和每小时的清理中回收（Compose 项目 `kiln-orca-<数据目录标识>-<attempt>`，只回收本数据目录的）。

**被吊销后**（Dashboard 或 `kiln runner revoke`）：节点停止领取，在途任务的 Agent 被停止（不推交付、checkpoint，不提交结果），数据目录中写 `revoked` 文件。不能确认 Agent 已经停止（或者启动结果还在查证），或者 `journal/` 中有读不了的记录（其中可能有停不掉的 Agent，处理见 #9）时连接器不退出、不联系 Server，每分钟再停一次、重新读取，全部了结后以退出码 0 退出；服务（launchd `SuccessfulExit=false`、systemd `Restart=on-failure`）只在非 0 退出和崩溃时重启，所以不会被反复拉起（同样，手工 `kill` 连接器后它正常退出、不会被自动拉起，用 `kiln-orca service restart` 启动）。之后再启动（包括清理中崩溃后被拉起）时不联系 Server，只继续停止 journal 中的任务。`kiln-orca doctor` 显示"已被吊销"。要继续使用这台机器：用新令牌 `kiln-orca enroll`（删除 `revoked`），再 `kiln-orca service restart`（`revoked` 中记的是被吊销的 runner id，旧的连接器晚于 `enroll` 才写下它也不影响新注册）。

数据目录中的文件各有一个写入者，`enroll` 与运行中的连接器可以同时进行、不会互相覆盖：`state.json` 是注册信息，只有 `enroll` 写（运行中的连接器在重启后才用新的注册）；`mailbox.json` 是连接器在 Orca 中的邮箱（终端、Run、没处理完的消息读取），只有运行中的连接器写，重新注册不碰它；`keys/` 中的私钥只在第一次注册时生成。不要手工编辑 `state.json`、`mailbox.json`。完整的表见 PLAN #33.3。

`kiln-orca run` 提示"另一个 kiln-orca（pid N）正在使用这个数据目录"时，同一个数据目录已经有连接器在运行（例如服务和手工 `run` 同时启动），先停掉其中一个；进程已经不在时连接器会自动接管。提示锁文件 `run/kiln-orca.pid` 的内容无效，或者 pid 被别的进程复用、连接器没在运行却一直提示时，确认没有 kiln-orca 在使用这个数据目录后删除这个文件。

**仓库开启审计**：加 `.kiln/agent/config.yml`，审计要求写在 `.kiln/agent/review.md`（字段见 PLAN #26.1）：

```yaml
version: 1
requirements:
  services: [pg, redis]  # 可选
review:
  agent: codex  # codex（默认）| claude-code；也可以写列表 [codex, claude-code]：审计 AI 开发的 PR 时用第一个没有开发过它的
  required_platforms:
    - { os: macos, arch: arm64 }
  # timeout: 45m  # 默认 45m，最长 3h
  # max_concurrency: 1
```

`required_platforms` 的每一项审计一次（一个任务，PLAN #26.1），支持 `macos/arm64`、`linux/amd64`、`linux/arm64`：写单个平台（`{ os: linux, arch: amd64 }`）时只派给这个平台的审计节点，写几项就在几个平台上各审计一次。**让几台不同平台的审计机分担审计**（哪台空闲就派给哪台、只审计一次）时写成一组：

```yaml
review:
  required_platforms:
    - id: any                # 一组必须写 id：任务名是 review-any，Check Run 显示 <agent> · any
      any_of:
        - { os: macos, arch: arm64 }
        - { os: linux, arch: amd64 }
```

组内任一平台的节点先来领取就由它执行，Dashboard、Check Run 和 PR review 末尾写明这次在哪个平台、哪个节点上执行；还没有领取时等待原因列出这一组平台。只有一个平台的节点在线时任务照样会被它领走；组内平台都没有在线的审计节点时任务一直等待。声明了 `requirements.tools` 时 lockfile 要有组内每个平台的条目（见下）。

PR 打开、推送、重新打开、转为 ready 时创建 AI 审计请求（Check Run 名称仍为 `ai-review`，在 Dashboard 的"AI 请求"中查看）。`review.md` 从 PR 的 base 读取，修改合入后才生效。开启 AI 开发见 #8.3。

**git 凭据**（PLAN #33.13）：节点不用这台机器上的 git 凭据。凭据由 Kiln Server 按操作签发、用后吊销：连接器准备仓库、推 attempt 分支，Agent 经 `kiln-orca task exec` 的每一个 git 传输，各自拿到一枚只覆盖所访问的那一个仓库的 GitHub App installation token（读：任务仓库或下面声明的依赖仓库之一；写：只有任务仓库，只签给开发），操作结束即吊销；连接器自己的网络 git 不读 `~/.gitconfig` 和系统配置（其中的凭据 helper、`insteadOf`、`http.proxy` 都不生效；要代理时给连接器服务设置 `HTTPS_PROXY` 等环境变量，见上面的代理表）。Agent 拉同账号的私有依赖（主要是 Go module）时，在仓库的 `.kiln/agent/config.yml` 中声明，必须与任务仓库在同一个 GitHub App installation 中：

```yaml
requirements:
  repositories: [acme/lib, acme/proto]   # owner/name，与 workflow 的 repositories 写法相同
```

审计读 PR base 上的声明，开发读默认分支上的；不在同一个 installation 中时任务以 `infra.credential` 失败，原因写明是哪个仓库。Agent 的网络 git 和拉依赖的构建命令都经 `kiln-orca task exec` 运行（任务说明中写明）：它对任务仓库所在的主机换上连接器的 credential helper，声明的依赖仓库加进 `GOPRIVATE`（仓库名不区分大小写：声明 `Acme/Lib` 时 go.mod 中写 `github.com/acme/lib` 同样按私有模块获取）。clone 的 origin 是任务仓库的真实地址，Agent 照常 `kiln-orca task exec -- git fetch origin`。不经 `task exec` 的网络 git（例如 IDE 的后台同步）在这个 clone 上访问 origin 时，不会用到个人的凭据 helper（clone 对任务仓库主机的 helper 只回答"退出"，不会落到钥匙串等）和全局配置中的个人请求头（clone 在任务仓库的地址上把 `http.<地址>.extraHeader` 置为空值）；netrc、进程自己环境中的认证（`GIT_CONFIG_*`、`git -c`）和子模块、依赖仓库的地址不在这层保护的范围内。

**部署前提：执行机器上不放 GitHub 的个人 git 凭据**——netrc 中没有 `github.com` 或 `default`，全局、系统 git 配置中没有对 GitHub 生效的 `http.extraHeader`。`kiln-orca doctor` 以服务用户的环境检查这两项（有则这一项失败，列出配置键和来源，不输出值）；连接器启动时做同样的检查，有则记 warn（不停止领取）。检查的范围：netrc（`NETRC` 指定的文件、`~/.netrc`、账户主目录下的 `.netrc`）中的 `github.com` 或 `default`；对 `https://github.com/` 生效的 `http.extraHeader`（全局、系统配置，以及 doctor 或连接器进程自己环境中的 `GIT_CONFIG_*`）；全局配置（git 实际读的每一份：`GIT_CONFIG_GLOBAL` 指定的文件，或者 `$XDG_CONFIG_HOME/git/config`（默认 `~/.config/git/config`）与 `~/.gitconfig` 两份）、系统配置（`GIT_CONFIG_NOSYSTEM` 按 git 的布尔语义：只有 true / yes / on 和非零整数才不查，`0`、空值、git 不认的值都照常查；含 include 的文件）中只对 GitHub 的某个组织或仓库声明的 `http.<地址>.extraHeader`（主机可以带 `*`），来源写声明它的具体文件。不检查：任何仓库的本地配置（包括按仓库生效的 `includeIf "gitdir:…"`、`hasconfig:`），进程环境、`git -c` 中只对某个组织或仓库声明的请求头，凭据 helper（clone 中已经对任务仓库主机关掉）。

- **执行机器上有 GitHub 的 netrc 或认证头时**：`task exec` 中的每个 git HTTP(S) 传输开始前检查本次生效的认证配置——地址中带用户名或密码、对这个地址生效的 `http.<地址>.extraHeader`（全局、本地、环境变量或命令行配置中，按 git 实际生效的值判断：clone 中任务仓库地址上的空值使不比它具体的请求头对 origin 不生效）、netrc（`NETRC` 指定的文件、`~/.netrc`、账户主目录下的 `.netrc`，Windows 另有 `_netrc`）中有这个主机或 `default`，或者查不了（配置文件损坏、netrc 读不了）。有任何一项就拒绝这一次传输、不签发：git 报错，Agent 的输出和连接器的 warn 日志写明 attempt、配置键和能确定的来源（不输出值），节点照常领取。处理：删掉这些配置中 GitHub 的条目（或改成只对别的主机生效），不用重启连接器，下一次传输就正常。netrc 只要文本中出现这个主机名就算（宏和值中碰巧出现也算），这是有意的保守判断。
- **查凭据有没有回收**：连接器日志和任务日志中是签发与关闭请求的配对（"签发 git 凭据""已请求 Server 关闭 git 操作、回收凭据"，带 op 和 token 指纹）；关闭请求成功只说明 Server 已经关闭了操作，GitHub 侧没能吊销时 Server 日志中有带同一指纹和到期时间的 warn（不重试，到期自然失效），按指纹在 Server 日志中查。CI 的 job 日志相同（"git 凭据：已请求 Server 关闭操作、回收 token"）。
- **macOS 上数据目录的路径长度**：连接器与 `task` 命令之间是数据目录中的 unix socket，路径上限 macOS 103 字节、Linux 107 字节。数据目录太长时连接器启动（`kiln-orca run`）和 `kiln-orca task …` 报"unix socket 路径 … 超过这个平台的上限"，用更短的 `--home` 或 `KILN_ORCA_HOME`（默认的 `~/Library/Application Support/kiln-orca` 在常见的用户名下不超过上限）。这些是护栏，不是安全边界：Agent 与连接器是同一个用户、完全权限，在有你自己凭据的机器上（`gh` 登录、SSH key、钥匙串中的 git 凭据），Agent 照样能读到它们；要隔离就用没有这些凭据的用户或机器运行节点。

**受管理的工具链**（PLAN #33.12）：任务要用固定版本的 Go、Node、pnpm 时，在 `requirements` 中声明，版本和摘要由 `.kiln/runtimes.lock` 决定（与 workflow 的 `runtimes` 共用）：

```yaml
requirements:
  tools: [go, node, pnpm]   # 内置 runtime：go、node、pnpm、bun；顺序是 PATH 中的先后
```

1. 先把 AI 执行节点升级到带 kiln-runner 的 kiln-orca（上面的升级命令），`kiln-orca doctor` 的 kiln-runner 一项是 ✓。没有升级的节点（或者这个平台没有 kiln-runner 包）不会领到声明了工具链的任务，等待原因写明；别的任务不受影响。
2. `kiln runtime lock` 重新生成 lockfile（会锁定审计的 `required_platforms` 和开发的 `development.platform`（不写时 macOS arm64），一组平台时组内每一个），`kiln lint` 检查，提交。审计或开发加了 Linux 平台时先重新生成：缺少某个平台的条目时审计请求以配置错误失败、开发不规划，原因写明是哪个平台，不会换版本。
3. 节点在准备阶段把工具装到数据目录的 `toolchains/<digest>/`（已装好的直接复用；30 天没用到或超过 5 GiB 时回收），任务日志中写明每个工具是复用还是下载。Agent 用任务说明中给出的 `kiln-orca task exec --attempt <id> -- <命令>` 运行构建和测试（PATH 最前面是这些工具，`GOTOOLCHAIN=local`），不用本机安装的版本。准备失败时任务以 `infra.toolchain` 失败。

Kiln 自己的仓库就是这样配置的：`.kiln/agent/config.yml` 声明 `tools: [go, node, pnpm]`，`runtimes.lock` 中有 macOS arm64 的 go、node、pnpm。

## 6. 第一个 workflow

```yaml
# .kiln/workflows/ci.yml
name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: darwin/arm64  # host executor
    runtimes: [go]
    network: true
    toolcache:
      - { name: gomod, env: GOMODCACHE }
      - { name: gocache, env: GOCACHE }
    steps:
      - run: go test ./...
```

```bash
kiln runtime lock  # 按 go.mod、package.json 生成 .kiln/runtimes.lock（需联网；monorepo 见 PLAN #12.3）
kiln lint  # 本地检查 workflow（和 .kiln/merge.yml）
git add .kiln && git commit -m "kiln" && git push
```

要点（PLAN #4）：

- 只认 `.kiln/workflows/*.yml`，不支持 `uses:`；表达式只能用 `github`、`matrix`、`inputs`、`vars`、`secrets`、`kiln`，`secrets` 只能用在 `env` 和发布凭据字段中。
- job 只看到 `inputs`（默认整个仓库，不含 `.kiln/workflows/`；CI 中跑 `kiln lint` 要写 `inputs: ["**"]`），工作区没有 `.git`，版本用 `${{ github.sha }}` 传入。
- 默认 `network: false`，下载依赖要写 `network: true`。
- 工作区没有 `.git`；要本仓库的历史时在 step 中补取：`git init -q && git fetch https://github.com/<owner>/<repo> <ref>`（不用声明）。私有依赖仓库写在 job 的 `repositories: [owner/name]` 中，step 里照常 `git fetch` / `go mod download`（Go 还要在 `env` 中设 `GOPRIVATE`）。每次 git 传输由 Runner 按需签发这一个仓库的只读 token、传输结束即吊销，不写任何凭据文件（PLAN #8.4）；不经系统 git 的 HTTP 客户端拿不到凭据。`container:` 的 job 只在 Linux 上的 Runner 支持这一点（macOS 上的 Docker Desktop / colima 不支持：声明了 `repositories` 的 job 失败，其他 job 照常运行，只是 git 要凭据时报错；远端的 Docker 不能挂载宿主机路径，与工作区一样不支持，容器启动失败）；镜像中要有 git 和 POSIX sh、`cksum`、`cut`、`ln`、`mkdir`、`mv`、`rm`（常见的 Debian、Alpine 镜像都有），缺少时同样按不可用处理。step 中途改 `PATH` 换用另一个 git、用 `git -c` 或自己改 git 的环境变量时不保证经过 Kiln 的凭据（PLAN #8.4）。Runner 拉取源码（缓存缺失时）时，只读 token 在那次 `git fetch` 及其远程助手的进程环境中，同一用户或 root 能读到，fetch 结束即吊销。
- job 间传文件用 `outputs` + `fetch`（12 小时后清理）；`toolcache` 持久保存 Go module / build cache 等。
- runtime（`runtimes: [go]` 等）由 Runner 下载安装并按版本复用（PLAN #12.8）。pnpm 12 起按平台发行原生程序，`kiln runtime lock` 为它写按平台的条目（如 `"linux/arm64"`）；这样的条目要由包含 PLAN #12.8 的 kiln-runner 安装，旧版 Runner 会下载到 npm 上的包装包，以"sha256 与 lockfile 不一致"（`infra.runtime`）失败。**先把所有会领到这些 job 的 Runner 升级（#5.5），再在 workflow 中改用 `runtimes: [pnpm]`**；在此之前照常在 step 中安装 pnpm。Node 容器中用 pnpm runtime 时，包存储可以放进 toolcache：`toolcache: [{ name: pnpm-store, env: PNPM_CONFIG_STORE_DIR }]`。
- 在 Docker 中执行：`runs-on: linux/arm64` 加 `container: <镜像>@sha256:…`（必须固定 digest，镜像带 shell，参考 [ci.yml](../.kiln/workflows/ci.yml)）；`resources: { cpu: 2, memory: 4Gi }` 只限制主容器；不挂载 Docker socket，Dockerfile 用 `buildkit: true`（#7.1）。

## 7. Secret、变量、Environment、发布

分账号级、仓库级、environment 级，在**设置**（`/settings/scope?account=<账号>` 或 `?repo=<owner/name>`，加 `&env=<名称>`）或 CLI 中配置。secret 只写不读，编辑时留空保持原值。同名的仓库级 environment **整体覆盖**账号级的（不继承其变量和 secret）。

```bash
echo -n 'xxx' | kiln secret set API_TOKEN --repo owner/myapp  # 值从标准输入读
kiln var set DEPLOY_HOST build.example.com --repo owner/myapp
kiln env set release --repo owner/myapp --ref 'refs/tags/v*'  # 只允许 v* tag 使用
kiln secret list --repo owner/myapp
```

`kiln env set` 整体替换，没写的 `--ref`、`--publish-to`（允许发布到另一个仓库 `owner/name`）会被清空。job 写 `environment: release`，`env:` 中用 `${{ vars.X }}`、`${{ secrets.X }}`。

**发布**：只有推送 tag 才发布（人推送，或合并后自动打，#8.2）。

```yaml
# .kiln/workflows/release.yml
name: release
on:
  push:
    tags: ["v*"]
jobs:
  build:
    runs-on: darwin/arm64
    runtimes: [go]
    network: true
    strategy:
      matrix:
        include: [{ os: darwin, arch: arm64 }, { os: linux, arch: amd64 }]
    env: { GOOS: "${{ matrix.os }}", GOARCH: "${{ matrix.arch }}", CGO_ENABLED: "0" }
    steps:
      - run: go build -trimpath -ldflags "-X main.version=${{ github.ref_name }}" -o dist/myapp .
    outputs:
      bin: dist/
  publish:
    needs: [build]
    environment: release
    release:
      assets:  # 打包一次，各目标共用（PLAN #13.4）；默认 .tar.gz，archive: zip | none
        - { from: build.bin, name: "myapp-${{ github.ref_name }}-${{ matrix.os }}-${{ matrix.arch }}" }
      github: {}  # 本仓库的 GitHub Release
```

其他目标：`github: { repository: owner/myapp-public, notes: { from: CHANGELOG.md } }` 发到另一个仓库（environment 要 `--publish-to`）；`uploads: [{ url: …, token: "${{ secrets.DL_TOKEN }}" }]`（[UPLOAD-PROTOCOL.md](UPLOAD-PROTOCOL.md)）；`npm: [{ from: build.bin, name: "myapp-${{ matrix.os }}-${{ matrix.arch }}", access: public }]` 发布为 `@<scope>/myapp-<os>-<arch>`。

```bash
git tag v1.0.0 && git push origin v1.0.0
```

所有目标暂存成功后才正式发布，重跑不会重复发布；Bot 触发的 run 不发布。同一 tag 已有别的正式 Release 时失败（先手动删除）；目标已有同版本但内容不同时失败（只能发新版本）。

**npm 账号级配置**（`release.npm` 用；Dashboard：设置 → 连接与发布凭据）：

```bash
kiln npm set --account your-github-login --scope @your-scope  # token 从标准输入读（粘贴后 Ctrl-D）；--registry 默认 https://registry.npmjs.org/
kiln npm list  # 确认 scope、registry；token 不显示
kiln npm delete --account your-github-login
```

token 用 npmjs.com 的 granular access token：对该 scope 读写，勾选 bypass 2FA；到期前重新 `kiln npm set`。

### 7.1 构建并发布容器镜像（Dockerfile + Docker Hub）

`buildkit: true` 的 job 在 Runner 上构建 OCI layout，由 **Server** 推送，不用 `docker login`（PLAN #13.8）；只支持公开的基础镜像。

**1. Runner 开启 buildkit**（Linux Runner，一次）：`config.toml` 加 `buildkit = true`，重启，`kiln-runner doctor` 的 executor 一行应有 `buildkit`。

**2. 凭据**：Docker Hub 建 PAT（Read & Write），存入账号级 environment `docker-release`（所有仓库共用）：**设置** → Docker Hub 卡片 →"配置 Docker Hub"（environment 不存在时按 `refs/tags/v*` 创建），或：

```bash
kiln env set docker-release --account your-github-login --ref 'refs/tags/v*'
kiln var set DOCKERHUB_USERNAME <Docker Hub 用户名> --account your-github-login --env docker-release  # PAT 所属账号
kiln var set DOCKERHUB_NAMESPACE <Docker Hub 命名空间> --account your-github-login --env docker-release  # 用户名或组织名
kiln secret set DOCKERHUB_TOKEN --account your-github-login --env docker-release < /private/path/dockerhub-pat.txt  # 只能在 release.images.password 中引用
```

**3. workflow**（完整例子见 [docker.yml](../.kiln/workflows/docker.yml)；上游矩阵合并为多平台 index）：

```yaml
  image:
    runs-on: linux/arm64
    container: docker.io/library/golang@sha256:…  # 只需要 sh；buildctl 由 Kiln 挂载
    buildkit: true
    network: true
    env: { VERSION: "${{ github.ref_name }}" }
    steps:
      - run: |
          buildctl build --progress plain --frontend dockerfile.v0 \
            --local context=. --local dockerfile=deploy \
            --opt platform=linux/arm64 --opt build-arg:VERSION="$VERSION" \
            --output type=oci,dest=out/image,tar=false
    outputs: { image: out/image/ }  # OCI image layout 目录
  publish:
    needs: [image]
    environment: docker-release
    release:
      images:
        - from: image.image
          name: docker.io/${{ vars.DOCKERHUB_NAMESPACE }}/myapp
          tags: ["${{ github.ref_name }}"]  # 不可变：已指向其他 digest 时失败
          floating-tags: [latest]  # 只在本版本是 Kiln 发布过的最大正式版时移动
          username: ${{ vars.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}
          sign: none  # 只能是 none
```

### 7.2 Kiln 自己的镜像：镜像名与隔离验收

Kiln 的镜像发布到 Docker Hub：`docker.io/hewenyulucky/kilnflow`（本仓库，`linux/arm64`、`linux/amd64`）。只用于 Kiln 自己发版，见仓库的 `.kiln/workflows/docker.yml` 与 `deploy/`；设计见 [PLAN #21.1](PLAN.md)（路由到 Outline `06 [K-GH-REL]`）。

凭据和 #7.1 相同，放在账号级 environment `docker-release` 中（`DOCKERHUB_USERNAME`、`DOCKERHUB_NAMESPACE`、`DOCKERHUB_TOKEN`），只是命名空间是 `hewenyulucky`、镜像名是 `kilnflow`。镜像是公开的，拉取不需要登录。

### 7.3 Kiln 自己的二进制：release.yml 与 environment `release`

只用于 Kiln 自己发版，见仓库的 `.kiln/workflows/` 与 `deploy/`；设计见 [PLAN #21.1](PLAN.md)（路由到 Outline `06 [K-GH-REL]`）。

### 7.4 macOS App 签名与公证

Runner 要有 Xcode 或 Command Line Tools；证书和公证凭据只在 job 期间导入临时 keychain（PLAN #8.10）。

**1. 导出证书**：钥匙串访问中右键"Developer ID Application: …"（含私钥）导出带证书链的 .p12 并设密码。公证用 App 专用密码（appleid.apple.com）或 App Store Connect API key（.p8）。

**2. 存入 Kiln**（推荐仓库级 environment + tag ref 策略；PR 也要签名时放仓库级，或 ref 策略加 `refs/pull/*`）：

```bash
kiln env set macos-release --repo owner/myapp --ref 'refs/tags/v*'
base64 -i DeveloperID.p12 | kiln secret set DEVELOPER_ID_P12 --repo owner/myapp --env macos-release
kiln secret set DEVELOPER_ID_P12_PASSWORD --repo owner/myapp --env macos-release  # 交互输入 .p12 的密码
kiln secret set NOTARY_PASSWORD --repo owner/myapp --env macos-release  # App 专用密码
kiln var set APPLE_ID you@example.com --repo owner/myapp
# 可选：MAC_PROFILE（base64 -i MyApp.provisionprofile）；ASC_KEY（< AuthKey_XXXXXXXXXX.p8，代替 Apple ID），同样 --env macos-release
```

**3. workflow**（例子见 PLAN #8.10）：job 写 `environment: macos-release`、`network: true`、`cache: false`，`signing.certificates` 填 `p12`、`password`，`signing.notary` 填 `profile`（如 `example-notary`）、`apple-id`、`team-id`、`password`。step 中有 `KILN_KEYCHAIN`（临时 keychain 路径）和 `KILN_PROVISIONING_PROFILES`（已装 profile 的路径，`:` 分隔）。`xcrun notarytool --keychain-profile <名字>` 会先用登录 keychain 中的同名 profile，要加 `--keychain "$KILN_KEYCHAIN"` 或用本机没有的名字。引用 secret 的 Mac job 独占 Runner。

## 8. 日常操作

| 要做的事 | Dashboard | CLI |
|---|---|---|
| 看最近的 run | 运行 | `kiln runs [--repo R]`、`kiln run <run>` |
| 看日志、下载产物 | job 页面 | `kiln logs <job> -f` |
| 取消 | run 页面 | `kiln cancel <run>` |
| 重跑未成功的 / 全部 / 不用缓存 | run 页面"重跑"；或 Check Run 的 "Re-run" | `kiln rerun <run> --mode failed\|all\|all_no_cache` |
| 重跑一个 job 及其下游 | job 页面"重跑这个 job…" | `kiln rerun-job <job>` |
| 手动触发（需要 `on: workflow_dispatch`） | 运行 →"启动构建"（`/runs/new`） | `kiln dispatch owner/repo ci.yml --ref main [-i key=value]` |
| 手动触发 AI 审计 | — | `kiln ai review owner/repo [--ref REF]` |
| 看 AI 审计、开发的请求和任务 | AI 请求（`/ai/requests`，任务在 `/ai/tasks/<id>`） | `kiln ai requests [--repo R] [--type review\|develop] [--state S]`、`kiln ai show <请求>`、`kiln ai log <任务> [-f]` |
| 取消、重跑 AI 审计 | AI 请求页面"取消""重跑"，任务页面"重跑这个任务"；或 Check Run 的 "Re-run" | `kiln ai cancel <请求>`、`kiln ai rerun <请求> [--mode failed\|all]` |
| 看 AI 执行节点（Orca、Agent、Docker 状态，并发） | AI 执行节点（`/ai/runners`） | `kiln ai runners` |
| AI 开发（#8.3） | AI 开发 | `kiln ai dev sync\|pause\|resume\|list\|stop\|retry` |
| 让仓库缓存全部失效 | — | `kiln cache bump owner/repo` |
| GitHub 请求失败（Check Run 没更新） | 设置 → 运维（outbox） | `kiln outbox --state dead`、`kiln outbox retry <id>` |

**界面语言**：Dashboard 支持简体中文和英文，不需要在 Server 上配置。第一次打开时按浏览器的语言偏好（中文为简体中文，英文为英文，都不是时为简体中文）；在头像菜单的"语言"（未登录时在页面右上角）中切换，选择保存在这个浏览器中，刷新、重新登录后保持。CLI 和 GitHub 上的 Check Run、PR review、评论不受影响。

### 8.1 合并队列

PR 加 label `kiln:merge`（或 run 页"加入合并队列"、`kiln merge-queue add owner/repo <PR>`）入队，验证通过后由 App 合并（落后时先更新分支、重跑 CI）并关闭 `Closes #N` 的 issue（PLAN #28），不需要分支保护。

验证失败、冲突、新 commit、转为 draft 时移出并在 PR 上评论原因，处理后重新加 label。手动移出：去掉 label 或 `kiln merge-queue remove owner/repo <PR>`。状态：`/merge-queue?repo=owner/name` 或 `kiln merge-queue list owner/repo`。

配置在**默认分支**的 `.kiln/merge.yml`（PR 中改它不影响该 PR；`kiln lint` 会检查）：

```yaml
method: squash  # squash（默认）| merge | rebase
required:  # 必须存在并成功的验证：<workflow> 或 <workflow>/<job>（不带 .yml）；ai-review 是 AI 审计
  - ci/test
  - ai-review
auto: true  # 可选：验证全部通过后自动入队；要求 required 中有 ai-review
branches: [main, dev]  # 可选：允许合并到的目标分支，不写时只允许仓库的默认分支
tag:  # 可选：合并后自动打 tag（#8.2）
  prefix: v  # 默认 v
  initial: 0.1.0  # 还没有版本 tag 时的第一个版本
  branch: main  # 可选：发版分支，不写时是合并那一刻的默认分支
```

- `branches` 允许合并到多个分支（最多 64 条，合法的分支名，不能重复）；每条分支各自排队，目标是其他分支的 PR 会被移出并评论。不写时只允许默认分支；**不要写空列表或 `null`**（配置错误）。`kiln lint` 会检查 `required` 中的 workflow 的 `pull_request` 分支过滤覆盖每一条。
- 开启 `auto` 后，不想自动入队的 PR 加 `kiln:hold`；`auto` 只对 `branches` 中的分支生效。被移出过的 head 不再自动入队（推新 commit 或手动加 `kiln:merge`）。
- 关闭 issue 需要 **Issues: Read and write**（#3）；没有时合并照常完成，加上权限后自动补上（合并 24 小时后仍失败则放弃）。

### 8.2 合并后自动打 tag

`.kiln/merge.yml` 有 `tag:` 时，PR 合入发版分支（`tag.branch`，不写时是合并那一刻的默认分支）后 Kiln 打下一个版本 tag 并规划发布（PLAN #29）：合入其他分支（`branches` 中的 dev 等）的合并跳过、不打 tag，同一个 commit 随后从发版分支合入（fast-forward）时会重新判断并打 tag；默认升 patch，PR label `release:minor` / `release:major` 升 minor / major，`release:skip` 不打。直接推送的 commit 不打；最大版本 tag 不在该 commit 历史中时跳过。人的合并总是打；机器人（AI 开发合并 PR 用的 Bot）合并时只有可信的 AI 开发实现 PR 才打——设计 PR、归属尚未确认的 PR 和其他 PR 跳过并写明原因，可信的实现 PR 照常执行发布 job。记录在合并队列页（显示发起者、机器人标记、工作项和原因，可重试），或 `kiln autotag list owner/repo [-n 50]`、`kiln autotag retry owner/repo <sha>`。

### 8.3 AI 开发

开发角色的 AI 执行节点从人加了 `kiln:ready` 的 Issue 中选题、领取、在 Orca 中开发，Kiln 核对后推进分支 `kiln/dev-<工作项>` 并开 PR；PR 照常经过 CI、另一个 Agent 的 AI 审计和合并队列，没通过时同一个工作项返修（PLAN #26.7–#26.16）。设计由 Agent 自己完成：默认先设计再开发、一个 PR 完成；它判断一个 PR 完成不了时先开设计 PR（分支 `kiln/design-<工作项>`，标题"设计："，只改 Markdown），合并后 Kiln 自动开始实现（PLAN #31）。前提：一个开发角色的 AI 执行节点（#5.6），App 订阅了 **Issues** 事件。

在**默认分支**的 `.kiln/agent/config.yml` 加 `development` 段（开发配置和说明只从默认分支读，PR 改不了）：

```yaml
development:
  agent: claude-code  # 必填：codex | claude-code
  dry_run: true       # 建议先试运行：只选题、写计划，不领取、不写代码（计划在选题 job 的详情页查看）
  # instructions: .kiln/agent/development.md  # 默认此路径，给开发 Agent 的说明
  # timeout: 3h        # 单次执行，10m–12h
  # max_fix_rounds: 3  # 自动返修轮数，1–10
  # max_issues: 5      # 一个 Agent 一次最多领取的 Issue 数（一个 PR），1–10，具体几个由 Agent 判断
  # platform: { os: macos, arch: arm64 }   # 开发机的平台，不写时就是它；可以写 linux/amd64、linux/arm64，或者一组：
  # platform:
  #   any_of: [ { os: macos, arch: arm64 }, { os: linux, arch: amd64 } ]
```

- 审计默认开启，仍要写 `review.required_platforms`；只开发不审计时写 `review: { enabled: false }`。
- 平台：`platform` 写法和规则与审计的 `required_platforms` 相同（单个平台或 `any_of` 一组，不写 `id`），支持 macOS arm64、Linux amd64、Linux arm64（Linux 可以配置，实机验证进行中）。几台不同平台的开发机分担开发时写一组：选题、实现、返修的每次执行由平台在组内、有空闲容量的开发机先领到就执行，返修、实现可以由另一台接着做（每次都从远端的分支重新开始，上一轮的 Orca 工作区在那一轮结束后已经回收）。声明了 `requirements.tools` 时 lockfile 要有每个平台的条目（#5.6 的受管理的工具链）。改了 `platform` 后同步一次任务池（Kiln 记下了上次读到的平台，空闲的开发机按它判断）。
- 任务池：带 `kiln:ready` 的打开的 Issue 由 Issues webhook 送进 Kiln（App 要订阅 Issues 事件）；webhook 丢失时在 Dashboard 的 AI 开发页（选一个仓库）点"同步任务池"，或 `kiln ai dev sync owner/repo`，从 GitHub 全量同步。可以批量打 label：Runner 准备好后才取候选，任务池刚有变化时会等它安静。
- 调度：开发节点有空闲容量时，Server 为任务池有可选 Issue 的仓库规划一次选题，Agent 从中选一批（最多 `max_issues` 个，合成一个 PR）。**同一个仓库按工作项串行**：一个工作项从领取到合并或终止（包括等待 CI / 审计 / 合并、返修、受阻、`kiln:hold`、设计合并后的实现）都占着仓库，结束后才从最新的默认分支开始下一个；等待期间不占开发节点的容量，节点去做别的仓库，不同仓库并行的数量取决于开发节点的容量（`max_tasks` 之和）。AI 开发页（选一个仓库）和 `kiln ai dev list owner/repo` 显示占用者和它在等什么。任务池没有变化时不再选（失败、`dry_run`、选不出都不会反复空跑），同步后会重新判断。
- 升级到这一版时，同一个仓库已有多个未结束的工作项（以前允许并行）：不再选题，编号最小的是占用者；其余的已有 PR 照常验证、返修、合并，只是不开始新的阶段（例如设计合并后的实现），原因写着在等谁。不需要人处理；想加快收敛可以合并或 `kiln ai dev stop` 其中的一些。
- 合并冲突：合并队列更新 PR 分支时 GitHub 确认冲突，PR 是 Kiln AI 开发的工作项的 PR（按 Kiln 记录的工作项与 PR 编号判断，不看作者和分支名）时自动交回 AI 开发：PR 移出队列并评论一次，开发节点把目标分支合并进来解决冲突、更新原 PR，新的 commit 经过 CI 和 AI 审计后按合并策略重新入队，不需要重新打 label 或 retry。冲突返修与审计返修分开计轮数（上限都是 `max_fix_rounds`），用完时受阻。别人的 PR 冲突时只移出队列、评论请提交者解决，不派开发任务；GitHub 还没算出可合并状态、没有权限等不算冲突。
- `kiln:skip` 排除 Issue；Agent 判断现在不适合、不用改代码或被人终止的 Issue 不再进入候选，直到内容变了或者重新加 `kiln:ready`（先去掉再加）；只是没放进这一批的留在任务池中。Issue 上的 `kiln:claimed`、`kiln:pr-open`、`kiln:blocked` 由 Kiln 维护。
- 完整解决的 Issue 在 PR 描述中写 `Fixes #N`，合并后关闭；部分解决的写 `Refs #N`。设计 PR 一律写 `Refs`，并加 `release:skip`（合并后不打版本 tag）。
- Agent 有疑问的决定不等人确认，写在 PR 描述的"决策记录（Agent 自报）"中供复盘；只有要改已确认的决定（PLAN #1.4）时 PR 加 `kiln:hold`，确认后去掉 label 或手动入队。
- Issue 依赖别的 Issue 时 Agent 写"等待 #N"（任务池显示"等待依赖"），#N 关闭后自动回到候选；webhook 丢了时同步任务池补上。
- 改动了 `.kiln/`、`.github/`、`AGENTS.md`、`CLAUDE.md`、`.claude/`、`.codex/`，或提交说明中的关闭关键字引用了别的 Issue 时，PR 加 `kiln:hold`（不自动入队），原因写在 PR 描述中；确认后去掉 label。label 还没加上时（例如加 label 失败、等待重试）这个 PR 同样不自动入队。
- 受阻（返修轮数用完、交付核对不通过、执行失败、Agent 报告受阻）时在 Issue 或 PR 上说明，`kiln ai dev retry <ID>` 再返修一轮，`kiln ai dev stop <ID>` 终止（释放 Issue，PR 保持原样）。部分完成的 PR 合并后工作项同样受阻（原因写还缺什么，例如要人提供的凭据），条件具备后 `retry` 从最新的默认分支、在新分支 `kiln/dev-<工作项>-r<轮次>` 上开始下一轮实现。
- `kiln ai dev pause owner/repo` 停止新的领取（已有的工作项照常收尾），`resume` 恢复；`kiln ai dev list [owner/repo] [-n 50]` 看工作项。
- 评论至多发一次：结果不明的记为 `unknown`，不自动重发。attempt 分支和 checkpoint 12 小时后删除。

### 8.4 外部设计文档（Outline）

把仓库关联到一个 Outline 文档集后，AI 开发（首次开发 / 设计、实现、返修）与审计在**规划任务时**把该文档集冻结成不可变快照，Agent 经 `kiln-orca task doc` 读取；执行机器上不需要任何外部文档凭据。设计见 [PLAN.md](PLAN.md) 路由到的 Outline `09 [K-DOC]`。需要 Outline 的 **API token**（Outline「设置 → API 和访问权限 → 创建 API 密钥」，可留空作用域即完全访问）。

```bash
# 绑定：token 从标准输入读取（加密保存，只写不读）。collection 是 Outline 文档集的 id（Collection 的 UUID，不是地址里的短 id）
kiln doc bind owner/repo --base-url https://outline.example.com --collection <collection-uuid> --name 设计
kiln doc list                 # 列出绑定（不含 token）
kiln doc unbind owner/repo    # 解绑
```

- **地址**：`--base-url` 除本机回环地址（`localhost`、`127.0.0.1`、`::1` 等）外必须使用 `https://`，`http://` 只用于本机自部署调试；地址不能包含用户名、密码、查询参数或片段，也不能有控制字符。
- **在 Dashboard 中查看**：设置 → 仓库，每个仓库（包括已停用的）都有“外部文档”入口。页面只读展示提供方、站点、文档集名称与链接、配置时间和操作者、当前是否已绑定，以及「待决策清单」的配置；token 和密文不会返回浏览器。**保存配置不等于连接正常**：绑定后状态是「已保存 · 未验证」，管理员点“验证读取”才做一次只读检查（列出文档集与文档、抽查一篇最小 id 的文档正文），结果分为「已验证可读取」「已验证 · 空集合」「读取失败」，失败显示固定文案（凭据被拒、无权限、文档集或抽查文档不存在、限流、上游错误、响应不合法、网络或超时）和错误代码，不显示 Outline 返回的原始信息。验证只检查只读绑定，**不测试待决策清单的写 token**。普通页面加载、每 5 秒自动刷新和查看快照正文都不访问 Outline；只有管理员显式点“验证读取”时才访问，一次验证最多 60 秒（查库与外部调用共用扣除 10 秒落库预算后的 50 秒，结论再从同一份总预算派生、最多 10 秒落库，所以总耗时不会超过 60 秒，超时也会显示为「读取失败 / 超时」而不是停留在旧状态；落库被数据库锁挡住时也按这份总预算取消，不会另起一段完整预算）。
- **AI 工作项 / 请求 / 任务是另一份数据**：它们的页面上展示的是冻结时保存的快照（冻结时间、来源、文档标题、修订号、指纹、原文链接），修改 Outline 或重新绑定**不会**改变历史依据；历史快照没有记录来源时明确标注，不按当前仓库绑定补造。正文默认不加载，点“查看正文”才单独读取那一篇（已读过的会缓存）；正文按保留策略清理后仍显示元数据并注明已清理，不会恢复旧正文。工作项页面还列出关联的开发、返修和审计任务，以及每个任务与**当前工作项**冻结依据的关系（同一份、文档引用一致、领取后关联、迁移前核对一致、没有依据、不一致、无法核对）。指向另一个工作项的任务标为不一致，不会被当成同一份依据。迁移前请求族在重跑时固化依据后，原旧任务仍按固化的依据展示和读取正文。

- **何时冻结**：仓库有绑定时，第一次「选题领取后开始开发 / 设计」、后续「实现」「返修」都会冻结（按工作项只冻结一次，开发与审计读到同一份）；不是 AI 开发 PR 的审计（人提交的 PR、手动全面审计）没有工作项，按**审计请求**冻结自己的快照集；没有绑定时不冻结，管理员之后绑定，在首次真正的开发 / 审计规划时生效。
- **Agent 读取**：prompt 会列出文档清单（标题 + 修订，按标题和文档 id 排序），Agent 用 `kiln-orca task doc list` / `kiln-orca task doc get <doc_id>` 读正文（经连接器向 Server 要快照）。
- **失败与重绑**：拉取失败（网络、token 失效、Outline 5xx、文档被移出集合或取消发布）时该次开发 / 审计失败、可稍后重试（不会带着空快照执行）。AI 开发规划遇到永久性的文档读取失败（外部文档不满足冻结条件，或 Outline 返回 400 / 401 / 403 / 404 / 422）时工作项转**受阻**并记录原因：修正文档或绑定后 `kiln ai dev retry <ID>` 继续；网络、超时、5xx、限流等临时失败仍自动重试，不会把工作项打成受阻。已经冻结的在途任务继续读旧快照，重新绑定 / 换新修订不影响它；要让之后的任务用新文档，重新 `bind` 即可。拉取期间如果绑定被改动（重绑、解绑），这次请求直接失败（依据已经不是同一份），下一个事件按新绑定重新冻结。
- **重跑与过期**：`kiln rerun` 固定使用原请求族的文档依据，不重新拉取文档；迁移前的请求没有记录依据（独立审计）或依据不完整时，要求发起一次新审计。正文已过保留期时，重跑只要有任务真正执行，就在**创建任务之前**以 "快照正文已过保留期清理"（FailedPrecondition）失败，需要发起新的审计（按当前文档重新冻结）；任务全部沿用已有结论（合并队列沿用）时不读取正文，照常创建。重跑与保留清理在同一个事务里用同一把咨询锁，重跑附着已有依据时不会与清理交错。
- **保留**：文档正文是过程数据，12 小时后清空，保留元数据与引用关系。任务再去读被清理的正文会收到 "快照正文已过保留期清理"（FailedPrecondition），列表仍能列出文档清单。
- **批量迁移文档与慢文档集**：一次冻结受 10 分钟总预算限制，正文按 8 篇并发读取；同一个去重键的规划在 Server 进程内串行，等待也计入这同一份预算。集合很大（数百篇以上）或 Outline 很慢时，每次重试都可能超时、任务一直失败。批量迁移文档要有可靠的维护窗口——**只 drain AI 执行节点不够**：drain 只停止派发任务，审计规划仍会在建任务之前冻结文档（PR 事件、手动审计都会触发），而且正在进行的冻结还没有落库，Dashboard 上也看不出"排队 / 执行中"。Outline Webhook 已经移除，更不能靠它隔离。可靠步骤：
  1. 停止 `kiln-server`（SIGTERM）：优雅退出会先停止接纳新的同步规划 / 文档冻结、取消并等待在途冻结结束，然后才关闭数据库。
  2. 在 Outline 执行批量迁移。
  3. 重启 `kiln-server`，对这些仓库发起一次审计、或让一个工作项首次冻结，确认整批能在预算内完成。

### 8.5 AI 开发决策同步到「待决策清单」文档（Outline）

AI 开发在交付报告里记录有疑问的决定。给仓库配置一篇专用的 Outline 文档（默认标题「待拍板」）后，Server 会把每次交付的决策**单向追加**到该文档的 `## 待拍板` 标题下（最新的在最上面），方便人集中查看、逐条拍板。它只是通知：不影响工作项状态，不改变 `kiln:hold` 的判定，也不会把文档里人的回复读回来。设计见 [PLAN.md](PLAN.md) 路由到的 Outline `09 [K-DOC]`。

**先做好 ACL 隔离**。写 token 的作用范围决定了最坏情况：它只能改这一篇文档。

1. 在 Outline 建一个**独立、非管理员**的 Member 账号（不要复用任何人的账号），只用于 Kiln 写入。需要给账号一个可登录的登录方式，但不要把它加进任何组。
2. 在 Outline 把只读设计文档集合（以及其它私有集合）逐个设为**私有**，并在集合和文档两级**移除**这个写账号（以及它可能所在的组）的授权——包括只读文档绑定用的集合，那里同样不该有它的访问权。
3. 新建一个**独立的私有集合**，在里面建一篇「待拍板」文档。
4. 只在这一篇文档上给写账号 `read_write` 授权。**不要**给集合级的写权限：集合级授权会让它以后能改这个集合里的任何文档。
5. 用这个账号创建 **API token**，作用域只保留 `documents.info` 和 `documents.update`。
6. **上线前验收**（两条都必须过）：
   - **越权被拒**：用写 token 对只读设计文档集合里的一篇真实文档调用 `documents.update`，必须返回 403（`authorization_error`）；对只读集合的任意文档调用 `documents.info` 同样应当被拒。任何一条返回成功都说明隔离没做到，先停下来修权限。
   - **marker 原样存活**：Kiln 写一次之后，在 Outline 网页里打开同一篇文档，手工编辑并保存（在条目附近改字即可），再用 `documents.info` 读回正文，确认行内代码里的 marker（形如 `` `kiln-dec:123:wi7:t2:0` ``）逐字未变——Outline 的网页编辑器如果把它拆成普通文本或多段代码，之后就无法再查证条目是否送达。
7. 验收写入链路：`kiln doc decision-doc set` → 跑一次 AI 开发交付（或等下一次）→ 在 Outline 网页里**手工编辑同一篇文档** → 下一次条目仍能写进去、且手工修改保留（说明冲突重试正常）；再 `kiln doc decision-sync list --repo owner/repo` 确认条目的是 `delivered`。

```bash
# 设置：token 从标准输入读取（加密保存，只写不读）。doc-id 是目标文档的 UUID
printf '%s' "$OUTLINE_WRITE_TOKEN" | kiln doc decision-doc set owner/repo --doc-id <doc-uuid> --base-url https://outline.example.com
kiln doc decision-doc list                     # 列出配置（不含 token），带待处理 / 结果不明的条目数
kiln doc decision-doc clear owner/repo        # 清除配置（未写入的条目取消、可能已写的转为 unknown）

kiln doc decision-sync list --repo owner/repo --state unknown   # 查看结果不明 / 待处理的条目
kiln doc decision-sync reconcile <id>                           # 只读查证：文档里已有该条目的标识就记为 delivered
kiln doc decision-sync resolve <id> --state cancelled --note "确认不需要"   # 人确认后结案
```

- **没有配置时不做事**：仓库没有配置「待决策清单」文档时，同步完全跳过，不影响原有的交付流程。
- **地址**：与只读绑定相同，`--base-url` 非本机地址必须是 `https://`（本机回环地址允许 `http://`）。
- **至多写一次**：追加到外部文档没有幂等键。Server 先在一个事务里登记意图（与交付报告同一个事务，失败会让这次结果记录整体回滚），真正写入前先提交"尝试中"，写入结果不明（网络错误、5xx、响应无法确认）时只做**只读**查证，绝不重发。结果不明（`unknown`）的条目需要人用 `decision-sync reconcile` 查证，或用 `resolve` 结案。
- **冲突与限流**：有人在 Outline 同时编辑时（409）会重新读取、把条目插到最新内容里再写，最多 3 次；Outline 限流（429）时按 `Retry-After` 退避，尊重服务端给的等待时间（最多 24 小时）。
- **错误信息**：列表和 `last_error` 里只有 HTTP 状态码、白名单内的 Outline 错误码和固定文案，不会出现 Outline 返回的原始 `message`（可能夹带 token 或正文），也不会有 token。
- **换文档**：换 `--doc-id` 或 `--base-url` 时，旧文档上还没写的条目会被取消、可能已写的转为 `unknown`；只换 token 不影响队列。地址按规范化后的形式比较，大小写、默认端口、结尾斜杠不同不算换文档。文档接近 Outline 的推荐篇幅（约 25 万 UTF-16 单位）时 `list` 会显示 `doc_near_limit`，请轮转到新文档；超过上限（约 150 万）的条目会直接取消。
- **保留**：同步状态（结果、时间、原因）和配置长期保留；文档正文归 Outline 管理，Kiln 不保存副本。

## 9. 排查

| 现象 | 原因 | 处理 |
|---|---|---|
| push 后没有 run | webhook 没送达；账号不在 `KILN_ALLOWED_ACCOUNTS`；`on:` 不匹配 | Server 日志搜 `webhook`；App 的 "Advanced → Recent Deliveries"；补对账或重新投递 |
| job 一直排队 | Runner 离线、平台不对、资源不够 | job 页面看原因；`kiln runner list` |
| 排队："Runner … 没有开启 buildkit" | 没开 buildkit | `config.toml` 设 `buildkit = true` 并重启 |
| runtime 下载失败，或 step 中联网失败 | 访问不了 go.dev、nodejs.org、registry.npmjs.org、github.com；step 不继承 Runner 的代理 | 打通网络或配代理（#5.4） |
| "docker 看不到 Runner 的数据目录" | 没共享给 docker 虚拟机（colima 默认只共享 `$HOME`） | 放到 `$HOME` 下，或加入 colima mounts / Docker Desktop 共享目录 |
| 不能升级："还没有查到最新版本" | 查询 npm 失败 | 看原因中的 npm 错误，必要时改 registry（#5.5） |
| 不能升级："没有经过启动器运行" / "版本不支持按台升级" | 入口不是启动器 | 换新版本后再 `service install`；入口应是 `…/bin/launcher/kiln-runner launch`（`launchctl print gui/$(id -u)/dev.kiln.runner \| grep -A2 arguments` 或 `systemctl --user cat kiln-runner`） |
| "job publish 引用的 environment release 不存在" | 没建 environment | `kiln env set` 建好后 `kiln rerun <run> --mode all`，不用重打 tag |
| "账号 … 没有配置 npm" | 没配 npm | `kiln npm set`（#7）后重跑 |
| 镜像发布"认证失败" | 凭据不对 | 查 `DOCKERHUB_USERNAME`、`DOCKERHUB_TOKEN`（PAT 需 Read & Write） |
| "不可变 tag 已指向其他 digest" | 该版本发布过不同内容 | 换新版本号 |
| 签名身份为 0 或 `unable to build chain` | .p12 没带证书链 | 重新导出带链的 .p12，或装 G2 Developer ID 中间证书（https://www.apple.com/certificateauthority/） |
| AI 审计失败或一直排队 | 节点没有就绪（Orca 没有打开或版本低于 1.4.258、仓库指定的 Agent 不可用、需要测试服务时的 Docker / Compose、kiln-task skill 同步不了，没有就绪不领取，等待原因写明缺什么）、连接器版本过旧（#5.5）、节点已满（`max_tasks`）；或者仓库配置的审计 Agent 都开发过这个 PR（`review.agent` 再加一个） | `kiln-orca doctor`；节点日志 `logs/`；Dashboard 的 AI 执行节点页 |
| AI 执行节点的 Docker 不可用："无法生成专用的 Docker 客户端配置（…）" | 用户的 Docker 配置（`DOCKER_CONFIG` 或 `~/.docker/config.json`）损坏、读写不了，或建不了 contexts 的链接；节点照常运行，只是需要测试服务的任务不派给它 | 按原因中的文件和步骤修好（`docker info` 同样会报错），一分钟内自动恢复；`kiln-orca doctor` 查看 |
| 加了 `kiln:ready` 没有选题 | 没有 `development` 段或已暂停；没有空闲的、`development.agent` 可用、平台在 `development.platform` 中的开发机（改了 Agent 或平台后要同步一次任务池）；同一个仓库有别的机器正在选题；Issue 之前被跳过且内容没变；App 没订阅 Issues 事件（任务池中没有它） | AI 开发页选仓库看任务池；`kiln ai dev sync owner/repo`；先去掉再加 `kiln:ready`；`kiln runner list` |
| AI 开发任务一直排队 | 没有开发角色、指定 Agent 可用、平台在任务可以执行的平台中、有空闲容量的节点 | 任务页面的等待原因；`kiln-orca doctor` |
| AI 任务等待"需要受管理的工具链（…），这个节点不能准备" | 仓库声明了 `requirements.tools`，节点的 kiln-orca 版本过旧（不上报工具链状态），或者找不到版本一致的 kiln-runner（安装时跳过了可选依赖、这个平台没有发布、`config.json` 的 `kiln_runner` 不对；新版本发布后几分钟内 npm 上的包还下载不到，这时安装会跳过它） | 按原因处理：`npm install -g @yuebanlaosiji/kilnflow-orca@latest` 后 `kiln-orca service restart`；`kiln-orca doctor` 的 kiln-runner 一项为 ✓ 后一分钟内领取 |
| AI 任务失败（`infra.toolchain`） | 准备工具链失败：下载源连不上、摘要与 lockfile 不一致、磁盘满 | 任务日志中有 kiln-runner 的输出；修好后在 Dashboard 重跑 |
| AI 任务失败（`infra.fetch`），提示 git 的锁文件（`….lock`）已经存在 | 机器崩溃（断电等）时中断的 git 操作在仓库的 clone 中留下了锁；连接器不自动删除锁（Orca 中的 Agent 共用这个 clone，可能正持有它） | 确认没有 git 进程在使用提示中的仓库（`<kiln-orca 数据目录>/repos/<repo id>/<仓库名>`，例如 `pgrep -fl git`）后删除提示中的锁文件；Kiln 会按原规则重试 |
| AI 执行节点不领取任务："连接器（kiln-orca）… journal 记录读不了" | `journal/<attempt>.json` 读不了（权限被改、磁盘损坏、手工编辑、更新版本的连接器写的）；其中可能有还在 Orca 中运行的 Agent，连接器不知道它占不占容量、用不用测试服务，所以不领取新任务、不回收测试服务，也不自动删除文件 | 按原因中的文件处理：权限问题改回（`chmod 600`，属主是运行连接器的用户）即可，连接器一分钟内读出来后接着跟踪这个任务；内容损坏时查看文件内容中的 `dispatchId` / `startRequestId`，在 Orca 中确认并停止对应的 Agent（`orca orchestration request-show --request <startRequestId> --json` 查 dispatch），然后删除这个文件，一分钟内恢复领取。被吊销的节点要处理完才会结束清理 |
| kiln-orca 日志中有"超过保留时间仍没能回收工作区""终端一直没能确认释放，不回收工作区"或"没能确认 Agent 停止，不回收工作区" | 任务已经结束、结果已经提交，但 12 小时内一直没能删除它的 Orca 工作区（`reason` 中是原因：Orca 拒绝删除——例如 archive hook、git 锁定、权限——、连不上 Orca、工作区中一直有活着的终端或终端没有释放的 worker、删除后核对没有通过、"不能再确认归属"——上一次删除去掉了登记但目录还在，常见于权限不够），或者 Agent、终端一直没能确认停止 | 先确认这次任务的 Agent 已经结束（`orca terminal list --worktree "id:<id>" --json` 为空；`orca orchestration run-list --limit 100 --json` 列出全部 Run（有 `nextCursor` 时带 `--cursor` 翻页），对每个 Run `orca orchestration worker-list --run <run id> --limit 100 --json --full` 中都没有工作区是它、`terminalState` 不是 `released` 的 worker——不带 `--run` 时只列出终端绑定的 Run，回执的 `scope.source` 是 `bound`，不能用来确认）。`orca worktree list --repo path:<数据目录>/repos/<repo id>/<仓库名> --limit 10000 --json` 找到备注为 `kiln:attempt=<attempt>;epoch=<epoch>;…` 的工作区，和路径在 `<数据目录>/tasks/<attempt>/` 之下的检出（列表中没有时用 `git -C <clone> worktree list` 找），逐个 `orca worktree rm --worktree "id:<repoId>::<路径>" --json`（不干净时加 `--force`；被 git 锁定时先 `git -C <clone> worktree unlock <路径>`），再 `git -C <clone> branch -D <分支>`（名字以 `kiln-a<attempt>-e<epoch>` 结尾的）。不要直接 `rm -rf` 还登记着的工作区目录；原因是"不能再确认归属"时 Orca 和 git 中都已经没有它了，确认路径就是日志中这次任务的工作区之后，修好权限（`chmod -R u+w <路径>`）再删除这个目录（`tasks/<attempt>/head` 由任务目录的 12 小时清理删除，删不掉时同样先修好权限）。`reason` 是 `list_scoped`（不带 `--run` 查询 worker 的旧版本连接器）的不用人处理：升级连接器后，周期清理把它们当作历史残留回收（1 小时没有活动之后，每小时最多 10 个）。`reason` 是 `list_truncated`、写明某个 Run 的 dispatch 超过 2 万个时（Orca 不清理单个 dispatch，连接器每启动一次 Agent 加一个），这个节点的回收都会失败：在节点上没有任务时停止 `kiln-orca`，执行 `orca orchestration reset`（清空 Orca 的全部编排记录，包括别的 Run），再启动 `kiln-orca`（邮箱的 Run 不在了时连接器会新建），或者按上面的方法手工清理 |
| 升级之后，Orca 中仍有 kiln-orca 0.3.6 及之前建的 `kiln-a<attempt>-e<epoch>` 工作区（日志中有"跳过旧版本建的工作区"） | 那些版本的备注中没有任务类型，连接器不能确认它们的来历，不自动删除 | 在 Dashboard 的 AI 任务中按 attempt 确认任务**已经结束**、Orca 中没有它活着的终端之后（开发的工作区中要留的改动先自己推到别的分支），按上一行的命令删除它、`tasks/<attempt>/head` 和分支。正在运行的任务的工作区不要动 |
| kiln-orca 日志中有"Orca 拒绝释放 Agent 的终端，不再重试"或"超过保留时间仍不能确认释放 Orca 中 Agent 的终端" | 任务已经结束、结果已经提交，只是 Orca 中这个 Agent 的终端没有回收：Orca 认为 worker 还没有结束（`dispatch_inactive`），或者 12 小时内一直连不上 Orca | 按日志中的 `dispatch`：`orca orchestration worker-show --dispatch <dispatch> --json` 查看，Agent 确实已经结束时 `orca orchestration worker-stop --dispatch <dispatch>` 再 `orca orchestration worker-release --dispatch <dispatch>`，或在 Orca 中关闭它的终端（工作区不动，随后由连接器回收） |
| AI 任务失败，错误分类 `infra.orca`，说明"Orca 的编排状态被重置" | Orca 中的编排状态被重置（例如执行了 `orca orchestration reset`）：连接器邮箱的 Run 和在途任务的 dispatch 都没有了，Agent 发出的结束消息再也收不到。连接器在邮箱终端上新建 Run，在途任务在一两分钟内以这个错误结束（开发任务的改动推为 checkpoint），之后照常领取 | 不需要处理；重置时还在运行的 Agent 的终端 Orca 不再管理，在 Orca 中关闭它们。Kiln 按原规则决定是否重试 |
| Orca 中有很多标题是"kiln-orca 邮箱"的终端；kiln-orca 日志中每一两分钟一组"邮箱终端接管 Run"（每次的 handle 都不同），常带"读取 Orca 邮箱失败 … consumer_fenced" | kiln-orca 0.3.12 及之前把没有界面窗格（`orphaned`）但还连着的邮箱终端当作不在，每次检查都新建一个（PLAN #33.4"邮箱终端的存活"） | 升级 kiln-orca：新版本沿用这样的终端，启动后和每小时关掉邮箱仓库中多余的邮箱终端（每轮 50 个），`kiln-orca doctor` 显示还剩多少。要立即手工清理：先看当前的邮箱句柄 `jq -r .handle "<数据目录>/mailbox.json"`，再列出邮箱仓库中的终端 `orca terminal list --worktree "path:<数据目录>/kiln-orca" --limit 10000 --json \| jq -r '.result.terminals[] \| select(.title=="kiln-orca 邮箱") \| .handle'`（Linux 上命令是 `orca-ide`；数据目录 macOS 是 `~/Library/Application Support/kiln-orca`，Linux 是 `~/.local/share/kiln-orca`），除当前句柄以外逐个 `orca terminal close --terminal <handle> --json`。标题不是"kiln-orca 邮箱"的终端不是连接器建的，确认没有用再关 |
| AI 任务失败，错误分类 `infra.lost` | 连接器没能确认 Orca 是否启动了 Agent（启动请求的结果查不到），为避免重复执行不再启动 | 在 Orca 中查看有没有名为 `kiln-a<attempt>-e<epoch>` 的 worktree；Kiln 会按原规则重试 |
| CI job 失败（`infra.credentials`）："Server 版本过旧，不支持按操作签发的源码凭据" | Runner 比 Server 新（或 Server 回滚了） | 先升级 Server（#5.5）；step 中 git 报同样的原因时也是这样 |
| step 中 git 报"kiln-runner：拒绝这次 git 传输…会带上别的凭据的配置" | 执行环境中有对这个地址生效的 `http.extraheader`、地址中带用户名密码，或 `~/.netrc` 中有这个主机或 `default`（host executor 以 Runner 用户运行，读得到它的全局 git 配置和 netrc） | 按说明中的键和来源去掉这些配置；Kiln 不使用机器上的个人凭据 |
| step 中 git 报"kiln-runner：地址 … 不是任务仓库 …，也不在 job 的 repositories 中" | 访问了没有声明的私有仓库 | 在 job 的 `repositories` 中声明（同一 installation） |
| CI job 失败（`infra.prepare`）："job 声明了 repositories，但 Kiln 的 git 凭据通道在这个执行环境中不可用" | `container:` 的 job 在不是 Linux 的 Runner 上（Docker Desktop、colima），`kiln-runner` 不是静态链接的，或者容器中连不上 socket（例如 `TMPDIR` 不在 docker 看得到的位置） | 把这类 job 放到 Linux Runner 上；用发布的 `kiln-runner`；`infra.prepare` 说"socket 路径…超过…上限"时把 Runner 的 `TMPDIR` 设为较短的目录 |
| 系统临时目录（`TMPDIR`）中有 `kiln-git-*` 目录 | Runner 被强行终止（SIGKILL、断电）时没有删掉的 git 凭据通道目录，里面只有 socket 文件，不含凭据；Runner 不在启动时回收它们 | 没有 Runner 在运行时可以手工删除；也可以留给系统的临时目录清理 |
| 要看失败现场 | 工作区默认删除 | 重跑时勾选"失败时保留工作区"，看 Runner 的 `~/kiln/work/<attempt>/`（保留 12 小时） |
