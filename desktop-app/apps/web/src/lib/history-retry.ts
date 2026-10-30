// An uncertain mutation may be retried only under its original identity.
export type HistoryAction = 'fork' | 'rewind'
export type PendingHistory = { kind: string; turnCount: number; operationId: string }

export function historyKey(sessionId: string, kind: HistoryAction, turnCount: number): string {
  return `${sessionId}:${kind}:${turnCount}`
}

export function hasPendingHistoryForSession(pending: ReadonlyMap<string, string>, sessionId: string): boolean {
  return [...pending.keys()].some((key) => key.startsWith(`${sessionId}:`))
}

export function historyActionEnabled(
  pending: ReadonlyMap<string, string>, sessionId: string, kind: HistoryAction, turnCount: number, negotiated: boolean,
): boolean {
  return hasPendingHistoryForSession(pending, sessionId)
    ? pending.has(historyKey(sessionId, kind, turnCount))
    : negotiated
}

export function restorePendingHistory(pending: Map<string, string>, sessionId: string, operation: PendingHistory | null | undefined): boolean {
  if (!operation || !['fork', 'rewind'].includes(operation.kind)
    || !Number.isSafeInteger(operation.turnCount) || operation.turnCount < 1
    || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu.test(operation.operationId)) return false
  for (const key of pending.keys()) {
    if (key.startsWith(`${sessionId}:`)) pending.delete(key)
  }
  pending.set(historyKey(sessionId, operation.kind as HistoryAction, operation.turnCount), operation.operationId)
  return true
}
