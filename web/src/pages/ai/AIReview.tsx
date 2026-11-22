import { Label, Link } from '@primer/react'
import { Box, BoxBody, BoxHeader, BoxRow } from '../../components/Box'
import { short } from '../../lib/format'
import css from '../job.module.css'
import type { AIReport, AIReview } from './schema'
import { lookup } from '../../lib/lookup'

// AI 审计任务的结果：结论、摘要、编号的问题（严重程度、证据等级、影响、触发条件、实际与预期行为、证据）、
// 精简的验证结果、未验证的部分、环境备注、顺带发现的已有问题、完整的命令记录（按 outcome 标注），以及 PR review 的发布状态。
// PR review 中只有精简的验证结果，完整的命令记录在这里。

const verdicts: Record<string, { text: string; variant: 'success' | 'danger' | 'attention' }> = {
  no_issues: { text: '未发现问题', variant: 'success' },
  issues_found: { text: '发现问题', variant: 'danger' },
  incomplete: { text: '审计未完成', variant: 'attention' },
}

const severities: Record<string, { text: string; variant: 'danger' | 'severe' | 'attention' }> = {
  high: { text: '高', variant: 'danger' },
  medium: { text: '中', variant: 'severe' },
  low: { text: '低', variant: 'attention' },
}

const evidences: Record<string, string> = { reproduced: '已复现', code_evidence: '代码证据', suspicion: '推测' }

const checkKinds: { kind: string; text: string }[] = [
  { kind: 'change', text: '本次改动' },
  { kind: 'suite', text: '整体验证' },
  { kind: 'mutation', text: '变异验证' },
]

const checkResults: Record<string, { text: string; variant: 'success' | 'danger' | 'secondary' }> = {
  passed: { text: '通过', variant: 'success' },
  failed: { text: '失败', variant: 'danger' },
  not_run: { text: '未运行', variant: 'secondary' },
}

/** 命令的 outcome：说明命令对结论意味着什么，不只看退出码。 */
const outcomes: Record<string, { text: string; variant: 'success' | 'danger' | 'attention' | 'accent' | 'secondary' }> = {
  passed: { text: '通过', variant: 'success' },
  failed: { text: '最终失败', variant: 'danger' },
  env_resolved: { text: '环境问题，已解决', variant: 'attention' },
  expected_failure: { text: '预期失败', variant: 'accent' },
  no_match: { text: '无匹配', variant: 'secondary' },
  explore: { text: '探索', variant: 'secondary' },
}

type Finding = AIReport['findings'][number]

/** findingFields 返回问题的细节（按显示顺序，省略空的）；mono 表示用等宽字体显示（证据中多是命令和输出）。 */
function findingFields(f: Finding): { label: string; text: string; mono?: boolean }[] {
  const fields = [
    { label: '影响', text: f.impact },
    { label: '触发条件', text: f.scenario },
    { label: '实际结果', text: f.actual },
    { label: '预期结果', text: f.expected },
    { label: '证据', text: f.proof, mono: true },
    { label: '修复方向', text: f.fix },
    { label: '说明', text: f.body },
  ]
  return fields.filter((x) => x.text.trim() !== '')
}

function location(path: string, line: number): string {
  return line > 0 ? `${path}:${line}` : path
}

/** reviewStateText 返回 PR review 发布状态的说明。 */
export function reviewStateText(r: AIReview): string {
  switch (r.ReviewState) {
    case 'published':
      return '已发布为 PR review'
    case 'pending':
      return '等待发布为 PR review'
    case 'publishing':
      return '正在发布为 PR review'
    case 'failed':
      return '发布 PR review 失败'
    case 'unknown':
      return '发布 PR review 的结果不明（没有自动重发）'
    default:
      return r.PRNumber > 0 ? '不发布 PR review（配置关闭了 review_comments）' : '手动审计，不发布 PR review'
  }
}

