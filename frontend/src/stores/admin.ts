import { ref } from 'vue'
import { defineStore } from 'pinia'
import { extractErrorMessage } from '@/api/group'
import {
  forceDisbandAdminGroup,
  getAdminDependenciesHealth,
  getAdminGroupDetail,
  getAdminMetricsOverview,
  getAdminMetricsStreams,
  getAdminUserDetail,
  kickAdminUser,
  listAdminGroups,
  listAdminMessageAudit,
  listAdminUsers,
  updateAdminUserStatus,
} from '@/api/admin'
import type {
  AdminDependenciesHealth,
  AdminGroupDetail,
  AdminGroupSummary,
  AdminMessageAuditItem,
  AdminMetricsOverview,
  AdminStreamConsumerMetric,
  AdminUserDetail,
  AdminUserSummary,
  KickAdminUserPayload,
  ListAdminGroupsParams,
  ListAdminMessageAuditParams,
  ListAdminUsersParams,
  UserAccountStatus,
} from '@/types/admin'

export const useAdminStore = defineStore('admin', () => {
  const users = ref<AdminUserSummary[]>([])
  const usersNextCursor = ref<string | null>(null)
  const usersHasMore = ref(false)
  const usersLoading = ref(false)
  const selectedUserDetail = ref<AdminUserDetail | null>(null)
  const userDetailLoading = ref(false)

  const groups = ref<AdminGroupSummary[]>([])
  const groupsNextCursor = ref<string | null>(null)
  const groupsHasMore = ref(false)
  const groupsLoading = ref(false)
  const selectedGroupDetail = ref<AdminGroupDetail | null>(null)
  const groupDetailLoading = ref(false)

  const auditMessages = ref<AdminMessageAuditItem[]>([])
  const auditNextCursor = ref<string | null>(null)
  const auditHasMore = ref(false)
  const auditLoading = ref(false)

  const metricsOverview = ref<AdminMetricsOverview | null>(null)
  const streamMetrics = ref<AdminStreamConsumerMetric[]>([])
  const dependenciesHealth = ref<AdminDependenciesHealth | null>(null)
  const monitoringLoading = ref(false)
  const monitoringLastUpdated = ref<string | null>(null)
  const monitoringError = ref<string | null>(null)

  const actionSubmitting = ref(false)
  const errorMessage = ref<string | null>(null)

  async function fetchUsers(params: ListAdminUsersParams = {}, reset = true) {
    if (!reset && !usersHasMore.value) return
    usersLoading.value = true
    errorMessage.value = null
    try {
      const page = await listAdminUsers({
        ...params,
        cursor: reset ? undefined : usersNextCursor.value ?? undefined,
        limit: params.limit ?? 20,
      })
      users.value = reset ? page.items : [...users.value, ...page.items]
      usersNextCursor.value = page.next_cursor
      usersHasMore.value = page.has_more
      return page
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载用户列表失败')
      throw err
    } finally {
      usersLoading.value = false
    }
  }

  async function fetchUserDetail(userId: string) {
    userDetailLoading.value = true
    errorMessage.value = null
    try {
      const detail = await getAdminUserDetail(userId)
      selectedUserDetail.value = detail
      return detail
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载用户详情失败')
      throw err
    } finally {
      userDetailLoading.value = false
    }
  }

  async function toggleUserStatus(userId: string, status: UserAccountStatus, reason: string) {
    actionSubmitting.value = true
    errorMessage.value = null
    try {
      const res = await updateAdminUserStatus(userId, { status, reason })
      const idx = users.value.findIndex((u) => u.id === userId)
      if (idx !== -1) {
        users.value[idx] = {
          ...users.value[idx],
          status: res.status,
          online: res.status === 0 ? false : users.value[idx].online,
          session_count:
            res.status === 0
              ? Math.max(0, users.value[idx].session_count - res.revoked_session_count)
              : users.value[idx].session_count,
        }
      }
      if (selectedUserDetail.value?.user.id === userId) {
        await fetchUserDetail(userId)
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '更新账号状态失败')
      throw err
    } finally {
      actionSubmitting.value = false
    }
  }

  async function kickUser(userId: string, payload: KickAdminUserPayload) {
    actionSubmitting.value = true
    errorMessage.value = null
    try {
      const res = await kickAdminUser(userId, payload)
      const idx = users.value.findIndex((u) => u.id === userId)
      if (idx !== -1) {
        users.value[idx] = {
          ...users.value[idx],
          session_count: Math.max(0, users.value[idx].session_count - res.revoked_session_count),
          online: payload.session_id ? users.value[idx].online : false,
        }
      }
      if (selectedUserDetail.value?.user.id === userId) {
        await fetchUserDetail(userId)
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '踢出用户会话失败')
      throw err
    } finally {
      actionSubmitting.value = false
    }
  }

  async function fetchGroups(params: ListAdminGroupsParams = {}, reset = true) {
    if (!reset && !groupsHasMore.value) return
    groupsLoading.value = true
    errorMessage.value = null
    try {
      const page = await listAdminGroups({
        ...params,
        cursor: reset ? undefined : groupsNextCursor.value ?? undefined,
        limit: params.limit ?? 20,
      })
      groups.value = reset ? page.items : [...groups.value, ...page.items]
      groupsNextCursor.value = page.next_cursor
      groupsHasMore.value = page.has_more
      return page
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群组监管列表失败')
      throw err
    } finally {
      groupsLoading.value = false
    }
  }

  async function fetchGroupDetail(groupId: string) {
    groupDetailLoading.value = true
    errorMessage.value = null
    try {
      const detail = await getAdminGroupDetail(groupId)
      selectedGroupDetail.value = detail
      return detail
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '加载群组详情失败')
      throw err
    } finally {
      groupDetailLoading.value = false
    }
  }

  async function forceDisbandGroup(groupId: string, reason: string) {
    actionSubmitting.value = true
    errorMessage.value = null
    try {
      const res = await forceDisbandAdminGroup(groupId, reason)
      const idx = groups.value.findIndex((g) => g.id === groupId)
      if (idx !== -1) {
        groups.value[idx] = { ...groups.value[idx], status: 'disbanded' }
      }
      if (selectedGroupDetail.value?.group.id === groupId) {
        await fetchGroupDetail(groupId)
      }
      return res
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '强制解散群聊失败')
      throw err
    } finally {
      actionSubmitting.value = false
    }
  }

  async function fetchMessageAudit(params: ListAdminMessageAuditParams = {}, reset = true) {
    if (!reset && !auditHasMore.value) return
    auditLoading.value = true
    errorMessage.value = null
    try {
      const page = await listAdminMessageAudit({
        ...params,
        cursor: reset ? undefined : auditNextCursor.value ?? undefined,
        limit: params.limit ?? 30,
      })
      auditMessages.value = reset ? page.items : [...auditMessages.value, ...page.items]
      auditNextCursor.value = page.next_cursor
      auditHasMore.value = page.has_more
      return page
    } catch (err) {
      errorMessage.value = extractErrorMessage(err, '查询消息审计失败')
      throw err
    } finally {
      auditLoading.value = false
    }
  }

  async function fetchMonitoringData() {
    monitoringLoading.value = true
    monitoringError.value = null
    const [overviewRes, streamsRes, healthRes] = await Promise.allSettled([
      getAdminMetricsOverview(),
      getAdminMetricsStreams(),
      getAdminDependenciesHealth(),
    ])

    const errors: string[] = []
    if (overviewRes.status === 'fulfilled') {
      metricsOverview.value = overviewRes.value
    } else {
      errors.push(`总览指标：${extractErrorMessage(overviewRes.reason)}`)
    }

    if (streamsRes.status === 'fulfilled') {
      streamMetrics.value = streamsRes.value
    } else {
      errors.push(`Streams 消费状态：${extractErrorMessage(streamsRes.reason)}`)
    }

    if (healthRes.status === 'fulfilled') {
      dependenciesHealth.value = healthRes.value
    } else {
      errors.push(`依赖健康检查：${extractErrorMessage(healthRes.reason)}`)
    }

    if (errors.length > 0) {
      monitoringError.value = errors.join('；')
    }
    monitoringLastUpdated.value = new Date().toISOString()
    monitoringLoading.value = false
  }

  function reset() {
    users.value = []
    usersNextCursor.value = null
    usersHasMore.value = false
    selectedUserDetail.value = null
    groups.value = []
    groupsNextCursor.value = null
    groupsHasMore.value = false
    selectedGroupDetail.value = null
    auditMessages.value = []
    auditNextCursor.value = null
    auditHasMore.value = false
    metricsOverview.value = null
    streamMetrics.value = []
    dependenciesHealth.value = null
    monitoringError.value = null
    errorMessage.value = null
  }

  return {
    users,
    usersNextCursor,
    usersHasMore,
    usersLoading,
    selectedUserDetail,
    userDetailLoading,
    groups,
    groupsNextCursor,
    groupsHasMore,
    groupsLoading,
    selectedGroupDetail,
    groupDetailLoading,
    auditMessages,
    auditNextCursor,
    auditHasMore,
    auditLoading,
    metricsOverview,
    streamMetrics,
    dependenciesHealth,
    monitoringLoading,
    monitoringLastUpdated,
    monitoringError,
    actionSubmitting,
    errorMessage,
    fetchUsers,
    fetchUserDetail,
    toggleUserStatus,
    kickUser,
    fetchGroups,
    fetchGroupDetail,
    forceDisbandGroup,
    fetchMessageAudit,
    fetchMonitoringData,
    reset,
  }
})
