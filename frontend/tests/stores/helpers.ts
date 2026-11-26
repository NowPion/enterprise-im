import { WSClient, type Envelope, type Transport } from '@/utils/wsClient'

export class FakeTransport implements Transport {
  readyState = 0
  onopen: Transport['onopen'] = null
  onclose: Transport['onclose'] = null
  onerror: Transport['onerror'] = null
  onmessage: Transport['onmessage'] = null
  sent: string[] = []

  open() {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }

  close(code = 1000, reason = '') {
    this.readyState = 3
    this.onclose?.({ code, reason } as CloseEvent)
  }

  send(text: string) {
    if (this.readyState !== 1) {
      throw new Error('Transport is not open')
    }
    this.sent.push(text)
  }

  receive(frame: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(frame) }))
  }
}

export function createTestWSClient(options: {
  token?: string
  getLastSeq?: () => Record<string, string>
} = {}) {
  const sockets: FakeTransport[] = []
  const client = new WSClient({
    origin: 'https://im.example',
    getAccessToken: () => options.token ?? makeTestJwt('10001'),
    heartbeatMs: 10000,
    getLastSeq: options.getLastSeq,
    createSocket: () => {
      const socket = new FakeTransport()
      sockets.push(socket)
      return socket
    },
  })
  return { client, sockets }
}

export function authenticateFakeTransport(socket: FakeTransport) {
  const raw = socket.sent[0]
  if (!raw) throw new Error('Expected auth ping probe on socket')
  const probe = JSON.parse(raw) as Envelope
  socket.receive({
    type: 'pong',
    req_id: probe.req_id,
    timestamp: Date.now(),
    data: {
      nonce: probe.data.nonce,
      server_time: Date.now(),
      session_expires_at: '2026-11-27T00:00:00.000Z',
    },
  })
}

export function makeTestJwt(sub: string, jti = 'jti-test'): string {
  const encode = (obj: object) => Buffer.from(JSON.stringify(obj)).toString('base64url')
  return `${encode({ alg: 'HS256', typ: 'JWT' })}.${encode({
    sub,
    session_id: '501',
    jti,
    exp: 9999999999,
  })}.sig`
}

export interface TestMessage {
  msg_id: string
  client_msg_id: string
  conversation_id: string
  seq: string
  sender_id: string
  msg_type: 'text' | 'image' | 'file' | 'voice' | 'recall' | 'system'
  content: string
  extra: Record<string, unknown>
  status: 'normal' | 'recalled'
  created_at: string
  recalled_at: string | null
  delivery_status?: 'sending' | 'sent' | 'delivered' | 'read' | 'failed'
}

export interface TestConversation {
  id: string
  type: 'single' | 'group'
  peer: {
    id: string
    username: string
    nickname: string
    avatar_url: string
    signature: string
    online: boolean
    last_online_at: string | null
  } | null
  group_id: string | null
  title: string
  avatar_url: string
  last_msg_id: string
  last_seq: string
  last_msg_preview: string
  last_msg_at: string | null
  last_delivered_seq: string
  last_read_seq: string
  unread_count: number
  is_pinned: boolean
  is_muted: boolean
  is_visible: boolean
  can_send: boolean
}

export function makeConversation(overrides: Partial<TestConversation> = {}): TestConversation {
  return {
    id: '90001',
    type: 'single',
    peer: {
      id: '10002',
      username: 'peer_user',
      nickname: 'Peer User',
      avatar_url: '',
      signature: '',
      online: true,
      last_online_at: null,
    },
    group_id: null,
    title: 'Peer User',
    avatar_url: '',
    last_msg_id: '0',
    last_seq: '0',
    last_msg_preview: '',
    last_msg_at: null,
    last_delivered_seq: '0',
    last_read_seq: '0',
    unread_count: 0,
    is_pinned: false,
    is_muted: false,
    is_visible: true,
    can_send: true,
    ...overrides,
  }
}

export function makeMessage(overrides: Partial<TestMessage> = {}): TestMessage {
  const seq = overrides.seq ?? '1'
  return {
    msg_id: overrides.msg_id ?? `18890000000000${seq}`,
    client_msg_id: overrides.client_msg_id ?? `cmsg-${seq}`,
    conversation_id: overrides.conversation_id ?? '90001',
    seq,
    sender_id: overrides.sender_id ?? '10001',
    msg_type: 'text',
    content: `message-${seq}`,
    extra: {},
    status: 'normal',
    created_at: '2026-11-26T04:00:00.123Z',
    recalled_at: null,
    delivery_status: 'sent',
    ...overrides,
  }
}

export function apiSuccess<T>(data: T, requestId = 'req-test-1') {
  return {
    code: 0,
    message: 'ok',
    data,
    request_id: requestId,
  }
}

