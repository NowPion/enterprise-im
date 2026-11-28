/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import request from '@/api/request'
import {
  ensureChatRealtime,
  reconcileChatOnReconnect,
  stopChatRealtime,
  useChatRealtime,
} from '@/composables/chat/useChatRealtime'
import { useAuthStore } from '@/stores/auth'
import { useConversationStore } from '@/stores/conversation'
import { useMessageStore } from '@/stores/message'
import { resetSharedWSClient } from '@/utils/wsClient'
import {
  apiSuccess,
  authenticateFakeTransport,
  FakeTransport,
  installInMemoryRequestAdapter,
  makeConversation,
  makeMessage,
  makeTestJwt,
  seedAuthUser,
  type TestConversation,
  type TestMessage,
} from './helpers'

describe('useChatRealtime reconnect reconciliation, buffering, offline/online recovery, and account isolation', () => {
  let restoreHttp: (() => void) | undefined
  let restoreWs: (() => void) | undefined
  let sockets: FakeTransport[]
  let serverConversations: TestConversation[]
  let serverMessagesByConv: Record<string, TestMessage[]>
  let fetchListGate: (() => void) | null

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    stopChatRealtime()
    resetSharedWSClient()
    sockets = []
    fetchListGate = null

    const PreviousWebSocket = globalThis.WebSocket
    globalThis.WebSocket = class extends FakeTransport {
      constructor(_url: string | URL) {
        super()
        sockets.push(this)
      }
    } as unknown as typeof WebSocket
    restoreWs = () => {
      globalThis.WebSocket = PreviousWebSocket
    }

    serverConversations = [
      makeConversation({
        id: '90001',
        title: 'Active Chat',
        last_seq: '2',
        last_msg_id: '188900000000002',
        last_delivered_seq: '2',
        last_read_seq: '2',
        unread_count: 0,
      }),
      makeConversation({
        id: '90002',
        title: 'Background Chat',
        last_seq: '10',
        last_msg_id: '188900000000010',
        last_delivered_seq: '10',
        last_read_seq: '10',
        unread_count: 0,
      }),
    ]
    serverMessagesByConv = {
      '90001': [
        makeMessage({ conversation_id: '90001', seq: '1', sender_id: '10002', content: 'm1' }),
        makeMessage({ conversation_id: '90001', seq: '2', sender_id: '10002', content: 'm2' }),
      ],
      '90002': [
        makeMessage({ conversation_id: '90002', seq: '10', sender_id: '10002', content: 'bg10' }),
      ],
    }

    const adapter = installInMemoryRequestAdapter(request, (method, url, body, params) => {
      if (method === 'GET' && url.endsWith('/conversations')) {
        if (fetchListGate) {
          // Return synchronously in adapter? Adapter supports async, wait: our helper calls responder synchronously.
        }
        return { body: apiSuccess({ items: serverConversations, next_cursor: null, has_more: false }) }
      }
      const msgMatch = url.match(/\/conversations\/([^/]+)\/messages$/)
      if (method === 'GET' && msgMatch) {
        return { body: apiSuccess({ items: serverMessagesByConv[msgMatch[1]] ?? [], next_cursor: null, has_more: false }) }
      }
      const syncMatch = url.match(/\/conversations\/([^/]+)\/sync$/)
      if (method === 'GET' && syncMatch) {
        const convId = syncMatch[1]
        const afterSeq = BigInt(String(params.after_seq ?? '0'))
        const items = (serverMessagesByConv[convId] ?? []).filter(m => BigInt(m.seq) > afterSeq)
        const toSeq = items.length > 0 ? items[items.length - 1].seq : String(afterSeq)
        return { body: apiSuccess({ items, from_seq: String(afterSeq), to_seq: toSeq, has_more: false }) }
      }
      const delivMatch = url.match(/\/conversations\/([^/]+)\/delivered$/)
      if (method === 'POST' && delivMatch) {
        const seq = String((body as { last_delivered_seq?: string })?.last_delivered_seq ?? '0')
        return { body: apiSuccess({ conversation_id: delivMatch[1], last_delivered_seq: seq, last_read_seq: '0', unread_count: 0, delivered_at: '2026-11-26T05:00:00Z' }) }
      }
      const readMatch = url.match(/\/conversations\/([^/]+)\/read$/)
      if (method === 'POST' && readMatch) {
        const seq = String((body as { last_read_seq?: string })?.last_read_seq ?? '0')
        return { body: apiSuccess({ conversation_id: readMatch[1], last_read_seq: seq, last_delivered_seq: seq, unread_count: 0, read_at: '2026-11-26T05:00:00Z' }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })
    restoreHttp = adapter.restore
  })

  afterEach(() => {
    stopChatRealtime()
    resetSharedWSClient()
    restoreHttp?.()
    restoreHttp = undefined
    restoreWs?.()
    restoreWs = undefined
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
    localStorage.clear()
  })

  function seedSession(userId = '10001') {
    const authStore = useAuthStore()
    seedAuthUser(authStore, userId)
    const token = makeTestJwt(userId)
    authStore.tokens = {
      access_token: token,
      refresh_token: `rt-${userId}`,
      token_type: 'Bearer',
      expires_in: 900,
      refresh_expires_in: 604800,
      session_id: '501',
      device_id: 'dev-web-1',
    }
    return authStore
  }

  it('reconciles offline gaps on reconnect, resends pending sending messages, and buffers live pushes without double-counting unread', async () => {
    seedSession('10001')
    const convStore = useConversationStore()
    const msgStore = useMessageStore()
    convStore.setActiveConversation('90001')
    convStore.setChatVisible(true)

    const client = ensureChatRealtime()!
    expect(client).toBeTruthy()
    sockets[0].open()
    authenticateFakeTransport(sockets[0])
    await reconcileChatOnReconnect()

    expect(msgStore.getMessages('90001').map(m => m.seq)).toEqual(['1', '2'])

    // Queue an un-acked local message while disconnected, then drop connection
    sockets[0].close()
    const pendingMsg = await msgStore.sendMessage('90001', { msg_type: 'text', content: 'offline-outbox', extra: {} }, '10001')
    expect(pendingMsg.local_status).toBe('sending')

    // Remove from WSClient pending queue to verify reconcileChatOnReconnect -> resendPendingMessages restores it
    client.removePending(pendingMsg.req_id!)
    expect(client.hasPending(pendingMsg.req_id!)).toBe(false)

    // Meanwhile server received 2 offline messages in background conversation 90002 (seq 11, 12) and 1 in active 90001 (seq 3)
    serverConversations = [
      makeConversation({
        id: '90001',
        title: 'Active Chat',
        last_seq: '3',
        last_msg_id: '188900000000003',
        last_delivered_seq: '2',
        last_read_seq: '2',
        unread_count: 1,
      }),
      makeConversation({
        id: '90002',
        title: 'Background Chat',
        last_seq: '12',
        last_msg_id: '188900000000012',
        last_delivered_seq: '10',
        last_read_seq: '10',
        unread_count: 2,
      }),
    ]
    serverMessagesByConv['90001'].push(
      makeMessage({ conversation_id: '90001', seq: '3', sender_id: '10002', content: 'offline-active-3' }),
    )
    serverMessagesByConv['90002'].push(
      makeMessage({ conversation_id: '90002', seq: '11', sender_id: '10002', content: 'offline-bg-11' }),
      makeMessage({ conversation_id: '90002', seq: '12', sender_id: '10002', content: 'offline-bg-12' }),
    )

    // Simulate browser offline -> online recovery
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: false })
    window.dispatchEvent(new Event('offline'))
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
    window.dispatchEvent(new Event('online'))

    expect(sockets.length).toBeGreaterThanOrEqual(2)
    const reconnectedSocket = sockets[sockets.length - 1]
    reconnectedSocket.open()
    const probe = JSON.parse(reconnectedSocket.sent[0]) as { type: string; req_id: string; data: { last_seq: Record<string, string> } }
    expect(probe.data.last_seq['90001']).toBe('2')
    expect(probe.data.last_seq['90002']).toBe('10')

    authenticateFakeTransport(reconnectedSocket)
    // Wait for the 'open'-triggered reconcileChatOnReconnect to complete without queuing a duplicate force run
    await reconcileChatOnReconnect()
    await new Promise(resolve => setTimeout(resolve, 10))

    // Pending message re-enqueued & replayed with original client_msg_id
    const sentPayloads = reconnectedSocket.sent
      .map(s => JSON.parse(s) as { type: string; data: { client_msg_id?: string } })
      .filter(f => f.type === 'msg.send')
    expect(sentPayloads.some(f => f.data.client_msg_id === pendingMsg.client_msg_id)).toBe(true)

    // Active chat synced seq 3 and marked read (unread = 0); background chat synced 11, 12 and preserved unread = 2
    expect(msgStore.getMessages('90001').map(m => m.seq)).toContain('3')
    expect(msgStore.getMessages('90002').map(m => m.seq)).toEqual(['11', '12'])
    expect(convStore.itemsById['90001']?.unread_count).toBe(0)
    expect(convStore.itemsById['90002']?.unread_count).toBe(2)

    // Now test live push buffering during an in-flight reconcile:
    // - seq 12 (already in fetchList snapshot) must NOT double-count unread
    // - seq 13 (beyond fetchList snapshot) increments unread from 2 -> 3
    msgStore.beginRealtimeBuffering()
    reconnectedSocket.receive({
      type: 'msg.push',
      req_id: 'push-12',
      timestamp: Date.now(),
      data: {
        push_type: 'message',
        delivery_status: 'sent',
        message: makeMessage({ conversation_id: '90002', msg_id: '188900000000012', seq: '12', sender_id: '10002', content: 'offline-bg-12' }),
      },
    })
    reconnectedSocket.receive({
      type: 'msg.push',
      req_id: 'push-13',
      timestamp: Date.now(),
      data: {
        push_type: 'message',
        delivery_status: 'sent',
        message: makeMessage({ conversation_id: '90002', msg_id: '188900000000013', seq: '13', sender_id: '10002', content: 'live-bg-13' }),
      },
    })
    expect(msgStore.getMessages('90002').map(m => m.seq)).toEqual(['11', '12'])
    msgStore.flushRealtimeBuffer()
    expect(msgStore.getMessages('90002').map(m => m.seq)).toEqual(['11', '12', '13'])
    expect(convStore.itemsById['90002']?.unread_count).toBe(3)
  })

  it('clears chat stores on account switch and updates chat visibility / read status on window focus', async () => {
    const authStore = seedSession('10001')
    const convStore = useConversationStore()
    const msgStore = useMessageStore()

    const realtime = useChatRealtime({ trackChatVisibility: true })
    await new Promise(resolve => setTimeout(resolve, 10))
    expect(sockets.length).toBeGreaterThanOrEqual(1)
    sockets[0].open()
    authenticateFakeTransport(sockets[0])
    await reconcileChatOnReconnect()

    expect(convStore.itemsById['90001']).toBeTruthy()
    convStore.setActiveConversation('90002')
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    realtime.syncVisibilityState()
    expect(convStore.isChatVisible).toBe(false)

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
    convStore.applyIncomingMessage(
      {
        ...makeMessage({ conversation_id: '90002', msg_id: '188900000000099', seq: '11', sender_id: '10002', content: 'new' }),
        delivery_status: 'sent',
      },
      '10001',
    )
    expect(convStore.itemsById['90002']?.unread_count).toBe(1)

    realtime.syncVisibilityState()
    expect(convStore.isChatVisible).toBe(true)
    expect(convStore.itemsById['90002']?.unread_count).toBe(0)

    // Switch account from 10001 -> 20002: previous stores are wiped clean
    authStore.user = { ...authStore.user!, id: '20002', username: 'user_20002' }
    authStore.tokens = { ...authStore.tokens!, access_token: makeTestJwt('20002') }
    await new Promise(resolve => setTimeout(resolve, 10))

    expect(msgStore.getMessages('90001')).toEqual([])
    expect(Object.keys(convStore.itemsById)).toEqual([])
  })
})