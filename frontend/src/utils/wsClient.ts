type Json = string | number | boolean | null | Json[] | { [key: string]: Json }
type Data = { [key: string]: Json }
export interface Envelope {
  type: string
  req_id: string
  timestamp: number
  data: Data
}

export interface MessageInput extends Data {
  conversation_id: string
  client_msg_id: string
  msg_type: string
  content: string
  extra: Data
}

export interface Transport {
  readonly readyState: number
  onopen: ((event: Event) => void) | null
  onclose: ((event: CloseEvent) => void) | null
  onerror: ((event: Event) => void) | null
  onmessage: ((event: MessageEvent) => void) | null
  send(text: string): void
  close(): void
}

export interface WSClientOptions {
  getAccessToken: () => string
  origin?: string
  heartbeatMs?: number
  getLastSeq?: () => Record<string, string>
  createSocket?: (url: string) => Transport
  random?: () => number
}

type FrameEvent = 'pong' | 'presence.update' | 'system.notice' | 'msg.ack' | 'msg.push' | 'msg.recall'
type Events = Record<FrameEvent, Envelope> & {
  open: undefined
  close: { code: number; reason: string }
  error: { kind: 'frame'; frame: Envelope } | { kind: 'protocol' | 'transport'; cause: unknown }
}

function object(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function nonempty(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
}

function decode(raw: unknown): Envelope {
  if (typeof raw !== 'string') throw new Error('Expected a JSON text frame')
  const value = JSON.parse(raw)
  if (!object(value) || !nonempty(value.type) || !nonempty(value.req_id) ||
      !Number.isSafeInteger(value.timestamp) || !object(value.data)) throw new Error('Invalid WebSocket envelope')
  return value as unknown as Envelope
}

function encode(type: string, req_id: string, data: Data): string {
  return JSON.stringify({ type, req_id, timestamp: Date.now(), data } satisfies Envelope)
}

function requestId(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
}

export class WSClient {
  private readonly options: WSClientOptions
  private readonly listeners: { [Key in keyof Events]: Set<(event: Events[Key]) => void> } = {
    open: new Set(), close: new Set(), error: new Set(), pong: new Set(),
    'presence.update': new Set(), 'system.notice': new Set(), 'msg.ack': new Set(),
    'msg.push': new Set(), 'msg.recall': new Set(),
  }
  private readonly pending = new Map<string, { text: string; clientId: string; conversation: string }>()
  private socket?: Transport
  private retry?: ReturnType<typeof setTimeout>
  private heartbeat?: ReturnType<typeof setInterval>
  private awaitingPong?: string
  private retryCap = 1000
  private active = false

  constructor(options: WSClientOptions) {
    this.options = options
  }

  get pendingCount() { return this.pending.size }

  on<Key extends keyof Events>(type: Key, listener: (event: Events[Key]) => void) {
    const listeners = this.listeners[type]
    listeners.add(listener)
    return () => listeners.delete(listener)
  }

  private emit<Key extends keyof Events>(type: Key, event: Events[Key]) {
    this.listeners[type].forEach(listener => listener(event))
  }

  connect() {
    if (this.active) return
    this.active = true
    this.attempt()
  }

  disconnect() {
    this.active = false
    clearTimeout(this.retry)
    this.retryCap = 1000
    const socket = this.socket
    this.release()
    if (socket) {
      socket.close()
      this.emit('close', { code: 1000, reason: 'Client disconnected' })
    }
  }

  enqueue(data: MessageInput, reqId: string = requestId()): string {
    if (!nonempty(reqId) || !nonempty(data.client_msg_id) || !nonempty(data.conversation_id)) throw new Error('Missing message identity')
    if (this.pending.has(reqId)) throw new Error('Duplicate pending req_id')
    const text = encode('msg.send', reqId, data)
    this.pending.set(reqId, { text, clientId: data.client_msg_id, conversation: data.conversation_id })
    if (this.socket?.readyState === 1) this.write(this.socket, text)
    return reqId
  }

  private attempt() {
    let socket: Transport
    try {
      const url = new URL('/ws', this.options.origin ?? location.origin)
      if (url.protocol === 'http:') url.protocol = 'ws:'
      if (url.protocol === 'https:') url.protocol = 'wss:'
      const token = this.options.getAccessToken()
      if (!nonempty(token)) throw new Error('Missing access token')
      url.searchParams.set('access_token', token)
      socket = this.options.createSocket?.(url.href) ?? new WebSocket(url)
    } catch (cause) {
      this.scheduleRetry()
      this.emit('error', { kind: 'transport', cause })
      return
    }
    this.socket = socket
    socket.onopen = () => {
      if (socket !== this.socket) return
      this.retryCap = 1000
      this.heartbeat = setInterval(() => this.ping(socket), this.options.heartbeatMs ?? 25000)
      for (const { text } of this.pending.values()) {
        if (!this.write(socket, text)) return
      }
      this.emit('open', undefined)
    }
    socket.onclose = event => {
      if (socket !== this.socket) return
      this.release()
      this.scheduleRetry()
      this.emit('close', { code: event.code, reason: event.reason })
    }
    socket.onerror = cause => {
      if (socket === this.socket) this.emit('error', { kind: 'transport', cause })
    }
    socket.onmessage = event => {
      if (socket === this.socket) this.receive(event.data)
    }
  }

  private release() {
    clearInterval(this.heartbeat)
    this.awaitingPong = undefined
    this.socket = undefined
  }

  private scheduleRetry() {
    if (!this.active) return
    const delay = Math.floor((this.options.random ?? Math.random)() * this.retryCap)
    this.retryCap = Math.min(30000, this.retryCap * 2)
    this.retry = setTimeout(() => this.attempt(), delay)
  }

  private write(socket: Transport, text: string): boolean {
    try {
      socket.send(text)
      return true
    } catch (cause) {
      socket.close()
      this.emit('error', { kind: 'transport', cause })
      return false
    }
  }

  private ping(socket: Transport) {
    if (socket !== this.socket || socket.readyState !== 1) return
    if (this.awaitingPong) {
      socket.close()
      return
    }
    const nonce = requestId()
    this.awaitingPong = nonce
    this.write(socket, encode('ping', nonce, { nonce, last_seq: this.options.getLastSeq?.() ?? {} }))
  }

  private receive(raw: unknown) {
    let frame: Envelope
    try {
      frame = decode(raw)
    } catch (cause) {
      this.emit('error', { kind: 'protocol', cause })
      return
    }
    switch (frame.type) {
      case 'error':
        this.emit('error', { kind: 'frame', frame })
        break
      case 'pong':
        if (frame.req_id !== this.awaitingPong || frame.data.nonce !== this.awaitingPong) return
        this.awaitingPong = undefined
        this.emit('pong', frame)
        break
      case 'msg.ack': {
        const entry = this.pending.get(frame.req_id)
        const data = frame.data
        if (entry && entry.clientId === data.client_msg_id && entry.conversation === data.conversation_id &&
            nonempty(data.msg_id) && nonempty(data.seq) && (data.status === 'normal' || data.status === 'recalled') &&
            ['sent', 'delivered', 'read'].some(status => status === data.delivery_status)) {
          this.pending.delete(frame.req_id)
          this.emit('msg.ack', frame)
        }
        break
      }
      case 'presence.update':
      case 'system.notice':
      case 'msg.push':
      case 'msg.recall':
        this.emit(frame.type, frame)
        break
      default:
        this.emit('error', { kind: 'protocol', cause: new Error('Unexpected WebSocket frame type') })
    }
  }
}
