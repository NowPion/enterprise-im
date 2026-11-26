import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import {
  addGroupMembers,
  createGroup,
  disbandGroup,
  extractErrorMessage,
  getGroupDetail,
  leaveGroup,
  listGroupAuditLogs,
  listGroupMembers,
  listGroups,
  removeGroupMember,
  transferGroupOwnership,
  updateGroupMemberRole,
  updateGroupProfile,
  updateMyGroupNickname,
  uploadGroupAvatar,
} from '@/api/group'
import type {
  CreateGroupPayload,
  CreateGroupResult,
  GetGroupResult,
  GroupAuditLog,
  GroupMember,
  GroupRole,
  GroupStatus,
  GroupSummary,
  ListGroupAuditParams,
  ListGroupMembersParams,
  MutableGroupRole,
  UpdateGroupPayload,
  UploadAsset,
} from '@/types/group'
import type { Envelope, WSClient } from '@/utils/wsClient'

export const useGroupStore = defineStore('group', () => {
  const groups = ref<GroupSummary[]>([])
  const groupsNextCursor = ref<string | null>(null)
  const groupsHasMore = ref(false)
  const groupsLoading = ref(false)
  const statusFilter = ref<GroupStatus | ''>('')

  const activeGroupId = ref<string | null>(null)
  const activeGroupDetail = ref<GetGroupResult | null>(null)
  const detailLoading = ref(false)

  const members = ref<GroupMember[]>([])
  const membersNextCursor = ref<string | null>(null)
  const membersHasMore = ref(false)
  const membersLoading = ref(false)
  const memberRoleFilter = ref<GroupRole | ''>('')
  const memberOnlineOnly = ref(false)

  const auditLogs = ref<GroupAuditLog[]>([])
  const auditNextCursor = ref<string | null>(null)
  const auditHasMore = ref(false)
  const auditLoading = ref(false)

  const submitting = ref(false)
  const errorMessage = ref<string | null>(null)

  const activeGroup = computed(() => activeGroupDetail.value?.group ?? null)
  const activeConversation = computed(() => activeGroupDetail.value?.conversation ?? null)
  const activeRole = computed<GroupRole | null>(() => activeGroupDetail.value?.role ?? null)

  function syncGroupSummaryInList(summary: Partial<GroupSummary> & { id: string }) {
    const idx = groups.value.findIndex((item) => item.id === summary.id)
    if (idx !== -1) {
      groups.value[idx] = { ...groups.value[idx], ...summary }
    }
  }

  async function fetchGroups(opts: { reset?: boolean; status?: GroupStatus | '' } = {}) {
    const reset = opts.reset ?? true
    if (opts.status !== undefined) {
      statusFilter.value = opts.status
    }
    if (!reset && !groupsHasMore.value) return

    groupsLoading.value = true
    errorMessage.value = null
    try {
      const page = await listGroups({
        status: statusFilter.value || undefined,
        cursor: reset ? undefined : groupsNextCursor.value ?? undefined,
        limit: 20,
      })
      groups.value = reset ? page.items : [...groups.value, ...page.items]
      groupsNextCursor.value = page.next_cursor
      groupsHasMore.value = page.has_more
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群组列表失败')
      throw err
    } finally {
      groupsLoading.value = false
    }
  }

  async function selectGroup(groupId: string) {
    activeGroupId.value = groupId
    detailLoading.value = true
    errorMessage.value = null
    auditLogs.value = []
    auditNextCursor.value = null
    auditHasMore.value = false

    try {
      const detail = await getGroupDetail(groupId)
      if (activeGroupId.value !== groupId) return detail
      activeGroupDetail.value = detail
      syncGroupSummaryInList({
        ...detail.group,
        member_count: detail.member_count,
        role: detail.role,
      })
      await fetchMembers(groupId, { reset: true })
      return detail
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群详情失败')
      throw err
    } finally {
      detailLoading.value = false
    }
  }

  async function fetchMembers(
    groupId: string,
    opts: ListGroupMembersParams & { reset?: boolean } = {},
  ) {
    const reset = opts.reset ?? true
    if (opts.role !== undefined) memberRoleFilter.value = opts.role
    if (opts.online_only !== undefined) memberOnlineOnly.value = opts.online_only
    if (!reset && !membersHasMore.value) return

    membersLoading.value = true
    try {
      const page = await listGroupMembers(groupId, {
        role: memberRoleFilter.value || undefined,
        online_only: memberOnlineOnly.value || undefined,
        cursor: reset ? undefined : membersNextCursor.value ?? undefined,
        limit: opts.limit ?? 50,
      })
      if (activeGroupId.value !== groupId) return page
      members.value = reset ? page.items : [...members.value, ...page.items]
      membersNextCursor.value = page.next_cursor
      membersHasMore.value = page.has_more
      return page
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群成员失败')
      throw err
    } finally {
      membersLoading.value = false
    }
  }

  async function fetchAuditLogs(
    groupId: string,
    opts: ListGroupAuditParams & { reset?: boolean } = {},
  ) {
    const reset = opts.reset ?? true
    if (!reset && !auditHasMore.value) return

    auditLoading.value = true
    try {
      const page = await listGroupAuditLogs(groupId, {
        from: opts.from,
        to: opts.to,
        cursor: reset ? undefined : auditNextCursor.value ?? undefined,
        limit: opts.limit ?? 20,
      })
      if (activeGroupId.value !== groupId) return page
      auditLogs.value = reset ? page.items : [...auditLogs.value, ...page.items]
      auditNextCursor.value = page.next_cursor
      auditHasMore.value = page.has_more
      return page
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群审计记录失败')
      throw err
    } finally {
      auditLoading.value = false
    }
  }

  async function uploadAvatar(file: File): Promise<UploadAsset> {
    submitting.value = true
    try {
      return await uploadGroupAvatar(file)
    } finally {
      submitting.value = false
    }
  }

  async function createGroupAction(payload: CreateGroupPayload): Promise<CreateGroupResult> {
    submitting.value = true
    errorMessage.value = null
    try {
      const result = await createGroup(payload)
      const summary: GroupSummary = {
        ...result.group,
        role: 'owner',
      }
      if (statusFilter.value === '' || statusFilter.value === 'active') {
        groups.value = [summary, ...groups.value.filter((g) => g.id !== summary.id)]
      }
      activeGroupId.value = result.group.id
      activeGroupDetail.value = {
        group: result.group,
        conversation: result.conversation,
        role: 'owner',
        member_count: result.members.length,
      }
      members.value = result.members
      membersNextCursor.value = null
      membersHasMore.value = false
      return result
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '创建群聊失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function updateProfile(groupId: string, payload: UpdateGroupPayload) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await updateGroupProfile(groupId, payload)
      if (activeGroupDetail.value && activeGroupDetail.value.group.id === groupId) {
        activeGroupDetail.value = {
          ...activeGroupDetail.value,
          group: res.group,
        }
      }
      syncGroupSummaryInList(res.group)
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '更新群资料失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function disband(groupId: string) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await disbandGroup(groupId)
      if (activeGroupDetail.value && activeGroupDetail.value.group.id === groupId) {
        activeGroupDetail.value = {
          ...activeGroupDetail.value,
          group: { ...activeGroupDetail.value.group, status: 'disbanded' },
          conversation: { ...activeGroupDetail.value.conversation, can_send: false },
        }
      }
      syncGroupSummaryInList({ id: groupId, status: 'disbanded' })
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '解散群聊失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function addMembers(groupId: string, userIds: string[]) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await addGroupMembers(groupId, userIds)
      await selectGroup(groupId)
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '邀请成员失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function removeMember(groupId: string, userId: string) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await removeGroupMember(groupId, userId)
      members.value = members.value.filter((m) => m.user_id !== userId)
      if (activeGroupDetail.value && activeGroupDetail.value.group.id === groupId) {
        const nextCount = Math.max(0, activeGroupDetail.value.member_count - 1)
        activeGroupDetail.value = {
          ...activeGroupDetail.value,
          member_count: nextCount,
          group: { ...activeGroupDetail.value.group, member_count: nextCount },
        }
        syncGroupSummaryInList({ id: groupId, member_count: nextCount })
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '移除成员失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function leave(groupId: string) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await leaveGroup(groupId)
      groups.value = groups.value.filter((g) => g.id !== groupId)
      if (activeGroupId.value === groupId) {
        activeGroupId.value = null
        activeGroupDetail.value = null
        members.value = []
        auditLogs.value = []
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '退出群聊失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function transferOwnership(groupId: string, targetUserId: string) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await transferGroupOwnership(groupId, targetUserId)
      await selectGroup(groupId)
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '转让群主失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function updateMemberRole(groupId: string, userId: string, role: MutableGroupRole) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await updateGroupMemberRole(groupId, userId, role)
      const idx = members.value.findIndex((m) => m.user_id === userId)
      if (idx !== -1) {
        members.value[idx] = res.member
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '更新成员角色失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  async function updateMyNickname(groupId: string, nicknameInGroup: string) {
    submitting.value = true
    errorMessage.value = null
    try {
      const res = await updateMyGroupNickname(groupId, nicknameInGroup)
      const idx = members.value.findIndex((m) => m.user_id === res.member.user_id)
      if (idx !== -1) {
        members.value[idx] = res.member
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '修改群内昵称失败')
      throw err
    } finally {
      submitting.value = false
    }
  }

  function handlePresenceUpdate(frame: Envelope) {
    const userId = typeof frame.data.user_id === 'string' ? frame.data.user_id : ''
    const online = typeof frame.data.online === 'boolean' ? frame.data.online : null
    if (!userId || online === null) return
    const idx = members.value.findIndex((m) => m.user_id === userId)
    if (idx !== -1) {
      members.value[idx] = { ...members.value[idx], online }
    }
  }

  function handleSystemNotice(frame: Envelope) {
    const event = typeof frame.data.event === 'string' ? frame.data.event : ''
    if (!event.startsWith('group.')) return
    const payload =
      typeof frame.data.payload === 'object' && frame.data.payload !== null
        ? (frame.data.payload as Record<string, unknown>)
        : {}
    const noticeGroupId = typeof payload.group_id === 'string' ? payload.group_id : null

    if (event === 'group.disbanded' && noticeGroupId) {
      syncGroupSummaryInList({ id: noticeGroupId, status: 'disbanded' })
    }
    if (activeGroupId.value && (!noticeGroupId || noticeGroupId === activeGroupId.value)) {
      void selectGroup(activeGroupId.value).catch(() => undefined)
    }
  }

  function bindWsClient(client: WSClient): () => void {
    const offNotice = client.on('system.notice', handleSystemNotice)
    const offPresence = client.on('presence.update', handlePresenceUpdate)
    return () => {
      offNotice()
      offPresence()
    }
  }

  function reset() {
    groups.value = []
    groupsNextCursor.value = null
    groupsHasMore.value = false
    statusFilter.value = ''
    activeGroupId.value = null
    activeGroupDetail.value = null
    members.value = []
    membersNextCursor.value = null
    membersHasMore.value = false
    auditLogs.value = []
    auditNextCursor.value = null
    auditHasMore.value = false
    errorMessage.value = null
  }

  return {
    groups,
    groupsNextCursor,
    groupsHasMore,
    groupsLoading,
    statusFilter,
    activeGroupId,
    activeGroupDetail,
    activeGroup,
    activeConversation,
    activeRole,
    detailLoading,
    members,
    membersNextCursor,
    membersHasMore,
    membersLoading,
    memberRoleFilter,
    memberOnlineOnly,
    auditLogs,
    auditNextCursor,
    auditHasMore,
    auditLoading,
    submitting,
    errorMessage,
    fetchGroups,
    selectGroup,
    fetchMembers,
    fetchAuditLogs,
    uploadAvatar,
    createGroupAction,
    updateProfile,
    disband,
    addMembers,
    removeMember,
    leave,
    transferOwnership,
    updateMemberRole,
    updateMyNickname,
    handlePresenceUpdate,
    handleSystemNotice,
    bindWsClient,
    reset,
  }
})
