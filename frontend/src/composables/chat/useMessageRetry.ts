import type { ChatMessage } from '@/types/message'

export const RECALL_WINDOW_MS = 2 * 60 * 1000

export const RETRYABLE_SEND_ERROR_CODES = new Set<number>([
  14007, // 消息仍在处理，按原 client_msg_id 重试
  14008, // 消息已持久化但事件流暂不可用，可安全重试
  19001, // MySQL 不可用
  19002, // Redis 不可用
  18007, // WS 内部处理失败
])

export const NON_RETRYABLE_SEND_ERROR_CODES = new Set<number>([
  10000,
  10001,
  10002,
  12007,
  13001,
  13002,
  13003,
  14001,
  14003,
  14006,
  18002,
  18004,
  18005,
])

const DEFAULT_RETRY_DELAYS_MS = [1000, 2000, 4000]

export function isRetryableSendError(code?: number, retryableFlag?: boolean): boolean {
  if (typeof code === 'number') {
    if (RETRYABLE_SEND_ERROR_CODES.has(code)) return true
    if (NON_RETRYABLE_SEND_ERROR_CODES.has(code)) return false
  }
  return Boolean(retryableFlag)
}

export function canRecallMessage(
  msg: ChatMessage,
  currentUserId: string,
  nowMs: number = Date.now(),
  recallWindowMs: number = RECALL_WINDOW_MS,
): boolean {
  if (!currentUserId || msg.sender_id !== currentUserId) return false
  if (msg.status !== 'normal') return false
  if (msg.local_status === 'sending' || msg.local_status === 'failed') return false
  if (msg.msg_type === 'recall' || msg.msg_type === 'system') return false
  if (!msg.msg_id) return false
  const createdMs = Date.parse(msg.created_at)
  if (Number.isNaN(createdMs)) return false
  return nowMs - createdMs <= recallWindowMs
}

export interface RetryTask {
  clientMsgId: string
  reqId: string
  conversationId: string
  attempt: number
  timer: ReturnType<typeof setTimeout>
}

export function createRetryScheduler(
  onAttempt: (clientMsgId: string, reqId: string, conversationId: string, attempt: number) => void,
  onExhausted?: (clientMsgId: string, reqId: string, conversationId: string) => void,
  delaysMs: readonly number[] = DEFAULT_RETRY_DELAYS_MS,
) {
  const tasks = new Map<string, RetryTask>()
  const attemptCounts = new Map<string, number>()

  function schedule(clientMsgId: string, reqId: string, conversationId: string): boolean {
    const prevCount = attemptCounts.get(clientMsgId) ?? 0
    if (prevCount >= delaysMs.length) {
      cancel(clientMsgId)
      onExhausted?.(clientMsgId, reqId, conversationId)
      return false
    }

    const existing = tasks.get(clientMsgId)
    if (existing) {
      clearTimeout(existing.timer)
    }

    const nextAttempt = prevCount + 1
    attemptCounts.set(clientMsgId, nextAttempt)
    const delay = delaysMs[prevCount]
    const timer = setTimeout(() => {
      tasks.delete(clientMsgId)
      onAttempt(clientMsgId, reqId, conversationId, nextAttempt)
    }, delay)

    tasks.set(clientMsgId, {
      clientMsgId,
      reqId,
      conversationId,
      attempt: nextAttempt,
      timer,
    })
    return true
  }

  function cancel(clientMsgId: string): void {
    const existing = tasks.get(clientMsgId)
    if (existing) {
      clearTimeout(existing.timer)
      tasks.delete(clientMsgId)
    }
    attemptCounts.delete(clientMsgId)
  }

  function cancelAll(): void {
    for (const task of tasks.values()) {
      clearTimeout(task.timer)
    }
    tasks.clear()
    attemptCounts.clear()
  }

  function getAttemptCount(clientMsgId: string): number {
    return attemptCounts.get(clientMsgId) ?? 0
  }

  return {
    schedule,
    cancel,
    cancelAll,
    getAttemptCount,
  }
}