export function installInMemoryRequestAdapter(
  requestClient: { defaults?: { adapter?: unknown } },
  responder: (method: string, url: string, body: unknown, params: Record<string, unknown>) => { status?: number; body: unknown },
) {
  const previous = requestClient.defaults?.adapter
  if (!requestClient.defaults) {
    throw new Error('Expected Axios request client with defaults.adapter')
  }
  const calls: Array<{ method: string; url: string; body: unknown; params: Record<string, unknown> }> = []
  requestClient.defaults.adapter = async (config: {
    method?: string
    url?: string
    baseURL?: string
    data?: unknown
    params?: Record<string, unknown>
    headers?: Record<string, string>
  }) => {
    const method = (config.method ?? 'get').toUpperCase()
    const rawUrl = config.url ?? ''
    const normalizedUrl = rawUrl.replace(/^https?:\/\/[^/]+/, '')
    let parsedBody: unknown = config.data
    if (typeof parsedBody === 'string') {
      try {
        parsedBody = JSON.parse(parsedBody)
      } catch {
        // keep raw string
      }
    }
    const params = config.params ?? {}
    calls.push({ method, url: normalizedUrl, body: parsedBody, params })
    const result = responder(method, normalizedUrl, parsedBody, params)
    const status = result.status ?? 200
    if (status >= 400) {
      const error = Object.assign(new Error(`Request failed with status ${status}`), {
        isAxiosError: true,
        config,
        response: {
          status,
          statusText: 'Error',
          headers: {},
          config,
          data: result.body,
        },
      })
      throw error
    }
    return {
      status,
      statusText: 'OK',
      headers: { 'x-request-id': 'req-test-1' },
      config,
      data: result.body,
    }
  }
  return {
    calls,
    restore() {
      if (requestClient.defaults) {
        requestClient.defaults.adapter = previous
      }
    },
  }
}

type AnyRecord = Record<string, unknown>

export function findAction(store: unknown, candidates: string[]): ((...args: never[]) => unknown) | null {
  const target = store as AnyRecord
  for (const name of candidates) {
    if (typeof target[name] === 'function') {
      return (target[name] as (...args: never[]) => unknown).bind(target)
    }
  }
  return null
}

export function bindStoreWS(store: unknown, client: WSClient): boolean {
  const fn = findAction(store, [
    'bindWS',
    'bindWs',
    'bindWSClient',
    'bindWsClient',
    'attachWS',
    'attachWs',
    'initWS',
    'initWs',
    'initWsListeners',
    'bindListeners',
    'setWSClient',
    'setWsClient',
  ])
  if (!fn) return false
  ;(fn as (c: WSClient) => unknown)(client)
  return true
}

export function extractConversationList(store: unknown): TestConversation[] {
  const s = store as AnyRecord
  for (const key of ['conversations', 'items', 'list', 'conversationList']) {
    const val = s[key]
    if (Array.isArray(val)) return val as TestConversation[]
  }
  for (const key of ['byId', 'conversationMap', 'map']) {
    const val = s[key]
    if (val instanceof Map) return Array.from(val.values()) as TestConversation[]
    if (val && typeof val === 'object') return Object.values(val) as TestConversation[]
  }
  throw new Error('Could not locate conversation list on useConversationStore()')
}

export function extractConversationById(store: unknown, conversationId: string): TestConversation | undefined {
  const s = store as AnyRecord
  const getter = findAction(s, ['getConversation', 'getById', 'findConversation'])
  if (getter) {
    const found = (getter as (id: string) => TestConversation | undefined)(conversationId)
    if (found) return found
  }
  return extractConversationList(store).find(c => String(c.id) === String(conversationId))
}

export function extractMessagesForConversation(store: unknown, conversationId: string): TestMessage[] {
  const s = store as AnyRecord
  const getter = findAction(s, ['getMessages', 'getConversationMessages', 'messagesOf'])
  if (getter) {
    const list = (getter as (id: string) => TestMessage[] | undefined)(conversationId)
    if (Array.isArray(list)) return list
  }
  for (const key of ['messagesByConversation', 'byConversation', 'messagesMap', 'messageMap', 'messages']) {
    const container = s[key]
    if (container instanceof Map) {
      return (container.get(conversationId) as TestMessage[] | undefined) ?? []
    }
    if (container && typeof container === 'object' && !Array.isArray(container)) {
      const bucket = (container as Record<string, unknown>)[conversationId]
      if (Array.isArray(bucket)) return bucket as TestMessage[]
    }
  }
  throw new Error('Could not locate messages collection on useMessageStore()')
}

export function seedAuthUser(authStore: unknown, userId = '10001') {
  const s = authStore as AnyRecord
  const user = {
    id: userId,
    username: `user_${userId}`,
    nickname: `User ${userId}`,
    avatar_url: '',
    signature: '',
    online: true,
    last_online_at: null,
    role: 'user',
    status: 1,
  }
  if ('user' in s) s.user = user
  if ('currentUser' in s) s.currentUser = user
  if ('accessToken' in s) s.accessToken = makeTestJwt(userId)
  if ('token' in s) s.token = makeTestJwt(userId)
}
