---
name: Kiln Dashboard
description: 自托管构建系统 Kiln 的控制台，按 GitHub / Primer 的样子做，打开就像在看一个 GitHub 仓库的 Actions 页。
colors:
  accent-blue: "#0969da"
  success-green: "#1a7f37"
  danger-red: "#d1242f"
  attention-amber: "#9a6700"
  primary-button-green: "#1f883d"
  ink: "#1f2328"
  ink-muted: "#59636e"
  canvas: "#ffffff"
  canvas-muted: "#f6f8fa"
  border-default: "#d1d9e0"
  border-emphasis: "#818b98"
  accent-wash: "#ddf4ff"
  attention-wash: "#fff8c5"
  log-bg: "#0d1117"
  log-fg: "#e6edf3"
  log-muted: "#9198a1"
  log-border: "#3d444d"
  log-line: "#151b23"
  log-hit: "rgb(187 128 9 / 45%)"
  log-cur: "#e3b341"
typography:
  headline:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "24px"
    fontWeight: 400
    lineHeight: 1.25
  headline-small:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "20px"
    fontWeight: 400
    lineHeight: 1.25
  title:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: 1.5
  body:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  body-strong:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "14px"
    fontWeight: 600
    lineHeight: 1.5
  label:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.5
  mono-log:
    fontFamily: "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "20px"
rounded:
  md: "6px"
  pill: "2em"
  full: "50%"
spacing:
  xxs: "4px"
  xs: "8px"
  sm: "12px"
  md: "16px"
  lg: "24px"
  xl: "32px"
  xxl: "40px"
  page-bottom: "48px"
components:
  button-primary:
    backgroundColor: "{colors.primary-button-green}"
    textColor: "{colors.canvas}"
    rounded: "{rounded.md}"
    typography: "{typography.body-strong}"
    height: "32px"
  button-default:
    backgroundColor: "{colors.canvas-muted}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    typography: "{typography.body-strong}"
    height: "32px"
  button-danger:
    backgroundColor: "{colors.canvas-muted}"
    textColor: "{colors.danger-red}"
    rounded: "{rounded.md}"
    typography: "{typography.body-strong}"
    height: "32px"
  box:
    backgroundColor: "{colors.canvas}"
    rounded: "{rounded.md}"
  box-header:
    backgroundColor: "{colors.canvas-muted}"
    textColor: "{colors.ink}"
    typography: "{typography.body-strong}"
    padding: "12px 16px"
  box-row:
    backgroundColor: "{colors.canvas}"
    padding: "12px 16px"
  box-row-hover:
    backgroundColor: "{colors.canvas-muted}"
  run-row-title:
    textColor: "{colors.ink}"
    typography: "{typography.title}"
  subhead:
    textColor: "{colors.ink}"
    typography: "{typography.headline}"
    padding: "0 0 8px"
  header-bar:
    backgroundColor: "{colors.canvas-muted}"
    padding: "16px 32px 8px"
  job-node:
    backgroundColor: "{colors.canvas}"
    rounded: "{rounded.md}"
    padding: "8px 12px"
    height: "56px"
    width: "248px"
  log-panel:
    backgroundColor: "{colors.log-bg}"
    textColor: "{colors.log-fg}"
    rounded: "{rounded.md}"
    typography: "{typography.mono-log}"
  label-pill:
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.pill}"
    typography: "{typography.label}"
    padding: "0 6px"
---

# Design System: Kiln Dashboard

## Overview

**Creative North Star: "仓库里的另一个 Actions 标签页"**

不自建视觉，照搬 GitHub，零学习成本：同一概念放在同一位置，用同一图标和说法。配色（含深色模式）、字体、图标、组件全取自 Primer（`@primer/primitives` 变量、`@primer/react`）；顶栏 + 下划线标签仿仓库页，run / job 详情仿 Actions 运行页，Runner 仿 Settings → Runners，设置仿仓库 Settings。Kiln 自有的只有窑形标识和 "Kiln" 名字（顶栏、登录页、错误页）。

## Colors

灰阶承担几乎全部界面，蓝表示链接和"当前"。frontmatter 为浅色值；代码写 Primer 变量，深色值由主题提供（`.impeccable/design.json` 的 `colorMeta`）。

### Primary
- `--fgColor-accent`：链接、焦点轮廓；`--bgColor-accent-muted`：选中文字、正在查看的 attempt、info 通知条；`--bgColor-accent-emphasis`：跳转进度条。
- 主按钮绿 `variant="primary"`：每视图至多一个主操作（"启动构建""注册新的 Runner""保存"）。

