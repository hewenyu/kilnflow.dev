import { GitBranchIcon, GitPullRequestIcon, TagIcon } from '@primer/octicons-react'
import { BranchName } from '@primer/react'
import { useT } from '../i18n/locale'
import { refKind, refName } from '../lib/format'

/**
 * RefLabel 显示分支、tag 或 PR 的 ref（GitHub 的 BranchName 样式），title 是完整 ref。
 * name 替换显示的名称：PR（refs/pull/N/head）传来源分支时显示为分支；没有来源分支时显示 PR 图标和 #N。
 * href 不为空时是链接（例如 GitHub 上的分支）。
 */
export function RefLabel({ gitRef, name, href }: { gitRef: string; name?: string; href?: string }) {
  const t = useT()
  if (gitRef === '') return null
  const kind = refKind(gitRef)
  const pr = kind === null ? /^refs\/pull\/(\d+)\//.exec(gitRef)?.[1] : undefined
  let Icon = GitBranchIcon
  let text = name || refName(gitRef)
  let title = t('ref.branch', { ref: gitRef })
  if (kind === 'tag') {
    Icon = TagIcon
    title = t('ref.tag', { ref: gitRef })
  } else if (pr !== undefined) {
    title = name ? t('ref.prBranch', { pr, name }) : t('ref.pr', { pr, ref: gitRef })
    if (!name) {
      Icon = GitPullRequestIcon
      text = `#${pr}`
    }
  } else if (kind === null) {
    Icon = GitPullRequestIcon
    title = t('ref.other', { ref: gitRef })
  }
  return (
    <BranchName as={href ? 'a' : 'span'} href={href} title={title}>
      <Icon size={12} aria-hidden /> {text}
    </BranchName>
  )
}
