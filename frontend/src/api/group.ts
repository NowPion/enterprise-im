import request from '@/api/request'
import type {
  AddGroupMembersResult,
  CandidateUser,
  CreateGroupPayload,
  CreateGroupResult,
  CursorPage,
  DisbandGroupResult,
  FriendCandidateItem,
  GetGroupResult,
  GroupAuditLog,
  GroupMember,
  GroupStatus,
  GroupSummary,
  LeaveGroupResult,
  ListGroupAuditParams,
  ListGroupMembersParams,
  MutableGroupRole,
  RemoveGroupMemberResult,
  TransferGroupResult,
  UpdateGroupPayload,
  UpdateGroupResult,
  UpdateMemberRoleResult,
  UpdateMyMemberResult,
  UploadAsset,
} from '@/types/group'

export interface ApiEnvelopeError extends Error {
  code?: number
  requestId?: string
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * Normalizes responses from `@/api/request` whether its interceptors return
 * full AxiosResponse, `{ code, message, data, request_id }`, or unwrapped `data`.
 */
export function unwrapApiEnvelope<T>(raw: unknown): T {
  let current: unknown = raw
  if (
    isPlainRecord(current) &&
    'data' in current &&
    'status' in current &&
    'headers' in current &&
    typeof current.status === 'number'
  ) {
    current = current.data
  }
  if (
    isPlainRecord(current) &&
    'code' in current &&
    'data' in current &&
    typeof current.code === 'number'
  ) {
    if (current.code !== 0) {
      const msg =
        typeof current.message === 'string' && current.message.trim()
          ? current.message
          : '请求失败'
      const err: ApiEnvelopeError = new Error(msg)
      err.code = current.code
      if (typeof current.request_id === 'string') {
        err.requestId = current.request_id
      }
      throw err
    }
    return current.data as T
  }
  return current as T
}

export function extractErrorMessage(error: unknown, fallback = '操作失败，请稍后重试'): string {
  if (error instanceof Error && error.message.trim()) {
    return error.message
  }
  if (isPlainRecord(error)) {
    const resp = isPlainRecord(error.response) ? error.response : null
    const respData = resp && isPlainRecord(resp.data) ? resp.data : null
    if (respData && typeof respData.message === 'string' && respData.message.trim()) {
      return respData.message
    }
    if (typeof error.message === 'string' && error.message.trim()) {
      return error.message
    }
  }
  return fallback
}

function compactParams<T extends Record<string, unknown>>(params: T): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      out[key] = value
    }
  }
  return out
}

export async function uploadGroupAvatar(file: File): Promise<UploadAsset> {
  const formData = new FormData()
  formData.append('file', file)
  formData.append('purpose', 'group_avatar')
  const raw: unknown = await request.post<unknown>('/api/v1/uploads', formData, {
    headers: { 'Content-Type': 'multipart/form-data' },
  })
  return unwrapApiEnvelope<UploadAsset>(raw)
}

export async function createGroup(payload: CreateGroupPayload): Promise<CreateGroupResult> {
  const raw: unknown = await request.post<unknown>('/api/v1/groups', payload)
  return unwrapApiEnvelope<CreateGroupResult>(raw)
}

export async function listGroups(params: {
  status?: GroupStatus | ''
  cursor?: string
  limit?: number
} = {}): Promise<CursorPage<GroupSummary>> {
  const raw: unknown = await request.get<unknown>('/api/v1/groups', {
    params: compactParams(params),
  })
  return unwrapApiEnvelope<CursorPage<GroupSummary>>(raw)
}

export async function getGroupDetail(groupId: string): Promise<GetGroupResult> {
  const raw: unknown = await request.get<unknown>(`/api/v1/groups/${encodeURIComponent(groupId)}`)
  return unwrapApiEnvelope<GetGroupResult>(raw)
}

