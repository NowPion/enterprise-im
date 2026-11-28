/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import request from '@/api/request'
import { useAuthStore } from '@/stores/auth'
import { useConversationStore } from '@/stores/conversation'
import { useMessageStore } from '@/stores/message'
import {
  apiSuccess,
  authenticateFakeTransport,
  createTestWSClient,
  extractConversationById,
  installInMemoryRequestAdapter,
  makeConversation,
  makeMessage,
  seedAuthUser,
  type TestConversation,
  type TestMessage,
} from './helpers'

describe('useMessageStore incremental updates, ordering, deduplication, and 4-stage receipts', () => {
  let restoreHttp: (() => void) | undefined
  let conversationsFixture: TestConversation[]
  let syncResponses: Record<string, TestMessage[]>

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    conversationsFixture = [
      makeConversation({
        id: '90001',
        last_seq: '2',
        last_msg_id: '188900000000002',
        last_delivered_seq: '0',
        last_read_seq: '0',
        unread_count: 0,
      }),
    ]
    syncResponses = {
      '90001': [],
    }
    const adapter = installInMemoryRequestAdapter(request, (method, url, _body, params) => {
      if (method === 'GET' && url.endsWith('/conversations')) {
        return { body: apiSuccess({ items: conversationsFixture, next_cursor: null, has_more: false }) }
      }
      const syncMatch = url.match(/\/conversations\/([^/]+)\/sync/)
      if (method === 'GET' && syncMatch) {
        const convId = syncMatch[1]
        const afterSeq = BigInt(String(params.after_seq ?? '0'))
        const all = syncResponses[convId] ?? []
        const items = all.filter(m => BigInt(m.seq) > afterSeq)
        const maxSeq = items.length > 0 ? items[items.length - 1].seq : String(afterSeq)
        return {
          body: apiSuccess({
            items,
            from_seq: String(afterSeq),
            to_seq: maxSeq,
            has_more: false,
          }),
        }
      }
      const messagesMatch = url.match(/\/conversations\/([^/]+)\/messages$/)
      if (method === 'GET' && messagesMatch) {
        const convId = messagesMatch[1]
        return { body: apiSuccess({ items: syncResponses[convId] ?? [], next_cursor: null, has_more: false }) }
      }
      if (method === 'POST' && /\/conversations\/[^/]+\/read$/.test(url)) {
        return { body: apiSuccess({ conversation_id: '90001', last_read_seq: '0', last_delivered_seq: '0', unread_count: 0, read_at: '2026-11-26T04:00:10.000Z' }) }
      }
      if (method === 'POST' && /\/conversations\/[^/]+\/delivered$/.test(url)) {
        return { body: apiSuccess({ conversation_id: '90001', last_delivered_seq: '10' }) }
      }
      const sendMatch = url.match(/\/conversations\/([^/]+)\/messages$/)
      if (method === 'POST' && sendMatch) {
        const payload = (_body ?? {}) as { client_msg_id?: string; msg_type?: TestMessage['msg_type']; content?: string; extra?: Record<string, unknown> }
        return {
          status: 201,
          body: apiSuccess({
            message: makeMessage({
              conversation_id: sendMatch[1],
              client_msg_id: payload.client_msg_id ?? 'cmsg-http-1',
              msg_type: payload.msg_type ?? 'text',
              content: payload.content ?? '',
              extra: payload.extra ?? {},
              seq: '99',
              delivery_status: 'sent',
            }),
          }),
        }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected route in test', data: null, request_id: 'err-1' } }
    })
    restoreHttp = adapter.restore
    seedAuthUser(useAuthStore(), '10001')
  })

  afterEach(() => {
    useMessageStore().reset()
    useConversationStore().reset()
    restoreHttp?.()
    restoreHttp = undefined
  })

  function setupConnectedStores() {
    const convStore = useConversationStore()
    const msgStore = useMessageStore()
    const { client, sockets } = createTestWSClient()
    msgStore.bindWSClient(client, () => '10001')
    client.connect()
    sockets[0].open()
    authenticateFakeTransport(sockets[0])
    return { convStore, msgStore, client, socket: sockets[0] }
  }

  function dispatchMsgPush(
    socket: ReturnType<typeof createTestWSClient>['sockets'][number],
    msg: TestMessage,
    deliveryStatus: 'sent' | 'delivered' | 'read' = 'sent',
  ) {
    const payload = {
      push_type: 'message',
      message: msg,
      delivery_status: deliveryStatus,
    }
    socket.receive({ type: 'msg.push', req_id: `push-${msg.msg_id}`, timestamp: Date.now(), data: payload })
  }

  function dispatchMsgAck(
    socket: ReturnType<typeof createTestWSClient>['sockets'][number],
    ack: { client_msg_id: string; msg_id: string; conversation_id: string; seq: string; status: 'normal' | 'recalled'; delivery_status: 'sent' | 'delivered' | 'read' },
    reqId = 'req-send-1',
  ) {
    socket.receive({ type: 'msg.ack', req_id: reqId, timestamp: Date.now(), data: ack })
  }

  function dispatchReceiptPush(
    socket: ReturnType<typeof createTestWSClient>['sockets'][number],
    receipt: {
      push_type: 'receipt'
      conversation_id: string
      user_id: string
      delivery_status: 'delivered' | 'read'
      msg_id: string | null
      seq: string | null
      last_delivered_seq: string
      last_read_seq: string
      updated_at: string
    },
  ) {
    socket.receive({ type: 'msg.push', req_id: `receipt-${Date.now()}`, timestamp: Date.now(), data: receipt })
  }

  it('inserts out-of-order messages and sorts strictly by integer seq (avoiding lexicographical "10" < "2")', async () => {
    const { msgStore, client, socket } = setupConnectedStores()

    for (const seq of ['10', '2', '9', '1']) {
      dispatchMsgPush(socket, makeMessage({ conversation_id: '90001', seq, sender_id: '10002' }))
    }

    const list = msgStore.getMessages('90001')
    expect(list.map(m => m.seq)).toEqual(['1', '2', '9', '10'])
    client.disconnect()
  })

  it('deduplicates by client_msg_id and msg_id across optimistic sending, msg.ack, and msg.push replay', async () => {
    const { msgStore, client, socket } = setupConnectedStores()

    const optimistic = await msgStore.sendMessage(
      '90001',
      {
        msg_type: 'text',
        content: 'idempotent hello',
        extra: {},
      },
      '10001',
    )
    const clientMsgId = optimistic.client_msg_id

    let list = msgStore.getMessages('90001')
    expect(list).toHaveLength(1)
    expect(list[0].client_msg_id).toBe(clientMsgId)
    expect(list[0].local_status).toBe('sending')

    const sentReqId = socket.sent
      .map(s => JSON.parse(s) as { type: string; req_id: string; data: { client_msg_id?: string } })
      .find(f => f.type === 'msg.send' && f.data.client_msg_id === clientMsgId)?.req_id
    expect(sentReqId).toBeTruthy()

    dispatchMsgAck(
      socket,
      {
        client_msg_id: clientMsgId,
        msg_id: '188900000000005',
        conversation_id: '90001',
        seq: '5',
        status: 'normal',
        delivery_status: 'sent',
      },
      sentReqId!,
    )

    list = msgStore.getMessages('90001')
    expect(list).toHaveLength(1)
    expect(list[0].msg_id).toBe('188900000000005')
    expect(list[0].seq).toBe('5')
    expect(list[0].local_status).toBe('sent')
    expect(list[0].delivery_status).toBe('sent')

    // Server also delivers msg.push for the same message (plus a duplicate Stream replay)
    const serverMsg = makeMessage({
      msg_id: '188900000000005',
      client_msg_id: clientMsgId,
      conversation_id: '90001',
      seq: '5',
      sender_id: '10001',
      content: 'idempotent hello',
      delivery_status: 'sent',
    })
    dispatchMsgPush(socket, serverMsg, 'sent')
    dispatchMsgPush(socket, serverMsg, 'sent')

    list = msgStore.getMessages('90001')
    expect(list).toHaveLength(1)
    expect(list[0].msg_id).toBe('188900000000005')
    client.disconnect()
  })

  it('advances 4-stage status sending -> sent -> delivered -> read monotonically and blocks regression', async () => {
    const { msgStore, client, socket } = setupConnectedStores()

    const m8 = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'msg-8', extra: {} }, '10001')
    const m10 = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'msg-10', extra: {} }, '10001')
    const m11 = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'msg-11', extra: {} }, '10001')

    expect(msgStore.getMessages('90001').map(m => m.local_status)).toEqual(['sending', 'sending', 'sending'])

    const sendFrames = socket.sent
      .map(s => JSON.parse(s) as { type: string; req_id: string; data: { client_msg_id?: string } })
      .filter(f => f.type === 'msg.send')
    const reqIdFor = (cid: string) => sendFrames.find(f => f.data.client_msg_id === cid)?.req_id ?? `req-${cid}`

    dispatchMsgAck(socket, {
      client_msg_id: m8.client_msg_id,
      msg_id: '188900000000008',
      conversation_id: '90001',
      seq: '8',
      status: 'normal',
      delivery_status: 'sent',
    }, reqIdFor(m8.client_msg_id))
    dispatchMsgAck(socket, {
      client_msg_id: m10.client_msg_id,
      msg_id: '188900000000010',
      conversation_id: '90001',
      seq: '10',
      status: 'normal',
      delivery_status: 'sent',
    }, reqIdFor(m10.client_msg_id))
    dispatchMsgAck(socket, {
      client_msg_id: m11.client_msg_id,
      msg_id: '188900000000011',
      conversation_id: '90001',
      seq: '11',
      status: 'normal',
      delivery_status: 'sent',
    }, reqIdFor(m11.client_msg_id))

    let list = msgStore.getMessages('90001')
    expect(list.find(m => m.seq === '10')?.local_status).toBe('sent')
    expect(list.find(m => m.seq === '10')?.delivery_status).toBe('sent')

    // Stage 2: delivered receipt for seq 10
    dispatchReceiptPush(socket, {
      push_type: 'receipt',
      conversation_id: '90001',
      user_id: '10002',
      delivery_status: 'delivered',
      msg_id: '188900000000010',
      seq: '10',
      last_delivered_seq: '10',
      last_read_seq: '0',
      updated_at: '2026-11-26T04:00:02.000Z',
    })
    list = msgStore.getMessages('90001')
    expect(list.find(m => m.seq === '10')?.local_status).toBe('delivered')
    expect(list.find(m => m.seq === '10')?.delivery_status).toBe('delivered')
    expect(list.find(m => m.seq === '11')?.local_status).toBe('sent')

    // Stage 3: conversation-level read receipt up to last_read_seq='10' (msg_id/seq are null per docs/api.md)
    dispatchReceiptPush(socket, {
      push_type: 'receipt',
      conversation_id: '90001',
      user_id: '10002',
      delivery_status: 'read',
      msg_id: null,
      seq: null,
      last_delivered_seq: '10',
      last_read_seq: '10',
      updated_at: '2026-11-26T04:00:05.000Z',
    })
    list = msgStore.getMessages('90001')
    expect(list.find(m => m.seq === '8')?.local_status).toBe('read')
    expect(list.find(m => m.seq === '8')?.delivery_status).toBe('read')
    expect(list.find(m => m.seq === '10')?.local_status).toBe('read')
    expect(list.find(m => m.seq === '10')?.delivery_status).toBe('read')
    expect(list.find(m => m.seq === '11')?.local_status).toBe('sent')

    // Regression guard: delayed delivered receipt & stale sent ACK must NOT downgrade read -> delivered or sent
    dispatchReceiptPush(socket, {
      push_type: 'receipt',
      conversation_id: '90001',
      user_id: '10002',
      delivery_status: 'delivered',
      msg_id: '188900000000010',
      seq: '10',
      last_delivered_seq: '10',
      last_read_seq: '0',
      updated_at: '2026-11-26T04:00:01.000Z',
    })
    dispatchMsgAck(socket, {
      client_msg_id: m10.client_msg_id,
      msg_id: '188900000000010',
      conversation_id: '90001',
      seq: '10',
      status: 'normal',
      delivery_status: 'sent',
    }, reqIdFor(m10.client_msg_id))

    list = msgStore.getMessages('90001')
    expect(list.find(m => m.seq === '8')?.local_status).toBe('read')
    expect(list.find(m => m.seq === '10')?.local_status).toBe('read')
    expect(list.find(m => m.seq === '10')?.delivery_status).toBe('read')
    client.disconnect()
  })

  it('merges incremental /sync (after_seq) results cleanly without duplicates and updates conversation last_seq', async () => {
    const { convStore, msgStore, client, socket } = setupConnectedStores()
    await convStore.fetchList()

    dispatchMsgPush(socket, makeMessage({ conversation_id: '90001', seq: '1', sender_id: '10002' }))
    dispatchMsgPush(socket, makeMessage({ conversation_id: '90001', seq: '2', sender_id: '10002' }))

    syncResponses['90001'] = [
      makeMessage({ conversation_id: '90001', seq: '2', sender_id: '10002' }),
      makeMessage({ conversation_id: '90001', seq: '3', sender_id: '10002' }),
      makeMessage({ conversation_id: '90001', seq: '4', sender_id: '10002' }),
    ]

    await msgStore.syncConversation('90001', '10001', '2')

    const list = msgStore.getMessages('90001')
    expect(list.map(m => m.seq)).toEqual(['1', '2', '3', '4'])
    const conv = extractConversationById(convStore, '90001')
    expect(conv?.last_seq).toBe('4')
    client.disconnect()
  })

  it('updates recalled message status in place when msg.recall or message.recalled arrives', async () => {
    const { msgStore, client, socket } = setupConnectedStores()
    const original = makeMessage({
      msg_id: '188900000000020',
      client_msg_id: 'cmsg-recall-target',
      conversation_id: '90001',
      seq: '20',
      sender_id: '10001',
      status: 'normal',
      content: 'to be recalled',
    })
    dispatchMsgPush(socket, original)

    socket.receive({
      type: 'msg.recall',
      req_id: 'recall-1',
      timestamp: Date.now(),
      data: {
        conversation_id: '90001',
        recalled_msg_id: '188900000000020',
        recall_msg_id: '188900000000021',
        seq: '21',
        recalled_at: '2026-11-26T04:01:00.000Z',
      },
    })

    let list = msgStore.getMessages('90001')
    let target = list.find(m => m.msg_id === '188900000000020')
    expect(target?.status).toBe('recalled')
    expect(target?.recalled_at).toBe('2026-11-26T04:01:00.000Z')

    // Also verify system.notice message.recalled updates another message in place
    dispatchMsgPush(
      socket,
      makeMessage({
        msg_id: '188900000000022',
        client_msg_id: 'cmsg-recall-notice-target',
        conversation_id: '90001',
        seq: '22',
        sender_id: '10002',
        status: 'normal',
        content: 'peer message to recall',
      }),
    )
    socket.receive({
      type: 'system.notice',
      req_id: 'notice-recall-1',
      timestamp: Date.now(),
      data: {
        event: 'message.recalled',
        notice_id: 'n-recall-1',
        conversation_id: '90001',
        actor_id: '10002',
        target_user_id: null,
        payload: {
          recalled_msg_id: '188900000000022',
          recalled_at: '2026-11-26T04:02:00.000Z',
        },
      },
    })
    list = msgStore.getMessages('90001')
    target = list.find(m => m.msg_id === '188900000000022')
    expect(target?.status).toBe('recalled')
    expect(target?.recalled_at).toBe('2026-11-26T04:02:00.000Z')
    client.disconnect()
  })

  it('paginates loadInitialMessages and loadOlderMessages, deduplicates concurrent loads, and triggers delivered/read watermarks', async () => {
    const convStore = useConversationStore()
    const msgStore = useMessageStore()
    const messageCalls: Array<{ before_seq?: unknown }> = []
    const deliveredCalls: string[] = []
    const readCalls: string[] = []

    const adapter = installInMemoryRequestAdapter(request, (method, url, body, params) => {
      if (method === 'GET' && url.endsWith('/conversations')) {
        return {
          body: apiSuccess({
            items: [
              makeConversation({
                id: '90001',
                last_seq: '5',
                last_delivered_seq: '2',
                last_read_seq: '1',
                unread_count: 3,
              }),
            ],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'GET' && /\/conversations\/90001\/messages$/.test(url)) {
        messageCalls.push({ before_seq: params.before_seq })
        if (!params.before_seq) {
          return {
            body: apiSuccess({
              items: [
                makeMessage({ conversation_id: '90001', seq: '4', sender_id: '10002' }),
                makeMessage({ conversation_id: '90001', seq: '5', sender_id: '10002' }),
              ],
              next_cursor: '4',
              has_more: true,
            }),
          }
        }
        return {
          body: apiSuccess({
            items: [
              makeMessage({ conversation_id: '90001', seq: '2', sender_id: '10002' }),
              makeMessage({ conversation_id: '90001', seq: '3', sender_id: '10002' }),
            ],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'POST' && /\/conversations\/90001\/delivered$/.test(url)) {
        const seq = String((body as { last_delivered_seq?: string })?.last_delivered_seq ?? '0')
        deliveredCalls.push(seq)
        return { body: apiSuccess({ conversation_id: '90001', last_delivered_seq: seq, last_read_seq: '1', unread_count: 3, delivered_at: '2026-11-26T04:00:05.000Z' }) }
      }
      if (method === 'POST' && /\/conversations\/90001\/read$/.test(url)) {
        const seq = String((body as { last_read_seq?: string })?.last_read_seq ?? '0')
        readCalls.push(seq)
        return { body: apiSuccess({ conversation_id: '90001', last_read_seq: seq, last_delivered_seq: seq, unread_count: 0, read_at: '2026-11-26T04:00:06.000Z' }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected route', data: null, request_id: 'err' } }
    })

    try {
      await convStore.fetchList()
      convStore.setActiveConversation('90001')
      convStore.setChatVisible(true)

      await Promise.all([
        msgStore.loadInitialMessages('90001', '10001'),
        msgStore.loadInitialMessages('90001', '10001'),
      ])

      expect(messageCalls).toHaveLength(1)
      expect(msgStore.getMessages('90001').map(m => m.seq)).toEqual(['4', '5'])
      expect(msgStore.getPagination('90001')).toEqual({
        hasMoreOlder: true,
        oldestSeq: '4',
        loadingOlder: false,
        initialized: true,
      })
      expect(deliveredCalls).toEqual(['5'])
      expect(readCalls).toEqual(['5'])
      expect(extractConversationById(convStore, '90001')?.unread_count).toBe(0)

      const loadedMore = await msgStore.loadOlderMessages('90001', '10001')
      expect(loadedMore).toBe(true)
      expect(messageCalls[1].before_seq).toBe('4')
      expect(msgStore.getMessages('90001').map(m => m.seq)).toEqual(['2', '3', '4', '5'])
      expect(msgStore.getPagination('90001').oldestSeq).toBe('2')
      expect(msgStore.getPagination('90001').hasMoreOlder).toBe(false)

      const noMore = await msgStore.loadOlderMessages('90001', '10001')
      expect(noMore).toBe(false)
      expect(messageCalls).toHaveLength(2)
    } finally {
      adapter.restore()
    }
  })

  it('handles retryable vs non-retryable WS send errors, retryScheduler exhaustion, manual retryMessage, and resendPendingMessages', async () => {
    vi.useFakeTimers()
    try {
      const { msgStore, client, socket } = setupConnectedStores()

      // 1) Retryable WS error (14007): schedules 1000ms -> 2000ms -> 4000ms -> fails on 4th
      const retryableMsg = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'flaky', extra: {} }, '10001')
      const findSendFrames = () =>
        socket.sent
          .map(s => JSON.parse(s) as { type: string; req_id: string; data: { client_msg_id?: string } })
          .filter(f => f.type === 'msg.send' && f.data.client_msg_id === retryableMsg.client_msg_id)

      expect(findSendFrames()).toHaveLength(1)
      const firstReqId = findSendFrames()[0].req_id

      for (const delay of [1000, 2000, 4000]) {
        socket.receive({
          type: 'error',
          req_id: `err-${delay}`,
          timestamp: Date.now(),
          data: { code: 14007, message: 'busy', retryable: true, related_type: 'msg.send', related_req_id: firstReqId },
        })
        expect(msgStore.getMessages('90001')[0].local_status).toBe('sending')
        await vi.advanceTimersByTimeAsync(delay)
      }
      expect(findSendFrames()).toHaveLength(4)

      // 4th retryable error exhausts retryScheduler -> marks failed & retryable
      socket.receive({
        type: 'error',
        req_id: 'err-exhausted',
        timestamp: Date.now(),
        data: { code: 14007, message: 'still busy', retryable: true, related_type: 'msg.send', related_req_id: firstReqId },
      })
      let failed = msgStore.getMessages('90001').find(m => m.client_msg_id === retryableMsg.client_msg_id)
      expect(failed?.local_status).toBe('failed')
      expect(failed?.retryable).toBe(true)

      // Manual retry resets to sending and preserves original client_msg_id
      await msgStore.retryMessage('90001', retryableMsg.client_msg_id, '10001')
      expect(findSendFrames()).toHaveLength(5)
      expect(msgStore.getMessages('90001').find(m => m.client_msg_id === retryableMsg.client_msg_id)?.local_status).toBe('sending')

      // 2) Non-retryable error (13001) fails immediately and removes from WS pending queue
      const rejectedMsg = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'forbidden', extra: {} }, '10001')
      const rejectedReqId = socket.sent
        .map(s => JSON.parse(s) as { type: string; req_id: string; data: { client_msg_id?: string } })
        .find(f => f.type === 'msg.send' && f.data.client_msg_id === rejectedMsg.client_msg_id)!.req_id

      expect(client.hasPending(rejectedReqId)).toBe(true)
      socket.receive({
        type: 'error',
        req_id: 'err-non-retry',
        timestamp: Date.now(),
        data: { code: 13001, message: 'not a member', retryable: false, related_type: 'msg.send', related_req_id: rejectedReqId },
      })
      const rejectedState = msgStore.getMessages('90001').find(m => m.client_msg_id === rejectedMsg.client_msg_id)
      expect(rejectedState?.local_status).toBe('failed')
      expect(rejectedState?.retryable).toBe(false)
      expect(client.hasPending(rejectedReqId)).toBe(false)

      // 3) resendPendingMessages re-enqueues sending messages whose req_id is missing from WS pending queue
      client.removePending(firstReqId)
      const resent = msgStore.resendPendingMessages()
      expect(resent).toBe(1)
      expect(client.hasPending(firstReqId)).toBe(true)
      client.disconnect()
    } finally {
      vi.useRealTimers()
    }
  })

  it('supports HTTP fallback for sendMessage/recallMessage and rejects WS recall errors without duplicate HTTP calls', async () => {
    const convStore = useConversationStore()
    const msgStore = useMessageStore()
    const httpCalls: string[] = []

    const adapter = installInMemoryRequestAdapter(request, (method, url, body) => {
      httpCalls.push(`${method} ${url}`)
      if (method === 'GET' && url.endsWith('/conversations')) {
        return { body: apiSuccess({ items: [makeConversation({ id: '90001', last_seq: '5' })], next_cursor: null, has_more: false }) }
      }
      if (method === 'POST' && url.endsWith('/conversations/90001/messages')) {
        const p = body as { client_msg_id: string; msg_type: TestMessage['msg_type']; content: string }
        return {
          status: 201,
          body: apiSuccess({
            message: makeMessage({
              msg_id: '188900000000050',
              client_msg_id: p.client_msg_id,
              conversation_id: '90001',
              seq: '50',
              sender_id: '10001',
              msg_type: p.msg_type,
              content: p.content,
              delivery_status: 'sent',
            }),
          }),
        }
      }
      if (method === 'POST' && /\/conversations\/90001\/messages\/188900000000050\/recall$/.test(url)) {
        return {
          body: apiSuccess({
            recalled_message: makeMessage({
              msg_id: '188900000000050',
              client_msg_id: 'cmsg-http-send',
              conversation_id: '90001',
              seq: '50',
              sender_id: '10001',
              status: 'recalled',
              recalled_at: '2026-11-26T04:10:00.000Z',
            }),
            recall_message: makeMessage({
              msg_id: '188900000000051',
              client_msg_id: 'cmsg-http-recall',
              conversation_id: '90001',
              seq: '51',
              sender_id: '10001',
              msg_type: 'recall',
              content: '你撤回了一条消息',
              extra: { recalled_msg_id: '188900000000050', recalled_seq: '50' },
            }),
          }),
        }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })

    try {
      await convStore.fetchList()

      // Without boundWSClient -> sendMessage and recallMessage use HTTP fallback
      const sent = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'via http', client_msg_id: 'cmsg-http-send' }, '10001')
      expect(sent.msg_id).toBe('188900000000050')
      expect(sent.local_status).toBe('sent')
      expect(extractConversationById(convStore, '90001')?.last_seq).toBe('50')

      await msgStore.recallMessage('90001', '188900000000050', '10001')
      expect(msgStore.getMessages('90001').find(m => m.msg_id === '188900000000050')?.status).toBe('recalled')
      expect(msgStore.getMessages('90001').some(m => m.msg_id === '188900000000051')).toBe(true)

      // With WS connected -> recallMessage sends msg.recall and rejects explicitly on WS error without HTTP fallback
      const { client, sockets } = createTestWSClient()
      msgStore.bindWSClient(client, () => '10001')
      client.connect()
      sockets[0].open()
      authenticateFakeTransport(sockets[0])

      const beforeHttpCount = httpCalls.length
      const recallPromise = msgStore.recallMessage('90001', '188900000000052', '10001')
      const recallReqId = sockets[0].sent
        .map(s => JSON.parse(s) as { type: string; req_id: string })
        .find(f => f.type === 'msg.recall')!.req_id
      sockets[0].receive({
        type: 'error',
        req_id: 'err-recall',
        timestamp: Date.now(),
        data: { code: 14006, message: '超过可撤回时间窗', related_type: 'msg.recall', related_req_id: recallReqId },
      })
      await expect(recallPromise).rejects.toThrow('超过可撤回时间窗')
      expect(httpCalls).toHaveLength(beforeHttpCount)
      client.disconnect()
    } finally {
      adapter.restore()
    }
  })

  it('syncs multi-device self receipts, applies pre-existing peer watermarks, buffers realtime pushes, and paginates sync/receipts', async () => {
    const convStore = useConversationStore()
    const msgStore = useMessageStore()

    const adapter = installInMemoryRequestAdapter(request, (method, url, _body, params) => {
      if (method === 'GET' && url.endsWith('/conversations')) {
        return {
          body: apiSuccess({
            items: [makeConversation({ id: '90001', last_seq: '10', last_read_seq: '5', unread_count: 5 })],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'GET' && /\/conversations\/90001\/sync$/.test(url)) {
        const afterSeq = String(params.after_seq ?? '0')
        if (afterSeq === '0') {
          return {
            body: apiSuccess({
              items: [makeMessage({ conversation_id: '90001', msg_id: '188900000000014', seq: '14', sender_id: '10001' })],
              from_seq: '0',
              to_seq: '14',
              has_more: true,
            }),
          }
        }
        return {
          body: apiSuccess({
            items: [
              makeMessage({ conversation_id: '90001', msg_id: '188900000000015', seq: '15', sender_id: '10001' }),
              makeMessage({ conversation_id: '90001', msg_id: '188900000000016', seq: '16', sender_id: '10001' }),
            ],
            from_seq: '14',
            to_seq: '16',
            has_more: false,
          }),
        }
      }
      if (method === 'POST' && /\/conversations\/90001\/delivered$/.test(url)) {
        return { body: apiSuccess({ conversation_id: '90001', last_delivered_seq: '16' }) }
      }
      if (method === 'GET' && /\/conversations\/90001\/messages\/188900000000016\/receipts$/.test(url)) {
        if (!params.cursor) {
          return {
            body: apiSuccess({
              msg_id: '188900000000016',
              conversation_id: '90001',
              seq: '16',
              total_members: 3,
              delivered_count: 2,
              read_count: 1,
              unread_count: 1,
              items: [{ user: { id: '10002', username: 'u2', nickname: 'U2', avatar_url: '', signature: '', online: true, last_online_at: null }, role: 'member', delivery_status: 'read', last_delivered_seq: '16', last_read_seq: '16', delivered_at: '2026-11-26T04:00:01Z', read_at: '2026-11-26T04:00:02Z' }],
              next_cursor: 'cur-2',
              has_more: true,
            }),
          }
        }
        return {
          body: apiSuccess({
            msg_id: '188900000000016',
            conversation_id: '90001',
            seq: '16',
            total_members: 3,
            delivered_count: 2,
            read_count: 1,
            unread_count: 1,
            items: [{ user: { id: '10003', username: 'u3', nickname: 'U3', avatar_url: '', signature: '', online: false, last_online_at: null }, role: 'member', delivery_status: 'delivered', last_delivered_seq: '16', last_read_seq: '0', delivered_at: '2026-11-26T04:00:01Z', read_at: null }],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })

    try {
      await convStore.fetchList()
      const { client, sockets } = createTestWSClient()
      msgStore.bindWSClient(client, () => '10001')
      client.connect()
      sockets[0].open()
      authenticateFakeTransport(sockets[0])

      // Same-account multi-device read receipt updates local conversation watermark & clears unread
      dispatchReceiptPush(socketFrom(sockets[0]), {
        push_type: 'receipt',
        conversation_id: '90001',
        user_id: '10001',
        delivery_status: 'read',
        msg_id: null,
        seq: null,
        last_delivered_seq: '10',
        last_read_seq: '10',
        updated_at: '2026-11-26T04:00:00Z',
      })
      expect(extractConversationById(convStore, '90001')?.unread_count).toBe(0)
      expect(extractConversationById(convStore, '90001')?.last_read_seq).toBe('10')

      // Peer watermark arrives BEFORE messages 14,15,16 are synced
      dispatchReceiptPush(socketFrom(sockets[0]), {
        push_type: 'receipt',
        conversation_id: '90001',
        user_id: '10002',
        delivery_status: 'read',
        msg_id: null,
        seq: null,
        last_delivered_seq: '15',
        last_read_seq: '15',
        updated_at: '2026-11-26T04:01:00Z',
      })

      // Multi-page sync inserts 14, 15, 16; 14 & 15 inherit 'read', 16 remains 'sent'
      const count = await msgStore.syncConversation('90001', '10001', '0')
      expect(count).toBe(3)
      const list = msgStore.getMessages('90001')
      expect(list.find(m => m.seq === '14')?.local_status).toBe('read')
      expect(list.find(m => m.seq === '15')?.local_status).toBe('read')
      expect(list.find(m => m.seq === '16')?.local_status).toBe('sent')

      // Realtime buffering holds incoming msg.push until flushRealtimeBuffer
      msgStore.beginRealtimeBuffering()
      dispatchMsgPush(sockets[0], makeMessage({ conversation_id: '90001', seq: '17', sender_id: '10002' }))
      expect(msgStore.getMessages('90001')).toHaveLength(3)
      msgStore.flushRealtimeBuffer()
      expect(msgStore.getMessages('90001')).toHaveLength(4)
      expect(msgStore.getMessages('90001')[3].seq).toBe('17')

      // Receipts pagination appends second page
      await msgStore.loadMessageReceipts('90001', '188900000000016', { reset: true })
      expect(msgStore.receiptsModalData?.items).toHaveLength(1)
      await msgStore.loadMessageReceipts('90001', '188900000000016', { cursor: msgStore.receiptsModalData?.next_cursor ?? undefined })
      expect(msgStore.receiptsModalData?.items.map(i => i.user.id)).toEqual(['10002', '10003'])

      client.disconnect()
    } finally {
      adapter.restore()
    }

    function socketFrom<T>(s: T): T {
      return s
    }
  })
})
