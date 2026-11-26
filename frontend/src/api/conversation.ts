import request from '@/api/request'
import type {
  ApiEnvelope,
  ConversationDetail,
  ConversationListPage,
  ConversationMemberPage,
  ConversationSettingsResult,
  UpdateConversationSettingsPayload,
} from '@/types/conversation'

export class ApiClientError extends Error {
  readonly code: number
  readonly requestId?: string
  readonly httpStatus?: number

  constructor(code: number, message: string, requestId?: string, httpStatus?: number) {
    super(message || `请求失败 (${code})`)
    this.name = 'ApiClientError'
    this.code = code
    this.requestId = requestId
    this.httpStatus = httpStatus
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isApiEnvelope<T>(value: unknown): value is ApiEnvelope<T> {
  return (
    isRecord(value) &&
    typeof value.code === 'number' &&
    'data' in value &&
    ('message' in value || 'request_id' in value)
  )
}

export function unwrapApiResponse<T>(raw: unknown): T {
  let candidate: unknown = raw
  if (isRecord(raw) && 'data' in raw && 'status' in raw && 'headers' in raw) {
    candidate = raw.data
  }
  if (isApiEnvelope<T>(candidate)) {
    if (candidate.code !== 0) {
      throw new ApiClientError(candidate.code, candidate.message, candidate.request_id)
    }
    return candidate.data
  }
  return candidate as T
}

export function normalizeApiError(err: unknown): ApiClientError {
  if (err instanceof ApiClientError) {
    return err
  }
  if (isRecord(err)) {
    const response = isRecord(err.response) ? err.response : undefined
    const httpStatus = typeof response?.status === 'number' ? response.status : undefined
    const payload = response?.data
    if (isApiEnvelope<unknown>(payload)) {
      return new ApiClientError(payload.code, payload.message, payload.request_id, httpStatus)
    }
    if (typeof err.code === 'number') {
      return new ApiClientError(
        err.code,
        typeof err.message === 'string' ? err.message : '请求失败',
        typeof err.request_id === 'string' ? err.request_id : undefined,
        httpStatus,
      )
    }
    if (typeof err.message === 'string' && err.message.trim().length > 0) {
      return new ApiClientError(-1, err.message, undefined, httpStatus)
    }
  }
  return new ApiClientError(-1, '网络或服务异常，请稍后重试')
}

export interface FetchConversationsParams {
  cursor?: string
  limit?: number
  include_hidden?: boolean
}

export async function fetchConversations(params: FetchConversationsParams = {}): Promise<ConversationListPage> {
  try {
    const res = await request.get('/conversations', { params })
    return unwrapApiResponse<ConversationListPage>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function fetchConversationDetail(conversationId: string): Promise<ConversationDetail> {
  try {
    const res = await request.get(`/conversations/${encodeURIComponent(conversationId)}`)
    return unwrapApiResponse<ConversationDetail>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function updateConversationSettings(
  conversationId: string,
  payload: UpdateConversationSettingsPayload,
): Promise<ConversationSettingsResult> {
  try {
    const res = await request.patch(`/conversations/${encodeURIComponent(conversationId)}/settings`, payload)
    return unwrapApiResponse<ConversationSettingsResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function fetchConversationMembers(
  conversationId: string,
  params: { cursor?: string; limit?: number } = {},
): Promise<ConversationMemberPage> {
  try {
    const res = await request.get(`/conversations/${encodeURIComponent(conversationId)}/members`, { params })
    return unwrapApiResponse<ConversationMemberPage>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}
