-- AI 执行节点的受管理的工具链（PLAN #33.12）：节点上报能否准备任务声明的工具链（requirements.tools）。没有上报的节点
-- （不支持的旧连接器）为 unknown，Server 不把带工具链的任务派给它。
ALTER TABLE ai_runners
  ADD COLUMN toolchain_state  text NOT NULL DEFAULT 'unknown' CHECK (toolchain_state IN ('ready', 'unavailable', 'unknown')),
  ADD COLUMN toolchain_reason text NOT NULL DEFAULT '';
