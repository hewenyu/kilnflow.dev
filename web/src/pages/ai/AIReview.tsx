import { Label, Link } from '@primer/react'
import { Box, BoxBody, BoxHeader, BoxRow } from '../../components/Box'
import { useLocale, useT } from '../../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey } from '../../i18n/messages'
import { enumLabel, short } from '../../lib/format'
import css from '../job.module.css'
import type { AIReport, AIReview } from './schema'
import { lookup } from '../../lib/lookup'
// AI 审计任务的结果：结论、摘要、编号的问题（严重程度、证据等级、影响、触发条件、实际与预期行为、证据）、
// 精简的验证结果、未验证的部分、环境备注、顺带发现的已有问题、完整的命令记录（按 outcome 标注），以及 PR review 的发布状态。
// PR review 中只有精简的验证结果，完整的命令记录在这里。

// 结论、严重程度、证据等级、验证结果、命令 outcome 的颜色；名称在词典 aiReview.* 中，未知的值原样显示。报告的标题、说明、
// 命令和备注是 Agent 写的原文，原样显示。

const verdicts: Record<string, 'success' | 'danger' | 'attention'> = {
  no_issues: 'success',
  issues_found: 'danger',
  incomplete: 'attention',
}

const severities: Record<string, 'danger' | 'severe' | 'attention'> = { high: 'danger', medium: 'severe', low: 'attention' }

/** 验证结果按类别分组显示的顺序。 */
const checkKinds = ['change', 'suite', 'mutation'] as const

const checkResults: Record<string, 'success' | 'danger' | 'secondary'> = { passed: 'success', failed: 'danger', not_run: 'secondary' }

/** 命令的 outcome：说明命令对结论意味着什么，不只看退出码。 */
const outcomes: Record<string, 'success' | 'danger' | 'attention' | 'accent' | 'secondary'> = {
  passed: 'success',
  failed: 'danger',
  env_resolved: 'attention',
  expected_failure: 'accent',
  no_match: 'secondary',
  explore: 'secondary',
}

type Finding = AIReport['findings'][number]

/** findingFields 返回问题的细节（按显示顺序，省略空的）；mono 表示用等宽字体显示（证据中多是命令和输出）。 */
function findingFields(f: Finding): { key: string; text: string; mono?: boolean }[] {
  const fields = [
    { key: 'impact', text: f.impact },
    { key: 'scenario', text: f.scenario },
    { key: 'actual', text: f.actual },
    { key: 'expected', text: f.expected },
    { key: 'proof', text: f.proof, mono: true },
    { key: 'fix', text: f.fix },
    { key: 'body', text: f.body },
  ]
  return fields.filter((x) => x.text.trim() !== '')
}

function location(path: string, line: number): string {
  return line > 0 ? `${path}:${line}` : path
}

const reviewStates: Record<string, MsgKey> = {
  published: 'aiReview.state.published',
  pending: 'aiReview.state.pending',
  publishing: 'aiReview.state.publishing',
  failed: 'aiReview.state.failed',
  unknown: 'aiReview.state.unknown',
}

/** reviewStateText 返回 PR review 发布状态的说明（locale 不传时为 zh-CN）。 */
export function reviewStateText(r: AIReview, locale: Locale = defaultLocale): string {
  const id = lookup(reviewStates, r.ReviewState) ?? (r.PRNumber > 0 ? 'aiReview.state.disabled' : 'aiReview.state.manual')
  return translate(locale, msg(id))
}

