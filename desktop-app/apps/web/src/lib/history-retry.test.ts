import { expect, test } from 'bun:test'
import { historyActionEnabled, historyKey, restorePendingHistory } from './history-retry'

test('unknown history retry is restricted to the original session, action and turn', () => {
  const pending = new Map([[historyKey('session', 'fork', 3), 'operation']])
  expect(historyActionEnabled(pending, 'session', 'fork', 3, false)).toBe(true)
  expect(historyActionEnabled(pending, 'session', 'fork', 2, true)).toBe(false)
  expect(historyActionEnabled(pending, 'session', 'rewind', 3, true)).toBe(false)
  expect(historyActionEnabled(pending, 'other', 'fork', 3, false)).toBe(false)
  pending.clear() // success or confirmed refusal ends the retry exception
  expect(historyActionEnabled(pending, 'session', 'fork', 3, false)).toBe(false)
  expect(historyActionEnabled(pending, 'session', 'fork', 3, true)).toBe(true)
})

test('read-only attach restores an exact durable operation after client restart without a runtime', () => {
  const pending = new Map<string, string>()
  const operationId = '10000000-0000-4000-8000-000000000002'
  const attached = { runtimeId: null, pendingHistory: { kind: 'rewind', turnCount: 4, operationId } }
  expect(restorePendingHistory(pending, 'session', attached.pendingHistory)).toBe(true)
  expect(historyActionEnabled(pending, 'session', 'rewind', 4, false)).toBe(true)
  expect(pending.get(historyKey('session', 'rewind', 4))).toBe(operationId)
  expect(historyActionEnabled(pending, 'session', 'fork', 4, true)).toBe(false)
  expect(historyActionEnabled(pending, 'session', 'rewind', 5, true)).toBe(false)
  expect(restorePendingHistory(pending, 'other', { ...attached.pendingHistory, kind: 'delete' })).toBe(false)
  expect(historyActionEnabled(pending, 'other', 'rewind', 4, false)).toBe(false)
})
