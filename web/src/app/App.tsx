import { Banner } from '@primer/react'
import { Fragment, useEffect } from 'react'
import { LocaleProvider, Msg, Trans, useLocale } from '../i18n/locale'
import { isEmpty, msg, translate } from '../i18n/messages'
import type { Envelope } from '../lib/envelope'
import { pageEntry } from '../pages'
import { LiveProvider } from './live'
import { RouterProvider, useRouter } from './router'
import { Shell } from './Shell'

function Current() {
  const { view, failure } = useRouter()
  const { locale } = useLocale()
  // 标题随语言变化：切换语言后立即更新
  const title = view && !isEmpty(view.env.Title) ? translate(locale, view.env.Title) : ''
  useEffect(() => {
    if (title) document.title = `${title} · Kiln`
  }, [title])
  if (!view) {
    return failure ? <Banner variant="critical" title={translate(locale, msg('app.openFailed'))} description={<Msg text={failure} />} /> : null
  }
  const entry = pageEntry(view.env.Page)
  return (
    <Shell env={view.env}>
      {/* 跳转到新页面时重新挂载页面组件；定时刷新、切换语言时保留组件状态（输入、展开等） */}
      <Fragment key={view.seq}>
        {entry ? (
          entry.render(view.env)
        ) : (
          <Banner
            variant="critical"
            title={translate(locale, msg('app.unknownPage'))}
            description={<Trans id="app.unknownPageDetail" params={{ page: view.env.Page }} />}
          />
        )}
      </Fragment>
    </Shell>
  )
}

/**
 * App 是 Dashboard 前端：initial 是 index.html 中内嵌的页面数据（没有时前端自己取）。
 * LocaleProvider 在 RouterProvider 之上：切换语言不影响跳转状态，页面不重新挂载。
 */
export function App({ initial }: { initial: Envelope | null }) {
  return (
    <LocaleProvider>
      <RouterProvider initial={initial}>
        <LiveProvider>
          <Current />
        </LiveProvider>
      </RouterProvider>
    </LocaleProvider>
  )
}
