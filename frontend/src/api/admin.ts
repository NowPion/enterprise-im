import { apiDelete, apiGet, apiPatch, apiPost } from '@/api/request'
import { compactParams } from '@/api/group'
import type { CursorPage, GroupMember } from '@/types/group'
import type {
  AdminAuditSummaryItem,
  AdminDependenciesHealth,
  AdminGroupDetail,
  AdminGroupSummary,
  AdminMessageAuditItem,
  AdminMetricsOverview,
  AdminStreamConsumerMetric,
  AdminUserDetail,
  AdminUserSession,
  AdminUserSummary,
  ForceDisbandGroupResult,
  KickAdminUserPayload,
  KickAdminUserResult,
  ListAdminGroupsParams,
  ListAdminMessageAuditParams,
  ListAdminUsersParams,
  UpdateAdminUserStatusPayload,
  UpdateAdminUserStatusResult,
} from '@/types/admin'

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

export async function listAdminUsers(
  params: ListAdminUsersParams = {},
): Promise<CursorPage<AdminUserSummary>> {
  return apiGet<CursorPage<AdminUserSummary>>('/admin/users', {
    params: compactParams(params),
  })
}

export async function getAdminUserDetail(userId: string): Promise<AdminUserDetail> {
  const payload = await apiGet<Record<string, unknown>>(
    `/admin/users/${encodeURIComponent(userId)}`,
  )
  const user = (asRecord(payload.user) ?? payload) as unknown as AdminUserSummary
  const sessions = Array.isArray(payload.sessions)
    ? (payload.sessions as AdminUserSession[])
    : []
  const stats = asRecord(payload.stats)
  const friendCount = Number(payload.friend_count ?? stats?.friend_count ?? 0)
  const groupCount = Number(payload.group_count ?? stats?.group_count ?? 0)
  const auditRaw = payload.audit_summary ?? payload.recent_audits ?? payload.audit_logs ?? []
  const auditSummary = Array.isArray(auditRaw)
    ? (auditRaw as AdminAuditSummaryItem[])
    : []
  return {
    user,
    sessions,
    friend_count: Number.isFinite(friendCount) ? friendCount : 0,
    group_count: Number.isFinite(groupCount) ? groupCount : 0,
    audit_summary: auditSummary,
  }
}

export async function updateAdminUserStatus(
  userId: string,
  payload: UpdateAdminUserStatusPayload,
): Promise<UpdateAdminUserStatusResult> {
  return apiPatch<UpdateAdminUserStatusResult>(
    `/admin/users/${encodeURIComponent(userId)}/status`,
    payload,
  )
}

export async function kickAdminUser(
  userId: string,
  payload: KickAdminUserPayload,
): Promise<KickAdminUserResult> {
  const body: Record<string, unknown> = { reason: payload.reason }
  if (payload.session_id) {
    body.session_id = payload.session_id
  }
  return apiPost<KickAdminUserResult>(
    `/admin/users/${encodeURIComponent(userId)}/kick`,
    body,
  )
}

export async function listAdminGroups(
  params: ListAdminGroupsParams = {},
): Promise<CursorPage<AdminGroupSummary>> {
  return apiGet<CursorPage<AdminGroupSummary>>('/admin/groups', {
    params: compactParams(params),
  })
}

export async function getAdminGroupDetail(groupId: string): Promise<AdminGroupDetail> {
  const payload = await apiGet<Record<string, unknown>>(
    `/admin/groups/${encodeURIComponent(groupId)}`,
  )
  const group = (asRecord(payload.group) ?? payload) as unknown as AdminGroupSummary
  let members: GroupMember[] = []
  let membersPage: CursorPage<GroupMember> | undefined
  if (Array.isArray(payload.members)) {
    members = payload.members as GroupMember[]
  } else if (isMembersPage(payload.members)) {
    membersPage = payload.members
    members = payload.members.items
  }
  const eventsRaw = payload.system_events ?? payload.audit_summary ?? payload.audits ?? []
  const systemEvents = Array.isArray(eventsRaw)
    ? (eventsRaw as AdminAuditSummaryItem[])
    : []
  return {
    group,
    members,
    members_page: membersPage,
    system_events: systemEvents,
  }
}

function isMembersPage(value: unknown): value is CursorPage<GroupMember> {
  const rec = asRecord(value)
  return rec !== null && Array.isArray(rec.items)
}

export async function forceDisbandAdminGroup(
  groupId: string,
  reason: string,
): Promise<ForceDisbandGroupResult> {
  return apiDelete<ForceDisbandGroupResult>(
    `/admin/groups/${encodeURIComponent(groupId)}`,
    { data: { reason } },
  )
}

export async function listAdminMessageAudit(
  params: ListAdminMessageAuditParams = {},
): Promise<CursorPage<AdminMessageAuditItem>> {
  return apiGet<CursorPage<AdminMessageAuditItem>>('/admin/messages/audit', {
    params: compactParams(params),
  })
}

export async function getAdminMetricsOverview(): Promise<AdminMetricsOverview> {
  return apiGet<AdminMetricsOverview>('/admin/metrics/overview')
}

export async function getAdminMetricsStreams(): Promise<AdminStreamConsumerMetric[]> {
  const payload = await apiGet<unknown>('/admin/metrics/streams')
  if (Array.isArray(payload)) {
    return payload as AdminStreamConsumerMetric[]
  }
  const rec = asRecord(payload)
  if (rec && Array.isArray(rec.items)) {
    return rec.items as AdminStreamConsumerMetric[]
  }
  return []
}

export async function getAdminDependenciesHealth(): Promise<AdminDependenciesHealth> {
  return apiGet<AdminDependenciesHealth>('/admin/health/dependencies')
}
