import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { OrcaStatus, stageText } from './orca'

describe('Orca 进度', () => {
  it('已知阶段显示名称，未知阶段原样显示', () => {
    expect(stageText('running')).toBe('执行中')
    expect(stageText('verifying')).toBe('verifying')
  })

  // 阶段由节点上报：与对象原型上的属性同名时也只是一个未知的阶段
  it.each(['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf'])('阶段 %s 原样显示，渲染不抛错', (stage) => {
    expect(stageText(stage)).toBe(stage)
    render(<OrcaStatus stage={stage} text="运行测试" orca={{}} />)
    expect(screen.getByText(`${stage}：运行测试`)).toBeInTheDocument()
  })
})
