import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import jobFixture from '../../dev/fixtures/job.running.json'
import { jobPageSchema } from '../pages/job/schema'
import { RunSidebar } from './RunSidebar'

// run 和 job 详情页左侧的导航（窄屏时收在菜单中）。

const jobs = jobPageSchema.parse(jobFixture.Data).Jobs

describe('RunSidebar', () => {
  it('页面数据更新后菜单按钮中当前 job 的状态图标不重新挂载（执行中的动画不会从头开始）', () => {
    const cur = jobs[0]
    if (!cur) throw new Error('fixture 中没有 job')
    const { rerender } = render(<RunSidebar summaryURL="/runs/1" jobs={jobs} current={cur.ID} />)
    const button = screen.getByRole('button', { name: new RegExp(cur.DisplayName) })
    const icon = button.querySelector('svg')
    expect(icon).not.toBeNull()
    // 定时刷新：同样的数据，新的对象
    rerender(<RunSidebar summaryURL="/runs/1" jobs={jobs.map((j) => ({ ...j }))} current={cur.ID} />)
    expect(screen.getByRole('button', { name: new RegExp(cur.DisplayName) }).querySelector('svg')).toBe(icon)
  })
})
