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
  failPre101(reason = '') {
    this.readyState = 3
    this.onerror?.(new Event('error'))
    this.onclose?.({ code: 1006, reason } as CloseEvent)
  }
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

it('B2: treats pre-101 HTTP 401 rejection (onerror + onclose 1006 without onopen) as auth failure and never infinitely retries the same token', () => {
  let token = 'expired-pre101-token'
  const { client, sockets, urls } = setup({ getAccessToken: () => token, maxAuthFailures: 3 })
  const events: string[] = []
  client.on('open', () => events.push('open'))
  client.on('close', ev => events.push(`close:${ev.code}`))
  client.on('error', err => events.push(`error:${err.kind}`))

  client.enqueue(message({ client_msg_id: 'cmsg-pre101', content: 'wait-for-auth' }), 'req-pre101')
  client.connect()

  // Server rejects before HTTP 101 upgrade (e.g., HTTP 401 + 18001): browser only fires onerror + onclose(1006)
  sockets[0].failPre101()
  assert.deepEqual(events, ['error:transport', 'close:1006'])
  assert.equal(sockets[0].sent.length, 0)
  assert.equal(client.pendingCount, 1)

  // Across multiple backoff ticks with the same invalidated token, client must NOT dial 7 times with the same token
  for (const ms of [500, 1000, 2000, 4000, 8000, 16000, 30000]) {
    mock.timers.tick(ms)
  }
  assert.equal(sockets.length, 1)
  assert.deepEqual(urls.map(u => u.searchParams.get('access_token')), ['expired-pre101-token'])
  assert.equal(events.filter(e => e === 'open').length, 0)

  // Also verify recovery when a fresh valid token arrives during backoff after a pre-101 rejection
  client.disconnect()
  events.length = 0
  token = 'expired-token-2'
  client.connect()
  assert.equal(sockets.length, 2)
  sockets[1].failPre101()
  assert.deepEqual(events, ['error:transport', 'close:1006'])

  mock.timers.tick(500)
  assert.equal(sockets.length, 2) // still blocked while token unchanged

  token = 'fresh-valid-token'
  mock.timers.tick(1000)
  assert.equal(sockets.length, 3)
  sockets[2].open()
  assert.equal(events.filter(e => e === 'open').length, 0)
  assert.equal(sockets[2].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)

  authenticateSocket(sockets[2])
  assert.equal(events.filter(e => e === 'open').length, 1)
  assert.deepEqual(
    sockets[2].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').map(f => f.req_id),
    ['req-pre101'],
  )

  // Consecutive pre-101 rejections with distinct invalid tokens back off exponentially and stop at maxAuthFailures=3
  sockets[2].close()
  let idx = 0
  token = `bad-pre101-${++idx}`
  mock.timers.tick(500)
  assert.equal(sockets.length, 4)
  sockets[3].failPre101()

  token = `bad-pre101-${++idx}`
  mock.timers.tick(999)
  assert.equal(sockets.length, 4)
  mock.timers.tick(1)
  assert.equal(sockets.length, 5)
  sockets[4].failPre101()

  token = `bad-pre101-${++idx}`
  mock.timers.tick(1999)
  assert.equal(sockets.length, 5)
  mock.timers.tick(1)
  assert.equal(sockets.length, 6)
  sockets[5].failPre101()

  token = 'never-dialed-after-stop'
  mock.timers.tick(30000)
  assert.equal(sockets.length, 6)
})

