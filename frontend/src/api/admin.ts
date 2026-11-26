import request from '@/api/request'
import { unwrapApiEnvelope } from '@/api/group'
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

function compactAdminParams(params: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      out[key] = value
    }
  }
  return out
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

export async function listAdminUsers(
  params: ListAdminUsersParams = {},
): Promise<CursorPage<AdminUserSummary>> {
  const raw: unknown = await request.get<unknown>('/api/v1/admin/users', {
    params: compactAdminParams(params as Record<string, unknown>),
  })
  return unwrapApiEnvelope<CursorPage<AdminUserSummary>>(raw)
}

export async function getAdminUserDetail(userId: string): Promise<AdminUserDetail> {
  const raw: unknown = await request.get<unknown>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}`,
  )
  const payload = unwrapApiEnvelope<Record<string, unknown>>(raw)
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
  const raw: unknown = await request.patch<unknown>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}/status`,
    payload,
  )
  return unwrapApiEnvelope<UpdateAdminUserStatusResult>(raw)
}

export async function kickAdminUser(
  userId: string,
  payload: KickAdminUserPayload,
): Promise<KickAdminUserResult> {
  const body: Record<string, unknown> = { reason: payload.reason }
  if (payload.session_id) {
    body.session_id = payload.session_id
  }
  const raw: unknown = await request.post<unknown>(
    `/api/v1/admin/users/${encodeURIComponent(userId)}/kick`,
    body,
  )
  return unwrapApiEnvelope<KickAdminUserResult>(raw)
}

export async function listAdminGroups(
  params: ListAdminGroupsParams = {},
): Promise<CursorPage<AdminGroupSummary>> {
  const raw: unknown = await request.get<unknown>('/api/v1/admin/groups', {
    params: compactAdminParams(params as Record<string, unknown>),
  })
  return unwrapApiEnvelope<CursorPage<AdminGroupSummary>>(raw)
}

export async function getAdminGroupDetail(groupId: string): Promise<AdminGroupDetail> {
  const raw: unknown = await request.get<unknown>(
    `/api/v1/admin/groups/${encodeURIComponent(groupId)}`,
  )
  const payload = unwrapApiEnvelope<Record<string, unknown>>(raw)
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
  const raw: unknown = await request.delete<unknown>(
    `/api/v1/admin/groups/${encodeURIComponent(groupId)}`,
    { data: { reason } },
  )
  return unwrapApiEnvelope<ForceDisbandGroupResult>(raw)
}

export async function listAdminMessageAudit(
  params: ListAdminMessageAuditParams = {},
): Promise<CursorPage<AdminMessageAuditItem>> {
  const raw: unknown = await request.get<unknown>('/api/v1/admin/messages/audit', {
    params: compactAdminParams(params as Record<string, unknown>),
  })
  return unwrapApiEnvelope<CursorPage<AdminMessageAuditItem>>(raw)
}

export async function getAdminMetricsOverview(): Promise<AdminMetricsOverview> {
  const raw: unknown = await request.get<unknown>('/api/v1/admin/metrics/overview')
  return unwrapApiEnvelope<AdminMetricsOverview>(raw)
}

export async function getAdminMetricsStreams(): Promise<AdminStreamConsumerMetric[]> {
  const raw: unknown = await request.get<unknown>('/api/v1/admin/metrics/streams')
  const payload = unwrapApiEnvelope<unknown>(raw)
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
  const raw: unknown = await request.get<unknown>('/api/v1/admin/health/dependencies')
  return unwrapApiEnvelope<AdminDependenciesHealth>(raw)
}
