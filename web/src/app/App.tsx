import { Banner } from '@primer/react'
import { Fragment, useEffect } from 'react'
import type { Envelope } from '../lib/envelope'
import { pageEntry } from '../pages'
import { LiveProvider } from './live'
import { RouterProvider, useRouter } from './router'
import { Shell } from './Shell'

function Current() {
  const { view, failure } = useRouter()
  const title = view?.env.Title
  useEffect(() => {
    if (title) document.title = `${title} · Kiln`
  }, [title])
  if (!view) {
    return failure ? <Banner variant="critical" title="页面打不开" description={failure} /> : null
  }
  const entry = pageEntry(view.env.Page)
  return (
    <Shell env={view.env}>
      {/* 跳转到新页面时重新挂载页面组件；定时刷新时保留组件状态（输入、展开等） */}
      <Fragment key={view.seq}>
        {entry ? (
          entry.render(view.env)
        ) : (
          <Banner variant="critical" title="不认识的页面" description={`前端不认识页面 ${view.env.Page}：前端和 kiln-server 的版本可能不一致，刷新页面重试。`} />
        )}
      </Fragment>
    </Shell>
  )
}

/** App 是 Dashboard 前端：initial 是 index.html 中内嵌的页面数据（没有时前端自己取）。 */
export function App({ initial }: { initial: Envelope | null }) {
  return (
    <RouterProvider initial={initial}>
      <LiveProvider>
        <Current />
      </LiveProvider>
    </RouterProvider>
  )
}
