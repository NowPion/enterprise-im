import axios from 'axios'
import { ref } from 'vue'
import type { RefreshResponse, Tokens } from '@/types/auth'
import { ApiError, BIZ_CODE, type ApiEnvelope } from '@/types/common'

const DEVICE_ID_KEY = 'im_device_id'
const TOKENS_KEY = 'im_auth_tokens'
const REFRESH_LOCK_KEY = 'im_auth_refresh_lock'
const LOCK_TTL_MS = 5000
const LOCK_WAIT_TIMEOUT_MS = 3500

interface RefreshLockPayload {
  owner: string
  expiresAt: number
}

interface AuthLifecycleCallbacks {
  onTokensRotated?: (tokens: Tokens) => void
  onSessionExpired?: (code: number, message: string) => void
}

const tabOwnerId = generateRandomId()
const isRefreshing = ref(false)
let activeRefreshPromise: Promise<Tokens> | null = null
let lifecycleCallbacks: AuthLifecycleCallbacks = {}

const rawClient = axios.create({
  baseURL: '/api/v1',
  timeout: 15000,
})

function generateRandomId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  const bytes = new Uint8Array(16)
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) {
      bytes[i] = Math.floor(Math.random() * 256)
    }
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

function getStorage(): Storage | null {
  try {
    return typeof window !== 'undefined' ? window.localStorage : null
  } catch {
    return null
  }
}

export function setAuthLifecycleCallbacks(callbacks: AuthLifecycleCallbacks): void {
  lifecycleCallbacks = { ...lifecycleCallbacks, ...callbacks }
}

export function getDeviceId(): string {
  const storage = getStorage()
  if (!storage) return tabOwnerId
  const existing = storage.getItem(DEVICE_ID_KEY)?.trim()
  if (existing) return existing
  const created = `web-${generateRandomId()}`
  storage.setItem(DEVICE_ID_KEY, created)
  return created
}

export function getStoredTokens(): Tokens | null {
  const storage = getStorage()
  if (!storage) return null
  const raw = storage.getItem(TOKENS_KEY)
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as Partial<Tokens>
    if (parsed && typeof parsed.access_token === 'string' && typeof parsed.refresh_token === 'string') {
      return parsed as Tokens
    }
    return null
  } catch {
    return null
  }
}

export function saveStoredTokens(tokens: Tokens): void {
  const storage = getStorage()
  if (!storage) return
  storage.setItem(TOKENS_KEY, JSON.stringify(tokens))
}

export function clearStoredTokens(): void {
  const storage = getStorage()
  if (!storage) return
  storage.removeItem(TOKENS_KEY)
}

function readLock(storage: Storage): RefreshLockPayload | null {
  const raw = storage.getItem(REFRESH_LOCK_KEY)
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as RefreshLockPayload
    if (parsed && typeof parsed.owner === 'string' && typeof parsed.expiresAt === 'number') {
      return parsed
    }
  } catch {
    return null
  }
  return null
}

function tryAcquireCrossTabLock(): boolean {
  const storage = getStorage()
  if (!storage) return true
  const now = Date.now()
  const existing = readLock(storage)
  if (existing && existing.expiresAt > now && existing.owner !== tabOwnerId) {
    return false
  }
  const payload: RefreshLockPayload = {
    owner: tabOwnerId,
    expiresAt: now + LOCK_TTL_MS,
  }
  storage.setItem(REFRESH_LOCK_KEY, JSON.stringify(payload))
  const confirmed = readLock(storage)
  return confirmed?.owner === tabOwnerId
}

function releaseCrossTabLock(): void {
  const storage = getStorage()
  if (!storage) return
  const existing = readLock(storage)
  if (existing?.owner === tabOwnerId) {
    storage.removeItem(REFRESH_LOCK_KEY)
  }
}

