export type ConversationType = 'single' | 'group'

export type GroupRole = 'owner' | 'admin' | 'member'

export type GroupStatus = 'active' | 'disbanded'

export interface ChatUser {
  id: string
  username: string
  nickname: string
  avatar_url: string
  signature: string
  online: boolean
  last_online_at: string | null
}

export interface GroupSummary {
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

export interface Conversation {
  id: string
  type: ConversationType
  peer: ChatUser | null
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

export interface ConversationListPage {
  items: Conversation[]
  next_cursor: string | null
  has_more: boolean
}

export interface ConversationDetail {
  conversation: Conversation
  role: GroupRole | null
  peer: ChatUser | null
  group: GroupSummary | null
}

export interface UpdateConversationSettingsPayload {
  is_pinned?: boolean
  is_muted?: boolean
  is_visible?: boolean
}

export interface ConversationSettingsResult {
  conversation_id: string
  is_pinned: boolean
  is_muted: boolean
  is_visible: boolean
  updated_at: string
}

export interface ConversationMember {
  user: ChatUser
  role: GroupRole | null
  nickname_in_group: string
  online: boolean
  joined_at: string
  last_delivered_seq: string
  last_read_seq: string
}

export interface ConversationMemberPage {
  items: ConversationMember[]
  next_cursor: string | null
  has_more: boolean
}

export interface ApiEnvelope<T> {
  code: number
  message: string
  data: T
  request_id: string
}
