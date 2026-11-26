import assert from 'node:assert/strict'
import { mock } from 'node:test'
import { WSClient, type Transport } from '../src/utils/wsClient.ts'

class Socket implements Transport {
  readyState = 0
  onopen: Transport['onopen'] = null
  onmessage: Transport['onmessage'] = null
  onerror: Transport['onerror'] = null
  onclose: Transport['onclose'] = null
  sent: string[] = []
  open() { this.readyState = 1; this.onopen?.(new Event('open')) }
  close(code = 1006, reason = '') { this.readyState = 3; this.onclose?.({ code, reason } as CloseEvent) }
  send(text: string) { assert.equal(this.readyState, 1); this.sent.push(text) }
  receive(data: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) })) }
}

const frame = (type: string, data: object, req_id = 'req-1') => ({ type, data, req_id, timestamp: 123 })
const message = (overrides: Partial<Parameters<WSClient['enqueue']>[0]> = {}) => ({
  conversation_id: '9',
  client_msg_id: 'client-1',
  msg_type: 'text',
  content: 'hello',
  extra: {},
  ...overrides,
})
const ack = { client_msg_id: 'client-1', conversation_id: '9', msg_id: '19', seq: '42', status: 'normal', delivery_status: 'sent' }

function makeJwt(sub: string, jti = 'jti-1'): string {
  const b64 = (obj: object) => Buffer.from(JSON.stringify(obj)).toString('base64url')
  return `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({ sub, session_id: '1', jti, exp: 9999999999 })}.sig`
}

function authenticateSocket(socket: Socket) {
  const probe = JSON.parse(socket.sent[0])
  assert.equal(probe.type, 'ping')
  socket.receive(frame('pong', { nonce: probe.data.nonce, server_time: 123, session_expires_at: '2026-11-26T12:00:00Z' }, probe.req_id))
  return probe
}

function setup(overrides: Partial<ConstructorParameters<typeof WSClient>[0]> = {}) {
  const sockets: Socket[] = []
  const urls: URL[] = []
  const client = new WSClient({
    origin: 'https://im.example',
    getAccessToken: () => `token+${sockets.length}/=`,
    heartbeatMs: 100,
    getLastSeq: () => ({ '9': '42' }),
    random: () => 0.5,
    createSocket(url) {
      urls.push(new URL(url))
      const socket = new Socket()
      sockets.push(socket)
      return socket
    },
    ...overrides,
  })
  return { client, sockets, urls }
}

beforeEach(() => mock.timers.enable({ apis: ['setTimeout', 'setInterval'] }))
afterEach(() => mock.timers.reset())

it('authenticates once per attempt, verifies session via ping/pong before opening, and resumes heartbeats with a fresh token', () => {
  const { client, sockets, urls } = setup()
  const opens: number[] = []
  const pongs: string[] = []
  client.on('open', () => opens.push(1))
  client.on('pong', pong => pongs.push(pong.req_id))

  client.connect(); client.connect()
  assert.equal(urls.length, 1)
  assert.equal(urls[0].origin + urls[0].pathname, 'wss://im.example/ws')
  assert.equal(urls[0].searchParams.get('access_token'), 'token+0/=')
  mock.timers.tick(100)
  assert.equal(sockets[0].sent.length, 0)

  sockets[0].open()
  assert.equal(opens.length, 0)
  const ping = JSON.parse(sockets[0].sent[0])
  assert.equal(ping.type, 'ping'); assert.ok(ping.req_id); assert.ok(Number.isSafeInteger(ping.timestamp))
  assert.ok(ping.data.nonce); assert.deepEqual(ping.data.last_seq, { '9': '42' })

  sockets[0].receive(frame('pong', { nonce: ping.data.nonce, server_time: 123, session_expires_at: '2026-11-26T12:00:00Z' }, ping.req_id))
  assert.equal(opens.length, 1)
  assert.deepEqual(pongs, [ping.req_id])

  mock.timers.tick(100)
  assert.equal(sockets[0].sent.length, 2)
  mock.timers.tick(100); mock.timers.tick(500)
  assert.equal(urls[1].searchParams.get('access_token'), 'token+1/=')
  sockets[1].open()
  assert.equal(opens.length, 1)
  authenticateSocket(sockets[1])
  assert.equal(opens.length, 2)
  mock.timers.tick(100)
  assert.equal(JSON.parse(sockets[1].sent[1]).type, 'ping')
})

