import type { CursorPage, GroupMember, GroupStatus } from './group'

export type PlatformRole = 'user' | 'admin'
export type UserAccountStatus = 0 | 1

export interface AdminUserSummary {
  id: string
  username: string
  nickname: string
  avatar_url: string
  signature: string
  role: PlatformRole
  status: UserAccountStatus
  online: boolean
  last_online_at: string | null
  session_count: number
  created_at?: string
  updated_at?: string
}

export interface AdminUserSession {
  id: string
  device_id: string
  device_type: 'web' | 'desktop' | 'mobile' | string
  ip_address: string
  user_agent: string
  expires_at: string
  revoked_at: string | null
  last_active_at: string
  created_at: string
}

export interface AdminAuditSummaryItem {
  id?: string
  event_id?: string
  scope?: string
  action: string
  actor_id: string
  actor_role?: string
  target_type?: string
  target_id?: string
  reason: string
  detail?: Record<string, unknown> | null
  request_id?: string
  created_at: string
}

export interface AdminUserDetail {
  user: AdminUserSummary
  sessions: AdminUserSession[]
  friend_count: number
  group_count: number
  audit_summary: AdminAuditSummaryItem[]
}

export interface ListAdminUsersParams {
  q?: string
  status?: UserAccountStatus | ''
  role?: PlatformRole | ''
  cursor?: string
  limit?: number
}

export interface UpdateAdminUserStatusPayload {
  status: UserAccountStatus
  reason: string
}

export interface UpdateAdminUserStatusResult {
  user_id?: string
  status: UserAccountStatus
  revoked_session_count: number
  kicked_connection_count: number
}

export interface KickAdminUserPayload {
  session_id?: string
  reason: string
}

export interface KickAdminUserResult {
  user_id?: string
  revoked_session_count: number
  kicked_connection_count: number
}

export interface AdminGroupOwnerInfo {
  id: string
  username?: string
  nickname: string
  avatar_url?: string
}

export interface AdminGroupSummary {
  id: string
  conversation_id: string
  name: string
  avatar_url: string
  announcement: string | null
  owner_id: string
  owner?: AdminGroupOwnerInfo | null
  status: GroupStatus
  member_count: number
  max_members: number
  last_seq: string
  created_at: string
  updated_at?: string
}

export interface ListAdminGroupsParams {
  q?: string
  status?: GroupStatus | ''
  cursor?: string
  limit?: number
}

export interface AdminGroupDetail {
  group: AdminGroupSummary
  members: GroupMember[]
  members_page?: CursorPage<GroupMember>
  system_events: AdminAuditSummaryItem[]
}

export interface ForceDisbandGroupResult {
  group_id: string
  conversation_id?: string
  status: GroupStatus
  audit_log?: AdminAuditSummaryItem
}

export interface AdminMessageAuditItem {
  msg_id: string
  client_msg_id: string
  conversation_id: string
  seq: string
  sender_id: string
  sender_username?: string
  sender_nickname?: string
  msg_type: 'text' | 'image' | 'file' | 'voice' | 'recall' | 'system' | string
  content: string
  extra: Record<string, unknown> | null
  status: 'normal' | 'recalled'
  created_at: string
  recalled_at: string | null
}

export interface ListAdminMessageAuditParams {
  conversation_id?: string
  sender_id?: string
  keyword?: string
  from?: string
  to?: string
  cursor?: string
  limit?: number
}

export interface AdminStreamsSummary {
  messages_len?: number
  events_len?: number
  pending_total?: number
  dead_letter_count?: number
  [key: string]: unknown
}

export interface AdminMetricsOverview {
  online_users: number
  ws_connections: number
  messages_per_minute: number
  streams: AdminStreamsSummary | number
  goroutines: number
  heap_bytes: number
  timestamp?: string
}

export interface AdminStreamConsumerMetric {
  stream: string
  group: string
  consumer: string
  last_delivered_id: string
  pending_count: number
  oldest_pending_age: number | string
}

export interface DependencyHealthItem {
  name?: string
  status: string
  latency_ms: number | null
  version: string
  last_error_at: string | null
  last_error?: string | null
}

export interface AdminDependenciesHealth {
  mysql: DependencyHealthItem
  redis: DependencyHealthItem
  timestamp?: string
}
