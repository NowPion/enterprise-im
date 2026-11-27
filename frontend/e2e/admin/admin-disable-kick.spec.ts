import path from 'node:path'
import { expect, test } from '../fixtures/imFixtures'
import { getBaseUrl } from '../helpers/env'
import { parseEnvelope } from '../helpers/api'
import { expectRedirectedToLogin, loginViaUI, waitForWSConnected } from '../helpers/auth'

const screenshotDir = path.resolve(process.cwd(), '../docs/stage9/assets/admin')

test.describe('Admin authorization & real-time disable/kick flow', () => {
  test('non-admin user is blocked by router guard and backend admin API (17001)', async ({
    createUserSession,
    request,
  }) => {
    const userSession = await createUserSession({ prefix: 'nonadmin' })
    const accessToken = await userSession.page.evaluate(() => {
      const raw = window.localStorage.getItem('im_auth_tokens')
      return raw ? (JSON.parse(raw) as { access_token: string }).access_token : ''
    })
    expect(accessToken).toBeTruthy()

    await userSession.page.goto('/admin/overview')
    await expect(userSession.page).toHaveURL(/\/chat/)

    const res = await request.get(`${getBaseUrl()}/api/v1/admin/metrics/overview`, {
      headers: { Authorization: `Bearer ${accessToken}` },
    })
    expect(res.status()).toBe(403)
    const env = await parseEnvelope(res)
    expect(env.code).toBe(17001)
  })

  test('online user metrics, disable user kicks live WS and blocks requests, re-enable restores login', async ({
    createUserSession,
    adminSession,
    request,
  }, testInfo) => {
    const captureScreenshots = testInfo.project.name === 'admin'
    const userSession = await createUserSession({ prefix: 'kicktarget' })
    const targetUsername = userSession.user.username

    const capturedToken = await userSession.page.evaluate(() => {
      const raw = window.localStorage.getItem('im_auth_tokens')
      return raw ? (JSON.parse(raw) as { access_token: string }).access_token : ''
    })
    expect(capturedToken).toBeTruthy()

    const adminPage = adminSession.page
    await adminPage.goto('/admin/overview')
    await expect(adminPage.getByRole('heading', { name: '系统运行监控' })).toBeVisible()

    const onlineCard = adminPage.locator('.kpi-card', { hasText: '在线用户数' }).locator('.kpi-value')
    const wsCard = adminPage.locator('.kpi-card', { hasText: 'WebSocket 连接数' }).locator('.kpi-value')

    await expect(onlineCard).not.toHaveText('—')
    await expect.poll(async () => Number(await onlineCard.textContent())).toBeGreaterThanOrEqual(2)
    await expect.poll(async () => Number(await wsCard.textContent())).toBeGreaterThanOrEqual(2)

    const initialOnline = Number(await onlineCard.textContent())
    const initialWS = Number(await wsCard.textContent())

    await expect(adminPage.locator('td', { hasText: 'MySQL' }).first()).toBeVisible()
    await expect(adminPage.locator('td', { hasText: 'Redis' }).first()).toBeVisible()
    await expect(adminPage.locator('.status-ok', { hasText: 'ok' }).first()).toBeVisible()
    await expect(adminPage.locator('td', { hasText: 'im-msg-persist' }).first()).toBeVisible()

    if (captureScreenshots) {
      await adminPage.screenshot({
        path: path.join(screenshotDir, 'admin-overview-live.png'),
        fullPage: true,
      })
    }

    await adminPage.goto('/admin/users')
    await expect(adminPage.getByRole('heading', { name: '用户管理' })).toBeVisible()
    await adminPage.getByPlaceholder('搜索用户名、昵称或 ID').fill(targetUsername)
    await adminPage.getByRole('button', { name: '查询' }).click()

    const userRow = adminPage.locator('.el-table__body-wrapper tr', { hasText: targetUsername }).first()
    await expect(userRow).toBeVisible()
    await expect(userRow).toContainText('正常')
    await expect(userRow).toContainText('在线')

    if (captureScreenshots) {
      await adminPage.screenshot({
        path: path.join(screenshotDir, 'admin-users-online.png'),
        fullPage: true,
      })
    }

    await userRow.getByRole('button', { name: '禁用' }).click()
    const promptDialog = adminPage.locator('.el-message-box')
    await expect(promptDialog).toBeVisible()
    await promptDialog.locator('input').fill('E2E 禁用实时踢下线验证')
    await promptDialog.getByRole('button', { name: '确认禁用' }).click()

    await expect(adminPage.locator('.el-message--success')).toContainText('撤销 1 个会话')
    await expect(adminPage.locator('.el-message--success')).toContainText('关闭 1 条连接')
    await expect(userRow).toContainText('已禁用')
    await expect(userRow).toContainText('离线')

    // Target user's active browser session is kicked off WS in real time and redirected to /login
    await expectRedirectedToLogin(userSession.page, 15_000)

    // Subsequent HTTP API calls with old token fail (11004 account disabled / 11002 blacklisted)
    const meRes = await request.get(`${getBaseUrl()}/api/v1/me`, {
      headers: { Authorization: `Bearer ${capturedToken}` },
    })
    expect([401, 403]).toContain(meRes.status())
    const meEnv = await parseEnvelope(meRes)
    expect([11002, 11004, 11005]).toContain(meEnv.code)

    // Target user cannot log in while disabled
    await userSession.page.getByPlaceholder('请输入用户名').fill(targetUsername)
    await userSession.page.getByPlaceholder('请输入密码').fill(userSession.user.password)
    await userSession.page.getByRole('button', { name: '登录' }).click()
    await expect(userSession.page.locator('.el-alert')).toContainText('该账号已被禁用')

    // Overview online metrics reflect the kicked connection
    await adminPage.goto('/admin/overview')
    await expect
      .poll(async () => {
        await adminPage.getByRole('button', { name: '立即刷新' }).click()
        return Number(await onlineCard.textContent())
      }, { timeout: 10_000 })
      .toBeLessThan(initialOnline)
    expect(Number(await wsCard.textContent())).toBeLessThan(initialWS)

    // Re-enable user and verify login works again
    await adminPage.goto('/admin/users')
    await adminPage.getByPlaceholder('搜索用户名、昵称或 ID').fill(targetUsername)
    await adminPage.getByRole('button', { name: '查询' }).click()
    const disabledRow = adminPage.locator('.el-table__body-wrapper tr', { hasText: targetUsername }).first()
    await expect(disabledRow).toContainText('已禁用')
    await disabledRow.getByRole('button', { name: '启用' }).click()

    const enableDialog = adminPage.locator('.el-message-box')
    await expect(enableDialog).toBeVisible()
    await enableDialog.locator('input').fill('E2E 恢复启用验证')
    await enableDialog.getByRole('button', { name: '确认启用' }).click()
    await expect(disabledRow).toContainText('正常')

    await loginViaUI(userSession.page, userSession.user, { waitForWS: true })
    await waitForWSConnected(userSession.page)
  })
})
