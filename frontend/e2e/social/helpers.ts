import { expect, type BrowserContext, type Page, type WebSocket } from '@playwright/test'

export interface StoredTokens {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
  refresh_expires_in: number
  session_id: string
  device_id: string
}

export interface PageDiagnostics {
  consoleErrors: string[]
  pageErrors: string[]
  serverErrors: Array<{ url: string; status: number }>
  stop: () => void
}

export interface WSHangshakeTracker {
  waitForConnected: (timeoutMs?: number) => Promise<void>
  sawPing: () => boolean
  sawPong: () => boolean
  getOpenedUrl: () => string
  stop: () => void
}

// 1x1 transparent PNG (67 bytes) for real multipart avatar upload testing
export const SAMPLE_PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=='

export function uniqueSuffix(tag: string): string {
  const rand = Math.random().toString(36).slice(2, 7)
  return `${tag}_${Date.now()}_${rand}`
}

export function attachDiagnostics(page: Page): PageDiagnostics {
  const consoleErrors: string[] = []
  const pageErrors: string[] = []
  const serverErrors: Array<{ url: string; status: number }> = []

  const onConsole = (msg: { type: () => string; text: () => string }) => {
    if (msg.type() === 'error') {
      const text = msg.text()
      // Ignore expected 401/403/409 browser resource logs when intentionally testing expired/invalid creds
      if (!text.includes('status of 401') && !text.includes('status of 403') && !text.includes('status of 409')) {
        consoleErrors.push(text)
      }
    }
  }
  const onPageError = (err: Error) => {
    pageErrors.push(err.message)
  }
  const onResponse = (res: { url: () => string; status: () => number }) => {
    if (res.status() >= 500) {
      serverErrors.push({ url: res.url(), status: res.status() })
    }
  }

  page.on('console', onConsole)
  page.on('pageerror', onPageError)
  page.on('response', onResponse)

  return {
    consoleErrors,
    pageErrors,
    serverErrors,
    stop: () => {
      page.off('console', onConsole)
      page.off('pageerror', onPageError)
      page.off('response', onResponse)
    },
  }
}

export function trackWSHandshake(page: Page): WSHangshakeTracker {
  let pingSeen = false
  let pongSeen = false
  let openedUrl = ''
  const resolvers: Array<() => void> = []

  const maybeResolve = () => {
    if (pingSeen && pongSeen) {
      while (resolvers.length > 0) {
        resolvers.shift()?.()
      }
    }
  }

  const onWebSocket = (ws: WebSocket) => {
    if (!ws.url().includes('/ws')) return
    openedUrl = ws.url()
    ws.on('framesent', (payload) => {
      if (typeof payload.payload === 'string' && payload.payload.includes('"type":"ping"')) {
        pingSeen = true
        maybeResolve()
      }
    })
    ws.on('framereceived', (payload) => {
      if (typeof payload.payload === 'string' && payload.payload.includes('"type":"pong"')) {
        pongSeen = true
        maybeResolve()
      }
    })
  }

  page.on('websocket', onWebSocket)

  return {
    waitForConnected: (timeoutMs = 10_000) => {
      if (pingSeen && pongSeen) return Promise.resolve()
      return new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          reject(new Error(`WS ping/pong handshake timeout (ping=${pingSeen}, pong=${pongSeen}, url=${openedUrl})`))
        }, timeoutMs)
        resolvers.push(() => {
          clearTimeout(timer)
          resolve()
        })
      })
    },
    sawPing: () => pingSeen,
    sawPong: () => pongSeen,
    getOpenedUrl: () => openedUrl,
    stop: () => {
      page.off('websocket', onWebSocket)
    },
  }
}

export async function getStoredTokens(page: Page): Promise<StoredTokens | null> {
  return page.evaluate(() => {
    const raw = window.localStorage.getItem('im_auth_tokens')
    return raw ? JSON.parse(raw) : null
  })
}

export async function getDeviceId(page: Page): Promise<string | null> {
  return page.evaluate(() => window.localStorage.getItem('im_device_id'))
}

