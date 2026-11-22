import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { RefLabel } from './RefLabel'

describe('RefLabel', () => {
  it('分支和 tag 显示简短名称', () => {
    render(<RefLabel gitRef="refs/heads/main" />)
    expect(screen.getByTitle('分支 refs/heads/main')).toHaveTextContent('main')
  })

  it('PR 有来源分支时显示分支名，没有时显示 #编号', () => {
    const { rerender } = render(<RefLabel gitRef="refs/pull/42/head" name="feat/sync" />)
    expect(screen.getByTitle('PR #42 的来源分支 feat/sync')).toHaveTextContent('feat/sync')
    rerender(<RefLabel gitRef="refs/pull/42/head" />)
    expect(screen.getByTitle('PR #42（refs/pull/42/head）')).toHaveTextContent('#42')
  })
})
