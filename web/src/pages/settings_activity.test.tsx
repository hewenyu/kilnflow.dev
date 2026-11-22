import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings_activity.json'
import emptyFixture from '../../dev/fixtures/settings_activity.empty.json'
import { envelope, renderPage } from '../test/render'

describe('活动', () => {
  it('列出操作，带链接的操作可以点进去，可以翻到更早的活动', () => {
    renderPage(envelope(fixture), '/settings/activity')
    const rows = within(screen.getByRole('list', { name: '活动' })).getAllByRole('listitem')
    expect(rows).toHaveLength(6)
    const [first] = rows
    if (!first) throw new Error('没有活动')
    expect(within(first).getByRole('link', { name: '重跑了 run #409（第 1 次重跑）' })).toHaveAttribute('href', '/runs/409')
    expect(screen.getByRole('link', { name: '更早的活动' })).toHaveAttribute('href', '/settings/activity?before=203')
    expect(screen.queryByRole('link', { name: '回到最新' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '清空筛选' })).not.toBeInTheDocument()
  })

  it('按操作者筛选：按钮显示当前操作者，菜单中选中它', async () => {
    renderPage(envelope(emptyFixture), '/settings/activity?actor=lin-design')
    expect(screen.getByText('lin-design 还没有记录在案的操作。')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '清空筛选' })).toHaveAttribute('href', '/settings/activity')
    await userEvent.click(screen.getByRole('button', { name: '操作者：lin-design' }))
    const menu = await screen.findByRole('menu')
    expect(within(menu).getByRole('menuitemradio', { name: 'lin-design' })).toBeChecked()
    expect(within(menu).getByRole('menuitemradio', { name: '全部' })).not.toBeChecked()
  })
})
