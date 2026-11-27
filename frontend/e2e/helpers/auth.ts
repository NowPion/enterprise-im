import { expect, type Page } from '@playwright/test'
import type { TestUserCredentials } from './env'

export async function registerViaUI(page: Page, user: TestUserCredentials): Promise<void> {
  await page.goto('/register')
  await page.getByPlaceholder('1–64 个字符，用于登录').fill(user.username)
  await page.getByPlaceholder('1–64 个字符，用于对外展示').fill(user.nickname)
  await page.getByPlaceholder('8–72 字节').fill(user.password)
  await page.getByPlaceholder('请再次输入密码').fill(user.password)
  await page.getByRole('button', { name: '注册' }).click()
  await page.waitForURL((url) => url.pathname === '/login', { timeout: 15_000 })
}

export async function loginViaUI(
  page: Page,
  creds: { username: string; password: string },
  options: { waitForWS?: boolean } = {},
): Promise<void> {
  await page.goto('/login')
  await page.getByPlaceholder('请输入用户名').fill(creds.username)
  await page.getByPlaceholder('请输入密码').fill(creds.password)
  await page.getByRole('button', { name: '登录' }).click()
  await page.waitForURL((url) => url.pathname !== '/login' && url.pathname !== '/register', {
    timeout: 15_000,
  })
  if (options.waitForWS !== false) {
    await waitForWSConnected(page)
  }
}

export async function waitForWSConnected(page: Page, timeout = 15_000): Promise<void> {
  const avatarBtn = page.locator('button.nav-rail__avatar-btn')
  await expect(avatarBtn).toHaveAttribute('title', /实时连接正常/, { timeout })
}

export async function expectRedirectedToLogin(page: Page, timeout = 15_000): Promise<void> {
  await page.waitForURL((url) => url.pathname === '/login', { timeout })
}
