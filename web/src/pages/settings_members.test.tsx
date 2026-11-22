import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings_members.json'
import emptyFixture from '../../dev/fixtures/settings_members.empty.json'
import { envelope, renderPage } from '../test/render'

describe('成员', () => {
  it('每个成员显示角色、来源、最近登录和活动、运行的链接', () => {
    renderPage(envelope(fixture), '/settings/members')
    const rows = within(screen.getByRole('list', { name: '成员' })).getAllByRole('listitem')
    expect(rows).toHaveLength(3)
    const [me, admin, user] = rows
    if (!me || !admin || !user) throw new Error('成员不足 3 个')
    expect(within(me).getByText('你')).toBeInTheDocument()
    expect(within(me).getByText('管理员')).toBeInTheDocument()
    expect(within(me).getByText('来自 绑定的 GitHub 账号')).toBeInTheDocument()
    expect(within(admin).getByText('来自 KILN_ADMINS')).toBeInTheDocument()
    expect(within(user).getByText('成员')).toBeInTheDocument()
    expect(within(user).getByText('从未登录')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'lin-design 的活动' })).toHaveAttribute('href', '/settings/activity?actor=lin-design')
    expect(screen.getByRole('link', { name: 'lin-design 发起的运行' })).toHaveAttribute('href', '/runs?actor=lin-design')
  })

  it('没有成员时引导去绑定 GitHub 账号', () => {
    renderPage(envelope(emptyFixture), '/settings/members')
    expect(screen.getByText('还没有成员')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: '登录与身份' })[0]).toHaveAttribute('href', '/settings/account')
  })
})
