/** @vitest-environment jsdom */
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { ref } from 'vue'
import request from '@/api/request'
import { useScrollPagination } from '@/composables/chat/useScrollPagination'
import { useGroupPermissions } from '@/composables/groups/useGroupPermissions'
import { useAdminStore } from '@/stores/admin'
import { useAuthStore } from '@/stores/auth'
import { useGroupStore } from '@/stores/group'
import type { GroupInfo, GroupMember, GroupRole } from '@/types/group'
import {
  apiSuccess,
  authenticateFakeTransport,
  createTestWSClient,
  installInMemoryRequestAdapter,
  seedAuthUser,
} from './helpers'

describe('useGroupStore, useAdminStore, useGroupPermissions, and useScrollPagination', () => {
  let restoreHttp: (() => void) | undefined

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    seedAuthUser(useAuthStore(), '10001')
  })

  afterEach(() => {
    useGroupStore().reset()
    useAdminStore().reset()
    restoreHttp?.()
    restoreHttp = undefined
  })

  it('manages group CRUD, member updates, presence, and group.disbanded system notices', async () => {
    const baseGroup: GroupInfo = {
      id: '7001',
      conversation_id: '90701',
      name: 'Alpha Team',
      avatar_url: '',
      announcement: 'Welcome',
      owner_id: '10001',
      status: 'active',
      member_count: 2,
      max_members: 500,
      created_at: '2026-11-26T00:00:00Z',
      updated_at: '2026-11-26T00:00:00Z',
    }
    const membersFixture: GroupMember[] = [
      { user_id: '10001', nickname: 'Alice', nickname_in_group: 'Lead', avatar_url: '', role: 'owner', online: true, joined_at: '2026-11-26T00:00:00Z' },
      { user_id: '10002', nickname: 'Bob', nickname_in_group: '', avatar_url: '', role: 'member', online: false, joined_at: '2026-11-26T00:01:00Z' },
    ]

    const adapter = installInMemoryRequestAdapter(request, (method, url, body) => {
      if (method === 'GET' && url.endsWith('/groups')) {
        return { body: apiSuccess({ items: [{ ...baseGroup, role: 'owner' }], next_cursor: null, has_more: false }) }
      }
      if (method === 'GET' && url.endsWith('/groups/7001')) {
        return {
          body: apiSuccess({
            group: baseGroup,
            conversation: { id: '90701', type: 'group', title: 'Alpha Team', can_send: true, last_seq: '1', last_delivered_seq: '1', last_read_seq: '1', unread_count: 0, is_pinned: false, is_muted: false, is_visible: true, peer: null, group_id: '7001', avatar_url: '', last_msg_id: '1', last_msg_preview: '', last_msg_at: null },
            role: 'owner',
            member_count: 2,
          }),
        }
      }
      if (method === 'GET' && url.endsWith('/groups/7001/members')) {
        return { body: apiSuccess({ items: membersFixture, next_cursor: null, has_more: false }) }
      }
      if (method === 'PATCH' && url.endsWith('/groups/7001')) {
        return { body: apiSuccess({ group: { ...baseGroup, name: String((body as { name?: string })?.name ?? baseGroup.name) } }) }
      }
      if (method === 'PATCH' && url.endsWith('/groups/7001/members/10002/role')) {
        return { body: apiSuccess({ member: { ...membersFixture[1], role: 'admin' } }) }
      }
      if (method === 'DELETE' && url.endsWith('/groups/7001/members/10002')) {
        return { body: apiSuccess({ removed_user_id: '10002' }) }
      }
      if (method === 'DELETE' && url.endsWith('/groups/7001')) {
        return { body: apiSuccess({ disbanded: true }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })
    restoreHttp = adapter.restore

    const groupStore = useGroupStore()
    const { client, sockets } = createTestWSClient()
    const unbind = groupStore.bindWsClient(client)
    client.connect()
    sockets[0].open()
    authenticateFakeTransport(sockets[0])

    await groupStore.fetchGroups({ status: 'active' })
    expect(groupStore.groups).toHaveLength(1)

    await groupStore.selectGroup('7001')
    expect(groupStore.activeGroup?.name).toBe('Alpha Team')
    expect(groupStore.members).toHaveLength(2)

    // Presence update toggles member online state
    sockets[0].receive({
      type: 'presence.update',
      req_id: 'p-1',
      timestamp: Date.now(),
      data: { user_id: '10002', online: true, device_count: 1, last_online_at: null },
    })
    expect(groupStore.members.find(m => m.user_id === '10002')?.online).toBe(true)

    await groupStore.updateProfile('7001', { name: 'Alpha v2' })
    expect(groupStore.activeGroup?.name).toBe('Alpha v2')
    expect(groupStore.groups[0].name).toBe('Alpha v2')

    await groupStore.updateMemberRole('7001', '10002', 'admin')
    expect(groupStore.members.find(m => m.user_id === '10002')?.role).toBe('admin')

    await groupStore.removeMember('7001', '10002')
    expect(groupStore.members).toHaveLength(1)
    expect(groupStore.activeGroupDetail?.member_count).toBe(1)

    await groupStore.disband('7001')
    expect(groupStore.activeGroup?.status).toBe('disbanded')
    expect(groupStore.activeConversation?.can_send).toBe(false)
    expect(groupStore.groups[0].status).toBe('disbanded')

    unbind()
    client.disconnect()
  })

  it('handles admin user status toggle, kick, force disband, audit pagination, and partial monitoring failures', async () => {
    let failStreams = true
    const adapter = installInMemoryRequestAdapter(request, (method, url, _body, params) => {
      if (method === 'GET' && url.endsWith('/admin/users')) {
        return {
          body: apiSuccess({
            items: [{ id: '10002', username: 'bob', nickname: 'Bob', avatar_url: '', role: 'user', status: 1, online: true, session_count: 2, last_online_at: null, created_at: '2026-11-26T00:00:00Z' }],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'PATCH' && url.endsWith('/admin/users/10002/status')) {
        return { body: apiSuccess({ user_id: '10002', status: 0, revoked_session_count: 2 }) }
      }
      if (method === 'POST' && url.endsWith('/admin/users/10002/kick')) {
        return { body: apiSuccess({ user_id: '10002', revoked_session_count: 1 }) }
      }
      if (method === 'GET' && url.endsWith('/admin/groups')) {
        return {
          body: apiSuccess({
            items: [{ id: '7001', name: 'Bad Group', avatar_url: '', owner_id: '10002', status: 'active', member_count: 3, max_members: 500, created_at: '2026-11-26T00:00:00Z' }],
            next_cursor: null,
            has_more: false,
          }),
        }
      }
      if (method === 'DELETE' && url.endsWith('/admin/groups/7001')) {
        return { body: apiSuccess({ group_id: '7001', status: 'disbanded' }) }
      }
      if (method === 'GET' && url.endsWith('/admin/messages/audit')) {
        if (!params.cursor) {
          return { body: apiSuccess({ items: [{ msg_id: '1', conversation_id: '90001', seq: '1', sender_id: '10002', msg_type: 'text', content: 'a', status: 'normal', created_at: '2026-11-26T00:00:00Z' }], next_cursor: 'c1', has_more: true }) }
        }
        return { body: apiSuccess({ items: [{ msg_id: '2', conversation_id: '90001', seq: '2', sender_id: '10002', msg_type: 'text', content: 'b', status: 'normal', created_at: '2026-11-26T00:01:00Z' }], next_cursor: null, has_more: false }) }
      }
      if (method === 'GET' && url.endsWith('/admin/metrics/overview')) {
        return { body: apiSuccess({ online_users: 42, ws_connections: 60, total_messages: 1000, goroutines: 25 }) }
      }
      if (method === 'GET' && url.endsWith('/admin/metrics/streams')) {
        if (failStreams) {
          return { status: 503, body: { code: 19002, message: 'redis timeout', data: null, request_id: 'err' } }
        }
        return { body: apiSuccess([]) }
      }
      if (method === 'GET' && url.endsWith('/admin/health/dependencies')) {
        return { body: apiSuccess({ mysql: { ok: true }, redis: { ok: true } }) }
      }
      return { status: 404, body: { code: 10000, message: 'unexpected', data: null, request_id: 'err' } }
    })
    restoreHttp = adapter.restore

    const adminStore = useAdminStore()
    await adminStore.fetchUsers()
    await adminStore.toggleUserStatus('10002', 0, 'policy violation')
    expect(adminStore.users[0].status).toBe(0)
    expect(adminStore.users[0].online).toBe(false)
    expect(adminStore.users[0].session_count).toBe(0)

    await adminStore.fetchGroups()
    await adminStore.forceDisbandGroup('7001', 'spam')
    expect(adminStore.groups[0].status).toBe('disbanded')

    await adminStore.fetchMessageAudit({}, true)
    await adminStore.fetchMessageAudit({}, false)
    expect(adminStore.auditMessages.map(m => m.msg_id)).toEqual(['1', '2'])

    await adminStore.fetchMonitoringData()
    expect(adminStore.metricsOverview?.online_users).toBe(42)
    expect(adminStore.dependenciesHealth).toBeTruthy()
    expect(adminStore.monitoringError).toContain('Streams 消费状态')
  })

  it('enforces group permission matrix and scroll pagination unreadBelowCount / prepend preservation', async () => {
    const groupRef = ref<GroupInfo | null>({
      id: '7001',
      conversation_id: '90701',
      name: 'Alpha',
      avatar_url: '',
      announcement: null,
      owner_id: '10001',
      status: 'active',
      member_count: 3,
      max_members: 500,
      created_at: '2026-11-26T00:00:00Z',
      updated_at: '2026-11-26T00:00:00Z',
    })
    const roleRef = ref<GroupRole | null>('admin')
    const perms = useGroupPermissions(groupRef, roleRef)

    const ownerMember: GroupMember = { user_id: '10002', nickname: 'O', nickname_in_group: '', avatar_url: '', role: 'owner', online: true, joined_at: '' }
    const regularMember: GroupMember = { user_id: '10003', nickname: 'M', nickname_in_group: '', avatar_url: '', role: 'member', online: true, joined_at: '' }
    const selfAdmin: GroupMember = { user_id: '10001', nickname: 'Me', nickname_in_group: '', avatar_url: '', role: 'admin', online: true, joined_at: '' }

    expect(perms.canEditGroupInfo.value).toBe(true)
    expect(perms.canDisbandGroup.value).toBe(false)
    expect(perms.canLeaveGroup.value).toBe(true)
    expect(perms.canRemoveMember(regularMember)).toBe(true)
    expect(perms.canRemoveMember(ownerMember)).toBe(false)
    expect(perms.canRemoveMember(selfAdmin)).toBe(false)

    roleRef.value = 'owner'
    expect(perms.canDisbandGroup.value).toBe(true)
    expect(perms.canLeaveGroup.value).toBe(false)
    expect(perms.canChangeMemberRole(regularMember)).toBe(true)

    groupRef.value = { ...groupRef.value!, status: 'disbanded' }
    expect(perms.canEditGroupInfo.value).toBe(false)
    expect(perms.canRemoveMember(regularMember)).toBe(false)

    // Scroll pagination behavior
    let hasMore = true
    let loading = false
    let reachBottomCalls = 0
    const container = {
      scrollHeight: 1000,
      scrollTop: 500,
      clientHeight: 300,
      scrollTo(opts: { top?: number }) {
        ;(this as unknown as { scrollTop: number }).scrollTop = opts.top ?? 0
      },
    } as unknown as HTMLElement

    const sp = useScrollPagination(ref(container), {
      hasMoreOlder: () => hasMore,
      isLoadingOlder: () => loading,
      onLoadOlder: async () => {
        loading = true
        ;(container as { scrollHeight: number }).scrollHeight = 1400
        loading = false
        hasMore = false
      },
      onReachBottom: () => {
        reachBottomCalls += 1
      },
    })

    // Scrolled away from bottom -> incoming peer message increments unreadBelowCount
    await sp.handleScroll()
    expect(sp.isNearBottom.value).toBe(false)
    sp.notifyIncomingBottomOrHold(false)
    sp.notifyIncomingBottomOrHold(false)
    expect(sp.unreadBelowCount.value).toBe(2)

    // Scrolled to top -> loads older and preserves relative scrollTop (+400px delta)
    container.scrollTop = 20
    await sp.handleScroll()
    expect(container.scrollTop).toBe(420)

    // Scrolled to bottom -> resets unreadBelowCount and triggers onReachBottom
    container.scrollTop = 1100
    await sp.handleScroll()
    expect(sp.isNearBottom.value).toBe(true)
    expect(sp.unreadBelowCount.value).toBe(0)
    expect(reachBottomCalls).toBe(1)
  })
})