### Secondary
- `--fgColor-success`：成功、"已设置""已验证"。
- `--fgColor-danger`：失败及原因、升级失败、危险按钮文字；危险区边框 `--borderColor-danger-emphasis`。
- `--fgColor-attention`（琥珀）：执行中、排队和等待、Runner 离线、"可以升级"；`--bgColor-attention-muted` 为 warn 通知条。

### Neutral
- `--fgColor-muted`（最常用）：元信息、说明、时间、计数、键值的键、空的小节。
- `--bgColor-muted`：Box 标题行、可点行悬停、表头、job 图画布、登录表单框；`--bgColor-inset`：顶栏。
- 边线：`--borderColor-default` 外框，`--borderColor-muted` 行间，`--borderColor-emphasis` 仅 job 图连线和节点悬停。

### Named Rules
**The Primer-Only Rule.** 只写 Primer 功能变量，不写死颜色、不另做主题开关。例外只有：`--kiln-log-*`（日志面板两种模式下都是深色）和首字母头像底色（按登录名取色相）。

**The Color-Means-State Rule.** 绿、红、琥珀只表示状态、不做装饰；同一状态在图标、Label、文字中同色。

## Typography

正文 `--kiln-font`（系统字体 + 中文字体，也覆盖 BaseStyles 和弹出层），等宽 `--kiln-mono`（commit、路径、tag、版本号、日志、job spec）。不用展示字体，只靠字号字重分层。

### Hierarchy
- **Headline** 24 / 400：页面标题（PageHeader、设置页 Subhead、RunHeader、错误页）；run 编号灰字 300；窄屏 RunHeader 20px。
- **Headline small** 20 / 400：页内分节 Subhead、危险区标题、job 页的 job 名称。
- **Title** 16 / 600：运行列表行标题、Runner 名称、摘要格的值。
- **Body** 14：正文；Box 标题、名称、键值的键 600。
- **Label** 12：元信息、时间与耗时、Runner 的键值、表格、Label 徽标。

### Named Rules
**The Tabular Numbers Rule.** 表格、`<time>`、`.num` 用 `tabular-nums`；时间和耗时不折行。

## Layout

内容区居中限宽 1280px（`--kiln-container`），内边距 24 / 32 / 48px（≤767px 左右和上 16px），顶栏、标签导航、通知条同宽对齐；登录页例外，居中 340px。间距取 4 的倍数：Box 内 12 16，区块间 16 或 24，分节 Subhead 上方 40。

- **总览**：初始设置清单（管理员）在最上方；运行分区 + 右侧 296px 栏（今日统计、Runner）。
- **run / job 详情**：共用 RunHeader 和 256px 侧栏（切换时不动）。run 主栏：通知、触发摘要、job 图、活动；job 主栏：job 名称和信息、通知、日志，再按类型显示产物或发布结果，然后是尝试、job spec。AI 审计和 AI 开发不在 job 详情页，见 `AI 请求` / `AI 开发` 下的任务页。
- **Runner**：CI Runner 列表为一个 Box（标题行是可点的分类计数和筛选菜单），详情为概况、升级、升级记录三个 Box；AI 执行节点列表同为 Box，详情只有概况（连接器在节点上用 npm 升级，没有按台升级）。合并队列每个仓库一个 Box。
- **设置**：256px NavList + 内容，表单最宽 640px。

断点（Primer 1012 / 768 / 544px）：
- **≤1011px**：run / job 侧栏收进 ActionMenu；run 摘要四格变两格；总览单栏；运行筛选五列变三列。
- **≤767px**：设置侧栏收进 ActionMenu；运行行的 ref 和时间、Runner 行的操作移到内容下方；表格改为每行一块，列名取自 `data-label`。
- **≤543px**：运行筛选两列；设置页键值上下排列。

### Named Rules
**The Sidebar-Becomes-Menu Rule.** 窄屏侧栏不堆到正文上方，而是收进整宽 ActionMenu 按钮，标题留在第一屏。

## Elevation & Depth

层次靠边线、浅灰标题行和顶栏底色；阴影仅限 Primer 弹出层和下列几种。

### Shadow Vocabulary
- **节点静止阴影** `--shadow-resting-small`：job 图节点。
- **滚动边缘阴影**：job 图横向放不下时，有内容的一侧。
- **日志浮动按钮** `0 6px 16px rgb(1 4 9 / 60%)`：日志面板内的"跳到底部"。
- **顶栏下边线**：内阴影画线，与 UnderlineNav 选中下划线重合。

### Named Rules
**The Flat-By-Default Rule.** 卡片、Box、表单不加阴影或渐变；分组用 1px 边线和浅灰标题行。

