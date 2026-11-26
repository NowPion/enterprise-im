import { apiDelete, apiGet, apiPatch, apiPost } from './request'
import type { CursorPage } from '@/types/common'
import type {
  AcceptFriendRequestResponse,
  CreateFriendRequestPayload,
  DeleteFriendResponse,
  FriendItem,
  FriendRequestActionResponse,
  FriendRequestDirection,
  FriendRequestItem,
  FriendRequestStatus,
  UpdateFriendRemarkResponse,
} from '@/types/friend'

export interface ListFriendRequestsParams {
  direction: FriendRequestDirection
  status?: FriendRequestStatus
  cursor?: string
  limit?: number
}

export interface ListFriendsParams {
  cursor?: string
  limit?: number
}

export function createFriendRequest(
  payload: CreateFriendRequestPayload,
): Promise<FriendRequestActionResponse> {
  return apiPost<FriendRequestActionResponse>('/friend-requests', payload)
}

export function listFriendRequests(
  params: ListFriendRequestsParams,
): Promise<CursorPage<FriendRequestItem>> {
  return apiGet<CursorPage<FriendRequestItem>>('/friend-requests', { params })
}

export function acceptFriendRequest(requestId: string): Promise<AcceptFriendRequestResponse> {
  return apiPost<AcceptFriendRequestResponse>(`/friend-requests/${encodeURIComponent(requestId)}/accept`)
}

export function rejectFriendRequest(requestId: string): Promise<FriendRequestActionResponse> {
  return apiPost<FriendRequestActionResponse>(`/friend-requests/${encodeURIComponent(requestId)}/reject`)
}

export function cancelFriendRequest(requestId: string): Promise<FriendRequestActionResponse> {
  return apiPost<FriendRequestActionResponse>(`/friend-requests/${encodeURIComponent(requestId)}/cancel`)
}

export function listFriends(params?: ListFriendsParams): Promise<CursorPage<FriendItem>> {
  return apiGet<CursorPage<FriendItem>>('/friends', { params })
}

export function updateFriendRemark(
  friendId: string,
  remark: string,
): Promise<UpdateFriendRemarkResponse> {
  return apiPatch<UpdateFriendRemarkResponse>(`/friends/${encodeURIComponent(friendId)}`, { remark })
}

export function deleteFriend(friendId: string): Promise<DeleteFriendResponse> {
  return apiDelete<DeleteFriendResponse>(`/friends/${encodeURIComponent(friendId)}`)
}