it('uses full jitter with 1s exponential bounds capped at 30s, resets only after authenticated open, and cancels on disconnect', () => {
  const { client, sockets } = setup()
  client.connect()
  for (const cap of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
    const count = sockets.length
    sockets[count - 1].close(); mock.timers.tick(cap / 2 - 1)
    assert.equal(sockets.length, count)
    mock.timers.tick(1); assert.equal(sockets.length, count + 1)
  }

  // onopen without pong authentication must NOT reset backoff
  sockets.at(-1)!.open(); sockets.at(-1)!.close(); mock.timers.tick(500)
  assert.equal(sockets.length, 8)
  mock.timers.tick(14500)
  assert.equal(sockets.length, 9)

  // authenticated open resets backoff to 1s cap
  sockets.at(-1)!.open(); authenticateSocket(sockets.at(-1)!); sockets.at(-1)!.close()
  mock.timers.tick(500)
  assert.equal(sockets.length, 10)
  sockets[9].close(); client.disconnect(); mock.timers.tick(30000)
  assert.equal(sockets.length, 10)
})

it('rejects malformed envelopes and separates lifecycle, server error and push events', () => {
  const { client, sockets } = setup()
  const seen: string[] = []
  for (const type of ['open', 'close', 'presence.update', 'system.notice'] as const) client.on(type, () => seen.push(type))
  const unsubscribe = client.on('error', error => seen.push(error.kind))
  client.connect(); const socket = sockets[0]
  socket.open(); authenticateSocket(socket)
  socket.onerror?.(new Event('error'))
  for (const invalid of [{ online: true }, { ...frame('presence.update', {}), req_id: '' }, { ...frame('pong', {}), timestamp: 1.5 }, frame('system.notice', [])]) socket.receive(invalid)
  socket.onmessage?.(new MessageEvent('message', { data: '{' }))
  socket.receive(frame('presence.update', { user_id: '2', online: true, device_count: 1, last_online_at: null }))
  socket.receive(frame('system.notice', { event: 'server.draining', payload: {} }))
  socket.receive(frame('error', { code: 18002, message: 'unsupported' }))
  unsubscribe(); socket.onerror?.(new Event('error')); socket.close()
  assert.deepEqual(seen, ['open', 'transport', ...Array(5).fill('protocol'), 'presence.update', 'system.notice', 'frame', 'close'])
})

it('replays stable messages only after authenticated open until matching persisted ACK, ignoring errors, partial ACKs and stale sockets', () => {
  const { client, sockets } = setup()
  const data = message()
  client.enqueue(data, 'req-1'); data.content = 'edited'
  client.connect(); const first = sockets[0]; first.open()
  assert.deepEqual(first.sent.map(text => JSON.parse(text).type), ['ping'])
  authenticateSocket(first)
  assert.equal(JSON.parse(first.sent[1]).type, 'msg.send')
  assert.equal(JSON.parse(first.sent[1]).data.content, 'hello')
  const staleMessage = first.onmessage
  first.receive(frame('error', { related_req_id: 'req-1' })); first.close()
  client.enqueue(message({ client_msg_id: 'client-2' }), 'req-2')
  mock.timers.tick(500); const second = sockets[1]; second.open()
  assert.deepEqual(second.sent.map(text => JSON.parse(text).type), ['ping'])
  authenticateSocket(second)
  assert.deepEqual(second.sent.slice(1).map(text => JSON.parse(text).req_id), ['req-1', 'req-2'])
  assert.deepEqual(JSON.parse(second.sent[1]).data, JSON.parse(first.sent[1]).data)
  staleMessage?.(new MessageEvent('message', { data: JSON.stringify(frame('msg.ack', ack)) }))
  second.receive(frame('msg.ack', { client_msg_id: 'client-1' })); second.receive(frame('msg.ack', ack, 'req-2'))
  assert.equal(client.pendingCount, 2)
  second.receive(frame('msg.ack', ack)); assert.equal(client.pendingCount, 1)
  second.close(); mock.timers.tick(500); sockets[2].open(); authenticateSocket(sockets[2])
  assert.deepEqual(sockets[2].sent.slice(1).map(text => JSON.parse(text).req_id), ['req-2'])
})