export function AIReviewSection({ review }: { review: AIReview | null | undefined }) {
  if (!review) {
    return (
      <Box as="section" aria-labelledby="ai-h">
        <BoxHeader title="AI 审计" id="ai-h" />
        <BoxBody>
          <p className={css.emptyText}>还没有审计报告：AI 执行节点完成审计并提交有效的报告后显示在这里。</p>
        </BoxBody>
      </Box>
    )
  }
  const rep = review.Report
  const v = lookup(verdicts, rep.verdict) ?? { text: rep.verdict, variant: 'attention' as const }
  return (
    <Box as="section" aria-labelledby="ai-h">
      <BoxHeader title="AI 审计" id="ai-h">
        <Label variant={v.variant}>{v.text}</Label>
      </BoxHeader>
      <BoxBody>
        <dl className={css.kv}>
          <dt>Agent</dt>
          <dd>{review.Agent}</dd>
          <dt>范围</dt>
          <dd className="mono">
            {review.PRNumber > 0 ? `PR #${review.PRNumber} · ` : ''}
            {review.BaseSHA ? `${short(review.BaseSHA)}…${short(review.HeadSHA)}` : short(review.HeadSHA)}
          </dd>
          {rep.scope && (
            <>
              <dt>审计范围</dt>
              <dd className={css.wrap}>{rep.scope}</dd>
            </>
          )}
          <dt>摘要</dt>
          <dd className={css.wrap}>{rep.summary}</dd>
          <dt>PR review</dt>
          <dd className={review.ReviewState === 'failed' || review.ReviewState === 'unknown' ? `${css.wrap} ${css.bad}` : css.wrap}>
            {review.ReviewURL ? <Link href={review.ReviewURL}>{reviewStateText(review)}</Link> : reviewStateText(review)}
            {review.ReviewError && `：${review.ReviewError}`}
          </dd>
        </dl>
      </BoxBody>
      {rep.findings.map((f, i) => {
        const s = lookup(severities, f.severity) ?? { text: f.severity, variant: 'attention' as const }
        const fields = findingFields(f)
        return (
          <BoxRow key={i} className={css.finding}>
            <p className={css.findingTitle}>
              <b>{i + 1}.</b> <Label variant={s.variant}>{s.text}</Label> <Label variant="secondary">{lookup(evidences, f.evidence) ?? f.evidence}</Label> <b>{f.title}</b>
              {f.path && <code className={css.findingLoc}>{location(f.path, f.line)}</code>}
            </p>
            {fields.length > 0 && (
              <dl className={`${css.kv} ${css.findingBody}`}>
                {fields.map((x) => (
                  <div key={x.label} className={css.kvRow}>
                    <dt>{x.label}</dt>
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
            <b>验证</b>
          </p>
          <ul className={css.aiList}>
            {checkKinds.flatMap(({ kind, text }) =>
              rep.checks
                .filter((c) => c.kind === kind)
                .map((c, i) => {
                  const r = lookup(checkResults, c.result) ?? { text: c.result, variant: 'secondary' as const }
                  return (
                    <li key={`${kind}-${i}`} className={css.wrap}>
                      <Label variant={r.variant}>{r.text}</Label> <Label variant="secondary">{text}</Label> {c.title}
                      {c.detail && `：${c.detail}`}
                    </li>
                  )
                }),
            )}
          </ul>
        </BoxRow>
      )}
      {rep.unverified.length > 0 && (
        <BoxRow className={css.finding}>
          <p className={css.findingTitle}>
            <b>未验证</b>
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
            <b>环境备注</b>
            <span className={css.findingLoc}>不是本次改动的问题，不影响结论</span>
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
            <b>顺带发现的已有问题</b>
            <span className={css.findingLoc}>与本次改动无关，不影响结论</span>
          </p>
          <ul className={css.aiList}>
            {rep.preexisting.map((p, i) => (
              <li key={i} className={css.wrap}>
                {p.path && <code>{location(p.path, p.line)}</code>} {p.title}
                {p.detail && `：${p.detail}`}
              </li>
            ))}
          </ul>
        </BoxRow>
      )}
      {rep.commands_run.length > 0 && (
        <BoxRow className={css.finding}>
          <details>
            <summary>完整执行记录（{rep.commands_run.length} 条命令）</summary>
            <ul className={css.aiList}>
              {rep.commands_run.map((c, i) => {
                const o = lookup(outcomes, c.outcome)
                return (
                  <li key={i} className={css.wrap}>
                    {o && (
                      <>
                        <Label variant={o.variant}>{o.text}</Label>{' '}
                      </>
                    )}
                    <code>{c.command}</code> → <span className={!o && c.exit_code !== 0 ? css.bad : undefined}>{c.exit_code}</span>
                    {c.note && `：${c.note}`}
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
