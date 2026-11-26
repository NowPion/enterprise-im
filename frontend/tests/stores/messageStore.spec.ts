/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
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
})