it('B1: resends pending frames with original req_id and client_msg_id after same-account credential rotation', () => {
  let token = makeJwt('1001', 'jti-a1')
  const { client, sockets, urls } = setup({ getAccessToken: () => token })

  client.enqueue(message({ client_msg_id: 'cmsg-a1', content: 'persist-across-rotation' }), 'req-a1')
  client.connect()
  sockets[0].open()
  authenticateSocket(sockets[0])
  const firstSend = sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.equal(firstSend.length, 1)
  assert.equal(firstSend[0].req_id, 'req-a1')
  assert.equal(firstSend[0].data.client_msg_id, 'cmsg-a1')

  // Rotate credentials for the same account (sub remains 1001, jti changes)
  client.disconnect()
  token = makeJwt('1001', 'jti-a2')
  assert.equal(client.pendingCount, 1)

  client.connect()
  assert.equal(urls[1].searchParams.get('access_token'), token)
  sockets[1].open()
  authenticateSocket(sockets[1])
  const replayed = sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.equal(replayed.length, 1)
  assert.equal(replayed[0].req_id, 'req-a1')
  assert.equal(replayed[0].data.client_msg_id, 'cmsg-a1')
  assert.equal(replayed[0].data.content, 'persist-across-rotation')
})

it('B1: isolates pending frames on account switch so B receives zero frames from A', () => {
  let token = makeJwt('1001', 'jti-a1')
  const { client, sockets } = setup({ getAccessToken: () => token })

  client.enqueue(message({ client_msg_id: 'cmsg-a1', content: 'from-account-A' }), 'req-a1')
  client.connect()
  sockets[0].open()
  authenticateSocket(sockets[0])
  assert.equal(client.pendingCount, 1)

  // Disconnect A and switch to account B
  client.disconnect()
  token = makeJwt('2002', 'jti-b1')
  assert.equal(client.pendingCount, 0)

  client.connect()
  sockets[1].open()
  authenticateSocket(sockets[1])
  const sentOnB = sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.equal(sentOnB.length, 0)

  // Enqueuing on B sends only B's frame and never leaks A's frame
  client.enqueue(message({ client_msg_id: 'cmsg-b1', content: 'from-account-B' }), 'req-b1')
  const sentOnBAfter = sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.deepEqual(sentOnBAfter.map(f => f.req_id), ['req-b1'])

  // Also test getAccountId isolation with opaque tokens and active-switch protection
  let accountId = 'user-A'
  let opaqueToken = 'opaque-1'
  const explicit = setup({ getAccessToken: () => opaqueToken, getAccountId: () => accountId })
  explicit.client.enqueue(message({ client_msg_id: 'cmsg-exp-a', content: 'secret-A' }), 'req-exp-a')
  explicit.client.connect()
  explicit.sockets[0].open()
  authenticateSocket(explicit.sockets[0])

  accountId = 'user-B'
  opaqueToken = 'opaque-2'
  assert.equal(explicit.client.pendingCount, 0)
  // Calling connect() after account change disconnects A and connects B cleanly with zero A frames
  explicit.client.connect()
  explicit.sockets[1].open()
  authenticateSocket(explicit.sockets[1])
  const explicitBFrames = explicit.sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.equal(explicitBFrames.length, 0)
})

