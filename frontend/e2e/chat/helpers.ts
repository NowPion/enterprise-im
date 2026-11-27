import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test'

export interface TestAccount {
  username: string
  nickname: string
  password: string
}

export function makeUniqueAccounts(prefix: string, count: number): TestAccount[] {
  const stamp = `${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 6)}`
  return Array.from({ length: count }, (_, idx) => ({
    username: `${prefix}_u${idx + 1}_${stamp}`,
    nickname: `${prefix}_Nick${idx + 1}_${stamp.slice(-4)}`,
    password: 'Stage9Chat!2026',
  }))
}

export async function openCleanContext(browser: Browser): Promise<BrowserContext> {
  return browser.newContext({
    viewport: { width: 1360, height: 860 },
    acceptDownloads: true,
  })
}

export async function registerAndLoginViaUI(page: Page, account: TestAccount): Promise<string> {
  await page.goto('/register')
  await page.getByPlaceholder('1–64 个字符，用于登录').fill(account.username)
  await page.getByPlaceholder('1–64 个字符，用于对外展示').fill(account.nickname)
  await page.getByPlaceholder('8–72 字节').fill(account.password)
  await page.getByPlaceholder('请再次输入密码').fill(account.password)
  await page.getByRole('button', { name: '注册' }).click()

  await page.waitForURL(/\/login/)
  await page.getByPlaceholder('请输入用户名').fill(account.username)
  await page.getByPlaceholder('请输入密码').fill(account.password)
  await page.getByRole('button', { name: '登录' }).click()

  await page.waitForURL(/\/chat/)
  await page.waitForFunction(() => Boolean(localStorage.getItem('im_auth_tokens')))

  const token = await getAccessTokenFromPage(page)
  const meResp = await page.request.get('/api/v1/me', {
    headers: { Authorization: `Bearer ${token}` },
  })
  expect(meResp.ok()).toBeTruthy()
  const meJson = await meResp.json()
  expect(meJson.code).toBe(0)
  return String(meJson.data.user.id)
}

export async function getAccessTokenFromPage(page: Page): Promise<string> {
  return page.evaluate(() => {
    const raw = localStorage.getItem('im_auth_tokens')
    if (!raw) return ''
    try {
      const parsed = JSON.parse(raw)
      return typeof parsed?.access_token === 'string' ? parsed.access_token : ''
    } catch {
      return ''
    }
  })
}

export async function becomeFriendsViaUI(
  pageSender: Page,
  pageReceiver: Page,
  sender: TestAccount,
  receiver: TestAccount,
): Promise<void> {
  await pageSender.locator('.nav-rail__item', { hasText: '联系人' }).click()
  await pageSender.waitForURL(/\/contacts/)
  await pageSender.locator('.contacts-list-pane__header').getByRole('button', { name: '添加好友' }).click()
  await pageSender.getByPlaceholder('输入用户名或昵称搜索').fill(receiver.username)
  await pageSender.getByRole('button', { name: '搜索' }).click()

  const card = pageSender.locator('.user-card', { hasText: receiver.username })
  await expect(card).toBeVisible()
  await card.getByRole('button', { name: '添加好友' }).click()

  const dialog = pageSender.getByRole('dialog', { name: '发送好友申请' })
  await expect(dialog).toBeVisible()
  await dialog.getByPlaceholder(/你好/).fill(`Hi from ${sender.nickname}`)
  await dialog.getByRole('button', { name: '发送申请' }).click()
  await expect(dialog).toBeHidden()

  await pageReceiver.locator('.nav-rail__item', { hasText: '联系人' }).click()
  await pageReceiver.waitForURL(/\/contacts/)
  await pageReceiver.locator('.contact-entry--special').click()
  await pageReceiver.getByRole('button', { name: '刷新' }).click()

  const reqCard = pageReceiver.locator('.request-card', { hasText: sender.username })
  await expect(reqCard).toBeVisible()
  await reqCard.getByRole('button', { name: '同意' }).click()
  await expect(reqCard.getByRole('button', { name: '发消息' })).toBeVisible()
}

export async function waitForWsConnected(page: Page): Promise<void> {
  await expect(page.locator('.nav-rail__avatar-btn')).toHaveAttribute(
    'title',
    /实时连接正常/,
    { timeout: 20_000 },
  )
}

export async function openConversationByTitle(page: Page, titleSubstring: string): Promise<void> {
  const item = page.locator('.conv-item', { hasText: titleSubstring }).first()
  await expect(item).toBeVisible({ timeout: 20_000 })
  await item.click()
  await expect(page.locator('.chat-header__title')).toContainText(titleSubstring)
}