it('B2: keeps 18001/1008 post-upgrade rejection and immediate account switch during backoff working', () => {
  let token = makeJwt('1001', 'jti-a1')
  const { client, sockets, urls } = setup({ getAccessToken: () => token, maxAuthFailures: 3 })
  const events: string[] = []
  const errorCodes: number[] = []
  client.on('open', () => events.push('open'))
  client.on('close', ev => events.push(`close:${ev.code}`))
  client.on('error', err => {
    events.push(`error:${err.kind}`)
    if (err.kind === 'frame') errorCodes.push(err.frame.data.code as number)
  })

  client.enqueue(message({ client_msg_id: 'cmsg-a1', content: 'from-A' }), 'req-a1')
  client.connect()

  // Post-101 18001 + 1008 defense-in-depth path still blocks replay, reports error, and blocks token reuse
  sockets[0].open()
  assert.deepEqual(events, [])
  assert.equal(sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)
  sockets[0].receive(frame('error', { code: 18001, message: '11001 access token expired', retryable: false, related_type: 'handshake', related_req_id: '' }, 'ws-req-1'))
  sockets[0].close(1008, '18001')

  assert.deepEqual(events, ['error:frame', 'close:1008'])
  assert.deepEqual(errorCodes, [18001])
  assert.equal(sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)

  mock.timers.tick(500)
  assert.equal(sockets.length, 1)

  // Switching to account B while A is waiting in backoff dials B immediately without leaking A's frames
  token = makeJwt('2002', 'jti-b1')
  client.connect()
  assert.equal(sockets.length, 2)
  assert.equal(urls[1].searchParams.get('access_token'), token)
  sockets[1].open()
  authenticateSocket(sockets[1])
  assert.equal(sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)
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

it('applies full jitter upper/lower bounds during exponential backoff reconnection', () => {
  const minJitter = setup({ random: () => 0 })
  minJitter.client.connect()
  minJitter.sockets[0].close()
  mock.timers.tick(0)
  assert.equal(minJitter.sockets.length, 2)
  minJitter.client.disconnect()

  const maxJitter = setup({ random: () => 0.999 })
  maxJitter.client.connect()
  for (const cap of [1000, 2000, 4000]) {
    const delay = Math.floor(0.999 * cap)
    const count = maxJitter.sockets.length
    maxJitter.sockets[count - 1].close()
    mock.timers.tick(delay - 1)
    assert.equal(maxJitter.sockets.length, count)
    mock.timers.tick(1)
    assert.equal(maxJitter.sockets.length, count + 1)
  }
  maxJitter.client.disconnect()
})

it('handles steady-state heartbeat ping/pong, rejects mismatched pongs, and times out unauthenticated probes', () => {
  const { client, sockets } = setup({ heartbeatMs: 100, random: () => 0.5 })
  const closes: number[] = []
  const opens: number[] = []
  const pongs: string[] = []
  client.on('open', () => opens.push(1))
  client.on('close', ev => closes.push(ev.code))
  client.on('pong', ev => pongs.push(ev.req_id))

  // Auth probe timeout closes socket and schedules reconnect without emitting open
  client.connect()
  sockets[0].open()
  const badProbe = JSON.parse(sockets[0].sent[0])
  sockets[0].receive(frame('pong', { nonce: 'wrong-nonce' }, badProbe.req_id))
  sockets[0].receive(frame('pong', { nonce: badProbe.data.nonce }, 'wrong-req-id'))
  assert.equal(opens.length, 0)
  mock.timers.tick(100)
  assert.equal(closes.length, 1)

  // Reconnect and verify steady-state multi-cycle ping/pong keeps connection open
  mock.timers.tick(500)
  assert.equal(sockets.length, 2)
  sockets[1].open()
  authenticateSocket(sockets[1])
  assert.equal(opens.length, 1)

  for (let cycle = 1; cycle <= 3; cycle++) {
    mock.timers.tick(100)
    const pingFrame = JSON.parse(sockets[1].sent[cycle])
    assert.equal(pingFrame.type, 'ping')
    // Mismatched pong must not clear awaitingPong
    sockets[1].receive(frame('pong', { nonce: 'stale-nonce' }, pingFrame.req_id))
    assert.equal(pongs.length, cycle)
    // Matching pong clears awaitingPong and emits pong
    sockets[1].receive(frame('pong', { nonce: pingFrame.data.nonce, server_time: 1000 + cycle }, pingFrame.req_id))
    assert.equal(pongs.length, cycle + 1)
  }
  assert.equal(closes.length, 1)

  // Missing pong on next heartbeat triggers close
  mock.timers.tick(100)
  mock.timers.tick(100)
  assert.equal(closes.length, 2)
  client.disconnect()
})

it('evaluates getLastSeq dynamically on auth probes, heartbeats, and reconnects, defaulting to empty object', () => {
  let seqMap: Record<string, string> = { '9': '42' }
  const { client, sockets } = setup({ getLastSeq: () => ({ ...seqMap }), heartbeatMs: 100, random: () => 0.5 })

  client.connect()
  sockets[0].open()
  const authPing = authenticateSocket(sockets[0])
  assert.deepEqual(authPing.data.last_seq, { '9': '42' })

  seqMap = { '9': '45', '12': '3' }
  mock.timers.tick(100)
  const hbPing = JSON.parse(sockets[0].sent[1])
  assert.equal(hbPing.type, 'ping')
  assert.deepEqual(hbPing.data.last_seq, { '9': '45', '12': '3' })

  seqMap = { '9': '48', '12': '3' }
  sockets[0].close()
  mock.timers.tick(500)
  sockets[1].open()
  const reconnectPing = authenticateSocket(sockets[1])
  assert.deepEqual(reconnectPing.data.last_seq, { '9': '48', '12': '3' })
  client.disconnect()

  const withoutHook = setup({ getLastSeq: undefined })
  withoutHook.client.connect()
  withoutHook.sockets[0].open()
  const defaultPing = JSON.parse(withoutHook.sockets[0].sent[0])
  assert.deepEqual(defaultPing.data.last_seq, {})
  withoutHook.client.disconnect()
})

it('preserves offline FIFO pending queue per account across A->B->A switch and dispatches push/recall/delivered/read acks', () => {
  let token = makeJwt('1001', 'jti-a1')
  const { client, sockets } = setup({ getAccessToken: () => token })
  const pushes: unknown[] = []
  const recalls: unknown[] = []
  const acks: string[] = []
  client.on('msg.push', ev => pushes.push(ev.data))
  client.on('msg.recall', ev => recalls.push(ev.data))
  client.on('msg.ack', ev => acks.push(`${ev.req_id}:${ev.data.delivery_status}:${ev.data.status}`))

  // Enqueue offline messages before connect
  client.enqueue(message({ client_msg_id: 'c-fifo-1', content: 'first' }), 'req-fifo-1')
  client.enqueue(message({ client_msg_id: 'c-fifo-2', content: 'second' }), 'req-fifo-2')
  assert.equal(client.pendingCount, 2)
  assert.equal(sockets.length, 0)

  client.connect()
  sockets[0].open()
  authenticateSocket(sockets[0])

  const sentA1 = sockets[0].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.deepEqual(sentA1.map(f => f.req_id), ['req-fifo-1', 'req-fifo-2'])
  assert.deepEqual(sentA1.map(f => f.data.client_msg_id), ['c-fifo-1', 'c-fifo-2'])

  // Settle first with delivered, keep second pending
  sockets[0].receive(frame('msg.ack', { ...ack, client_msg_id: 'c-fifo-1', msg_id: '201', seq: '50', delivery_status: 'delivered' }, 'req-fifo-1'))
  assert.equal(client.pendingCount, 1)
  assert.deepEqual(acks, ['req-fifo-1:delivered:normal'])

  // Switch to account B: pendingCount is 0, B gets zero A frames
  client.disconnect()
  token = makeJwt('2002', 'jti-b1')
  assert.equal(client.pendingCount, 0)
  client.connect()
  sockets[1].open()
  authenticateSocket(sockets[1])
  assert.equal(sockets[1].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send').length, 0)

  // Emit msg.push and msg.recall events on active socket
  sockets[1].receive(frame('msg.push', { push_type: 'message', message: { msg_id: '301', seq: '1', conversation_id: '15' } }, 'push-1'))
  sockets[1].receive(frame('msg.recall', { conversation_id: '15', recalled_msg_id: '301', recall_msg_id: '302', seq: '2' }, 'recall-1'))
  assert.equal(pushes.length, 1)
  assert.equal(recalls.length, 1)

  // Switch back to account A: A's un-acked second message is preserved and replayed with original client_msg_id
  client.disconnect()
  token = makeJwt('1001', 'jti-a2')
  assert.equal(client.pendingCount, 1)
  client.connect()
  sockets[2].open()
  authenticateSocket(sockets[2])
  const sentA2 = sockets[2].sent.map(raw => JSON.parse(raw)).filter(f => f.type === 'msg.send')
  assert.deepEqual(sentA2.map(f => f.req_id), ['req-fifo-2'])
  assert.deepEqual(sentA2.map(f => f.data.client_msg_id), ['c-fifo-2'])

  sockets[2].receive(frame('msg.ack', { ...ack, client_msg_id: 'c-fifo-2', msg_id: '202', seq: '51', status: 'recalled', delivery_status: 'read' }, 'req-fifo-2'))
  assert.equal(client.pendingCount, 0)
  assert.deepEqual(acks, ['req-fifo-1:delivered:normal', 'req-fifo-2:read:recalled'])
  client.disconnect()
})

it('closes socket and emits transport error when socket.send throws synchronously', () => {
  const { client, sockets } = setup({ heartbeatMs: 100 })
  const errors: string[] = []
  const closes: number[] = []
  client.on('error', err => errors.push(err.kind))
  client.on('close', ev => closes.push(ev.code))

  client.connect()
  sockets[0].open()
  authenticateSocket(sockets[0])

  sockets[0].send = () => {
    throw new Error('socket write failure')
  }
  mock.timers.tick(100)
  assert.deepEqual(errors, ['transport'])
  assert.equal(closes.length, 1)
  client.disconnect()
})
