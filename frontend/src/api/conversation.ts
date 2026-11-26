import { apiGet, apiPatch } from '@/api/request'
import type {
  ConversationDetail,
  ConversationListPage,
  ConversationMemberPage,
  ConversationSettingsResult,
  UpdateConversationSettingsPayload,
} from '@/types/conversation'

export interface FetchConversationsParams {
  cursor?: string
  limit?: number
  include_hidden?: boolean
}

export function fetchConversations(params: FetchConversationsParams = {}): Promise<ConversationListPage> {
  return apiGet<ConversationListPage>('/conversations', { params })
}

export function fetchConversationDetail(conversationId: string): Promise<ConversationDetail> {
  return apiGet<ConversationDetail>(`/conversations/${encodeURIComponent(conversationId)}`)
}

export function updateConversationSettings(
  conversationId: string,
  payload: UpdateConversationSettingsPayload,
): Promise<ConversationSettingsResult> {
  return apiPatch<ConversationSettingsResult>(
    `/conversations/${encodeURIComponent(conversationId)}/settings`,
    payload,
  )
}

export function fetchConversationMembers(
  conversationId: string,
  params: { cursor?: string; limit?: number } = {},
): Promise<ConversationMemberPage> {
  return apiGet<ConversationMemberPage>(
    `/conversations/${encodeURIComponent(conversationId)}/members`,
    { params },
  )
}
