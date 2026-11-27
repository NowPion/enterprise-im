import type { APIRequestContext, APIResponse } from '@playwright/test'
import { getBaseUrl } from './env'

export interface ApiEnvelope<T = unknown> {
  code: number
  message: string
  data: T
  request_id: string
}

export async function parseEnvelope<T = unknown>(res: APIResponse): Promise<ApiEnvelope<T>> {
  const text = await res.text()
  try {
    return JSON.parse(text) as ApiEnvelope<T>
  } catch {
    throw new Error(`Expected JSON envelope (status ${res.status()}), got: ${text.slice(0, 300)}`)
  }
}

export async function assertEnvelopeOK<T = unknown>(res: APIResponse): Promise<T> {
  const env = await parseEnvelope<T>(res)
  if (!res.ok() || env.code !== 0) {
    throw new Error(
      `API failed: status=${res.status()} code=${env.code} message=${env.message} request_id=${env.request_id}`,
    )
  }
  return env.data
}

export async function registerUserViaAPI(
  request: APIRequestContext,
  user: { username: string; password: string; nickname: string },
) {
  const res = await request.post(`${getBaseUrl()}/api/v1/auth/register`, {
    data: user,
  })
  return assertEnvelopeOK<{ user: { id: string; username: string; nickname: string } }>(res)
}

export async function loginUserViaAPI(
  request: APIRequestContext,
  creds: { username: string; password: string; device_id?: string; device_type?: string },
) {
  const res = await request.post(`${getBaseUrl()}/api/v1/auth/login`, {
    data: {
      username: creds.username,
      password: creds.password,
      device_id: creds.device_id || `pw-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      device_type: creds.device_type || 'web',
    },
  })
  return assertEnvelopeOK<{
    user: { id: string; username: string; nickname: string; role: string; status: number }
    tokens: { access_token: string; refresh_token: string; session_id: string; device_id: string }
  }>(res)
}

export async function getHealthz(request: APIRequestContext) {
  const res = await request.get(`${getBaseUrl()}/healthz`)
  return assertEnvelopeOK<{ status: string; mysql: string; redis: string; timestamp: string }>(res)
}