it('B2: handshake rejection only reports failure, never replays pending before auth, and stops reusing invalidated credentials', () => {
  let token = 'expired-token-1'
  const { client, sockets } = setup({ getAccessToken: () => token, maxAuthFailures: 3 })
  const events: string[] = []
  const errorCodes: number[] = []
  client.on('open', () => events.push('open'))
  client.on('close', ev => events.push(`close:${ev.code}`))
  client.on('error', err => {
    events.push(`error:${err.kind}`)
    if (err.kind === 'frame') errorCodes.push(err.frame.data.code as number)
  })

  client.enqueue(message({ client_msg_id: 'cmsg-1', content: 'wait-for-auth' }), 'req-1')
  client.connect()

  // Server completes HTTP 101 upgrade, then immediately rejects with 18001 + 1008
  sockets[0].open()
  assert.deepEqual(events, [])
  assert.equal(sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)

  sockets[0].receive(frame('error', { code: 18001, message: '11001 access token expired', retryable: false, related_type: 'handshake', related_req_id: '' }, 'ws-req-1'))
  sockets[0].close(1008, '18001')

  assert.deepEqual(events, ['error:frame', 'close:1008'])
  assert.deepEqual(errorCodes, [18001])
  assert.equal(sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)
  assert.equal(client.pendingCount, 1)

  // Same invalidated credential must NOT be re-dialed across backoff ticks
  mock.timers.tick(500)
  assert.equal(sockets.length, 1)

  // If a refreshed valid token becomes available before next backoff tick, client connects and replays pending after pong
  token = 'fresh-valid-token'
  mock.timers.tick(1000)
  assert.equal(sockets.length, 2)
  sockets[1].open()
  assert.equal(events.filter(e => e === 'open').length, 0)
  assert.equal(sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)

  authenticateSocket(sockets[1])
  assert.equal(events.filter(e => e === 'open').length, 1)
  const replayed = sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.deepEqual(replayed.map(f => f.req_id), ['req-1'])

  // Consecutive rejections with distinct bad tokens back off exponentially and stop at maxAuthFailures
  sockets[1].close()
  let attemptIdx = 0
  token = `bad-tok-${++attemptIdx}`
  mock.timers.tick(500)
  assert.equal(sockets.length, 3)
  sockets[2].open()
  sockets[2].receive(frame('error', { code: 18001, message: 'invalid', retryable: false }))
  sockets[2].close(1008, '18001')

  token = `bad-tok-${++attemptIdx}`
  mock.timers.tick(999)
  assert.equal(sockets.length, 3)
  mock.timers.tick(1)
  assert.equal(sockets.length, 4)
  sockets[3].open()
  sockets[3].receive(frame('error', { code: 18001, message: 'invalid', retryable: false }))
  sockets[3].close(1008, '18001')

  // Backoff doubled to 4000*0.5=2000ms (not reset by onopen!)
  token = `bad-tok-${++attemptIdx}`
  mock.timers.tick(1999)
  assert.equal(sockets.length, 4)
  mock.timers.tick(1)
  assert.equal(sockets.length, 5)
  sockets[4].open()
  sockets[4].receive(frame('error', { code: 18001, message: 'invalid', retryable: false }))
  sockets[4].close(1008, '18001')

  // Reached maxAuthFailures=3 -> stopped retrying
  token = 'never-dialed-after-stop'
  mock.timers.tick(30000)
  assert.equal(sockets.length, 5)
  assert.equal(events.filter(e => e === 'open').length, 1)
})

it('B2: stops immediately on 18006 session revocation without replaying pending frames', () => {
  const { client, sockets } = setup({ getAccessToken: () => 'revoked-token' })
  const events: string[] = []
  client.on('open', () => events.push('open'))
  client.on('error', err => events.push(`error:${err.kind}`))
  client.on('system.notice', frame => events.push(`notice:${frame.data.event}`))
  client.on('close', ev => events.push(`close:${ev.code}`))

  client.enqueue(message({ client_msg_id: 'cmsg-rev' }), 'req-rev')
  client.connect()
  sockets[0].open()
  sockets[0].receive(frame('error', { code: 18006, message: 'session revoked', retryable: false }))
  sockets[0].receive(frame('system.notice', { event: 'session.revoked', notice_id: 'n1', payload: {} }))
  sockets[0].close(1008, '18006')

  mock.timers.tick(30000)
  assert.equal(sockets.length, 1)
  assert.deepEqual(events, ['error:frame', 'notice:session.revoked', 'close:1008'])
  assert.equal(sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)
})
