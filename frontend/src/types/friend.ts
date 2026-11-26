import type { User } from './user'

export type FriendRequestStatus = 'pending' | 'accepted' | 'rejected' | 'canceled'
export type FriendRequestDirection = 'received' | 'sent'

export interface FriendRequest {
  id: string
  from_user_id: string
  to_user_id: string
  message: string
  status: FriendRequestStatus
  created_at: string
  handled_at: string | null
}

export interface FriendRequestItem extends FriendRequest {
  peer: User | null
}

export interface CreateFriendRequestPayload {
  to_user_id: string
  message?: string
}

export interface FriendRequestActionResponse {
  request: FriendRequest
}

export interface AcceptFriendRequestResponse {
  request: FriendRequest
  friend_id: string
  conversation_id: string
  system_msg_id: string
}

export interface FriendItem {
  friend_id: string
  user: User
  remark: string
  online: boolean
  conversation_id: string | null
}

export interface UpdateFriendRemarkResponse {
  friend_id: string
  remark: string
}

export interface DeleteFriendResponse {
  friend_id: string
  conversation_id: string | null
  is_visible: boolean
}
