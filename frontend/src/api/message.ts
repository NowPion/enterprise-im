import request from '@/api/request'
import { normalizeApiError, unwrapApiResponse } from '@/api/conversation'
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

export async function fetchMessages(
  conversationId: string,
  params: FetchMessagesParams = {},
): Promise<MessagePage> {
  try {
    const res = await request.get(`/conversations/${encodeURIComponent(conversationId)}/messages`, { params })
    return unwrapApiResponse<MessagePage>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function syncMessages(
  conversationId: string,
  params: { after_seq: string; limit?: number },
): Promise<MessageSyncResult> {
  try {
    const res = await request.get(`/conversations/${encodeURIComponent(conversationId)}/sync`, { params })
    return unwrapApiResponse<MessageSyncResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function sendMessageHttp(
  conversationId: string,
  payload: SendMessagePayload,
): Promise<SendMessageResponse> {
  try {
    const res = await request.post(`/conversations/${encodeURIComponent(conversationId)}/messages`, payload)
    return unwrapApiResponse<SendMessageResponse>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function recallMessageHttp(
  conversationId: string,
  msgId: string,
): Promise<RecallMessageResult> {
  try {
    const res = await request.post(
      `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(msgId)}/recall`,
    )
    return unwrapApiResponse<RecallMessageResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function fetchMessageReceipts(
  conversationId: string,
  msgId: string,
  params: { status?: ReceiptFilterStatus; cursor?: string; limit?: number } = {},
): Promise<MessageReceiptList> {
  try {
    const res = await request.get(
      `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(msgId)}/receipts`,
      { params },
    )
    return unwrapApiResponse<MessageReceiptList>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function markConversationRead(
  conversationId: string,
  lastReadSeq: string,
): Promise<MarkReadResult> {
  try {
    const res = await request.post(`/conversations/${encodeURIComponent(conversationId)}/read`, {
      last_read_seq: lastReadSeq,
    })
    return unwrapApiResponse<MarkReadResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function markConversationDelivered(
  conversationId: string,
  lastDeliveredSeq: string,
): Promise<MarkDeliveredResult> {
  try {
    const res = await request.post(`/conversations/${encodeURIComponent(conversationId)}/delivered`, {
      last_delivered_seq: lastDeliveredSeq,
    })
    return unwrapApiResponse<MarkDeliveredResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}