export async function updateGroupProfile(
  groupId: string,
  payload: UpdateGroupPayload,
): Promise<UpdateGroupResult> {
  const raw: unknown = await request.patch<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}`,
    payload,
  )
  return unwrapApiEnvelope<UpdateGroupResult>(raw)
}

export async function disbandGroup(groupId: string): Promise<DisbandGroupResult> {
  const raw: unknown = await request.delete<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}`,
  )
  return unwrapApiEnvelope<DisbandGroupResult>(raw)
}

export async function listGroupMembers(
  groupId: string,
  params: ListGroupMembersParams = {},
): Promise<CursorPage<GroupMember>> {
  const query: Record<string, unknown> = {}
  if (params.role) query.role = params.role
  if (params.online_only) query.online_only = true
  if (params.cursor) query.cursor = params.cursor
  if (params.limit) query.limit = params.limit
  const raw: unknown = await request.get<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/members`,
    { params: query },
  )
  return unwrapApiEnvelope<CursorPage<GroupMember>>(raw)
}

export async function addGroupMembers(
  groupId: string,
  userIds: string[],
): Promise<AddGroupMembersResult> {
  const raw: unknown = await request.post<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/members`,
    { user_ids: userIds },
  )
  return unwrapApiEnvelope<AddGroupMembersResult>(raw)
}

export async function removeGroupMember(
  groupId: string,
  userId: string,
): Promise<RemoveGroupMemberResult> {
  const raw: unknown = await request.delete<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(userId)}`,
  )
  return unwrapApiEnvelope<RemoveGroupMemberResult>(raw)
}

export async function leaveGroup(groupId: string): Promise<LeaveGroupResult> {
  const raw: unknown = await request.post<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/leave`,
    {},
  )
  return unwrapApiEnvelope<LeaveGroupResult>(raw)
}

export async function transferGroupOwnership(
  groupId: string,
  targetUserId: string,
): Promise<TransferGroupResult> {
  const raw: unknown = await request.post<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/transfer`,
    { target_user_id: targetUserId },
  )
  return unwrapApiEnvelope<TransferGroupResult>(raw)
}

export async function updateGroupMemberRole(
  groupId: string,
  userId: string,
  role: MutableGroupRole,
): Promise<UpdateMemberRoleResult> {
  const raw: unknown = await request.patch<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(userId)}/role`,
    { role },
  )
  return unwrapApiEnvelope<UpdateMemberRoleResult>(raw)
}

export async function updateMyGroupNickname(
  groupId: string,
  nicknameInGroup: string,
): Promise<UpdateMyMemberResult> {
  const raw: unknown = await request.patch<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/members/me`,
    { nickname_in_group: nicknameInGroup },
  )
  return unwrapApiEnvelope<UpdateMyMemberResult>(raw)
}

export async function listGroupAuditLogs(
  groupId: string,
  params: ListGroupAuditParams = {},
): Promise<CursorPage<GroupAuditLog>> {
  const raw: unknown = await request.get<unknown>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/audit`,
    { params: compactParams(params) },
  )
  return unwrapApiEnvelope<CursorPage<GroupAuditLog>>(raw)
}

export async function listFriendCandidates(
  cursor?: string,
  limit = 50,
): Promise<CursorPage<FriendCandidateItem>> {
  const raw: unknown = await request.get<unknown>('/api/v1/friends', {
    params: compactParams({ cursor, limit }),
  })
  return unwrapApiEnvelope<CursorPage<FriendCandidateItem>>(raw)
}

export async function searchCandidateUsers(params: {
  q: string
  field?: 'username' | 'nickname'
  match?: 'exact' | 'prefix'
  cursor?: string
  limit?: number
}): Promise<CursorPage<CandidateUser>> {
  const raw: unknown = await request.get<unknown>('/api/v1/users/search', {
    params: compactParams({
      q: params.q.trim(),
      field: params.field ?? 'nickname',
      match: params.match ?? 'prefix',
      cursor: params.cursor,
      limit: params.limit ?? 20,
    }),
  })
  return unwrapApiEnvelope<CursorPage<CandidateUser>>(raw)
}