export function AIReviewSection({ review }: { review: AIReview | null | undefined }) {
  const t = useT()
  const { locale } = useLocale()
  const colon = t('list.colon')
  if (!review) {
    return (
      <Box as="section" aria-labelledby="ai-h">
        <BoxHeader title={t('aiType.review')} id="ai-h" />
        <BoxBody>
          <p className={css.emptyText}>{t('aiReview.empty')}</p>
        </BoxBody>
      </Box>
    )
  }
  const rep = review.Report
  const verdict = enumLabel(locale, 'aiReview.verdict', rep.verdict)
  return (
    <Box as="section" aria-labelledby="ai-h">
      <BoxHeader title={t('aiType.review')} id="ai-h">
        <Label variant={lookup(verdicts, rep.verdict) ?? 'attention'}>{verdict}</Label>
      </BoxHeader>
      <BoxBody>
        <dl className={css.kv}>
          <dt>Agent</dt>
          <dd>{review.Agent}</dd>
          <dt>{t('aiReview.range')}</dt>
          <dd className="mono">
            {review.PRNumber > 0 ? `PR #${review.PRNumber} · ` : ''}
            {review.BaseSHA ? `${short(review.BaseSHA)}…${short(review.HeadSHA)}` : short(review.HeadSHA)}
          </dd>
          {rep.scope && (
            <>
              <dt>{t('aiReview.scope')}</dt>
              <dd className={css.wrap}>{rep.scope}</dd>
            </>
          )}
          <dt>{t('aiReview.summary')}</dt>
          <dd className={css.wrap}>{rep.summary}</dd>
          <dt>PR review</dt>
          <dd className={review.ReviewState === 'failed' || review.ReviewState === 'unknown' ? `${css.wrap} ${css.bad}` : css.wrap}>
            {review.ReviewURL ? <Link href={review.ReviewURL}>{reviewStateText(review, locale)}</Link> : reviewStateText(review, locale)}
            {review.ReviewError && `${colon}${review.ReviewError}`}
          </dd>
        </dl>
      </BoxBody>
      {rep.findings.map((f, i) => {
        const fields = findingFields(f)
        return (
          <BoxRow key={i} className={css.finding}>
            <p className={css.findingTitle}>
              <b>{i + 1}.</b> <Label variant={lookup(severities, f.severity) ?? 'attention'}>{enumLabel(locale, 'aiReview.severity', f.severity)}</Label>{' '}
              <Label variant="secondary">{enumLabel(locale, 'aiReview.evidence', f.evidence)}</Label> <b>{f.title}</b>
              {f.path && <code className={css.findingLoc}>{location(f.path, f.line)}</code>}
            </p>
            {fields.length > 0 && (
              <dl className={`${css.kv} ${css.findingBody}`}>
                {fields.map((x) => (
                  <div key={x.key} className={css.kvRow}>
                    <dt>{enumLabel(locale, 'aiReview.field', x.key)}</dt>
                    <dd className={`${css.wrap} ${css.preWrap}${x.mono ? ' mono' : ''}`}>{x.text}</dd>
                  </div>
                ))}
              </dl>
            )}
          </BoxRow>
        )
      })}
      {rep.checks.length > 0 && (
        <BoxRow className={css.finding}>
          <p className={css.findingTitle}>
            <b>{t('aiReview.checks')}</b>
          </p>
          <ul className={css.aiList}>
            {checkKinds.flatMap((kind) =>
              rep.checks
                .filter((c) => c.kind === kind)
                .map((c, i) => (
                  <li key={`${kind}-${i}`} className={css.wrap}>
                    <Label variant={lookup(checkResults, c.result) ?? 'secondary'}>{enumLabel(locale, 'aiReview.checkResult', c.result)}</Label>{' '}
                    <Label variant="secondary">{enumLabel(locale, 'aiReview.checkKind', kind)}</Label> {c.title}
                    {c.detail && `${colon}${c.detail}`}
                  </li>
                )),
            )}
          </ul>
        </BoxRow>
      )}
      {rep.unverified.length > 0 && (
        <BoxRow className={css.finding}>
          <p className={css.findingTitle}>
            <b>{t('aiReview.unverified')}</b>
          </p>
          <ul className={css.aiList}>
            {rep.unverified.map((u, i) => (
              <li key={i} className={css.wrap}>
                {u}
              </li>
            ))}
          </ul>
        </BoxRow>
      )}
      {rep.env_notes.length > 0 && (
        <BoxRow className={css.finding}>
          <p className={css.findingTitle}>
            <b>{t('aiReview.envNotes')}</b>
            <span className={css.findingLoc}>{t('aiReview.envNotesHint')}</span>
          </p>
          <ul className={css.aiList}>
            {rep.env_notes.map((u, i) => (
              <li key={i} className={css.wrap}>
                {u}
              </li>
            ))}
          </ul>
        </BoxRow>
      )}
      {rep.preexisting.length > 0 && (
        <BoxRow className={css.finding}>
          <p className={css.findingTitle}>
            <b>{t('aiReview.preexisting')}</b>
            <span className={css.findingLoc}>{t('aiReview.preexistingHint')}</span>
          </p>
          <ul className={css.aiList}>
            {rep.preexisting.map((p, i) => (
              <li key={i} className={css.wrap}>
                {p.path && <code>{location(p.path, p.line)}</code>} {p.title}
                {p.detail && `${colon}${p.detail}`}
              </li>
            ))}
          </ul>
        </BoxRow>
      )}
      {rep.commands_run.length > 0 && (
        <BoxRow className={css.finding}>
          <details>
            <summary>{t('aiReview.commands', { count: rep.commands_run.length })}</summary>
            <ul className={css.aiList}>
              {rep.commands_run.map((c, i) => {
                const o = lookup(outcomes, c.outcome)
                return (
                  <li key={i} className={css.wrap}>
                    {o && (
                      <>
                        <Label variant={o}>{enumLabel(locale, 'aiReview.outcome', c.outcome)}</Label>{' '}
                      </>
                    )}
                    <code>{c.command}</code> → <span className={!o && c.exit_code !== 0 ? css.bad : undefined}>{c.exit_code}</span>
                    {c.note && `${colon}${c.note}`}
                  </li>
                )
              })}
            </ul>
          </details>
        </BoxRow>
      )}
    </Box>
  )
}