function waitForCrossTabTokens(previousAccessToken?: string, timeoutMs = LOCK_WAIT_TIMEOUT_MS): Promise<Tokens | null> {
  return new Promise((resolve) => {
    const storage = getStorage()
    if (!storage || typeof window === 'undefined') {
      resolve(null)
      return
    }

    const deadline = Date.now() + timeoutMs
    let settled = false

    const finish = (tokens: Tokens | null) => {
      if (settled) return
      settled = true
      window.removeEventListener('storage', onStorage)
      clearInterval(timer)
      resolve(tokens)
    }

    const checkNow = () => {
      const latest = getStoredTokens()
      if (latest && latest.access_token && latest.access_token !== previousAccessToken) {
        finish(latest)
        return
      }
      const lock = readLock(storage)
      if (!lock || lock.expiresAt <= Date.now()) {
        const current = getStoredTokens()
        if (current && current.access_token && current.access_token !== previousAccessToken) {
          finish(current)
          return
        }
        finish(null)
        return
      }
      if (Date.now() >= deadline) {
        finish(null)
      }
    }

    const onStorage = (event: StorageEvent) => {
      if (event.key === TOKENS_KEY || event.key === REFRESH_LOCK_KEY) {
        checkNow()
      }
    }

    window.addEventListener('storage', onStorage)
    const timer = setInterval(checkNow, 80)
    checkNow()
  })
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

async function performRefreshRequest(failedAccessToken?: string): Promise<Tokens> {
  const latestBeforeLock = getStoredTokens()
  if (
    latestBeforeLock?.access_token &&
    failedAccessToken &&
    latestBeforeLock.access_token !== failedAccessToken
  ) {
    lifecycleCallbacks.onTokensRotated?.(latestBeforeLock)
    return latestBeforeLock
  }

  if (!tryAcquireCrossTabLock()) {
    const awaited = await waitForCrossTabTokens(failedAccessToken)
    if (awaited) {
      lifecycleCallbacks.onTokensRotated?.(awaited)
      return awaited
    }
  }

  const currentTokens = getStoredTokens()
  if (
    currentTokens?.access_token &&
    failedAccessToken &&
    currentTokens.access_token !== failedAccessToken
  ) {
    releaseCrossTabLock()
    lifecycleCallbacks.onTokensRotated?.(currentTokens)
    return currentTokens
  }

  if (!currentTokens?.refresh_token) {
    releaseCrossTabLock()
    const err = new ApiError(BIZ_CODE.REFRESH_TOKEN_INVALID, '登录凭证已失效，请重新登录', '', 401)
    clearStoredTokens()
    lifecycleCallbacks.onSessionExpired?.(err.code, err.message)
    throw err
  }

  const sentRefreshToken = currentTokens.refresh_token
  const deviceId = currentTokens.device_id || getDeviceId()

  try {
    const response = await rawClient.post<ApiEnvelope<RefreshResponse>>('/auth/refresh', {
      refresh_token: sentRefreshToken,
      device_id: deviceId,
    })

    const envelope = response.data
    if (!envelope || envelope.code !== BIZ_CODE.OK || !envelope.data?.tokens) {
      throw new ApiError(
        envelope?.code ?? BIZ_CODE.REFRESH_TOKEN_INVALID,
        envelope?.message ?? '刷新凭证失败',
        envelope?.request_id ?? '',
        response.status,
      )
    }

    const nextTokens = envelope.data.tokens
    saveStoredTokens(nextTokens)
    lifecycleCallbacks.onTokensRotated?.(nextTokens)
    return nextTokens
  } catch (error) {
    let apiErr: ApiError
    if (error instanceof ApiError) {
      apiErr = error
    } else if (axios.isAxiosError<ApiEnvelope<unknown>>(error)) {
      const payload = error.response?.data
      apiErr = new ApiError(
        payload?.code ?? BIZ_CODE.REFRESH_TOKEN_INVALID,
        payload?.message ?? error.message ?? '刷新凭证失败',
        payload?.request_id ?? '',
        error.response?.status ?? 0,
      )
    } else {
      apiErr = new ApiError(BIZ_CODE.REFRESH_TOKEN_INVALID, '刷新凭证失败', '', 0)
    }

    // Concurrent rotation race (11008) or response-lost race: inspect latest persisted tokens first
    if (apiErr.code === BIZ_CODE.REFRESH_CONFLICT || apiErr.code === BIZ_CODE.REFRESH_TOKEN_INVALID) {
      for (let attempt = 0; attempt < 3; attempt += 1) {
        const persisted = getStoredTokens()
        if (
          persisted &&
          persisted.refresh_token &&
          persisted.refresh_token !== sentRefreshToken &&
          persisted.access_token
        ) {
          lifecycleCallbacks.onTokensRotated?.(persisted)
          return persisted
        }
        if (apiErr.code !== BIZ_CODE.REFRESH_CONFLICT) {
          break
        }
        await sleep(120)
      }
    }

    if (
      apiErr.code === BIZ_CODE.REFRESH_TOKEN_INVALID ||
      apiErr.code === BIZ_CODE.REFRESH_CONFLICT ||
      apiErr.code === BIZ_CODE.ACCOUNT_DISABLED ||
      apiErr.code === BIZ_CODE.SESSION_REVOKED
    ) {
      clearStoredTokens()
      lifecycleCallbacks.onSessionExpired?.(apiErr.code, apiErr.message)
    }
    throw apiErr
  } finally {
    releaseCrossTabLock()
  }
}

export function refreshTokensSingleFlight(failedAccessToken?: string): Promise<Tokens> {
  if (activeRefreshPromise) {
    return activeRefreshPromise
  }

  isRefreshing.value = true
  activeRefreshPromise = performRefreshRequest(failedAccessToken).finally(() => {
    activeRefreshPromise = null
    isRefreshing.value = false
  })

  return activeRefreshPromise
}

export function useAuthRefresh() {
  return {
    isRefreshing,
    refreshTokens: refreshTokensSingleFlight,
  }
}
