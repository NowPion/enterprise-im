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
  close() { this.readyState = 3; this.onclose?.({ code: 1006, reason: '' } as CloseEvent) }
  send(text: string) { assert.equal(this.readyState, 1); this.sent.push(text) }
  receive(data: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) })) }
}

const frame = (type: string, data: object, req_id = 'req-1') => ({ type, data, req_id, timestamp: 123 })
const message = () => ({ conversation_id: '9', client_msg_id: 'client-1', msg_type: 'text', content: 'hello', extra: {} })
const ack = { client_msg_id: 'client-1', conversation_id: '9', msg_id: '19', seq: '42', status: 'normal', delivery_status: 'sent' }

function setup() {
  const sockets: Socket[] = []
  const urls: URL[] = []
  const client = new WSClient({
    origin: 'https://im.example', getAccessToken: () => `token+${sockets.length}/=`,
    heartbeatMs: 100, getLastSeq: () => ({ '9': '42' }), random: () => 0.5,
    createSocket(url) { urls.push(new URL(url)); const socket = new Socket(); sockets.push(socket); return socket },
  })
  return { client, sockets, urls }
}

beforeEach(() => mock.timers.enable({ apis: ['setTimeout', 'setInterval'] }))
afterEach(() => mock.timers.reset())

it('authenticates once per attempt, resumes heartbeats with a fresh token and dispatches matching pong', () => {
  const { client, sockets, urls } = setup()
  client.connect(); client.connect()
  assert.equal(urls.length, 1)
  assert.equal(urls[0].origin + urls[0].pathname, 'wss://im.example/ws')
  assert.equal(urls[0].searchParams.get('access_token'), 'token+0/=')
  mock.timers.tick(100)
  assert.equal(sockets[0].sent.length, 0)
  sockets[0].open(); mock.timers.tick(100)
  const ping = JSON.parse(sockets[0].sent[0])
  assert.equal(ping.type, 'ping'); assert.ok(ping.req_id); assert.ok(Number.isSafeInteger(ping.timestamp))
  assert.ok(ping.data.nonce); assert.deepEqual(ping.data.last_seq, { '9': '42' })
  const seen: string[] = []
  client.on('pong', pong => seen.push(pong.req_id))
  sockets[0].receive(frame('pong', { nonce: ping.data.nonce, server_time: 123, session_expires_at: '2026-11-26T12:00:00Z' }, ping.req_id))
  mock.timers.tick(100)
  assert.deepEqual(seen, [ping.req_id]); assert.equal(sockets[0].sent.length, 2)
  mock.timers.tick(100); mock.timers.tick(500)
  assert.equal(urls[1].searchParams.get('access_token'), 'token+1/=')
  sockets[1].open(); mock.timers.tick(100)
  assert.equal(JSON.parse(sockets[1].sent[0]).type, 'ping')
})

it('uses full jitter with 1s exponential bounds capped at 30s, resets after open and cancels on disconnect', () => {
  const { client, sockets } = setup()
  client.connect()
  for (const cap of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
    const count = sockets.length
    sockets[count - 1].close(); mock.timers.tick(cap / 2 - 1)
    assert.equal(sockets.length, count)
    mock.timers.tick(1); assert.equal(sockets.length, count + 1)
  }
  sockets.at(-1)!.open(); sockets.at(-1)!.close(); mock.timers.tick(500)
  assert.equal(sockets.length, 9)
  sockets[8].close(); client.disconnect(); mock.timers.tick(30000)
  assert.equal(sockets.length, 9)
})

it('rejects malformed envelopes and separates lifecycle, server error and push events', () => {
  const { client, sockets } = setup()
  const seen: string[] = []
  for (const type of ['open', 'close', 'presence.update', 'system.notice'] as const) client.on(type, () => seen.push(type))
  const unsubscribe = client.on('error', error => seen.push(error.kind))
  client.connect(); const socket = sockets[0]; socket.open()
  socket.onerror?.(new Event('error'))
  for (const invalid of [{ online: true }, { ...frame('presence.update', {}), req_id: '' }, { ...frame('pong', {}), timestamp: 1.5 }, frame('system.notice', [])]) socket.receive(invalid)
  socket.onmessage?.(new MessageEvent('message', { data: '{' }))
  socket.receive(frame('presence.update', { user_id: '2', online: true, device_count: 1, last_online_at: null }))
  socket.receive(frame('system.notice', { event: 'server.draining', payload: {} }))
  socket.receive(frame('error', { code: 18002, message: 'unsupported' }))
  unsubscribe(); socket.onerror?.(new Event('error')); socket.close()
  assert.deepEqual(seen, ['open', 'transport', ...Array(5).fill('protocol'), 'presence.update', 'system.notice', 'frame', 'close'])
})

it('replays stable messages until a matching persisted ACK, ignoring errors, partial ACKs and stale sockets', () => {
  const { client, sockets } = setup()
  const data = message()
  client.enqueue(data, 'req-1'); data.content = 'edited'
  client.connect(); const first = sockets[0]; first.open()
  assert.equal(JSON.parse(first.sent[0]).data.content, 'hello')
  const staleMessage = first.onmessage
  first.receive(frame('error', { related_req_id: 'req-1' })); first.close()
  client.enqueue({ ...message(), client_msg_id: 'client-2' }, 'req-2')
  mock.timers.tick(500); const second = sockets[1]; second.open()
  assert.deepEqual(second.sent.map(text => JSON.parse(text).req_id), ['req-1', 'req-2'])
  assert.deepEqual(JSON.parse(second.sent[0]).data, JSON.parse(first.sent[0]).data)
  staleMessage?.(new MessageEvent('message', { data: JSON.stringify(frame('msg.ack', ack)) }))
  second.receive(frame('msg.ack', { client_msg_id: 'client-1' })); second.receive(frame('msg.ack', ack, 'req-2'))
  assert.equal(client.pendingCount, 2)
  second.receive(frame('msg.ack', ack)); assert.equal(client.pendingCount, 1)
  second.close(); mock.timers.tick(500); sockets[2].open()
  assert.deepEqual(sockets[2].sent.map(text => JSON.parse(text).req_id), ['req-2'])
})
