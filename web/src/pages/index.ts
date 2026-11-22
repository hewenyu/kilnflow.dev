import type { PageEntry } from '../app/definePage'
import { lookup } from '../lib/lookup'
import Login from './login'
import Error from './error'
import Overview from './overview'
import Runs from './runs'
import RunNew from './run_new'
import Run from './run'
import Job from './job'
import MergeQueue from './merge_queue'
import AIDev from './ai_dev'
import AIDevItem from './ai_dev_item'
import AIRequests from './ai_requests'
import AIRequest from './ai_request'
import AITask from './ai_task'
import AIRunners from './ai_runners'
import AIRunnerDetail from './ai_runner'
import Runners from './runners'
import RunnerDetail from './runner'
import RunnerNew from './runner_new'
import RunnerCache from './runner_cache'
import Settings from './settings'
import SettingsDocker from './settings_docker'
import SettingsNpm from './settings_npm'
import SettingsVars from './settings_vars'
import SettingsScope from './settings_scope'
import SettingsItem from './settings_item'
import SettingsEnvs from './settings_envs'
import SettingsEnvNew from './settings_env_new'
import SettingsRepos from './settings_repos'
import SettingsRepoDocs from './settings_repo_docs'
import SettingsOps from './settings_ops'
import SettingsUpgrade from './settings_upgrade'
import SettingsAccount from './settings_account'
import SettingsMembers from './settings_members'
import SettingsActivity from './settings_activity'

// 页面组件，按服务端的页面名称（uiPage.Page）索引；页面名称与 internal/server/web 的 render 调用一致。
export const pages = {
  login: Login,
  error: Error,
  overview: Overview,
  runs: Runs,
  run_new: RunNew,
  run: Run,
  job: Job,
  merge_queue: MergeQueue,
  ai_dev: AIDev,
  ai_dev_item: AIDevItem,
  ai_requests: AIRequests,
  ai_request: AIRequest,
  ai_task: AITask,
  ai_runners: AIRunners,
  ai_runner: AIRunnerDetail,
  runners: Runners,
  runner: RunnerDetail,
  runner_new: RunnerNew,
  runner_cache: RunnerCache,
  settings: Settings,
  settings_docker: SettingsDocker,
  settings_npm: SettingsNpm,
  settings_vars: SettingsVars,
  settings_scope: SettingsScope,
  settings_item: SettingsItem,
  settings_envs: SettingsEnvs,
  settings_env_new: SettingsEnvNew,
  settings_repos: SettingsRepos,
  settings_repo_docs: SettingsRepoDocs,
  settings_ops: SettingsOps,
  settings_upgrade: SettingsUpgrade,
  settings_account: SettingsAccount,
  settings_members: SettingsMembers,
  settings_activity: SettingsActivity,
} satisfies Record<string, PageEntry>

export type PageName = keyof typeof pages

/** pageEntry 返回页面名称对应的页面；未知的名称（前端和服务端版本不一致）返回 null。 */
export function pageEntry(name: string): PageEntry | null {
  return lookup(pages, name) ?? null
}
