import { apiDelete, apiGet, apiPatch, apiPost } from '@/api/request'
import { isApiError } from '@/types/common'
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

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function extractErrorMessage(error: unknown, fallback = '操作失败，请稍后重试'): string {
  if (isApiError(error) && error.message.trim()) {
    return error.message
  }
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

export function compactParams<T extends object>(params: T): Record<string, unknown> {
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
  return apiPost<UploadAsset>('/uploads', formData, {
    headers: { 'Content-Type': 'multipart/form-data' },
  })
}

export async function createGroup(payload: CreateGroupPayload): Promise<CreateGroupResult> {
  return apiPost<CreateGroupResult>('/groups', payload)
}

export async function listGroups(params: {
  status?: GroupStatus | ''
  cursor?: string
  limit?: number
} = {}): Promise<CursorPage<GroupSummary>> {
  return apiGet<CursorPage<GroupSummary>>('/groups', {
    params: compactParams(params),
  })
}

export async function getGroupDetail(groupId: string): Promise<GetGroupResult> {
  return apiGet<GetGroupResult>(`/groups/${encodeURIComponent(groupId)}`)
}

export async function updateGroupProfile(
  groupId: string,
  payload: UpdateGroupPayload,
): Promise<UpdateGroupResult> {
  return apiPatch<UpdateGroupResult>(`/groups/${encodeURIComponent(groupId)}`, payload)
}

export async function disbandGroup(groupId: string): Promise<DisbandGroupResult> {
  return apiDelete<DisbandGroupResult>(`/groups/${encodeURIComponent(groupId)}`)
}

export async function listGroupMembers(
  groupId: string,
  params: ListGroupMembersParams = {},
): Promise<CursorPage<GroupMember>> {
  return apiGet<CursorPage<GroupMember>>(`/groups/${encodeURIComponent(groupId)}/members`, {
    params: compactParams({
      role: params.role || undefined,
      online_only: params.online_only ? true : undefined,
      cursor: params.cursor,
      limit: params.limit,
    }),
  })
}

export async function addGroupMembers(
  groupId: string,
  userIds: string[],
): Promise<AddGroupMembersResult> {
  return apiPost<AddGroupMembersResult>(`/groups/${encodeURIComponent(groupId)}/members`, {
    user_ids: userIds,
  })
}

export async function removeGroupMember(
  groupId: string,
  userId: string,
): Promise<RemoveGroupMemberResult> {
  return apiDelete<RemoveGroupMemberResult>(
    `/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(userId)}`,
  )
}

export async function leaveGroup(groupId: string): Promise<LeaveGroupResult> {
  return apiPost<LeaveGroupResult>(`/groups/${encodeURIComponent(groupId)}/leave`, {})
}

export async function transferGroupOwnership(
  groupId: string,
  targetUserId: string,
): Promise<TransferGroupResult> {
  return apiPost<TransferGroupResult>(`/groups/${encodeURIComponent(groupId)}/transfer`, {
    target_user_id: targetUserId,
  })
}

export async function updateGroupMemberRole(
  groupId: string,
  userId: string,
  role: MutableGroupRole,
): Promise<UpdateMemberRoleResult> {
  return apiPatch<UpdateMemberRoleResult>(
    `/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(userId)}/role`,
    { role },
  )
}

export async function updateMyGroupNickname(
  groupId: string,
  nicknameInGroup: string,
): Promise<UpdateMyMemberResult> {
  return apiPatch<UpdateMyMemberResult>(`/groups/${encodeURIComponent(groupId)}/members/me`, {
    nickname_in_group: nicknameInGroup,
  })
}

export async function listGroupAuditLogs(
  groupId: string,
  params: ListGroupAuditParams = {},
): Promise<CursorPage<GroupAuditLog>> {
  return apiGet<CursorPage<GroupAuditLog>>(`/groups/${encodeURIComponent(groupId)}/audit`, {
    params: compactParams(params),
  })
}

export async function listFriendCandidates(
  cursor?: string,
  limit = 50,
): Promise<CursorPage<FriendCandidateItem>> {
  return apiGet<CursorPage<FriendCandidateItem>>('/friends', {
    params: compactParams({ cursor, limit }),
  })
}

export async function searchCandidateUsers(params: {
  q: string
  field?: 'username' | 'nickname'
  match?: 'exact' | 'prefix'
  cursor?: string
  limit?: number
}): Promise<CursorPage<CandidateUser>> {
  return apiGet<CursorPage<CandidateUser>>('/users/search', {
    params: compactParams({
      q: params.q.trim(),
      field: params.field ?? 'nickname',
      match: params.match ?? 'prefix',
      cursor: params.cursor,
      limit: params.limit ?? 20,
    }),
  })
}
