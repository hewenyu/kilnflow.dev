# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

主要使用者是 Kiln 管理员，即仓库所有者本人（与其 GitHub 账号为同一人，PLAN #16.5），熟悉 GitHub 和 Actions 界面。用桌面浏览器看构建、排查失败 job、重跑，管理 Runner、合并队列、变量、secret、environment；偶尔用手机看结果。Server 配置的成员只能查看、启动构建、重跑和取消。

## Product Purpose

Kiln 是自托管 CI，替代 GitHub Actions 构建 Go/TS 二进制和各类 runtime，发布到 GitHub Release、npm、容器镜像、HTTP 上传目标；另有合并队列、自动打 tag、AI 审计和 AI 开发。Dashboard 是查看和操作一切状态的入口（GitHub Check Run、PR review 只是投影）。成功标准：打开即知在跑什么、哪里坏了、为什么，一两步内能重跑或处理。

## Positioning

是 GitHub Actions self-hosted runner 的替代，不是又一个 CI 平台：workflow 写法（`.kiln/workflows/*.yml`）和概念（environment、变量、secret 三级作用域、Re-run）沿用 GitHub。状态都在 Server 的 Postgres，Dashboard 都能查到并解释原因（如为何没命中缓存、为何还在排队）。

## Operating Context

- 架构：GitHub App（事件与结果展示）+ Server（Go，公网小 VPS）+ Runner（只发出站连接；CI Runner `kiln-runner`，AI 执行节点是常驻的 Orca 加连接器 `kiln-orca`，负责 AI 审计与开发）+ Dashboard。
- Dashboard：React + `@primer/react` SPA（`web/`），构建后内嵌于 kiln-server；页面地址、登录、权限和页面数据（JSON）由 `internal/server/web` 提供；列表和进行中的页面每 5 秒重取数据原处更新，日志实时追加。
- 页面：总览；运行（列表、run 详情、job 详情与日志、启动构建）；合并队列；AI 开发（工作项列表、工作项详情，含设计依据）；AI 请求与任务（含设计依据）；Runner（列表、详情与按台升级、本地缓存、注册）；设置（连接与发布凭据、变量与密钥、环境、仓库与仓库的外部文档、Runner 升级、运维；登录与身份、成员、活动）；登录、错误页。
- 只服务自有私有仓库；只做 CI，不做 CD，无审批 / 人工门禁。
- 外部文档（Outline）：管理员配置绑定、验证读取和待决策清单；绑定只保存配置，只有管理员显式“验证读取”才访问 Outline。成员可以查看 AI 工作项 / 请求 / 任务冻结时的设计依据和按需读取正文（受登录与保留策略约束），但不能进入设置页。

## Capabilities and Constraints

- 界面支持简体中文（默认）和英文，用户可以随时切换（选择保存在浏览器中）；术语沿用 Actions（run、job、workflow、Runner、environment、secret、Re-run）。日志、仓库名、提交说明、AI 报告等原始内容按原文显示。
- 不支持 `uses:`；fork PR 一律丢弃；secret 和发布凭据（Docker Hub、npm token）只写不读。
- 保留策略：日志、产物、缓存等过程数据只存 12 小时，之后只留结果，页面需说明已清理。
- 窄屏（≤1011 / ≤767 / ≤543px，两种语言）和键盘靠人工验收；尊重"减少动态效果"，跟随系统深色模式。

## Brand Commitments

- 品牌名 Kiln（已确定），二进制 `kiln-server`、`kiln-runner`、`kiln`，AI 执行节点连接器 `kiln-orca`；保留窑形标识。
- 用户明确要求 GitHub / Primer 风格（配色含深色模式、系统字体、Octicons），仓库页式顶栏 + 下划线标签，run / job 详情仿 Actions 运行页。理由：Kiln 本就是替代 GitHub runner 的。

## Evidence on Hand

- 设计：产品与信息架构见 [docs/PLAN.md](docs/PLAN.md) 路由到的 Outline `10 [K-PRODUCT]`；前端视觉与组件契约见 [DESIGN.md](DESIGN.md)。
- 无用户证言、客户、性能基准，不得编造。

## Product Principles

1. 像 GitHub 一样熟悉：相同概念用相同位置、图标和说法。
2. 状态可解释：给出原因和下一步。
3. Dashboard 是唯一操作面：不必去 GitHub Settings 或服务器上找。
4. 不打断操作：自动更新不丢焦点、选中文字、输入和打开的对话框。
5. 操作先说后果：改变状态的操作先确认，写清影响、能否撤销和怎么恢复。
