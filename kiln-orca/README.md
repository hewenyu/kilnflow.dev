# kiln-orca

Kiln（自托管 CI）的 AI 执行节点连接器：从 Kiln 领取 AI 审计、AI 开发任务，交给本机常驻的 Orca 执行，把报告和交付交回 Kiln。Orca 内部怎么分工（主 Agent、子 Agent）由 Orca 决定。

需要 Node.js 24+、已打开的 Orca 桌面版，以及已登录的 Codex 或 Claude Code。

```bash
npm install -g @yuebanlaosiji/kilnflow-orca
kiln-orca enroll --server https://kiln.example.com --token kiln-enroll-… --name mini-ai-01
kiln-orca doctor
kiln-orca service install      # macOS LaunchAgent / Linux systemd --user
```

其他命令：`run`、`service uninstall|restart|status`、`drain on|off`、`version`；`--home <目录>` 指定数据目录（同一台机器注册多个节点时用）。升级：`npm install -g @yuebanlaosiji/kilnflow-orca@latest && kiln-orca service restart`。

`kiln-orca task …` 只给 Agent 使用：它的完整用法（子命令、退出码、输出格式）写在随包分发的 `kiln-task` skill 中，任务说明只保留任务本身的规则并指向它。连接器在启动 Agent 之前把 skill 装到 Agent 加载 skill 的目录（Codex 是 `~/.agents/skills/kiln-task`，Claude Code 是 `$CLAUDE_CONFIG_DIR/skills/kiln-task`，没有设置时 `~/.claude/skills/kiln-task`），已是最新内容时不写文件。只覆盖自己安装的目录：用户自建的同名目录、符号链接不覆盖，同步不了时节点不领取新任务、上报原因；`kiln-orca doctor` 只读地显示每个 Agent 的 skill 状态（尚未安装、已安装的版本、冲突和怎么处理）。其中 `task exec -- <命令>` 用任务的受管理的工具链（仓库在 `requirements.tools` 中声明的 Go、Node、pnpm）运行命令。`task done --outcome succeeded|failed` 结束这一轮：连接器校验通过后，由这个命令在 Agent 的进程中向 Orca 报告结束（`worker_done`）。

工具链由随包安装的本平台 `@yuebanlaosiji/kilnflow-runner-<os>-<arch>`（可选依赖，版本与 kiln-orca 相同）装在数据目录的 `toolchains/` 中；不要用 `--omit=optional` 安装。没有它时节点不领取声明了工具链的任务，别的任务不受影响；`config.json` 的 `kiln_runner` 可以指定 kiln-runner 的绝对路径。
