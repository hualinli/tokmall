// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { OrderLesson } from './order-lesson'
import { parseProgress, progressKey } from './order-lesson-data'

// Provide deterministic storage; Node's experimental Web Storage can shadow jsdom's.
beforeEach(() => {
  const entries = new Map<string, string>()
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => {
      entries.set(key, value)
    },
    removeItem: (key: string) => {
      entries.delete(key)
    },
    clear: () => entries.clear(),
    key: (index: number) => [...entries.keys()][index] ?? null,
    get length() {
      return entries.size
    },
  })
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function finishExperiment(prediction: string) {
  fireEvent.click(screen.getByRole('radio', { name: prediction }))
  fireEvent.click(screen.getByRole('button', { name: '开始推演' }))
  for (let i = 0; i < 3; i++)
    fireEvent.click(screen.getByRole('button', { name: '下一步' }))
}

describe('下单样章', () => {
  it('requires a prediction, reveals rollback and counts repeat exploration only once', () => {
    render(<OrderLesson />)
    expect(
      (screen.getByRole('button', { name: '开始推演' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '最后扣库存时失败' }))
    finishExperiment('返回原订单')
    expect(screen.getByText(/预测需要调整/)).toBeTruthy()
    expect(screen.getByText(/提交后的事实：订单总数 0.*券 UNUSED/)).toBeTruthy()
    expect(screen.getByText('学习进度 1 / 11')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '重新预测' }))
    finishExperiment('失败并整体回滚')
    expect(screen.getByText(/预测正确/)).toBeTruthy()
    expect(screen.getByText('学习进度 1 / 11')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '同键改成买两件' }))
    expect(screen.queryByText(/提交后的事实/)).toBeNull()
    expect(
      (screen.getByRole('button', { name: '开始推演' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
  })

  it('persists answers, self assessments and notes across remounts and resets the chapter', () => {
    const view = render(<OrderLesson />)
    fireEvent.click(
      screen.getByRole('radio', { name: '先检查券是不是 UNUSED' }),
    )
    expect(screen.getByText(/^再想一想/)).toBeTruthy()
    expect(screen.getByText('学习进度 0 / 11')).toBeTruthy()
    fireEvent.click(
      screen.getByRole('radio', { name: '先按用户和幂等键加锁，再查原订单' }),
    )
    fireEvent.click(screen.getAllByRole('checkbox')[0])
    fireEvent.change(screen.getByLabelText('我的修改与验证记录'), {
      target: { value: '补充换券测试；PG 集成未运行。' },
    })
    finishExperiment('返回原订单')
    view.unmount()
    render(<OrderLesson />)
    expect(screen.getByText('学习进度 3 / 11')).toBeTruthy()
    expect(
      (screen.getByLabelText('我的修改与验证记录') as HTMLTextAreaElement)
        .value,
    ).toBe('补充换券测试；PG 集成未运行。')
    expect(
      (screen.getAllByRole('checkbox')[0] as HTMLInputElement).checked,
    ).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '重置本章进度和笔记' }))
    expect(screen.getByText('学习进度 0 / 11')).toBeTruthy()
    expect(
      (screen.getByLabelText('我的修改与验证记录') as HTMLTextAreaElement)
        .value,
    ).toBe('')
    expect(
      parseProgress(window.localStorage.getItem(progressKey)).explored,
    ).toEqual([])
  })

  it('keeps the tutorial usable when browser storage is unavailable', () => {
    vi.spyOn(window.localStorage, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(window.localStorage, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    render(<OrderLesson />)
    expect(screen.getByText(/浏览器存储不可用/)).toBeTruthy()
    finishExperiment('返回原订单')
    expect(screen.getByText('学习进度 1 / 11')).toBeTruthy()
  })

  it('ignores corrupted, stale and duplicate saved progress', () => {
    expect(parseProgress('{oops').explored).toEqual([])
    expect(
      parseProgress(
        JSON.stringify({
          explored: ['replay', 'replay', 'obsolete'],
          answers: { 0: 99, 1: 0, 2: '2' },
          tasks: [0, 0, 9],
          notes: 123,
        }),
      ),
    ).toEqual({
      explored: ['replay'],
      answers: { 1: 0 },
      tasks: [0],
      notes: '',
    })
  })
})
