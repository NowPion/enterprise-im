import assert from 'node:assert/strict'
import { mock } from 'node:test'
import {
  compareSeq,
  formatDurationMs,
  formatFileSize,
  generateClientMsgId,
  isPositiveSeq,
  maxSeq,
  minSeq,
  parseDecimalSeq,
} from '../src/composables/chat/seq.ts'
import {
  canRecallMessage,
  createRetryScheduler,
  isRetryableSendError,
  RECALL_WINDOW_MS,
} from '../src/composables/chat/useMessageRetry.ts'
import type { ChatMessage } from '../src/types/message.ts'

function chatMessage(overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    msg_id: '188900000000001',
    client_msg_id: 'cmsg-1',
    conversation_id: '90001',
    seq: '1',
    sender_id: '10001',
    msg_type: 'text',
    content: 'hello',
    extra: {},
    status: 'normal',
    created_at: new Date(1_764_120_000_000).toISOString(),
    recalled_at: null,
    delivery_status: 'sent',
    local_status: 'sent',
    ...overrides,
  }
}

describe('seq utilities and message retry/recall rules', () => {
  afterEach(() => mock.timers.reset())

  it('compares 64-bit decimal seq strings accurately without lexicographical or float precision bugs', () => {
    assert.equal(compareSeq('10', '2'), 1)
    assert.equal(compareSeq('1889000000000000002', '1889000000000000001'), 1)
    assert.equal(maxSeq('9', '100'), '100')
    assert.equal(minSeq('9', '100'), '9')
    assert.equal(parseDecimalSeq('abc'), 0n)
    assert.equal(parseDecimalSeq(-5), 0n)
    assert.equal(isPositiveSeq('0'), false)
    assert.equal(isPositiveSeq('1'), true)
    assert.match(generateClientMsgId(), /^[0-9a-f-]{20,}$/i)
    assert.equal(formatFileSize(1536), '1.5 KB')
    assert.equal(formatDurationMs(65_000), '1\'05"')
  })

  it('enforces retryable error codes, 2-minute recall window, and retry scheduler backoff/exhaustion', () => {
    assert.equal(isRetryableSendError(14007, false), true)
    assert.equal(isRetryableSendError(19001, false), true)
    assert.equal(isRetryableSendError(13001, true), false)
    assert.equal(isRetryableSendError(14006, true), false)

    const baseTime = 1_764_120_000_000
    assert.equal(canRecallMessage(chatMessage(), '10001', baseTime + RECALL_WINDOW_MS), true)
    assert.equal(canRecallMessage(chatMessage(), '10001', baseTime + RECALL_WINDOW_MS + 1), false)
    assert.equal(canRecallMessage(chatMessage({ sender_id: '10002' }), '10001', baseTime + 1000), false)
    assert.equal(canRecallMessage(chatMessage({ local_status: 'sending' }), '10001', baseTime + 1000), false)
    assert.equal(canRecallMessage(chatMessage({ status: 'recalled' }), '10001', baseTime + 1000), false)
    assert.equal(canRecallMessage(chatMessage({ msg_type: 'system' }), '10001', baseTime + 1000), false)

    mock.timers.enable({ apis: ['setTimeout'] })
    const attempts: number[] = []
    let exhausted = 0
    const scheduler = createRetryScheduler(
      (_cid, _rid, _conv, attempt) => attempts.push(attempt),
      () => {
        exhausted += 1
      },
      [100, 200],
    )

    assert.equal(scheduler.schedule('c1', 'r1', '90001'), true)
    mock.timers.tick(99)
    assert.deepEqual(attempts, [])
    mock.timers.tick(1)
    assert.deepEqual(attempts, [1])

    assert.equal(scheduler.schedule('c1', 'r1', '90001'), true)
    mock.timers.tick(200)
    assert.deepEqual(attempts, [1, 2])

    assert.equal(scheduler.schedule('c1', 'r1', '90001'), false)
    assert.equal(exhausted, 1)
    assert.equal(scheduler.getAttemptCount('c1'), 0)
  })
})