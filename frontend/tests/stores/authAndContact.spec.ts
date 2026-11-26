/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import request from '@/api/request'
import { getStoredTokens } from '@/composables/useAuthRefresh'
import { useAuthStore } from '@/stores/auth'
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
})
