/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import request from '@/api/request'
import { getStoredTokens } from '@/composables/useAuthRefresh'
import { setAuthRedirectHandler, useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import {
  apiSuccess,
  authenticateFakeTransport,
  FakeTransport,
  installInMemoryRequestAdapter,
  installInMemoryXhr,
  makeTestJwt,
  seedAuthUser,
} from './helpers'

describe('auth single-flight token refresh and contact presence updates', () => {
  let restoreHttp: (() => void) | undefined
  let restoreWs: (() => void) | undefined

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
  })

  afterEach(() => {
    useContactStore().reset()
    useAuthStore().clearLocalSession()
    restoreHttp?.()
    restoreHttp = undefined
    restoreWs?.()
    restoreWs = undefined
    localStorage.clear()
  })

  it('deduplicates concurrent 401/11001 responses into a single /auth/refresh call and replays queued requests', async () => {
    const initialToken = makeTestJwt('10001', 'jti-expired')
    const rotatedToken = makeTestJwt('10001', 'jti-fresh')
    const initialTokens = {
      access_token: initialToken,
      refresh_token: 'rt-old-1',
      token_type: 'Bearer',
      expires_in: 900,
      refresh_expires_in: 604800,
      session_id: '501',
      device_id: 'dev-web-1',
    }
    localStorage.setItem('im_auth_tokens', JSON.stringify(initialTokens))
    localStorage.setItem('im_device_id', 'dev-web-1')

    const authStore = useAuthStore()
    seedAuthUser(authStore, '10001')
    authStore.tokens = { ...initialTokens }

    let refreshCount = 0
    let activeToken = initialToken

    // Intercept at the XHR transport level so both business `request` and `useAuthRefresh`'s private `rawClient` are covered.
    const xhr = installInMemoryXhr((method, url, body, headers) => {
      if (method === 'POST' && url.endsWith('/auth/refresh')) {
        const reqBody = (body ?? {}) as { refresh_token?: string; device_id?: string }
        expect(reqBody.refresh_token).toBe('rt-old-1')
        expect(reqBody.device_id).toBe('dev-web-1')
        refreshCount += 1
        activeToken = rotatedToken
        return {
          body: apiSuccess({
            tokens: {
              access_token: rotatedToken,
              refresh_token: 'rt-new-2',
              token_type: 'Bearer',
              expires_in: 900,
              refresh_expires_in: 604800,
              session_id: '501',
              device_id: 'dev-web-1',
            },
          }),
        }
      }
      if (method === 'GET' && url.endsWith('/me')) {
        if (headers.authorization !== `Bearer ${rotatedToken}` || activeToken !== rotatedToken) {
          return {
            status: 401,
            body: { code: 11001, message: 'access token expired', data: null, request_id: 'err-11001' },
          }
        }
        return {
          body: apiSuccess({
            user: { id: '10001', username: 'alice', nickname: 'Alice', avatar_url: '', signature: '', online: true, last_online_at: null },
            session_id: '501',
            device_id: 'dev-web-1',
          }),
        }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected route', data: null, request_id: 'err-404' } }
    })
    restoreHttp = xhr.restore

    const responses = await Promise.all([
      request.get('/me'),
      request.get('/me'),
      request.get('/me'),
    ])

    expect(refreshCount).toBe(1)
    expect(responses).toHaveLength(3)
    expect(responses.every(r => r.data?.code === 0)).toBe(true)
    expect(xhr.calls.filter(c => c.method === 'POST' && c.url.endsWith('/auth/refresh'))).toHaveLength(1)
    expect(xhr.calls.filter(c => c.method === 'GET' && c.url.endsWith('/me'))).toHaveLength(6)
    expect(getStoredTokens()?.access_token).toBe(rotatedToken)
    expect(getStoredTokens()?.refresh_token).toBe('rt-new-2')
  })

  it('updates friend online status and last_online_at when presence.update arrives', async () => {
    const adapter = installInMemoryRequestAdapter(request, (method, url) => {
      if (method === 'GET' && url.endsWith('/friends')) {
        return {
          body: apiSuccess({
            items: [
              {
                friend_id: '10002',
                user: {
                  id: '10002',
                  username: 'bob',
                  nickname: 'Bob',
                  avatar_url: '',
                  signature: '',
                  online: false,
                  last_online_at: '2026-11-26T01:00:00.000Z',
                },
                remark: '',
                online: false,
                conversation_id: '90001',
              },
            ],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      return { body: apiSuccess({ items: [], next_cursor: null, has_more: false }) }
    })
    restoreHttp = adapter.restore

    const sockets: FakeTransport[] = []
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

    const token = makeTestJwt('10001')
    localStorage.setItem(
      'im_auth_tokens',
      JSON.stringify({
        access_token: token,
        refresh_token: 'rt-1',
        token_type: 'Bearer',
        expires_in: 900,
        refresh_expires_in: 604800,
        session_id: '501',
        device_id: 'dev-web-1',
      }),
    )

    const authStore = useAuthStore()
    seedAuthUser(authStore, '10001')
    authStore.tokens = getStoredTokens()

    const contactStore = useContactStore()
    const maybeBindWSClient = (contactStore as unknown as { bindWSClient?: () => void }).bindWSClient
    if (typeof maybeBindWSClient === 'function') {
      maybeBindWSClient()
    } else {
      contactStore.bindRealtimeListeners()
    }

    authStore.connectWS()
    expect(sockets.length).toBeGreaterThanOrEqual(1)
    const activeSocket = sockets[sockets.length - 1]
    activeSocket.open()
    authenticateFakeTransport(activeSocket)

    await contactStore.fetchFriends()

    activeSocket.receive({
      type: 'presence.update',
      req_id: 'pres-1',
      timestamp: Date.now(),
      data: { user_id: '10002', online: true, device_count: 1, last_online_at: null },
    })

    const bob = contactStore.findFriend('10002')
    expect(bob?.online).toBe(true)
    expect(bob?.user.online).toBe(true)
    expect(bob?.user.last_online_at).toBeNull()

    const offlineAt = '2026-11-26T12:00:00.000Z'
    activeSocket.receive({
      type: 'presence.update',
      req_id: 'pres-2',
      timestamp: Date.now(),
      data: { user_id: '10002', online: false, device_count: 0, last_online_at: offlineAt },
    })

    expect(bob?.online).toBe(false)
    expect(bob?.user.online).toBe(false)
    expect(bob?.user.last_online_at).toBe(offlineAt)
  })

  it('handles login, initAuth restoration/error branches, logout, and WS session revocation redirects', async () => {
    const token = makeTestJwt('10001', 'jti-login')
    const tokensPayload = {
      access_token: token,
      refresh_token: 'rt-login-1',
      token_type: 'Bearer',
      expires_in: 900,
      refresh_expires_in: 604800,
      session_id: '501',
      device_id: 'dev-web-1',
    }
    const selfUser = {
      id: '10001',
      username: 'alice',
      nickname: 'Alice',
      avatar_url: '',
      signature: '',
      role: 'user',
      status: 1,
      online: true,
      last_online_at: null,
    }

    let meBehavior: 'ok' | '401' | '500' = 'ok'
    let logoutCalled = false
    const adapter = installInMemoryRequestAdapter(request, (method, url) => {
      if (method === 'POST' && url.endsWith('/auth/login')) {
        return { body: apiSuccess({ user: selfUser, tokens: tokensPayload }) }
      }
      if (method === 'POST' && url.endsWith('/auth/logout')) {
        logoutCalled = true
        return { body: apiSuccess({ revoked_count: 1 }) }
      }
      if (method === 'GET' && url.endsWith('/me')) {
        if (meBehavior === '401') {
          return { status: 401, body: { code: 11005, message: 'session revoked', data: null, request_id: 'err-401' } }
        }
        if (meBehavior === '500') {
          return { status: 500, body: { code: 19001, message: 'db down', data: null, request_id: 'err-500' } }
        }
        return { body: apiSuccess({ user: selfUser, session_id: '501', device_id: 'dev-web-1' }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })
    restoreHttp = adapter.restore

    const sockets: FakeTransport[] = []
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

    const redirectReasons: Array<string | undefined> = []
    setAuthRedirectHandler(reason => redirectReasons.push(reason))

    try {
      const authStore = useAuthStore()
      await authStore.login('alice', 'Secret123!')
      expect(authStore.isAuthenticated).toBe(true)
      expect(getStoredTokens()?.access_token).toBe(token)
      expect(sockets.length).toBeGreaterThanOrEqual(1)
      sockets[0].open()
      authenticateFakeTransport(sockets[0])
      expect(authStore.wsState).toBe('connected')

      // WS_SESSION_REVOKED (18006) error frame terminates local session & triggers redirect
      sockets[0].receive({
        type: 'error',
        req_id: 'rev-1',
        timestamp: Date.now(),
        data: { code: 18006, message: 'session revoked' },
      })
      expect(authStore.isAuthenticated).toBe(false)
      expect(getStoredTokens()).toBeNull()
      expect(redirectReasons).toHaveLength(1)

      // Fresh Pinia instance restores session via initAuth()
      setActivePinia(createPinia())
      localStorage.setItem('im_auth_tokens', JSON.stringify(tokensPayload))
      const restoredStore = useAuthStore()
      expect(await restoredStore.initAuth()).toBe(true)
      expect(restoredStore.user?.id).toBe('10001')

      // Logout calls /auth/logout and clears local session
      await restoredStore.logout('current')
      expect(logoutCalled).toBe(true)
      expect(restoredStore.isAuthenticated).toBe(false)
      expect(getStoredTokens()).toBeNull()

      // Transient 500 during initAuth keeps stored tokens instead of wiping them
      setActivePinia(createPinia())
      localStorage.setItem('im_auth_tokens', JSON.stringify(tokensPayload))
      meBehavior = '500'
      const transientStore = useAuthStore()
      expect(await transientStore.initAuth()).toBe(false)
      expect(getStoredTokens()?.access_token).toBe(token)

      // 401/11002 during initAuth clears stored tokens
      setActivePinia(createPinia())
      meBehavior = '401'
      const invalidStore = useAuthStore()
      expect(await invalidStore.initAuth()).toBe(false)
      expect(getStoredTokens()).toBeNull()
    } finally {
      setAuthRedirectHandler(() => {})
    }
  })

  it('paginates friends/requests and updates contact state across accept, reject, remark, remove, and system notices', async () => {
    const friendUser = (id: string, name: string) => ({
      id,
      username: name.toLowerCase(),
      nickname: name,
      avatar_url: '',
      signature: '',
      online: false,
      last_online_at: null,
    })

    let friendListFixture = [
      { friend_id: '10002', user: friendUser('10002', 'Bob'), remark: '', online: false, conversation_id: '90001' },
    ]
    let receivedListFixture = [
      { id: 'req-1', from_user_id: '10003', to_user_id: '10001', message: 'hi', status: 'pending', handled_at: null, created_at: '2026-11-26T00:00:00Z', peer: friendUser('10003', 'Carol') },
      { id: 'req-2', from_user_id: '10004', to_user_id: '10001', message: 'hey', status: 'pending', handled_at: null, created_at: '2026-11-26T00:01:00Z', peer: friendUser('10004', 'Dave') },
    ]
    let sentListFixture = [
      { id: 'req-s1', from_user_id: '10001', to_user_id: '10005', message: 'add me', status: 'pending', handled_at: null, created_at: '2026-11-26T00:02:00Z', peer: friendUser('10005', 'Eve') },
    ]

    const adapter = installInMemoryRequestAdapter(request, (method, url, body, params) => {
      if (method === 'GET' && url.endsWith('/friends')) {
        if (params.cursor === 'cur-f2') {
          return {
            body: apiSuccess({
              items: [
                // Duplicate 10002 should be deduplicated + new 10003 appended
                friendListFixture[0],
                { friend_id: '10003', user: friendUser('10003', 'Carol'), remark: 'C', online: true, conversation_id: '90002' },
              ],
              next_cursor: null,
              has_more: false,
            }),
          }
        }
        return {
          body: apiSuccess({
            items: friendListFixture,
            next_cursor: friendListFixture.length === 1 ? 'cur-f2' : null,
            has_more: friendListFixture.length === 1,
          }),
        }
      }
      if (method === 'GET' && url.endsWith('/friend-requests')) {
        if (params.direction === 'received') {
          return {
            body: apiSuccess({
              items: receivedListFixture.map(item => ({ ...item, peer: item.peer ? { ...item.peer } : null })),
              next_cursor: null,
              has_more: false,
            }),
          }
        }
        return {
          body: apiSuccess({
            items: sentListFixture.map(item => ({ ...item, peer: item.peer ? { ...item.peer } : null })),
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'POST' && url.endsWith('/friend-requests/req-1/accept')) {
        friendListFixture = [
          ...friendListFixture,
          { friend_id: '10003', user: friendUser('10003', 'Carol'), remark: '', online: false, conversation_id: '90002' },
        ]
        return {
          body: apiSuccess({
            request: { ...receivedListFixture[0], status: 'accepted', handled_at: '2026-11-26T01:00:00Z' },
            friend_id: '10003',
            conversation_id: '90002',
          }),
        }
      }
      if (method === 'POST' && url.endsWith('/friend-requests/req-2/reject')) {
        return { body: apiSuccess({ request: { ...receivedListFixture[1], status: 'rejected', handled_at: '2026-11-26T01:05:00Z' } }) }
      }
      if (method === 'POST' && url.endsWith('/friend-requests/req-s1/cancel')) {
        return { body: apiSuccess({ request: { ...sentListFixture[0], status: 'canceled', handled_at: '2026-11-26T01:06:00Z' } }) }
      }
      if (method === 'PATCH' && url.endsWith('/friends/10002')) {
        return { body: apiSuccess({ friend_id: '10002', remark: String((body as { remark?: string })?.remark ?? '') }) }
      }
      if (method === 'DELETE' && url.endsWith('/friends/10002')) {
        return { body: apiSuccess({ deleted: true }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })
    restoreHttp = adapter.restore

    const sockets: FakeTransport[] = []
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

    const token = makeTestJwt('10001')
    localStorage.setItem(
      'im_auth_tokens',
      JSON.stringify({ access_token: token, refresh_token: 'rt-1', token_type: 'Bearer', expires_in: 900, refresh_expires_in: 604800, session_id: '501', device_id: 'dev-1' }),
    )
    const authStore = useAuthStore()
    seedAuthUser(authStore, '10001')
    authStore.tokens = getStoredTokens()

    const contactStore = useContactStore()
    contactStore.bindWSClient()
    authStore.connectWS()
    sockets[0].open()
    authenticateFakeTransport(sockets[0])

    // Pagination + deduplication
    await contactStore.fetchFriends(true)
    expect(contactStore.friends.map(f => f.friend_id)).toEqual(['10002'])
    expect(contactStore.friendsHasMore).toBe(true)
    await contactStore.fetchFriends(false)
    expect(contactStore.friends.map(f => f.friend_id)).toEqual(['10002', '10003'])
    expect(contactStore.conversationIdMap['10003']).toBe('90002')

    await contactStore.fetchRequests('received')
    await contactStore.fetchRequests('sent')
    expect(contactStore.pendingReceivedCount).toBe(2)

    await contactStore.acceptRequest('req-1')
    expect(contactStore.pendingReceivedCount).toBe(1)
    expect(contactStore.conversationIdMap['10003']).toBe('90002')

    await contactStore.rejectRequest('req-2')
    expect(contactStore.pendingReceivedCount).toBe(0)

    await contactStore.cancelRequest('req-s1')
    expect(contactStore.sentRequests[0].status).toBe('canceled')

    await contactStore.updateRemark('10002', 'Bobby')
    expect(contactStore.findFriend('10002')?.remark).toBe('Bobby')

    await contactStore.removeFriend('10002')
    expect(contactStore.findFriend('10002')).toBeUndefined()
    expect(contactStore.conversationIdMap['10002']).toBeUndefined()

    // Real-time system.notice friend.requested triggers received list refresh
    receivedListFixture = [
      ...receivedListFixture,
      { id: 'req-3', from_user_id: '10006', to_user_id: '10001', message: 'new', status: 'pending', handled_at: null, created_at: '2026-11-26T02:00:00Z', peer: friendUser('10006', 'Frank') },
    ]
    sockets[0].receive({
      type: 'system.notice',
      req_id: 'notice-fr-1',
      timestamp: Date.now(),
      data: { event: 'friend.requested', notice_id: 'n-fr', payload: {} },
    })
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(contactStore.pendingReceivedCount).toBe(3)
  })
})
