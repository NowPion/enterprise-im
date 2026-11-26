import type { ChatUser, GroupRole } from './conversation'

export type MessageType = 'text' | 'image' | 'file' | 'voice' | 'recall' | 'system'

export type MessageStatus = 'normal' | 'recalled'

export type DeliveryStatus = 'sent' | 'delivered' | 'read'

export type LocalDeliveryStatus = 'sending' | 'sent' | 'delivered' | 'read' | 'failed'

export interface MessageAssetExtra {
  asset_id?: string
  purpose?: string
  url?: string
  filename?: string
  mime?: string
  size_bytes?: number
  width?: number
  height?: number
  thumbnail_url?: string
  duration_ms?: number
}

export interface RecallExtra {
  recalled_msg_id?: string
  recalled_seq?: string
  recalled_at?: string
}

export type MessageExtra = MessageAssetExtra & RecallExtra & Record<string, unknown>

export interface Message {
  msg_id: string
  client_msg_id: string
  conversation_id: string
  seq: string
  sender_id: string
  msg_type: MessageType
  content: string
  extra: MessageExtra
  status: MessageStatus
  created_at: string
  recalled_at: string | null
  delivery_status?: DeliveryStatus | null
}

export interface ChatMessage extends Message {
  local_status: LocalDeliveryStatus
  req_id?: string
  error_code?: number
  error_message?: string
  retryable?: boolean
  retry_count?: number
  optimistic?: boolean
  local_preview_url?: string
}

export interface MessagePage {
  items: Message[]
  next_cursor: string | null
  has_more: boolean
}

export interface MessageSyncResult {
  items: Message[]
  from_seq: string
  to_seq: string
  has_more: boolean
}

export interface SendMessagePayload {
  client_msg_id: string
  msg_type: 'text' | 'image' | 'file' | 'voice'
  content: string
  extra: Record<string, unknown>
}

export interface SendMessageResponse {
  message: Message
}

export interface RecallMessageResult {
  recalled_message: Message
  recall_message: Message
}

export type ReceiptFilterStatus = 'read' | 'unread' | 'delivered' | 'pending'

export type ReceiptDeliveryStatus = 'sent' | 'delivered' | 'read' | 'pending'

export interface MessageReceiptItem {
  user: ChatUser
  role: GroupRole | null
  delivery_status: ReceiptDeliveryStatus
  last_delivered_seq: string
  last_read_seq: string
  delivered_at: string | null
  read_at: string | null
}

export interface MessageReceiptList {
  msg_id: string
  conversation_id: string
  seq: string
  total_members: number
  delivered_count: number
  read_count: number
  unread_count: number
  items: MessageReceiptItem[]
  next_cursor: string | null
  has_more: boolean
}

export interface MarkReadResult {
  conversation_id: string
  last_read_seq: string
  last_delivered_seq: string
  unread_count: number
  read_at: string
}

export interface MarkDeliveredResult {
  conversation_id: string
  last_delivered_seq: string
  last_read_seq: string
  unread_count: number
  delivered_at: string
}

export interface MsgAckData {
  client_msg_id: string
  msg_id: string
  conversation_id: string
  seq: string
  status: MessageStatus
  delivery_status: DeliveryStatus
}

export interface MessagePushData {
  push_type: 'message'
  message: Message
  delivery_status: DeliveryStatus
}

export interface ReceiptPushData {
  push_type: 'receipt'
  conversation_id: string
  user_id: string
  delivery_status: 'delivered' | 'read'
  msg_id: string | null
  seq: string | null
  last_delivered_seq: string
  last_read_seq: string
  updated_at: string
}

export type MsgPushData = MessagePushData | ReceiptPushData

export interface MsgRecallAckData {
  conversation_id: string
  recalled_msg_id: string
  recall_msg_id: string
  seq: string
  recalled_at: string
}

export interface PresenceUpdateData {
  user_id: string
  online: boolean
  device_count: number
  last_online_at: string | null
}

export interface SystemNoticeData {
  event: string
  notice_id: string
  conversation_id: string | null
  actor_id: string | null
  target_user_id: string | null
  payload: Record<string, unknown>
}

export interface WSErrorData {
  code: number
  message: string
  retryable?: boolean
  related_type?: string
  related_req_id?: string
}
