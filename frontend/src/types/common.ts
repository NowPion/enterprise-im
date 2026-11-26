export interface ApiEnvelope<T = unknown> {
  code: number
  message: string
  data: T
  request_id: string
}

export interface CursorPage<T> {
  items: T[]
  next_cursor: string | null
  has_more: boolean
}

export interface Asset {
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

export const BIZ_CODE = {
  OK: 0,
  INVALID_PARAMS: 10000,
  INVALID_VALUE: 10001,
  PAYLOAD_TOO_LARGE: 10002,
  RATE_LIMITED: 10003,
  ACCESS_TOKEN_INVALID: 11001,
  ACCESS_TOKEN_BLACKLISTED: 11002,
  REFRESH_TOKEN_INVALID: 11003,
  ACCOUNT_DISABLED: 11004,
  SESSION_REVOKED: 11005,
  INVALID_CREDENTIALS: 11006,
  USERNAME_EXISTS: 11007,
  REFRESH_CONFLICT: 11008,
  SESSION_LIMIT_REACHED: 11009,
  USER_NOT_FOUND: 12001,
  FRIEND_REQUEST_SELF: 12002,
  ALREADY_FRIENDS: 12003,
  FRIEND_REQUEST_PENDING: 12004,
  FRIEND_REQUEST_NOT_FOUND: 12005,
  FRIEND_REQUEST_HANDLED: 12006,
  NOT_FRIENDS: 12007,
  FRIENDSHIP_NOT_FOUND: 12008,
  ADMIN_REQUIRED: 17001,
  WS_AUTH_FAILED: 18001,
  WS_SESSION_REVOKED: 18006,
} as const

export class ApiError extends Error {
  readonly code: number
  readonly requestId: string
  readonly httpStatus: number

  constructor(code: number, message: string, requestId = '', httpStatus = 0) {
    super(message || '请求失败')
    this.name = 'ApiError'
    this.code = code
    this.requestId = requestId
    this.httpStatus = httpStatus
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}