## Shapes

一律 6px 圆角（Box、按钮、输入框、日志面板、job 图节点、行内 code）。徽标（Label、"最新"）2em 胶囊；头像圆形，admin token 头像为 6px 圆角方块以区别 GitHub 账号。边线 1px，仅"选择有效的尝试后显示日志"占位框用虚线。

## Components

### Buttons
只用 Primer `Button` / `LinkButton` / `IconButton`。列表行和工具栏 `size="small"`，页头默认尺寸。图标按钮 `variant="invisible"`，必须有 `aria-label`（兼作悬停提示）。SubmitButton 提交中 loading 并禁用。

### Labels（状态徽标）
Primer `Label`。run / job / attempt 状态按大类映射（`labelVariant(stateTone())`）：成功 `success`、失败 `danger`、执行中和排队 `attention`、其他 `secondary`。其他状态照此取色（在线 `success`、离线 `attention`、已吊销 `danger`）；`accent` 表示当前 / 进行中 / 已保存未验证和 AI 执行节点的角色 Label，`secondary` 为中性信息，`done` 只用于已合并。

### Box / Cards
列表一律用 Box：BoxHeader 浅灰，标题后可放 CounterLabel，右侧放操作或统计；BoxRow 间淡边线，`hoverable` 时整行悬停变浅灰；BoxFooter 灰字（"共 N 台，更新于…"）。Box 内再分节用浅灰小标题行；表格放在 Box 内，表头浅灰、12px；危险 Box 用于设置页危险区。

### RunRow 与 Runner 行
- **RunRow**：状态图标 / 标题与说明（仓库、事件、commit、提交者）/ ref / 时间与耗时；重跑和配置错误用 Label 标在标题后。
- **CI Runner 行**：RunnerIcon / 名称 + pool + 状态 Label / 12px 键值和正在执行的 job / 管理员操作。连接（在线 / 离线 / 已吊销）和接单（接单中 / 已暂停接单）分成两个 Label；升级只显示一句状态，操作在详情页。
- **CI Runner 与 AI 执行节点**：分开注册、分开列出，不是同一列表里的类型标签。注册页单选 Runner 类型（CI Runner、AI 审计节点、AI 开发节点），AI 节点的角色注册后不能改。`Runner`（`/runners`）只列 `kiln-runner`；`AI 执行节点`（`/ai/runners`）列执行机器上的 Orca + `kiln-orca`，行显示角色 Label（`accent`：AI 审计 / AI 开发）、连接与接单 Label、协议、并发、Orca / Docker / 本机 Agent 和正在执行的 AI 任务及其在 Orca 中的关联。升级只在 CI Runner 详情页按台操作；AI 执行节点在节点上用 `npm install -g @yuebanlaosiji/kilnflow-orca@latest && kiln-orca service restart` 升级。

### StatusIcon
run / job / attempt 状态都用它，同 Actions、不自创：成功绿勾、失败红叉、执行中自绘琥珀转动圆环、排队 `DotFillIcon` 和等待上游 `ClockIcon`（琥珀）、取消 `StopIcon` 和跳过 `SkipIcon`（灰）、未知灰色 `AlertFillIcon`。RunnerIcon 同理：已吊销红叉、离线琥珀警告、执行中圆环、暂停接单灰色 `CircleSlashIcon`、空闲绿勾。旁有文字时 `aria-hidden`，单独出现必须有 `label`；定时刷新不能让它重新挂载（动画会从头开始）。

### Navigation
- **顶栏**：左 32px 窑形标识 + "Kiln"，右 24px 头像菜单（身份、登录与身份、退出登录）。
- **UnderlineNav**：总览、运行、合并队列、AI 请求、AI 开发、Runner、AI 执行节点、设置（仅管理员），各带 Octicon；"运行"显示进行中 run 的计数。
- **PageHeader**（总览、运行、合并队列、AI 请求、AI 开发、Runner 与 AI 执行节点列表和详情、本地缓存）：标题 + 至多一行灰字说明，右侧操作；子页面的返回按钮也放在右侧；长说明放帮助 details。
- **RunHeader**（run / job 详情）：位置行（运行 / 仓库 / #ID）、24px 状态图标 + run 标题 + 补充 Label、右侧操作、底部分隔线。
- **Subhead**（设置页）：下边线，右侧操作或状态 Label，下方灰字说明；子页在标题中用 Crumbs（"环境 / release"）；`danger` 时标题变红。
- **SettingsLayout / RunSidebar**：NavList（设置分"构建""账号"两组；run 侧栏为"概要" + "Jobs"，job 前带 StatusIcon），sticky。
- **RefLabel**：Primer `BranchName` + 12px 分支 / tag / PR 图标，`title` 为完整 ref。

