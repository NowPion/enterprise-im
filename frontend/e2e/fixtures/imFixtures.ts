import { test as base, type BrowserContext, type Page } from '@playwright/test'
import { getAdminCredentials, getBaseUrl, makeTestUser, type TestUserCredentials } from '../helpers/env'
import { loginViaUI, registerViaUI, waitForWSConnected } from '../helpers/auth'

export interface ManagedUserSession {
  user: TestUserCredentials
  context: BrowserContext
  page: Page
}

interface IMFixtures {
  createUserSession: (opts?: {
    prefix?: string
    register?: boolean
    creds?: TestUserCredentials
    waitForWS?: boolean
  }) => Promise<ManagedUserSession>
  adminSession: {
    context: BrowserContext
    page: Page
  }
}

async function logoutContextBestEffort(page: Page): Promise<void> {
  try {
    const token = await page.evaluate(() => {
      const raw = window.localStorage.getItem('im_auth_tokens')
      return raw ? (JSON.parse(raw) as { access_token?: string }).access_token || '' : ''
    })
    if (token) {
      await page.request.post(`${getBaseUrl()}/api/v1/auth/logout`, {
        headers: { Authorization: `Bearer ${token}` },
        data: { scope: 'current' },
      })
    }
  } catch {
    // Best-effort cleanup if user was already kicked or disabled
  }
}

export const test = base.extend<IMFixtures>({
  createUserSession: async ({ browser }, use) => {
    const sessions: Array<{ context: BrowserContext; page: Page }> = []
    const factory = async (opts?: {
      prefix?: string
      register?: boolean
      creds?: TestUserCredentials
      waitForWS?: boolean
    }): Promise<ManagedUserSession> => {
      const user = opts?.creds ?? makeTestUser(opts?.prefix ?? 'user')
      const context = await browser.newContext({ baseURL: getBaseUrl() })
      const page = await context.newPage()
      sessions.push({ context, page })
      if (opts?.register !== false) {
        await registerViaUI(page, user)
      }
      await loginViaUI(page, user, { waitForWS: opts?.waitForWS ?? true })
      if (opts?.waitForWS !== false) {
        await waitForWSConnected(page)
      }
      return { user, context, page }
    }

    await use(factory)

    await Promise.allSettled(
      sessions.map(async ({ context, page }) => {
        await logoutContextBestEffort(page)
        await context.close()
      }),
    )
  },

  adminSession: async ({ browser }, use) => {
    const context = await browser.newContext({ baseURL: getBaseUrl() })
    await context.addInitScript(() => {
      if (!window.localStorage.getItem('im_device_id')) {
        window.localStorage.setItem('im_device_id', 'web-e2e-admin-console')
      }
    })
    const page = await context.newPage()
    const admin = getAdminCredentials()
    await loginViaUI(page, admin, { waitForWS: true })
    await use({ context, page })
    await logoutContextBestEffort(page)
    await context.close()
  },
})

export { expect } from '@playwright/test'
