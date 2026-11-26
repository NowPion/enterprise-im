export type GroupStatus = 'active' | 'disbanded'
export type GroupRole = 'owner' | 'admin' | 'member'
export type MutableGroupRole = 'admin' | 'member'

export interface CursorPage<T> {
  items: T[]
  next_cursor: string | null
  has_more: boolean
}

export interface UploadAsset {
  asset_id: string
  purpose: string
  url: string
  download_url?: string
  filename: string
  mime: string
  size_bytes: number
  width?: number
  height?: number
  thumbnail_url?: string
  duration_ms?: number
}

export interface GroupInfo {
  id: string
  conversation_id: string
  name: string
  avatar_url: string
  announcement: string | null
  owner_id: string
  status: GroupStatus
  member_count: number
  max_members: number
  created_at: string
  updated_at: string
}

export interface GroupSummary extends GroupInfo {
  role: GroupRole
}

export interface GroupMember {
  user_id: string
  nickname: string
  avatar_url: string
  nickname_in_group: string
  role: GroupRole
  online: boolean
  joined_at: string
}

export interface GroupConversation {
  id: string
  type: 'group'
  peer: null
  group_id: string | null
  title: string
  avatar_url: string
  last_msg_id: string
  last_seq: string
  last_msg_preview: string
  last_msg_at: string | null
  last_delivered_seq: string
  last_read_seq: string
  unread_count: number
  is_pinned: boolean
  is_muted: boolean
  is_visible: boolean
  can_send: boolean
}

export interface GroupMessage {
  msg_id: string
  client_msg_id: string
  conversation_id: string
  seq: string
  sender_id: string
  msg_type: string
  content: string
  extra: Record<string, unknown> | null
  status: 'normal' | 'recalled'
  created_at: string
  recalled_at: string | null
}

export interface GroupConversationMember {
  conversation_id: string
  user_id: string
  last_delivered_seq: string
  last_read_seq: string
  unread_count: number
  is_visible: boolean
  joined_at: string
}

export interface GroupAuditLog {
  id: string
  event_id: string
  scope: string
  action: string
  actor_id: string
  actor_role: string
  target_type: string
  target_id: string
  group_id: string | null
  conversation_id: string | null
  reason: string
  detail: Record<string, unknown> | null
  request_id: string
  created_at: string
}

export interface CreateGroupPayload {
  name: string
  member_ids: string[]
  avatar_asset_id?: string | null
  announcement?: string | null
}

export interface CreateGroupResult {
  group: GroupInfo
  conversation: GroupConversation
  members: GroupMember[]
  system_message: GroupMessage
}

export interface GetGroupResult {
  group: GroupInfo
  conversation: GroupConversation
  role: GroupRole
  member_count: number
}

export interface UpdateGroupPayload {
  name?: string
  avatar_asset_id?: string | null
  announcement?: string | null
}

export interface UpdateGroupResult {
  group: GroupInfo
  system_message: GroupMessage
}

export interface DisbandGroupResult {
  group_id: string
  conversation_id: string
  status: GroupStatus
  system_message: GroupMessage
}

export interface ListGroupMembersParams {
  role?: GroupRole | ''
  online_only?: boolean
  cursor?: string
  limit?: number
}

export interface AddGroupMembersResult {
  group_id: string
  conversation_id: string
  members: GroupMember[]
  conversation_members: GroupConversationMember[]
  system_message: GroupMessage
}

export interface RemoveGroupMemberResult {
  group_id: string
  conversation_id: string
  user_id: string
  removed_at: string
  system_message: GroupMessage
}

export interface LeaveGroupResult {
  group_id: string
  conversation_id: string
  user_id: string
  left_at: string
  system_message: GroupMessage
}

export interface TransferGroupResult {
  group_id: string
  owner_id: string
  previous_owner_id: string
  system_message: GroupMessage
}

export interface UpdateMemberRoleResult {
  group_id: string
  user_id: string
  role: MutableGroupRole
  member: GroupMember
  system_message: GroupMessage
}

export interface UpdateMyMemberResult {
  member: GroupMember
}

export interface ListGroupAuditParams {
  from?: string
  to?: string
  cursor?: string
  limit?: number
}

export interface CandidateUser {
  id: string
  username: string
  nickname: string
  avatar_url: string
  signature: string
  online: boolean
  last_online_at: string | null
  remark?: string
}

export interface FriendCandidateItem {
  friend_id: string
  user: CandidateUser
  remark: string
  online: boolean
  conversation_id: string | null
}
