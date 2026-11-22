-- 删除已吊销的 Runner（PLAN #9.3）：只能删除已吊销、没有未结束任务的 Runner，删除后名称可以重新注册。
-- attempt 属于结果、永久保留（#10.3）：删除 Runner 时先把它的名称记在 attempts.runner_name，再删除 runners 行，
-- runner_id 随之置为 NULL；Runner 还在时 runner_name 为 NULL，名称从 runners 读取。
-- runner_cache_purges 随 Runner 删除（ON DELETE CASCADE）；auth_nonces、enrollment_tokens.runner_id 没有外键，由删除操作一并清理。
ALTER TABLE attempts ADD COLUMN runner_name text;
ALTER TABLE attempts ALTER COLUMN runner_id DROP NOT NULL;
ALTER TABLE attempts
  DROP CONSTRAINT attempts_runner_id_fkey,
  ADD CONSTRAINT attempts_runner_id_fkey FOREIGN KEY (runner_id) REFERENCES runners(id) ON DELETE SET NULL,
  ADD CONSTRAINT attempts_runner_known CHECK (runner_id IS NOT NULL OR runner_name IS NOT NULL);
