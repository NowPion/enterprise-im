import { expect, test } from '@playwright/test'
import {
  attachDiagnostics,
  callRefreshApi,
  checkMeApiStatus,
  getDeviceId,
  getStoredTokens,
  loginViaUI,
  logoutCurrentViaUI,
  openContactsPage,
  openProfileDialog,
  registerUserViaUI,
  SAMPLE_PNG_BASE64,
  trackWSHandshake,
  uniqueSuffix,
} from './helpers'

test.describe('Stage 9 Social Full-Stack E2E (Auth, Multi-Device, Contacts & WS)', () => {
  test('@core 1. Register multiple accounts, duplicate guard, login, WS handshake, logout, re-login', async ({
    browser,
    baseURL,
  }) => {
    const suffix = uniqueSuffix('auth')
    const userA = {
      username: `s9_${suffix}_a`,
      nickname: `User A ${suffix}`,
      password: 'Password@1234',
    }
    const userB = {
      username: `s9_${suffix}_b`,
      nickname: `User B ${suffix}`,
      password: 'Password@5678',
    }

    const ctx = await browser.newContext()
    const page = await ctx.newPage()
    const diag = attachDiagnostics(page)

    try {
      // Register account A and account B via real UI
      await registerUserViaUI(page, userA)
      await registerUserViaUI(page, userB)

      // Duplicate username registration should return 409 / 11007 and show alert
      await page.goto('/register')
      await page.getByPlaceholder('1–64 个字符，用于登录').fill(userA.username)
      await page.getByPlaceholder('1–64 个字符，用于对外展示').fill('Dup User')
      await page.getByPlaceholder('8–72 字节').fill(userA.password)
      await page.getByPlaceholder('请再次输入密码').fill(userA.password)
      const dupRespPromise = page.waitForResponse(
        (res) => res.url().includes('/api/v1/auth/register') && res.request().method() === 'POST',
      )
      await page.getByRole('button', { name: '注册' }).click()
      const dupResp = await dupRespPromise
      expect(dupResp.status()).toBe(409)
      const dupJson = await dupResp.json()
      expect(dupJson.code).toBe(11007)
      await expect(page.getByText('该用户名已被注册，请更换后重试')).toBeVisible()

      // Login as userA and verify WS handshake + ProfileSettingsDialog WS status
      const { tokens: firstTokens, deviceId } = await loginViaUI(page, userA)
      await openProfileDialog(page)
      await expect(page.getByText('实时通道：已连接')).toBeVisible()
      await expect(page.getByText(`@${userA.username}`)).toBeVisible()
      await page.keyboard.press('Escape')

      // Logout current device
      await logoutCurrentViaUI(page)

      // Old access & refresh tokens from logged-out session must be rejected by real backend
      const meAfterLogout = await checkMeApiStatus(ctx, baseURL!, firstTokens.access_token)
      expect(meAfterLogout.status).toBe(401)
      expect([11001, 11002]).toContain(meAfterLogout.code)

      const refreshAfterLogout = await callRefreshApi(ctx, baseURL!, firstTokens.refresh_token, deviceId)
      expect([401, 403]).toContain(refreshAfterLogout.status)
      expect([11003, 11005]).toContain(refreshAfterLogout.code)

      // Re-login with userA succeeds and keeps same localStorage im_device_id
      const { tokens: secondTokens, deviceId: deviceId2 } = await loginViaUI(page, userA)
      expect(deviceId2).toBe(deviceId)
      expect(secondTokens.session_id).not.toBe(firstTokens.session_id)

      expect(diag.consoleErrors).toEqual([])
      expect(diag.pageErrors).toEqual([])
      expect(diag.serverErrors).toEqual([])
    } finally {
      diag.stop()
      await ctx.close()
    }
  })

  test('2. Multi-device concurrent sessions, single-flight refresh on expired token, password change & logout all revocation', async ({
    browser,
    baseURL,
  }) => {
    const suffix = uniqueSuffix('multi')
    const user = {
      username: `s9_${suffix}`,
      nickname: `Multi ${suffix}`,
      password: 'Password@1234',
      newPassword: 'NewPassword@5678',
    }

    const ctxDevice1 = await browser.newContext()
    const ctxDevice2 = await browser.newContext()
    const page1 = await ctxDevice1.newPage()
    const page2 = await ctxDevice2.newPage()
    const diag1 = attachDiagnostics(page1)
    const diag2 = attachDiagnostics(page2)

    try {
      await registerUserViaUI(page1, user)

      // Login on Device 1 and Device 2 simultaneously
      const dev1 = await loginViaUI(page1, user)
      const dev2 = await loginViaUI(page2, user)

      expect(dev1.deviceId).not.toBe(dev2.deviceId)
      expect(dev1.tokens.session_id).not.toBe(dev2.tokens.session_id)

      // Both devices simultaneously hold valid active sessions
      const me1 = await checkMeApiStatus(ctxDevice1, baseURL!, dev1.tokens.access_token)
      const me2 = await checkMeApiStatus(ctxDevice2, baseURL!, dev2.tokens.access_token)
      expect(me1).toEqual({ status: 200, code: 0 })
      expect(me2).toEqual({ status: 200, code: 0 })

      // Single-flight refresh test across TWO tabs sharing the SAME device_id & localStorage (ctxDevice1)
      const page1Tab2 = await ctxDevice1.newPage()
      await page1Tab2.goto('/contacts')
      await expect(page1Tab2).toHaveURL(/\/contacts/)
      await expect(page1Tab2.locator('.requests-pane')).toBeVisible()
      await page1Tab2.waitForLoadState('networkidle')

      // Corrupt access_token in localStorage on Device 1 (simulating expired access_token while refresh_token is valid)
      const expiredLikeAccessToken = `${dev1.tokens.access_token.slice(0, -6)}000000`
      await page1.evaluate((badAccess) => {
        const raw = window.localStorage.getItem('im_auth_tokens')
        if (!raw) throw new Error('missing im_auth_tokens')
        const parsed = JSON.parse(raw)
        parsed.access_token = badAccess
        window.localStorage.setItem('im_auth_tokens', JSON.stringify(parsed))
      }, expiredLikeAccessToken)

      let refreshRequestCount = 0
      const onRequest = (req: { url: () => string; method: () => string }) => {
        if (req.url().includes('/api/v1/auth/refresh') && req.method() === 'POST') {
          refreshRequestCount += 1
        }
      }
      ctxDevice1.on('request', onRequest)

      // Trigger concurrent authenticated API calls across BOTH tabs simultaneously via reload
      // (Each tab calls GET /api/v1/me, GET /api/v1/friends, GET /api/v1/friend-requests, GET /api/v1/conversations)
      await Promise.all([page1.reload(), page1Tab2.reload()])

      await expect(page1).not.toHaveURL(/\/login/)
      await expect(page1Tab2).toHaveURL(/\/contacts/)

      // Single-flight lock must ensure /auth/refresh was called exactly once across concurrent requests & tabs
      expect(refreshRequestCount).toBe(1)
      ctxDevice1.off('request', onRequest)

      const rotatedTokens = await getStoredTokens(page1)
      expect(rotatedTokens).not.toBeNull()
      expect(rotatedTokens!.access_token).not.toBe(expiredLikeAccessToken)
      expect(rotatedTokens!.refresh_token).not.toBe(dev1.tokens.refresh_token)
      expect(rotatedTokens!.session_id).toBe(dev1.tokens.session_id)

      // Old refresh_token from before rotation must now fail on real backend
      const replayOldRefresh = await callRefreshApi(ctxDevice1, baseURL!, dev1.tokens.refresh_token, dev1.deviceId)
      expect(replayOldRefresh.status).toBe(401)
      expect(replayOldRefresh.code).toBe(11003)

      await page1Tab2.close()

      // Device 2 must STILL be valid after Device 1 refreshed!
      const me2AfterDev1Refresh = await checkMeApiStatus(ctxDevice2, baseURL!, dev2.tokens.access_token)
      expect(me2AfterDev1Refresh).toEqual({ status: 200, code: 0 })

      // Logout Device 1 with scope: current -> Device 1 tokens invalid, Device 2 stays valid
      await logoutCurrentViaUI(page1)
      const me2AfterDev1Logout = await checkMeApiStatus(ctxDevice2, baseURL!, dev2.tokens.access_token)
      expect(me2AfterDev1Logout).toEqual({ status: 200, code: 0 })

      // Re-login Device 1, then change password on Device 1 -> revokes ALL sessions (Device 1 & Device 2)
      const dev1Relogin = await loginViaUI(page1, user)
      await openProfileDialog(page1)
      await page1.getByRole('tab', { name: '修改密码' }).click()
      const settingsDialog = page1.getByRole('dialog', { name: '账号设置' })
      await settingsDialog.getByLabel('当前密码').fill(user.password)
      await settingsDialog.getByLabel('新密码', { exact: true }).fill(user.newPassword)
      await settingsDialog.getByLabel('确认新密码').fill(user.newPassword)

      const pwdRespPromise = page1.waitForResponse(
        (res) => res.url().includes('/api/v1/me/password') && res.request().method() === 'PUT',
      )
      await settingsDialog.getByRole('button', { name: '确认修改密码' }).click()
      const pwdResp = await pwdRespPromise
      expect(pwdResp.status()).toBe(200)
      const pwdJson = await pwdResp.json()
      expect(pwdJson.code).toBe(0)
      expect(pwdJson.data.revoked_count).toBeGreaterThanOrEqual(2)

      await expect(page1).toHaveURL(/\/login/)

      // Both Device 1 and Device 2 old tokens must be rejected by real backend
      const dev1OldCheck = await checkMeApiStatus(ctxDevice1, baseURL!, dev1Relogin.tokens.access_token)
      const dev2OldCheck = await checkMeApiStatus(ctxDevice2, baseURL!, dev2.tokens.access_token)
      expect(dev1OldCheck.status).toBe(401)
      expect(dev2OldCheck.status).toBe(401)

      const dev2RefreshAfterPwd = await callRefreshApi(ctxDevice2, baseURL!, dev2.tokens.refresh_token, dev2.deviceId)
      expect([401, 403]).toContain(dev2RefreshAfterPwd.status)

      // Old password must fail login; new password succeeds
      await page1.getByPlaceholder('请输入用户名').fill(user.username)
      await page1.getByPlaceholder('请输入密码').fill(user.password)
      const badLoginPromise = page1.waitForResponse(
        (res) => res.url().includes('/api/v1/auth/login') && res.request().method() === 'POST',
      )
      await page1.getByRole('button', { name: '登录' }).click()
      const badLoginResp = await badLoginPromise
      expect(badLoginResp.status()).toBe(401)
      await expect(page1.getByText('用户名或密码错误')).toBeVisible()

      // Login with new password on both Device 1 and Device 2, then test logout all
      const dev1New = await loginViaUI(page1, { username: user.username, password: user.newPassword })
      const dev2New = await loginViaUI(page2, { username: user.username, password: user.newPassword })

      await page1.locator('.nav-rail').getByRole('button', { name: /退出/ }).click()
      await page1.getByRole('menuitem', { name: '下线全部设备' }).click()
      const logoutAllPromise = page1.waitForResponse(
        (res) => res.url().includes('/api/v1/auth/logout') && res.request().method() === 'POST',
      )
      await page1.getByRole('button', { name: '确认退出' }).click()
      const logoutAllResp = await logoutAllPromise
      expect(logoutAllResp.status()).toBe(200)
      const logoutAllJson = await logoutAllResp.json()
      expect(logoutAllJson.data.scope).toBe('all')
      expect(logoutAllJson.data.revoked_count).toBeGreaterThanOrEqual(2)

      // Verify both devices' tokens are now rejected
      expect((await checkMeApiStatus(ctxDevice1, baseURL!, dev1New.tokens.access_token)).status).toBe(401)
      expect((await checkMeApiStatus(ctxDevice2, baseURL!, dev2New.tokens.access_token)).status).toBe(401)

      expect(diag1.consoleErrors).toEqual([])
      expect(diag1.pageErrors).toEqual([])
      expect(diag1.serverErrors).toEqual([])
      expect(diag2.consoleErrors).toEqual([])
      expect(diag2.pageErrors).toEqual([])
      expect(diag2.serverErrors).toEqual([])
    } finally {
      diag1.stop()
      diag2.stop()
      await ctxDevice1.close()
      await ctxDevice2.close()
    }
  })

  test('@core 3. Search users, friend request cancel/reject/accept, online presence, remark, profile/avatar, reload persistence, delete friend', async ({
    browser,
  }) => {
    const suffix = uniqueSuffix('social')
    const alice = {
      username: `s9_${suffix}_alice`,
      nickname: `Alice ${suffix}`,
      password: 'Password@1234',
    }
    const bob = {
      username: `s9_${suffix}_bob`,
      nickname: `Bob ${suffix}`,
      password: 'Password@1234',
    }
    const charlie = {
      username: `s9_${suffix}_charlie`,
      nickname: `Charlie ${suffix}`,
      password: 'Password@1234',
    }

    const ctxAlice = await browser.newContext()
    const ctxBob = await browser.newContext()
    const ctxCharlie = await browser.newContext()
    const pageAlice = await ctxAlice.newPage()
    const pageBob = await ctxBob.newPage()
    const pageCharlie = await ctxCharlie.newPage()
    const diagAlice = attachDiagnostics(pageAlice)
    const diagBob = attachDiagnostics(pageBob)

    try {
      // Register Alice, Bob, Charlie
      await registerUserViaUI(pageAlice, alice)
      await registerUserViaUI(pageBob, bob)
      await registerUserViaUI(pageCharlie, charlie)

      // Alice and Bob log in concurrently (both online with WS handshakes)
      await loginViaUI(pageAlice, alice)
      await loginViaUI(pageBob, bob)

      // Alice opens Contacts -> Add Friend -> searches Bob by prefix username & exact nickname
      await openContactsPage(pageAlice)
      await pageAlice.getByRole('button', { name: '添加好友' }).click()
      await pageAlice.getByPlaceholder('输入用户名或昵称搜索').fill(bob.username.slice(0, -2))
      await pageAlice.getByRole('button', { name: '搜索' }).click()
      await expect(pageAlice.locator('.user-card', { hasText: `@${bob.username}` })).toBeVisible()
      await expect(
        pageAlice.locator('.user-card', { hasText: `@${bob.username}` }).getByText('在线'),
      ).toBeVisible()

      // Alice sends friend request #1 to Bob and cancels it
      await pageAlice
        .locator('.user-card', { hasText: `@${bob.username}` })
        .getByRole('button', { name: '添加好友' })
        .click()
      await pageAlice.getByPlaceholder('例如：你好，我是产品部的...').fill('第一次申请（待撤销）')
      await pageAlice.getByRole('button', { name: '发送申请' }).click()
      await expect(pageAlice.locator('.el-message', { hasText: '好友申请已发送' }).last()).toBeVisible()

      // Duplicate pending request must be rejected cleanly with friendly toast (12004)
      await pageAlice
        .locator('.user-card', { hasText: `@${bob.username}` })
        .getByRole('button', { name: '添加好友' })
        .click()
      await pageAlice.getByRole('button', { name: '发送申请' }).click()
      await expect(pageAlice.locator('.el-message', { hasText: '已有待处理的好友申请，请勿重复发送' }).last()).toBeVisible()
      await pageAlice.getByRole('button', { name: '取消' }).click()

      // Switch to Requests pane -> Sent -> Cancel request #1
      await pageAlice.getByRole('button', { name: /新的朋友/ }).click()
      await pageAlice.locator('label.el-radio-button', { hasText: '发出的申请' }).click({ force: true })
      const cancelCard = pageAlice.locator('.request-card', { hasText: `@${bob.username}` }).first()
      await expect(cancelCard.getByText('待处理')).toBeVisible()
      await cancelCard.getByRole('button', { name: '撤销申请' }).click()
      await expect(cancelCard.getByText('已撤销')).toBeVisible()

      // Alice sends friend request #2 -> Bob rejects it
      await pageAlice.getByRole('button', { name: '添加好友' }).click()
      await pageAlice
        .locator('.user-card', { hasText: `@${bob.username}` })
        .getByRole('button', { name: '添加好友' })
        .click()
      await pageAlice.getByPlaceholder('例如：你好，我是产品部的...').fill('第二次申请（待拒绝）')
      await pageAlice.getByRole('button', { name: '发送申请' }).click()
      await expect(pageAlice.locator('.el-message', { hasText: '好友申请已发送' }).last()).toBeVisible()

      await openContactsPage(pageBob)
      await pageBob.getByRole('button', { name: '刷新' }).click()
      const bobReceivedRejectCard = pageBob
        .locator('.request-card', { hasText: `@${alice.username}` })
        .filter({ hasText: '待处理' })
        .first()
      await expect(bobReceivedRejectCard).toBeVisible()
      await bobReceivedRejectCard.getByRole('button', { name: '拒绝' }).click()
      await expect(
        pageBob.locator('.request-card', { hasText: '第二次申请（待拒绝）' }).getByText('已拒绝'),
      ).toBeVisible()

      // Alice sends friend request #3 -> Bob accepts it
      await pageAlice
        .locator('.user-card', { hasText: `@${bob.username}` })
        .getByRole('button', { name: '添加好友' })
        .click()
      await pageAlice.getByPlaceholder('例如：你好，我是产品部的...').fill('你好 Bob，加个好友')
      await pageAlice.getByRole('button', { name: '发送申请' }).click()
      await expect(pageAlice.locator('.el-message', { hasText: '好友申请已发送' }).last()).toBeVisible()

      await pageBob.getByRole('button', { name: '刷新' }).click()
      const bobAcceptCard = pageBob
        .locator('.request-card', { hasText: '你好 Bob，加个好友' })
        .first()
      await expect(bobAcceptCard.getByText('待处理')).toBeVisible()
      const acceptRespPromise = pageBob.waitForResponse(
        (res) => res.url().includes('/accept') && res.request().method() === 'POST',
      )
      await bobAcceptCard.getByRole('button', { name: '同意' }).click()
      const acceptResp = await acceptRespPromise
      expect(acceptResp.status()).toBe(200)
      const acceptJson = await acceptResp.json()
      expect(acceptJson.code).toBe(0)
      expect(typeof acceptJson.data.conversation_id).toBe('string')
      expect(BigInt(acceptJson.data.conversation_id) > 0n).toBe(true)

      // Both Bob and Alice see each other in My Friends list
      await expect(pageBob.locator('.contacts-list-pane').getByText(bob.nickname).first()).not.toBeVisible()
      await expect(pageBob.locator('.contacts-list-pane').getByText(alice.nickname)).toBeVisible()

      // Alice refreshes friend list (or clicks into friends / reloads) and sees Bob
      await pageAlice.reload()
      await expect(pageAlice).toHaveURL(/\/contacts/)
      const aliceFriendEntry = pageAlice.locator('.contacts-list-pane .contact-entry', {
        hasText: bob.nickname,
      })
      await expect(aliceFriendEntry).toBeVisible()
      await aliceFriendEntry.click()

      // Friend detail shows online status, conversation_id, and allows independent remark editing
      await expect(pageAlice.locator('.friend-detail').getByText('在线', { exact: true })).toBeVisible()
      await pageAlice.getByRole('button', { name: '修改备注' }).click()
      const bobRemark = `Bob备忘_${suffix}`
      await pageAlice.getByPlaceholder('输入备注，留空则清除').fill(bobRemark)
      await pageAlice.getByRole('button', { name: '保存' }).click()
      await expect(pageAlice.locator('.friend-detail__name', { hasText: bobRemark })).toBeVisible()
      await expect(pageAlice.locator('.contacts-list-pane').getByText(bobRemark)).toBeVisible()
      if (process.env.SAVE_STAGE9_SHOTS === '1') {
        await pageAlice.screenshot({
          path: '../../../docs/stage9/assets/social/contacts-friend-detail.png',
        })
      }

      // Verify Bob's side does NOT show Alice's remark (remarks are independent)
      await pageBob.locator('.contacts-list-pane .contact-entry', { hasText: alice.nickname }).click()
      await expect(pageBob.locator('.friend-detail').getByText('未设置备注')).toBeVisible()

      // Alice updates profile nickname, signature, and uploads real PNG avatar
      const updatedAliceNick = `Alice新版_${suffix}`
      const updatedAliceSig = `测试签名_${suffix}`
      await openProfileDialog(pageAlice)
      const dialog = pageAlice.getByRole('dialog', { name: '账号设置' })
      await dialog.locator('input[type="file"]').setInputFiles({
        name: 'avatar.png',
        mimeType: 'image/png',
        buffer: Buffer.from(SAMPLE_PNG_BASE64, 'base64'),
      })
      await expect(pageAlice.getByText('头像已更新')).toBeVisible()

      await dialog.getByLabel('昵称').fill(updatedAliceNick)
      await dialog.getByLabel('个性签名').fill(updatedAliceSig)
      await dialog.getByRole('button', { name: '保存资料' }).click()
      await expect(pageAlice.getByText('个人资料已保存')).toBeVisible()
      await pageAlice.keyboard.press('Escape')

      // Reload Alice's page -> verify im_auth_tokens, im_device_id, profile, avatar, friends & remark persist, and WS reconnects
      const tokensBeforeReload = await getStoredTokens(pageAlice)
      const deviceBeforeReload = await getDeviceId(pageAlice)
      const wsTrackerReload = trackWSHandshake(pageAlice)
      await pageAlice.reload()
      await expect(pageAlice).toHaveURL(/\/contacts/)
      await wsTrackerReload.waitForConnected(12_000)
      wsTrackerReload.stop()

      const tokensAfterReload = await getStoredTokens(pageAlice)
      const deviceAfterReload = await getDeviceId(pageAlice)
      expect(tokensAfterReload?.session_id).toBe(tokensBeforeReload?.session_id)
      expect(deviceAfterReload).toBe(deviceBeforeReload)

      await expect(pageAlice.locator('.contacts-list-pane').getByText(bobRemark)).toBeVisible()
      await openProfileDialog(pageAlice)
      await expect(pageAlice.getByRole('dialog', { name: '账号设置' }).getByLabel('昵称')).toHaveValue(
        updatedAliceNick,
      )
      await expect(pageAlice.getByRole('dialog', { name: '账号设置' }).getByLabel('个性签名')).toHaveValue(
        updatedAliceSig,
      )
      await expect(pageAlice.getByRole('dialog', { name: '账号设置' }).locator('img')).toBeVisible()
      if (process.env.SAVE_STAGE9_SHOTS === '1') {
        await pageAlice.screenshot({
          path: '../../../docs/stage9/assets/social/profile-settings.png',
        })
      }
      await pageAlice.keyboard.press('Escape')

      // Alice adds Charlie and then deletes Charlie to verify delete-friend flow
      await loginViaUI(pageCharlie, charlie)
      await pageAlice.getByRole('button', { name: '添加好友' }).click()
      await pageAlice.getByPlaceholder('输入用户名或昵称搜索').fill(charlie.username)
      await pageAlice.getByRole('button', { name: '搜索' }).click()
      await pageAlice
        .locator('.user-card', { hasText: `@${charlie.username}` })
        .getByRole('button', { name: '添加好友' })
        .click()
      await pageAlice.getByRole('button', { name: '发送申请' }).click()
      await expect(pageAlice.locator('.el-message', { hasText: '好友申请已发送' }).last()).toBeVisible()

      await openContactsPage(pageCharlie)
      await pageCharlie.getByRole('button', { name: '刷新' }).click()
      await pageCharlie
        .locator('.request-card', { hasText: `@${alice.username}` })
        .getByRole('button', { name: '同意' })
        .click()
      await expect(pageCharlie.locator('.contacts-list-pane').getByText(updatedAliceNick)).toBeVisible()

      // Alice reloads to pull Charlie into list, then deletes Charlie
      await pageAlice.reload()
      const charlieEntry = pageAlice.locator('.contacts-list-pane .contact-entry', {
        hasText: charlie.nickname,
      })
      await expect(charlieEntry).toBeVisible()
      await charlieEntry.click()
      await pageAlice.getByRole('button', { name: '删除好友' }).click()
      await pageAlice.getByRole('button', { name: '确认删除' }).click()
      await expect(pageAlice.getByText('已删除好友')).toBeVisible()
      await expect(charlieEntry).not.toBeVisible()

      // Reload Alice and verify Bob remains while Charlie is gone
      await pageAlice.reload()
      await expect(pageAlice.locator('.contacts-list-pane').getByText(bobRemark)).toBeVisible()
      await expect(
        pageAlice.locator('.contacts-list-pane .contact-entry', { hasText: charlie.nickname }),
      ).not.toBeVisible()

      expect(diagAlice.consoleErrors).toEqual([])
      expect(diagAlice.pageErrors).toEqual([])
      expect(diagAlice.serverErrors).toEqual([])
      expect(diagBob.consoleErrors).toEqual([])
      expect(diagBob.pageErrors).toEqual([])
      expect(diagBob.serverErrors).toEqual([])
    } finally {
      diagAlice.stop()
      diagBob.stop()
      await ctxAlice.close()
      await ctxBob.close()
      await ctxCharlie.close()
    }
  })
})