export async function registerUserViaUI(
  page: Page,
  user: { username: string; nickname: string; password: string },
): Promise<void> {
  await page.goto('/register')
  await page.getByPlaceholder('1–64 个字符，用于登录').fill(user.username)
  await page.getByPlaceholder('1–64 个字符，用于对外展示').fill(user.nickname)
  await page.getByPlaceholder('8–72 字节').fill(user.password)
  await page.getByPlaceholder('请再次输入密码').fill(user.password)

  const registerRespPromise = page.waitForResponse(
    (res) => res.url().includes('/api/v1/auth/register') && res.request().method() === 'POST',
  )
  await page.getByRole('button', { name: '注册' }).click()
  const resp = await registerRespPromise
  expect(resp.status()).toBe(201)
  const body = await resp.json()
  expect(body.code).toBe(0)
  expect(body.data.user.username).toBe(user.username)
  expect(typeof body.data.user.id).toBe('string')
  expect(BigInt(body.data.user.id) > 0n).toBe(true)

  await expect(page).toHaveURL(/\/login/)
}

export async function loginViaUI(
  page: Page,
  creds: { username: string; password: string },
): Promise<{ tokens: StoredTokens; deviceId: string }> {
  const wsTracker = trackWSHandshake(page)
  try {
    await page.goto('/login')
    await page.getByPlaceholder('请输入用户名').fill(creds.username)
    await page.getByPlaceholder('请输入密码').fill(creds.password)

    const loginReqPromise = page.waitForRequest(
      (req) => req.url().includes('/api/v1/auth/login') && req.method() === 'POST',
    )
    const loginRespPromise = page.waitForResponse(
      (res) => res.url().includes('/api/v1/auth/login') && res.request().method() === 'POST',
    )

    await page.getByRole('button', { name: '登录' }).click()

    const loginReq = await loginReqPromise
    const reqJson = loginReq.postDataJSON() as {
      username: string
      device_id: string
      device_type: string
    }
    expect(reqJson.username).toBe(creds.username)
    expect(reqJson.device_type).toBe('web')
    expect(reqJson.device_id).toMatch(/^web-/)

    const loginResp = await loginRespPromise
    expect(loginResp.status()).toBe(200)
    const loginBody = await loginResp.json()
    expect(loginBody.code).toBe(0)
    expect(loginBody.data.tokens.device_id).toBe(reqJson.device_id)

    await expect(page).toHaveURL(/\/chat/)
    await wsTracker.waitForConnected(12_000)

    const tokens = await getStoredTokens(page)
    const deviceId = await getDeviceId(page)
    expect(tokens).not.toBeNull()
    expect(deviceId).toBe(reqJson.device_id)
    expect(tokens!.device_id).toBe(deviceId)
    return { tokens: tokens!, deviceId: deviceId! }
  } finally {
    wsTracker.stop()
  }
}

export async function logoutCurrentViaUI(page: Page): Promise<void> {
  await page.locator('.nav-rail').getByRole('button', { name: /退出/ }).click()
  await page.getByRole('menuitem', { name: '退出当前设备' }).click()
  const logoutRespPromise = page.waitForResponse(
    (res) => res.url().includes('/api/v1/auth/logout') && res.request().method() === 'POST',
  )
  await page.getByRole('button', { name: '确认退出' }).click()
  const resp = await logoutRespPromise
  expect(resp.status()).toBe(200)
  await expect(page).toHaveURL(/\/login/)
  const tokens = await getStoredTokens(page)
  expect(tokens).toBeNull()
}

export async function openContactsPage(page: Page): Promise<void> {
  await page.locator('.nav-rail').getByRole('button', { name: /联系人/ }).click()
  await expect(page).toHaveURL(/\/contacts/)
}

export async function openProfileDialog(page: Page): Promise<void> {
  await page.locator('.nav-rail button[title="个人资料与安全"]').click()
  await expect(page.getByRole('dialog', { name: '账号设置' })).toBeVisible()
}

export async function checkMeApiStatus(
  context: BrowserContext,
  baseUrl: string,
  accessToken: string,
): Promise<{ status: number; code: number }> {
  const res = await context.request.get(`${baseUrl}/api/v1/me`, {
    headers: { Authorization: `Bearer ${accessToken}` },
  })
  const json = await res.json()
  return { status: res.status(), code: json.code }
}

export async function callRefreshApi(
  context: BrowserContext,
  baseUrl: string,
  refreshToken: string,
  deviceId: string,
): Promise<{ status: number; code: number }> {
  const res = await context.request.post(`${baseUrl}/api/v1/auth/refresh`, {
    data: { refresh_token: refreshToken, device_id: deviceId },
  })
  const json = await res.json()
  return { status: res.status(), code: json.code }
}
