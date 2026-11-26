import { apiGet, apiPost } from '@/api/request'
import type {
  MarkDeliveredResult,
  MarkReadResult,
  MessagePage,
  MessageReceiptList,
  MessageSyncResult,
  RecallMessageResult,
  ReceiptFilterStatus,
  SendMessagePayload,
  SendMessageResponse,
} from '@/types/message'

export interface FetchMessagesParams {
  before_seq?: string
  after_seq?: string
  limit?: number
}

export function fetchMessages(
  conversationId: string,
  params: FetchMessagesParams = {},
): Promise<MessagePage> {
  return apiGet<MessagePage>(`/conversations/${encodeURIComponent(conversationId)}/messages`, { params })
}

export function syncMessages(
  conversationId: string,
  params: { after_seq: string; limit?: number },
): Promise<MessageSyncResult> {
  return apiGet<MessageSyncResult>(`/conversations/${encodeURIComponent(conversationId)}/sync`, { params })
}

export function sendMessageHttp(
  conversationId: string,
  payload: SendMessagePayload,
): Promise<SendMessageResponse> {
  return apiPost<SendMessageResponse>(`/conversations/${encodeURIComponent(conversationId)}/messages`, payload)
}

export function recallMessageHttp(
  conversationId: string,
  msgId: string,
): Promise<RecallMessageResult> {
  return apiPost<RecallMessageResult>(
    `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(msgId)}/recall`,
  )
}

export function fetchMessageReceipts(
  conversationId: string,
  msgId: string,
  params: { status?: ReceiptFilterStatus; cursor?: string; limit?: number } = {},
): Promise<MessageReceiptList> {
  return apiGet<MessageReceiptList>(
    `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(msgId)}/receipts`,
    { params },
  )
}

export function markConversationRead(
  conversationId: string,
  lastReadSeq: string,
): Promise<MarkReadResult> {
  return apiPost<MarkReadResult>(`/conversations/${encodeURIComponent(conversationId)}/read`, {
    last_read_seq: lastReadSeq,
  })
}

export function markConversationDelivered(
  conversationId: string,
  lastDeliveredSeq: string,
): Promise<MarkDeliveredResult> {
  return apiPost<MarkDeliveredResult>(`/conversations/${encodeURIComponent(conversationId)}/delivered`, {
    last_delivered_seq: lastDeliveredSeq,
  })
}
