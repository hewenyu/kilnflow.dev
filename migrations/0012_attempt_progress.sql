-- attempt 当前的执行阶段与进度：Runner 在心跳中上报（kiln.runner.v1.Progress），
-- Server 换算成绝对时间后保存为 store.AttemptProgress 的 JSON；NULL 表示旧版 Runner 没有上报。
-- 只用于展示进行中的 attempt，属于过程数据，不参与调度和结果判断。
ALTER TABLE attempts ADD COLUMN progress jsonb;
