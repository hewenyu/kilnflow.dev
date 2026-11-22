package devreport

// SelectionSchema 返回交给 Agent 的选题报告 JSON Schema。version 由 AI 执行节点写入，不在其中。
func SelectionSchema() string { return selectionSchema }

// DeliverySchema 返回交给 Agent 的交付报告 JSON Schema。
func DeliverySchema() string { return deliverySchema }

const selectionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["selected", "batch_reason", "plan", "design_first", "design_reason", "skipped"],
  "properties": {
    "selected": {"type": "array", "items": {"type": "integer"},
      "description": "要领取的 Issue 编号（一批有内聚目标）；没有可做的写 []"},
    "batch_reason": {"type": "string", "description": "选多个时说明为什么合成一批；只选一个或不选时写空字符串"},
    "plan": {"type": "string", "description": "初步计划：要解决什么、大致怎么做、怎么验证；不选时写空字符串"},
    "design_first": {"type": "boolean",
      "description": "false：在同一次执行中先设计再开发，一个 PR 完成；true：判断一个 PR 完成不了，先交付设计 PR，合并后再实现。不选时写 false"},
    "design_reason": {"type": "string", "description": "选择这种方式的理由；不选时写空字符串"},
    "skipped": {"type": "array", "description": "每个未选的候选都要写一项",
      "items": {"type": "object", "additionalProperties": false, "required": ["number", "reason", "reconsider", "later", "wait_for"],
      "properties": {
        "number": {"type": "integer"},
        "reason": {"type": "string", "description": "不选的原因（依赖未完成、与这一批无关、与进行中的工作项冲突、超出授权等）"},
        "reconsider": {"type": "string", "description": "重新评估的条件，例如：#12 关闭后、提供了 GCP 项目后"},
        "later": {"type": "boolean", "description": "true：Issue 可以做，只是不放进这一批（与这一批无关、超过数量、与进行中的工作项会改到同一处），留给之后的选题；false：现在不做，内容不变就不再选（写了 wait_for 时等依赖关闭后自动回到候选）"},
        "wait_for": {"type": "array", "items": {"type": "integer"},
          "description": "它依赖、还没有完成的 Issue 编号（这些 Issue 都关闭后自动回到候选，此时 later 写 false）；不是等依赖写 []"}}}}
  }
}`

const deliverySchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["status", "summary", "pr_title", "issues", "verification", "dispositions", "handoff", "design", "design_deviations"],
  "properties": {
    "status": {"type": "string", "enum": ["delivered", "no_change", "blocked"],
      "description": "delivered：已在分支上 commit 了改动；no_change：不需要改代码（每个 Issue 写明原因）；blocked：无法继续（写明还剩什么、需要什么条件）"},
    "summary": {"type": "string", "description": "这一轮做了什么，一两句话"},
    "pr_title": {"type": "string", "description": "PR 标题（delivered 时必填；与仓库的提交信息语言一致）"},
    "issues": {"type": "array", "description": "工作项的每个 Issue 恰好一项",
      "items": {"type": "object", "additionalProperties": false, "required": ["number", "resolution", "acceptance", "notes"],
      "properties": {
        "number": {"type": "integer"},
        "resolution": {"type": "string", "enum": ["full", "partial", "none"],
          "description": "full：验收项全部完成（合并后关闭 Issue）；partial：部分完成；none：没有完成。部分完成不能写 full"},
        "acceptance": {"type": "array", "description": "验收项：从 Issue 的要求逐条列出，写明是否完成和证据",
          "items": {"type": "object", "additionalProperties": false, "required": ["item", "done", "evidence"],
          "properties": {
            "item": {"type": "string"},
            "done": {"type": "boolean"},
            "evidence": {"type": "string", "description": "实现位置和验证方式（完成时必填）"}}}},
        "notes": {"type": "string", "description": "补充说明；no_change 时写不需要改代码的原因"}}}},
    "verification": {"type": "array", "description": "实际运行过的验证命令（Agent 自报，会标明）",
      "items": {"type": "object", "additionalProperties": false, "required": ["command", "outcome", "note"],
      "properties": {
        "command": {"type": "string"},
        "outcome": {"type": "string", "enum": ["passed", "failed", "not_run"]},
        "note": {"type": "string"}}}},
    "dispositions": {"type": "array", "description": "返修时：本轮要处理的每个问题恰好一项；不是返修写 []",
      "items": {"type": "object", "additionalProperties": false, "required": ["finding", "action", "commit", "duplicate_of", "reason"],
      "properties": {
        "finding": {"type": "string", "description": "问题的 ID，例如 F123-1"},
        "action": {"type": "string", "enum": ["fixed", "rejected", "duplicate", "unable"]},
        "commit": {"type": "string", "description": "fixed 时写修复的 commit"},
        "duplicate_of": {"type": "string", "description": "duplicate 时写重复的另一个问题的 ID"},
        "reason": {"type": "string", "description": "修复了什么 / 不采纳的依据（代码、复现、设计）/ 无法完成的原因"}}}},
    "handoff": {"type": "object", "additionalProperties": false, "required": ["done", "remaining", "decisions", "next"],
      "description": "交给下一轮（可能在另一台机器上）的交接",
      "properties": {
        "done": {"type": "string"},
        "remaining": {"type": "string", "description": "还没完成的部分；blocked 时必填"},
        "decisions": {"type": "string", "description": "关键决定及理由"},
        "next": {"type": "string"}}},
    "design": {"type": "object", "additionalProperties": false, "required": ["documents", "decisions", "external_inputs"],
      "description": "设计：设计阶段必填（documents 至少一个）；一个 PR 完成时有设计文档、有疑问的决定或外部输入时填写，没有写空数组",
      "properties": {
        "documents": {"type": "array", "items": {"type": "string"}, "description": "设计文档的路径（仓库中的 *.md，相对路径）"},
        "decisions": {"type": "array", "description": "有疑问的决定的决策记录（写进 PR 描述供复盘）；只有 confirmed_decision 会让 PR 暂停等人确认",
          "items": {"type": "object", "additionalProperties": false, "required": ["category", "ref", "context", "options", "choice", "impact", "review"],
          "properties": {
            "category": {"type": "string", "enum": ["confirmed_decision", "acceptance", "policy", "other"],
              "description": "confirmed_decision：改已确认的决定（PLAN #1.4、AGENTS.md #2）；acceptance：删减或改变原验收项；policy：预算、范围等取舍；other：其他"},
            "ref": {"type": "string", "description": "confirmed_decision 时写被改变的决定编号（例如 C20），其他写空字符串"},
            "context": {"type": "string", "description": "现状和依据"},
            "options": {"type": "string", "description": "考虑过的方案"},
            "choice": {"type": "string", "description": "选择及理由"},
            "impact": {"type": "string", "description": "影响"},
            "review": {"type": "string", "description": "复盘时要看什么"}}}},
        "external_inputs": {"type": "array", "description": "真实验证需要、只有人能提供的条件（项目、凭据、费用）",
          "items": {"type": "object", "additionalProperties": false, "required": ["what", "blocks", "resume"],
          "properties": {
            "what": {"type": "string"},
            "blocks": {"type": "string", "description": "阻塞哪些验收项"},
            "resume": {"type": "string", "description": "恢复的条件"}}}}}},
    "design_deviations": {"type": "array", "description": "实现与已合并设计的出入（先设计、在实现阶段时）；没有写 []",
      "items": {"type": "object", "additionalProperties": false, "required": ["item", "reason"],
      "properties": {"item": {"type": "string"}, "reason": {"type": "string"}}}}
  }
}`