### Feedback
- **Notice**（顶栏下通知条）：`info` 操作结果（重定向带的 `?flash=`，可关闭）、`warn` admin token 提醒和登录已过期、`error` 请求失败（可关闭）。
- **Callout**：页内提示，Primer `Banner` compact，`info / warning / critical / success`，必有 `title`（可 `hideTitle`）。
- **EmptyState**：Primer `Blankslate` 窄版：图标 + 标题（无句号）+ 一句说明 + 下一步；页头已有同一主要操作时不放按钮，"清空筛选"用次要链接；整页列表 medium，总览分区 small。Box 内某一小节为空只用一行灰字 BoxRow。
- **表单错误**：顶部 critical Callout"没有保存。"，字段下 Validation，焦点移到第一个出错字段，已填的值由服务端回填。

### Confirmation
- 改变状态的操作都先确认：单纯确认用 `ConfirmForm`（`ConfirmationDialog`），要选项的（重跑）用 `Dialog` + `Form`。
- 标题是带对象的问句（"吊销 mac-mini？"），确认按钮重复动作和对象；正文用短列表写清何时生效、对正在执行的任务的影响、能否撤销、怎么恢复；有更温和的做法时指出（"先暂停接单，等任务结束后再吊销"）。
- 破坏性操作确认按钮为红，可恢复的（暂停接单、升级、加入合并队列）用 `danger={false}`；操作本身叫"取消…""移出"时，退出按钮叫"返回"。暂时不能操作时不放按钮，灰字说明原因。

### Live Refresh
- 页面用 `useLive(active)` 声明自动更新：列表页始终开启，其他页面只在有进行中的事（run、升级、排队、待投递）时开启。
- 每 5 秒重取页面数据原处替换，不滚动、不重新挂载；出错时退避（最长 60 秒）。页面在后台、正在输入、选中了文字、对话框打开或提交中时跳过，表单校验失败的页面不刷新，登录过期后停止并提示。
- 自动更新的列表写明"每 5 秒自动更新"（取 `liveSeconds`）。日志由日志面板自己轮询追加；耗时和相对时间在本地走。

### Job Graph 与 Log Viewer
- **Job Graph**：浅灰画布按阶段从左到右；节点为 StatusIcon、名称和灰字（耗时 · Runner，或"沿用 #388 的结果""等待上游"），失败原因红字；放不下时横向滚动，可键盘聚焦。
- **Log Viewer**：面板内设 `data-color-mode="dark"`；工具栏为搜索、上 / 下一个、"跟随最新输出"（仅仍在增长时）、下载；行号复制时不带；已清理或不可用时显示同为深色的说明框。
- **AI 审计与开发**：不是 CI job，也不在 job 详情页显示；`AI 请求` / `AI 开发` 下有各自的请求页和任务页。任务页中，`AIReviewSection` 显示审计结论 Label、摘要、PR review 状态、编号的问题（严重程度、证据等级），不影响结论的部分单独标明，完整命令记录折叠；`AIDevelopSection` 用 `ReportBox` 展示选题与交付报告；日志（`LogViewer`）和 spec（`SpecSection`）与 job 页共用组件，attempt 列表由 AI 任务页各自渲染。

### Motion
- 执行中圆环 1s 线性旋转；日志圆点 1.6s 呼吸；跳转进度条延迟 150ms 出现（快速跳转不闪）。
- `prefers-reduced-motion: reduce`：动画和过渡压到 0.01ms、只播一次；状态仍靠图标形状和颜色表达。

### Copy
- 中文界面，Actions 术语保留英文（run、job、workflow、Runner、environment、secret、tag、pool、executor），Re-run 译"重跑"；中文与英文 / 数字间留空格（"6 个 job"）。
- 相对时间（"13 分钟前"，悬停显示具体时间），耗时"3 分 30 秒"；长版本号和 SHA 缩短，完整值放 `title` 或复制按钮。
- 按钮动词开头并说清对象（"重跑这个 job""取消运行""升级到 0.1.44…"）。
- 状态、空状态、失败都给原因和下一步。

## Do's and Don'ts

其余规则见上文各节。

- **Do** 先找 GitHub 上对应的页面和 Primer 组件，照它的结构、图标和说法做；Primer 有的组件不自己写。
- **Don't** 在窑形标识和名字之外加 Kiln 自己的品牌色、插画或展示字体；不用 emoji 或字符代替 Octicons。
- **Don't** 显示 secret、token 的值，或在表单出错后回填它们。
