import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { KilnMark } from '../app/Shell'
import { list } from '../lib/envelope'
import css from './error.module.css'

// 统一的错误页：可读的原因和恢复入口；HTTP 状态码作为辅助信息，不显示内部错误。

const schema = z.object({
  Status: z.number(),
  Heading: z.string(),
  Reason: z.string(),
  /** 错误编号（与服务器日志中的 ref 对应），只有内部错误才有。 */
  Ref: z.string(),
  Links: list(z.object({ Label: z.string(), URL: z.string(), Primary: z.boolean() })),
})
type Data = z.infer<typeof schema>

function ErrorPage({ data, env }: PageProps<Data>) {
  return (
    <section className={css.page} aria-labelledby="err-h">
      {!env.Session && (
        <a className={css.brand} href="/">
          <KilnMark size={28} />
          Kiln
        </a>
      )}
      <h1 className={css.heading} id="err-h">
        {data.Heading}
      </h1>
      <p className={css.reason}>{data.Reason}</p>
      <p className={css.ref}>
        HTTP {data.Status}
        {data.Ref && (
          <>
            {' · '}错误编号 <code>{data.Ref}</code>（与服务器日志对应）
          </>
        )}
      </p>
      {data.Links.length > 0 && (
        <nav className={css.links} aria-label="接下来可以">
          {data.Links.map((l) => (
            <LinkButton key={l.URL} href={l.URL} variant={l.Primary ? 'primary' : 'default'}>
              {l.Label}
            </LinkButton>
          ))}
        </nav>
      )}
    </section>
  )
}

export default definePage(schema, ErrorPage)
