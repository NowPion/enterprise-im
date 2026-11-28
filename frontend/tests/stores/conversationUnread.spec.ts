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

describe('useConversationStore unread counts, read_ack watermarks, and idempotent push replay', () => {
  let restoreHttp: (() => void) | undefined
  let conversationsFixture: TestConversation[]
  let readPostCalls: Array<{ conversationId: string; last_read_seq: string }>

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    readPostCalls = []
    conversationsFixture = [
      makeConversation({
        id: '90001',
        title: 'Active Chat',
        last_msg_id: '188900000000005',
        last_seq: '5',
        last_delivered_seq: '5',
        last_read_seq: '5',
        unread_count: 0,
      }),
      makeConversation({
        id: '90002',
        title: 'Background Chat',
        last_msg_id: '188900000000010',
        last_seq: '10',
        last_delivered_seq: '10',
        last_read_seq: '10',
        unread_count: 0,
      }),
      makeConversation({
        id: '90003',
        title: 'Replay Chat',
        last_msg_id: '188900000000020',
        last_seq: '20',
        last_delivered_seq: '20',
        last_read_seq: '20',
        unread_count: 0,
      }),
    ]
    const adapter = installInMemoryRequestAdapter(request, (method, url, body) => {
      if (method === 'GET' && url.endsWith('/conversations')) {
        return { body: apiSuccess({ items: conversationsFixture, next_cursor: null, has_more: false }) }
      }
      const detailMatch = url.match(/\/conversations\/([^/]+)$/)
      if (method === 'GET' && detailMatch) {
        const found = conversationsFixture.find(c => c.id === detailMatch[1])
        return {
          body: apiSuccess(
            found
              ? { conversation: found, role: null, peer: found.peer, group: null }
              : null,
          ),
        }
      }
      if (method === 'GET' && /\/conversations\/[^/]+\/messages/.test(url)) {
        return { body: apiSuccess({ items: [], next_cursor: null, has_more: false }) }
      }
      if (method === 'GET' && /\/conversations\/[^/]+\/sync/.test(url)) {
        return { body: apiSuccess({ items: [], from_seq: '0', to_seq: '0', has_more: false }) }
      }
      const readMatch = url.match(/\/conversations\/([^/]+)\/read$/)
      if (method === 'POST' && readMatch) {
        const convId = readMatch[1]
        const seq = String((body as { last_read_seq?: string } | undefined)?.last_read_seq ?? '0')
        readPostCalls.push({ conversationId: convId, last_read_seq: seq })
        return {
          body: apiSuccess({
            conversation_id: convId,
            last_read_seq: seq,
            last_delivered_seq: seq,
            unread_count: 0,
            read_at: '2026-11-26T04:05:00.000Z',
          }),
        }
      }
      if (method === 'POST' && /\/conversations\/[^/]+\/delivered$/.test(url)) {
        return { body: apiSuccess({ conversation_id: '90002', last_delivered_seq: '11' }) }
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

  async function initStores() {
    const convStore = useConversationStore()
    const msgStore = useMessageStore()
    const { client, sockets } = createTestWSClient()
    msgStore.bindWSClient(client, () => '10001')
    client.connect()
    sockets[0].open()
    authenticateFakeTransport(sockets[0])
    await convStore.fetchList()
    return { convStore, msgStore, client, socket: sockets[0] }
  }

  function pushIncomingMessage(
    socket: ReturnType<typeof createTestWSClient>['sockets'][number],
    msg: TestMessage,
  ) {
    const data = { push_type: 'message', message: msg, delivery_status: 'sent' }
    socket.receive({ type: 'msg.push', req_id: `push-${msg.msg_id}`, timestamp: Date.now(), data })
  }

  it('increments unread_count by 1 when msg.push arrives for a non-active conversation from another user', async () => {
    const { convStore, client, socket } = await initStores()

    convStore.setActiveConversation('90001')
    convStore.setChatVisible(true)

    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000011',
        client_msg_id: 'cmsg-bg-11',
        conversation_id: '90002',
        seq: '11',
        sender_id: '10002',
        content: 'unread ping',
      }),
    )

    const bg = extractConversationById(convStore, '90002')
    const active = extractConversationById(convStore, '90001')
    expect(bg?.unread_count).toBe(1)
    expect(bg?.last_seq).toBe('11')
    expect(active?.unread_count).toBe(0)
    client.disconnect()
  })

  it('resets unread_count to 0 and advances last_read_seq monotonically after opening conversation and sending read_ack', async () => {
    conversationsFixture[1].unread_count = 3
    conversationsFixture[1].last_seq = '15'
    conversationsFixture[1].last_read_seq = '12'

    const { convStore, msgStore, client, socket } = await initStores()
    convStore.setActiveConversation('90002')
    convStore.setChatVisible(true)

    await msgStore.markRead('90002', '15')

    const readAckFrames = socket.sent
      .map(raw => JSON.parse(raw) as { type: string; data: { conversation_id?: string; last_read_seq?: string } })
      .filter(frame => frame.type === 'msg.read_ack')
    expect(readAckFrames).toHaveLength(1)
    expect(readAckFrames[0].data).toEqual({ conversation_id: '90002', last_read_seq: '15' })

    const bg = extractConversationById(convStore, '90002')
    expect(bg?.unread_count).toBe(0)
    expect(bg?.last_read_seq).toBe('15')

    // Monotonic watermark guard: applying an older read_seq ('12') must not regress last_read_seq
    await msgStore.markRead('90002', '12')
    convStore.applyReadWatermark('90002', '12')
    const afterStale = extractConversationById(convStore, '90002')
    expect(afterStale?.last_read_seq).toBe('15')
    expect(afterStale?.unread_count).toBe(0)
    expect(readPostCalls.every(c => BigInt(c.last_read_seq) <= 15n)).toBe(true)
    client.disconnect()
  })

  it('does NOT double-count unread_count when Stream/WS replays the same msg.push or delivers seq <= last_read_seq', async () => {
    const { convStore, client, socket } = await initStores()
    convStore.setActiveConversation('90001')
    convStore.setChatVisible(true)

    const replayedMsg = makeMessage({
      msg_id: '188900000000021',
      client_msg_id: 'cmsg-replay-21',
      conversation_id: '90003',
      seq: '21',
      sender_id: '10002',
      content: 'replayed stream message',
    })

    pushIncomingMessage(socket, replayedMsg)
    pushIncomingMessage(socket, replayedMsg)
    pushIncomingMessage(socket, replayedMsg)

    let conv = extractConversationById(convStore, '90003')
    expect(conv?.unread_count).toBe(1)

    // Message with seq <= last_read_seq ('20') must not increment unread
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000019',
        client_msg_id: 'cmsg-old-19',
        conversation_id: '90003',
        seq: '19',
        sender_id: '10002',
        content: 'already read historical push',
      }),
    )
    conv = extractConversationById(convStore, '90003')
    expect(conv?.unread_count).toBe(1)
    client.disconnect()
  })

  it('supplies updated per-conversation last_seq map to WSClient getLastSeq hook on reconnect', async () => {
    const { convStore, client, socket } = await initStores()
    convStore.setActiveConversation('90001')
    convStore.setChatVisible(true)

    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000011',
        client_msg_id: 'cmsg-seq-11',
        conversation_id: '90002',
        seq: '11',
        sender_id: '10002',
        content: 'advance last_seq',
      }),
    )

    const getLastSeq = () => ({ ...convStore.lastSeqMap })

    const reconnect = createTestWSClient({ getLastSeq })
    reconnect.client.connect()
    reconnect.sockets[0].open()
    const probe = JSON.parse(reconnect.sockets[0].sent[0]) as { type: string; data: { last_seq: Record<string, string> } }
    expect(probe.type).toBe('ping')
    expect(probe.data.last_seq['90001']).toBe('5')
    expect(probe.data.last_seq['90002']).toBe('11')
    expect(probe.data.last_seq['90003']).toBe('20')

    reconnect.client.disconnect()
    client.disconnect()
  })

  it('clears unread and sends read_ack when chat is visible, but increments unread when active chat is hidden or peer recalls unread', async () => {
    const { convStore, client, socket } = await initStores()

    // Active and visible -> incoming peer message does NOT increment unread and emits msg.read_ack
    convStore.setActiveConversation('90001')
    convStore.setChatVisible(true)
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000006',
        client_msg_id: 'cmsg-active-6',
        conversation_id: '90001',
        seq: '6',
        sender_id: '10002',
        content: 'visible message',
      }),
    )
    expect(extractConversationById(convStore, '90001')?.unread_count).toBe(0)
    expect(extractConversationById(convStore, '90001')?.last_read_seq).toBe('6')
    const readFrames = socket.sent
      .map(raw => JSON.parse(raw) as { type: string; data: { conversation_id?: string; last_read_seq?: string } })
      .filter(f => f.type === 'msg.read_ack')
    expect(readFrames.some(f => f.data.conversation_id === '90001' && f.data.last_read_seq === '6')).toBe(true)

    // Active but hidden (e.g. tab blurred / navigated away) -> increments unread
    convStore.setChatVisible(false)
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000007',
        client_msg_id: 'cmsg-hidden-7',
        conversation_id: '90001',
        seq: '7',
        sender_id: '10002',
        content: 'hidden message',
      }),
    )
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000008',
        client_msg_id: 'cmsg-hidden-8',
        conversation_id: '90001',
        seq: '8',
        sender_id: '10002',
        content: 'hidden message 2',
      }),
    )
    expect(extractConversationById(convStore, '90001')?.unread_count).toBe(2)

    // Own outgoing message on background chat advances last_seq without incrementing unread
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000011',
        client_msg_id: 'cmsg-own-11',
        conversation_id: '90002',
        seq: '11',
        sender_id: '10001',
        content: 'my own message',
      }),
    )
    expect(extractConversationById(convStore, '90002')?.unread_count).toBe(0)
    expect(extractConversationById(convStore, '90002')?.last_seq).toBe('11')

    // Peer recalls an unread message (seq 7 > last_read_seq 6) -> unread decrements by 1
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000009',
        client_msg_id: 'cmsg-recall-9',
        conversation_id: '90001',
        seq: '9',
        sender_id: '10002',
        msg_type: 'recall',
        content: '对方撤回了一条消息',
        extra: { recalled_msg_id: '188900000000007', recalled_seq: '7' },
      }),
    )
    expect(extractConversationById(convStore, '90001')?.unread_count).toBe(1)

    // Peer recalls an already-read message (seq 5 <= last_read_seq 6) -> unread remains 1
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000010',
        client_msg_id: 'cmsg-recall-10',
        conversation_id: '90001',
        seq: '10',
        sender_id: '10002',
        msg_type: 'recall',
        content: '对方撤回了一条消息',
        extra: { recalled_msg_id: '188900000000005', recalled_seq: '5' },
      }),
    )
    expect(extractConversationById(convStore, '90001')?.unread_count).toBe(1)
    client.disconnect()
  })

  it('computes totalUnreadCount vs unmutedUnreadCount, hides invisible chats, and sorts pinned/recent conversations', async () => {
    conversationsFixture = [
      makeConversation({
        id: '90001',
        title: 'Normal Unread',
        last_seq: '5',
        last_read_seq: '3',
        unread_count: 2,
        last_msg_at: '2026-11-26T04:00:00.000Z',
        is_pinned: false,
        is_muted: false,
        is_visible: true,
      }),
      makeConversation({
        id: '90002',
        title: 'Muted Unread',
        last_seq: '10',
        last_read_seq: '6',
        unread_count: 4,
        last_msg_at: '2026-11-26T05:00:00.000Z',
        is_pinned: false,
        is_muted: true,
        is_visible: true,
      }),
      makeConversation({
        id: '90003',
        title: 'Pinned Older',
        last_seq: '20',
        last_read_seq: '20',
        unread_count: 0,
        last_msg_at: '2026-11-26T01:00:00.000Z',
        is_pinned: true,
        is_muted: false,
        is_visible: true,
      }),
      makeConversation({
        id: '90004',
        title: 'Hidden Chat',
        last_seq: '30',
        last_read_seq: '27',
        unread_count: 3,
        last_msg_at: '2026-11-26T06:00:00.000Z',
        is_pinned: false,
        is_muted: false,
        is_visible: false,
      }),
    ]

    const { convStore, client, socket } = await initStores()

    expect(convStore.visibleConversations.map(c => c.id)).toEqual(['90003', '90002', '90001'])
    expect(convStore.totalUnreadCount).toBe(6)
    expect(convStore.unmutedUnreadCount).toBe(2)

    // Incoming message resurfaces hidden conversation 90004
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000031',
        client_msg_id: 'cmsg-resurface-31',
        conversation_id: '90004',
        seq: '31',
        sender_id: '10002',
        content: 'wake up hidden chat',
        created_at: '2026-11-26T07:00:00.000Z',
      }),
    )
    expect(convStore.visibleConversations.map(c => c.id)).toEqual(['90003', '90004', '90002', '90001'])
    expect(convStore.totalUnreadCount).toBe(10)
    expect(convStore.unmutedUnreadCount).toBe(6)
    client.disconnect()
  })

  it('preserves monotonic watermarks against stale server snapshots and hydrates unknown conversations + system notices', async () => {
    const { convStore, client, socket } = await initStores()

    // Advance 90001 locally via live push
    convStore.setActiveConversation('90002')
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000008',
        client_msg_id: 'cmsg-live-8',
        conversation_id: '90001',
        seq: '8',
        sender_id: '10002',
        content: 'newer local live push',
        created_at: '2026-11-26T08:00:00.000Z',
      }),
    )
    expect(extractConversationById(convStore, '90001')?.unread_count).toBe(1)

    // Stale server snapshot with older last_seq=5 must NOT overwrite local seq=8 or preview or unread=1
    convStore.upsertConversation(
      makeConversation({
        id: '90001',
        last_msg_id: '188900000000005',
        last_seq: '5',
        last_msg_preview: 'stale preview',
        last_read_seq: '5',
        unread_count: 0,
      }),
    )
    const afterStale = extractConversationById(convStore, '90001')
    expect(afterStale?.last_seq).toBe('8')
    expect(afterStale?.last_msg_preview).toBe('newer local live push')
    expect(afterStale?.unread_count).toBe(1)

    // Local read watermark >= server last_seq keeps unread=0 even if stale snapshot says unread=3
    convStore.applyReadWatermark('90002', '10', '10', 0)
    convStore.upsertConversation(
      makeConversation({
        id: '90002',
        last_seq: '10',
        last_read_seq: '7',
        unread_count: 3,
      }),
    )
    expect(extractConversationById(convStore, '90002')?.last_read_seq).toBe('10')
    expect(extractConversationById(convStore, '90002')?.unread_count).toBe(0)

    // Unknown conversation push creates placeholder, increments unread, and hydrates from GET /conversations/:id
    conversationsFixture.push(
      makeConversation({
        id: '90099',
        title: 'Hydrated Group',
        type: 'group',
        peer: null,
        group_id: '7001',
        last_seq: '1',
        unread_count: 1,
      }),
    )
    pushIncomingMessage(
      socket,
      makeMessage({
        msg_id: '188900000000099',
        client_msg_id: 'cmsg-unknown-99',
        conversation_id: '90099',
        seq: '1',
        sender_id: '10002',
        content: 'hello from new group',
      }),
    )
    await new Promise(resolve => setTimeout(resolve, 10))
    const hydrated = extractConversationById(convStore, '90099')
    expect(hydrated?.title).toBe('Hydrated Group')
    expect(hydrated?.unread_count).toBe(1)

    // System notices update group metadata & can_send flags accurately
    convStore.applySystemNotice(
      {
        event: 'group.updated',
        notice_id: 'n-upd',
        conversation_id: '90099',
        actor_id: '10002',
        target_user_id: null,
        payload: { name: 'Renamed Group', avatar_url: '/uploads/g.png' },
      },
      '10001',
    )
    expect(extractConversationById(convStore, '90099')?.title).toBe('Renamed Group')

    // Removing another member does NOT disable current user's can_send; removing self DOES
    convStore.applySystemNotice(
      {
        event: 'group.member_removed',
        notice_id: 'n-rem-other',
        conversation_id: '90099',
        actor_id: '10002',
        target_user_id: '10003',
        payload: { user_id: '10003' },
      },
      '10001',
    )
    expect(extractConversationById(convStore, '90099')?.can_send).toBe(true)

    convStore.applySystemNotice(
      {
        event: 'group.member_removed',
        notice_id: 'n-rem-self',
        conversation_id: '90099',
        actor_id: '10002',
        target_user_id: '10001',
        payload: { user_id: '10001' },
      },
      '10001',
    )
    expect(extractConversationById(convStore, '90099')?.can_send).toBe(false)

    // friend.accepted re-enables can_send and visibility
    convStore.applySystemNotice(
      {
        event: 'friend.accepted',
        notice_id: 'n-friend',
        conversation_id: '90099',
        actor_id: '10002',
        target_user_id: '10001',
        payload: {},
      },
      '10001',
    )
    expect(extractConversationById(convStore, '90099')?.can_send).toBe(true)

    convStore.applySystemNotice(
      {
        event: 'group.disbanded',
        notice_id: 'n-disband',
        conversation_id: '90099',
        actor_id: '10002',
        target_user_id: null,
        payload: {},
      },
      '10001',
    )
    expect(extractConversationById(convStore, '90099')?.can_send).toBe(false)
    client.disconnect()
  })
})
