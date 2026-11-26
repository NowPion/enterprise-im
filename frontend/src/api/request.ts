import axios, { type AxiosRequestConfig, type InternalAxiosRequestConfig } from 'axios'
import {
  clearStoredTokens,
  getStoredTokens,
  refreshTokensSingleFlight,
} from '@/composables/useAuthRefresh'
import { ApiError, BIZ_CODE, type ApiEnvelope } from '@/types/common'

export interface RequestConfig extends AxiosRequestConfig {
  skipAuth?: boolean
  _retried?: boolean
}

interface InternalRequestConfig extends InternalAxiosRequestConfig {
  skipAuth?: boolean
  _retried?: boolean
}

const SKIP_REFRESH_PATHS = ['/auth/login', '/auth/register', '/auth/refresh']

function shouldSkipRefresh(url?: string): boolean {
  if (!url) return false
  return SKIP_REFRESH_PATHS.some((path) => url.includes(path))
}

function extractBearerToken(headerValue: unknown): string | undefined {
  if (typeof headerValue !== 'string') return undefined
  if (!headerValue.startsWith('Bearer ')) return undefined
  return headerValue.slice('Bearer '.length).trim() || undefined
}

function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error
  if (axios.isAxiosError<ApiEnvelope<unknown>>(error)) {
    const payload = error.response?.data
    const status = error.response?.status ?? 0
    if (payload && typeof payload.code === 'number') {
      return new ApiError(payload.code, payload.message || '请求失败', payload.request_id || '', status)
    }
    return new ApiError(-1, error.message || '网络请求失败', '', status)
  }
  if (error instanceof Error) {
    return new ApiError(-1, error.message, '', 0)
  }
  return new ApiError(-1, '未知请求错误', '', 0)
}

export const request = axios.create({
  baseURL: '/api/v1',
  timeout: 15000,
})

export const healthRequest = axios.create({
  baseURL: '',
  timeout: 8000,
})

request.interceptors.request.use((config: InternalRequestConfig) => {
  if (!config.skipAuth) {
    const tokens = getStoredTokens()
    if (tokens?.access_token) {
      config.headers = config.headers ?? {}
      config.headers.Authorization = `Bearer ${tokens.access_token}`
    }
  }
  return config
})

request.interceptors.response.use(
  (response) => {
    const envelope = response.data as ApiEnvelope<unknown>
    if (envelope && typeof envelope.code === 'number' && envelope.code !== BIZ_CODE.OK) {
      return Promise.reject(
        new ApiError(envelope.code, envelope.message || '请求失败', envelope.request_id || '', response.status),
      )
    }
    return response
  },
  async (error) => {
    if (!axios.isAxiosError<ApiEnvelope<unknown>>(error) || !error.config) {
      return Promise.reject(toApiError(error))
    }

    const originalConfig = error.config as InternalRequestConfig
    const status = error.response?.status ?? 0
    const bizCode = error.response?.data?.code ?? 0

    const isAccessExpired =
      status === 401 &&
      (bizCode === BIZ_CODE.ACCESS_TOKEN_INVALID ||
        bizCode === BIZ_CODE.ACCESS_TOKEN_BLACKLISTED ||
        bizCode === 0)

    if (
      isAccessExpired &&
      !originalConfig.skipAuth &&
      !originalConfig._retried &&
      !shouldSkipRefresh(originalConfig.url)
    ) {
      originalConfig._retried = true
      const failedAccessToken = extractBearerToken(originalConfig.headers?.Authorization)
      try {
        const nextTokens = await refreshTokensSingleFlight(failedAccessToken)
        originalConfig.headers = originalConfig.headers ?? {}
        originalConfig.headers.Authorization = `Bearer ${nextTokens.access_token}`
        return request(originalConfig)
      } catch (refreshError) {
        return Promise.reject(toApiError(refreshError))
      }
    }

    if (bizCode === BIZ_CODE.ACCOUNT_DISABLED || bizCode === BIZ_CODE.SESSION_REVOKED) {
      clearStoredTokens()
    }

    return Promise.reject(toApiError(error))
  },
)

export async function apiGet<T>(url: string, config?: RequestConfig): Promise<T> {
  const res = await request.get<ApiEnvelope<T>>(url, config)
  return res.data.data
}

export async function apiPost<T>(url: string, data?: unknown, config?: RequestConfig): Promise<T> {
  const res = await request.post<ApiEnvelope<T>>(url, data, config)
  return res.data.data
}

export async function apiPatch<T>(url: string, data?: unknown, config?: RequestConfig): Promise<T> {
  const res = await request.patch<ApiEnvelope<T>>(url, data, config)
  return res.data.data
}

export async function apiPut<T>(url: string, data?: unknown, config?: RequestConfig): Promise<T> {
  const res = await request.put<ApiEnvelope<T>>(url, data, config)
  return res.data.data
}

export async function apiDelete<T>(url: string, config?: RequestConfig): Promise<T> {
  const res = await request.delete<ApiEnvelope<T>>(url, config)
  return res.data.data
}

export default request
