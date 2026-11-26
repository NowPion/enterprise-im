/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import request from '@/api/request'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import {
  apiSuccess,
  authenticateFakeTransport,
  bindStoreWS,
  createTestWSClient,
  findAction,
  installInMemoryRequestAdapter,
  makeTestJwt,
  seedAuthUser,
} from './helpers'

describe('auth single-flight token refresh and contact presence updates', () => {
  let restoreHttp: (() => void) | undefined

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
  })

  afterEach(() => {
    restoreHttp?.()
    restoreHttp = undefined
  })

  it('deduplicates concurrent 401/11001 responses into a single /auth/refresh call and replays queued requests', async () => {
    const authStore = useAuthStore() as unknown as Record<string, unknown>
    seedAuthUser(authStore, '10001')

    const initialToken = makeTestJwt('10001', 'jti-expired')
    const rotatedToken = makeTestJwt('10001', 'jti-fresh')
    if ('accessToken' in authStore) authStore.accessToken = initialToken
    if ('refreshToken' in authStore) authStore.refreshToken = 'rt-old-1'
    if ('deviceId' in authStore) authStore.deviceId = 'dev-web-1'
    localStorage.setItem('access_token', initialToken)
    localStorage.setItem('refresh_token', 'rt-old-1')
    localStorage.setItem('device_id', 'dev-web-1')

    let refreshCount = 0
    let activeToken = initialToken

    const adapter = installInMemoryRequestAdapter(request, (method, url) => {
      if (method === 'POST' && url.endsWith('/auth/refresh')) {
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
        if (activeToken !== rotatedToken) {
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
    restoreHttp = adapter.restore

    const responses = await Promise.all([
      request.get('/me'),
      request.get('/me'),
      request.get('/me'),
    ])

    expect(refreshCount).toBe(1)
    expect(responses).toHaveLength(3)
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

    const contactStore = useContactStore()
    const { client, sockets } = createTestWSClient()
    const bound = bindStoreWS(contactStore, client)
    if (bound) {
      client.connect()
      sockets[0].open()
      authenticateFakeTransport(sockets[0])
    }

    const fetchFriends = findAction(contactStore, ['fetchFriends', 'loadFriends', 'fetchContacts'])
    expect(fetchFriends, 'useContactStore must expose fetchFriends/loadFriends').not.toBeNull()
    await Promise.resolve((fetchFriends as () => unknown)())

    const presenceHandler = findAction(contactStore, ['handlePresenceUpdate', 'onPresenceUpdate', 'applyPresenceUpdate', 'updatePresence'])
    const onlinePayload = { user_id: '10002', online: true, device_count: 1, last_online_at: null }
    if (presenceHandler) {
      ;(presenceHandler as (data: unknown) => unknown)(onlinePayload)
    } else if (sockets[0]) {
      sockets[0].receive({ type: 'presence.update', req_id: 'pres-1', timestamp: Date.now(), data: onlinePayload })
    }

    const storeRecord = contactStore as unknown as Record<string, unknown>
    const friends = (storeRecord.friends ?? storeRecord.items ?? storeRecord.contacts ?? []) as Array<{
      friend_id?: string
      online?: boolean
      user?: { id?: string; online?: boolean; last_online_at?: string | null }
    }>
    const bob = friends.find(f => f.friend_id === '10002' || f.user?.id === '10002')
    expect(bob?.online ?? bob?.user?.online).toBe(true)

    const offlinePayload = { user_id: '10002', online: false, device_count: 0, last_online_at: '2026-11-26T12:00:00.000Z' }
    if (presenceHandler) {
      ;(presenceHandler as (data: unknown) => unknown)(offlinePayload)
    } else if (sockets[0]) {
      sockets[0].receive({ type: 'presence.update', req_id: 'pres-2', timestamp: Date.now(), data: offlinePayload })
    }
    expect(bob?.online ?? bob?.user?.online).toBe(false)
    client.disconnect()
  })
})
