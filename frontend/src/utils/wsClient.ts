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
  getAccountId?: () => string | null | undefined
  origin?: string
  heartbeatMs?: number
  maxAuthFailures?: number
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

interface PendingEntry {
  text: string
  clientId: string
  conversation: string
}

const DEFAULT_HEARTBEAT_MS = 25000
const DEFAULT_MAX_AUTH_FAILURES = 5
const WS_ERR_AUTH_FAILED = 18001
const WS_ERR_SESSION_REVOKED = 18006
const WS_CLOSE_POLICY_VIOLATION = 1008

function object(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function nonempty(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
}

function decodeBase64Url(segment: string): string | null {
  try {
    const normalized = segment.replace(/-/g, '+').replace(/_/g, '/')
    const padded = normalized + '='.repeat((4 - (normalized.length % 4)) % 4)
    if (typeof atob === 'function') {
      const binary = atob(padded)
      const bytes = Uint8Array.from(binary, ch => ch.charCodeAt(0))
      return new TextDecoder().decode(bytes)
    }
    return Buffer.from(padded, 'base64').toString('utf8')
  } catch {
    return null
  }
}

function tokenSubject(token: string): string | null {
  const parts = token.split('.')
  if (parts.length !== 3 || !parts[1]) return null
  const decoded = decodeBase64Url(parts[1])
  if (!decoded) return null
  try {
    const payload = JSON.parse(decoded) as unknown
    if (!object(payload)) return null
    if (nonempty(payload.sub)) return payload.sub
    if (typeof payload.sub === 'number' && Number.isSafeInteger(payload.sub) && payload.sub > 0) {
      return String(payload.sub)
    }
    return null
  } catch {
    return null
  }
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
  private readonly pendingByAccount = new Map<string, Map<string, PendingEntry>>()
  private socket?: Transport
  private socketToken?: string
  private socketAccountKey?: string
  private activeAccountKey?: string
  private socketOpened = false
  private hadTransportError = false
  private authenticated = false
  private authProbeNonce?: string
  private authTimer?: ReturnType<typeof setTimeout>
  private authRejected = false
  private authFatal = false
  private rejectedToken?: string
  private lastPreOpenFailedToken?: string
  private authFailures = 0
  private retry?: ReturnType<typeof setTimeout>
  private heartbeat?: ReturnType<typeof setInterval>
  private awaitingPong?: string
  private retryCap = 1000
  private active = false

  constructor(options: WSClientOptions) {
    this.options = options
  }

  get pendingCount() {
    const accountKey = this.resolveCurrentAccountKey()
    if (!accountKey) return 0
    return this.pendingByAccount.get(accountKey)?.size ?? 0
  }

  on<Key extends keyof Events>(type: Key, listener: (event: Events[Key]) => void) {
    const listeners = this.listeners[type]
    listeners.add(listener)
    return () => listeners.delete(listener)
  }

  private emit<Key extends keyof Events>(type: Key, event: Events[Key]) {
    this.listeners[type].forEach(listener => listener(event))
  }

  private resolveAccountKey(token: string): string | null {
    if (this.options.getAccountId) {
      const accountId = this.options.getAccountId()
      return nonempty(accountId) ? `account:${accountId.trim()}` : null
    }
    if (!nonempty(token)) return null
    const sub = tokenSubject(token)
    return sub !== null ? `sub:${sub}` : 'default'
  }

  private resolveCurrentAccountKey(): string | null {
    try {
      return this.resolveAccountKey(this.options.getAccessToken())
    } catch {
      return null
    }
  }

  connect() {
    const nextAccount = this.resolveCurrentAccountKey()
    if (this.active) {
      if (this.activeAccountKey && nextAccount && this.activeAccountKey !== nextAccount) {
        this.disconnect()
      } else {
        return
      }
    }
    this.active = true
    this.activeAccountKey = nextAccount ?? undefined
    this.attempt()
  }

  disconnect() {
    this.active = false
    clearTimeout(this.retry)
    this.retry = undefined
    this.retryCap = 1000
    this.authFailures = 0
    this.rejectedToken = undefined
    this.lastPreOpenFailedToken = undefined
    this.activeAccountKey = undefined
    const socket = this.socket
    this.release()
    if (socket) {
      socket.close()
      this.emit('close', { code: 1000, reason: 'Client disconnected' })
    }
  }

  enqueue(data: MessageInput, reqId: string = requestId()): string {
    if (!nonempty(reqId) || !nonempty(data.client_msg_id) || !nonempty(data.conversation_id)) throw new Error('Missing message identity')
    const accountKey = this.resolveCurrentAccountKey()
    if (!accountKey) throw new Error('Missing active account')
    let accountPending = this.pendingByAccount.get(accountKey)
    if (!accountPending) {
      accountPending = new Map()
      this.pendingByAccount.set(accountKey, accountPending)
    }
    if (accountPending.has(reqId)) throw new Error('Duplicate pending req_id')
    const text = encode('msg.send', reqId, data)
    accountPending.set(reqId, { text, clientId: data.client_msg_id, conversation: data.conversation_id })
    if (this.authenticated && this.socket?.readyState === 1 && this.socketAccountKey === accountKey) {
      this.write(this.socket, text)
    }
    return reqId
  }

  private attempt() {
    clearTimeout(this.retry)
    this.retry = undefined
    let socket: Transport
    let token: string
    let accountKey: string | null
    try {
      const url = new URL('/ws', this.options.origin ?? location.origin)
      if (url.protocol === 'http:') url.protocol = 'ws:'
      if (url.protocol === 'https:') url.protocol = 'wss:'
      token = this.options.getAccessToken()
      if (!nonempty(token)) throw new Error('Missing access token')
      accountKey = this.resolveAccountKey(token)
      if (!accountKey) throw new Error('Missing active account')
      if (this.rejectedToken !== undefined && token === this.rejectedToken) {
        this.authFailures += 1
        if (this.authFailures >= (this.options.maxAuthFailures ?? DEFAULT_MAX_AUTH_FAILURES)) {
          this.active = false
          return
        }
        this.scheduleRetry()
        return
      }
      url.searchParams.set('access_token', token)
      socket = this.options.createSocket?.(url.href) ?? new WebSocket(url)
    } catch (cause) {
      this.scheduleRetry()
      this.emit('error', { kind: 'transport', cause })
      return
    }
    this.socket = socket
    this.socketToken = token
    this.socketAccountKey = accountKey
    this.activeAccountKey = accountKey
    this.socketOpened = false
    this.hadTransportError = false
    this.authenticated = false
    this.authRejected = false
    this.authFatal = false
    socket.onopen = () => {
      if (socket !== this.socket) return
      this.socketOpened = true
      this.lastPreOpenFailedToken = undefined
      // Do not treat HTTP 101 open as authenticated; verify session via ping/pong first.
      this.startAuthProbe(socket)
    }
    socket.onclose = event => {
      if (socket !== this.socket) return
      const wasOpened = this.socketOpened
      const wasAuthenticated = this.authenticated
      const hadErrorFrame = this.authRejected
      const pre101Error = !wasOpened && this.hadTransportError
      const repeatedPreOpenClose = !wasOpened && this.socketToken !== undefined && this.socketToken === this.lastPreOpenFailedToken
      const authRejected = this.authRejected || (!wasAuthenticated && event.code === WS_CLOSE_POLICY_VIOLATION) || pre101Error || repeatedPreOpenClose
      const authFatal = this.authFatal
      const rejectedToken = this.rejectedToken ?? (authRejected ? this.socketToken : undefined)
      if (!wasOpened && this.socketToken !== undefined) {
        this.lastPreOpenFailedToken = this.socketToken
      }
      this.release()
      if (!wasAuthenticated && event.code === WS_CLOSE_POLICY_VIOLATION && !hadErrorFrame) {
        this.emit('error', { kind: 'protocol', cause: new Error(event.reason || 'WebSocket handshake rejected') })
      }
      if (authRejected && rejectedToken !== undefined) {
        this.rejectedToken = rejectedToken
      }
      if (authFatal) {
        this.active = false
      } else if (authRejected) {
        this.authFailures += 1
        if (this.authFailures >= (this.options.maxAuthFailures ?? DEFAULT_MAX_AUTH_FAILURES)) {
          this.active = false
        } else {
          this.scheduleRetry()
        }
      } else {
        this.scheduleRetry()
      }
      this.emit('close', { code: event.code, reason: event.reason })
    }
    socket.onerror = cause => {
      if (socket !== this.socket) return
      this.hadTransportError = true
      this.emit('error', { kind: 'transport', cause })
    }
    socket.onmessage = event => {
      if (socket === this.socket) this.receive(socket, event.data)
    }
  }

  private startAuthProbe(socket: Transport) {
    const nonce = requestId()
    this.awaitingPong = nonce
    this.authProbeNonce = nonce
    const timeoutMs = this.options.heartbeatMs ?? DEFAULT_HEARTBEAT_MS
    this.authTimer = setTimeout(() => {
      if (socket === this.socket && !this.authenticated) socket.close()
    }, timeoutMs)
    this.write(socket, encode('ping', nonce, { nonce, last_seq: this.options.getLastSeq?.() ?? {} }))
  }

  private completeAuth(socket: Transport): boolean {
    if (socket !== this.socket) return false
    this.authenticated = true
    this.retryCap = 1000
    this.authFailures = 0
    this.rejectedToken = undefined
    this.lastPreOpenFailedToken = undefined
    this.heartbeat = setInterval(() => this.ping(socket), this.options.heartbeatMs ?? DEFAULT_HEARTBEAT_MS)
    const accountPending = this.socketAccountKey ? this.pendingByAccount.get(this.socketAccountKey) : undefined
    if (accountPending) {
      for (const { text } of accountPending.values()) {
        if (!this.write(socket, text)) return false
      }
    }
    this.emit('open', undefined)
    return socket === this.socket
  }

  private release() {
    clearTimeout(this.authTimer)
    this.authTimer = undefined
    clearInterval(this.heartbeat)
    this.heartbeat = undefined
    this.awaitingPong = undefined
    this.authProbeNonce = undefined
    this.socketOpened = false
    this.hadTransportError = false
    this.authenticated = false
    this.authRejected = false
    this.authFatal = false
    this.socketToken = undefined
    this.socketAccountKey = undefined
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
    if (socket !== this.socket || socket.readyState !== 1 || !this.authenticated) return
    if (this.awaitingPong) {
      socket.close()
      return
    }
    const nonce = requestId()
    this.awaitingPong = nonce
    this.write(socket, encode('ping', nonce, { nonce, last_seq: this.options.getLastSeq?.() ?? {} }))
  }

  private receive(socket: Transport, raw: unknown) {
    let frame: Envelope
    try {
      frame = decode(raw)
    } catch (cause) {
      this.emit('error', { kind: 'protocol', cause })
      return
    }
    switch (frame.type) {
      case 'error': {
        const code = frame.data.code
        if (code === WS_ERR_SESSION_REVOKED) {
          this.authRejected = true
          this.authFatal = true
          this.rejectedToken = this.socketToken
        } else if (!this.authenticated && code === WS_ERR_AUTH_FAILED) {
          this.authRejected = true
          this.rejectedToken = this.socketToken
        }
        this.emit('error', { kind: 'frame', frame })
        break
      }
      case 'pong': {
        if (frame.req_id !== this.awaitingPong || frame.data.nonce !== this.awaitingPong) return
        this.awaitingPong = undefined
        if (!this.authenticated && frame.req_id === this.authProbeNonce) {
          this.authProbeNonce = undefined
          clearTimeout(this.authTimer)
          this.authTimer = undefined
          if (!this.completeAuth(socket)) return
        }
        this.emit('pong', frame)
        break
      }
      case 'msg.ack': {
        if (!this.authenticated || !this.socketAccountKey) return
        const accountPending = this.pendingByAccount.get(this.socketAccountKey)
        const entry = accountPending?.get(frame.req_id)
        const data = frame.data
        if (entry && entry.clientId === data.client_msg_id && entry.conversation === data.conversation_id &&
            nonempty(data.msg_id) && nonempty(data.seq) && (data.status === 'normal' || data.status === 'recalled') &&
            ['sent', 'delivered', 'read'].some(status => status === data.delivery_status)) {
          accountPending!.delete(frame.req_id)
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
