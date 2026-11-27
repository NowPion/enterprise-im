import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from '@playwright/test'
import {
  becomeFriendsViaUI,
  getAccessTokenFromPage,
  makeUniqueAccounts,
  openCleanContext,
  openConversationByTitle,
  registerAndLoginViaUI,
  waitForWsConnected,
} from './helpers'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const FIXTURES_DIR = path.join(__dirname, 'fixtures')
const ASSETS_DIR = path.resolve(__dirname, '../../../docs/stage9/assets/chat')

test.describe('Single chat, media upload, recall, delivery & read receipts', () => {
  test('two real accounts run full single chat + upload + recall + receipts + refresh', async ({
    browser,
    browserName,
  }) => {
    const [alice, bob] = makeUniqueAccounts(`sc_${browserName}`, 2)
    const contextA = await openCleanContext(browser)
    let contextB = await openCleanContext(browser)
    const pageA = await contextA.newPage()
    let pageB = await contextB.newPage()

    const pageErrorsA: string[] = []
    const pageErrorsB: string[] = []
    pageA.on('pageerror', (err) => pageErrorsA.push(err.message))
    pageB.on('pageerror', (err) => pageErrorsB.push(err.message))

    try {
      const aliceId = await registerAndLoginViaUI(pageA, alice)
      const bobId = await registerAndLoginViaUI(pageB, bob)
      expect(aliceId).toBeTruthy()
      expect(bobId).toBeTruthy()

      await waitForWsConnected(pageA)
      await waitForWsConnected(pageB)

      // 1. Become friends via Contacts UI
      await becomeFriendsViaUI(pageA, pageB, alice, bob)

      // Alice opens conversation with Bob
      await pageA.locator('.nav-rail__item', { hasText: '消息' }).click()
      await pageA.waitForURL(/\/chat/)
      await pageA.locator('button[title="刷新会话"]').click()
      await openConversationByTitle(pageA, bob.nickname)

      // Close Bob's context briefly to observe stage 1 ('已发送' / sent) before Bob comes online
      await contextB.close()
      await pageA.waitForTimeout(400)

      const offlineMsgText = `Offline sent-only msg (${browserName})`
      const editorA = pageA.locator('.composer__textarea')
      await editorA.fill(offlineMsgText)
      await pageA.getByRole('button', { name: '发送', exact: true }).click()

      const offlineRowA = pageA.locator('.msg-row.is-self', { hasText: offlineMsgText })
      await expect(offlineRowA).toBeVisible()
      await expect(offlineRowA.locator('.msg-row__status')).toContainText('已发送', {
        timeout: 10_000,
      })

      // Bob logs back in on a fresh context (stays on /chat conversation list -> delivered!)
      contextB = await openCleanContext(browser)
      pageB = await contextB.newPage()
      pageB.on('pageerror', (err) => pageErrorsB.push(err.message))
      await pageB.goto('/login')
      await pageB.getByPlaceholder('请输入用户名').fill(bob.username)
      await pageB.getByPlaceholder('请输入密码').fill(bob.password)
      await pageB.getByRole('button', { name: '登录' }).click()
      await pageB.waitForURL(/\/chat/)
      await waitForWsConnected(pageB)

      // 2. Single chat text messages & seq ordering & delivered->read receipt transition
      const msg1Text = `Hello Bob text-1 (${browserName})`
      const msg2Text = `Hello Bob text-2 (${browserName})`

      await editorA.fill(msg1Text)
      await pageA.getByRole('button', { name: '发送', exact: true }).click()

      const msg1RowA = pageA.locator('.msg-row.is-self', { hasText: msg1Text })
      await expect(msg1RowA).toBeVisible()
      // Bob is online on /chat, so Bob's WSClient sends msg.delivered_ack -> Alice sees 已送达
      await expect(msg1RowA.locator('.msg-row__status')).toContainText(/已送达|已读/, {
        timeout: 15_000,
      })

      await editorA.fill(msg2Text)
      await editorA.press('Enter')
      const msg2RowA = pageA.locator('.msg-row.is-self', { hasText: msg2Text })
      await expect(msg2RowA).toBeVisible()
      await expect(msg2RowA.locator('.msg-row__status')).toContainText(/已送达|已读/, {
        timeout: 15_000,
      })

      // Bob sees unread badge on Alice's conversation item
      const convItemB = pageB.locator('.conv-item', { hasText: alice.nickname }).first()
      await expect(convItemB).toBeVisible({ timeout: 15_000 })
      await expect(convItemB.locator('.conv-item__badge')).toHaveText(/[3-9]/)

      // Bob opens Alice's conversation -> marks read -> Alice sees 已读
      await openConversationByTitle(pageB, alice.nickname)
      await expect(pageB.locator('.msg-row', { hasText: offlineMsgText })).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: msg1Text })).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: msg2Text })).toBeVisible()

      await expect(offlineRowA.locator('.msg-row__status')).toContainText('已读', { timeout: 15_000 })
      await expect(msg1RowA.locator('.msg-row__status')).toContainText('已读', { timeout: 15_000 })
      await expect(msg2RowA.locator('.msg-row__status')).toContainText('已读', { timeout: 15_000 })

      // Verify order in DOM: offlineMsgText -> msg1Text -> msg2Text
      const textsB = await pageB.locator('.msg-bubble__text').allInnerTexts()
      expect(textsB.indexOf(offlineMsgText)).toBeGreaterThanOrEqual(0)
      expect(textsB.indexOf(msg1Text)).toBeGreaterThan(textsB.indexOf(offlineMsgText))
      expect(textsB.indexOf(msg2Text)).toBeGreaterThan(textsB.indexOf(msg1Text))

      // 3. Upload & send real PNG image, TXT file, WAV voice
      await pageA
        .locator('input[type="file"][accept*="image/png"]')
        .setInputFiles(path.join(FIXTURES_DIR, 'sample.png'))
      const imgBubbleB = pageB.locator('.msg-row:not(.is-self) .msg-image img').first()
      await expect(imgBubbleB).toBeVisible({ timeout: 20_000 })
      const imgSrcB = await imgBubbleB.getAttribute('src')
      expect(imgSrcB).toMatch(/^blob:/)

      // Open image lightbox on Bob and close it
      await imgBubbleB.click()
      const previewDialogB = pageB.getByRole('dialog', { name: 'sample.png' })
      await expect(previewDialogB).toBeVisible()
      await expect(previewDialogB.locator('img')).toHaveAttribute('src', /^blob:/)
      await previewDialogB.getByRole('button', { name: '关闭', exact: true }).click()
      await expect(previewDialogB).toBeHidden()

      // Send real TXT file
      await pageA
        .locator('.composer input[type="file"]:not([accept])')
        .setInputFiles(path.join(FIXTURES_DIR, 'sample-doc.txt'))
      const fileRowB = pageB.locator('.msg-row:not(.is-self)', { hasText: 'sample-doc.txt' })
      await expect(fileRowB).toBeVisible({ timeout: 20_000 })

      // Bob triggers authorized file download and checks content
      const downloadPromise = pageB.waitForEvent('download')
      await fileRowB.locator('button[title="下载文件"]').click()
      const download = await downloadPromise
      expect(download.suggestedFilename()).toBe('sample-doc.txt')
      const dlPath = await download.path()
      expect(dlPath).toBeTruthy()
      const downloadedContent = fs.readFileSync(dlPath!, 'utf8')
      const expectedContent = fs.readFileSync(path.join(FIXTURES_DIR, 'sample-doc.txt'), 'utf8')
      expect(downloadedContent).toBe(expectedContent)

      // Send real WAV voice
      await pageA
        .locator('input[type="file"][accept*="audio/wav"]')
        .setInputFiles(path.join(FIXTURES_DIR, 'sample-voice.wav'))
      const voiceRowB = pageB.locator('.msg-row:not(.is-self) .msg-voice').first()
      await expect(voiceRowB).toBeVisible({ timeout: 20_000 })
      await expect(voiceRowB).toContainText(/1"|0:01|1s/)

      // Verify backend HTTP API contract, seq monotonicity, unauthorized media rejection, receipts
      const convIdMatch = pageA.url().match(/\/chat\/(\d+)/)
      expect(convIdMatch).toBeTruthy()
      const convId = convIdMatch![1]
      const tokenA = await getAccessTokenFromPage(pageA)
      const tokenB = await getAccessTokenFromPage(pageB)

      const msgsResp = await pageA.request.get(`/api/v1/conversations/${convId}/messages?limit=50`, {
        headers: { Authorization: `Bearer ${tokenA}` },
      })
      expect(msgsResp.ok()).toBeTruthy()
      const msgsJson = await msgsResp.json()
      expect(msgsJson.code).toBe(0)
      expect(typeof msgsJson.request_id).toBe('string')
      const items = msgsJson.data.items as Array<{
        msg_id: string
        seq: string
        msg_type: string
        delivery_status?: string
        extra?: { asset_id?: string; url?: string }
      }>
      expect(items.length).toBeGreaterThanOrEqual(7)
      for (let i = 1; i < items.length; i += 1) {
        expect(BigInt(items[i].seq) > BigInt(items[i - 1].seq)).toBeTruthy()
      }

      // Verify asset download without token is rejected (401 / 11001)
      const fileItem = items.find((m) => m.msg_type === 'file')
      expect(fileItem?.extra?.asset_id).toBeTruthy()
      const unauthDl = await pageB.request.get(`/api/v1/uploads/${fileItem!.extra!.asset_id}/download`)
      expect(unauthDl.status()).toBe(401)
      const unauthJson = await unauthDl.json()
      expect(unauthJson.code).toBe(11001)

      // Verify HTTP receipts, read, and delivered endpoints
      const msg1Item = items.find((m) => m.msg_type === 'text')
      expect(msg1Item).toBeTruthy()
      const receiptsResp = await pageA.request.get(
        `/api/v1/conversations/${convId}/messages/${msg1Item!.msg_id}/receipts`,
        { headers: { Authorization: `Bearer ${tokenA}` } },
      )
      expect(receiptsResp.ok()).toBeTruthy()
      const receiptsJson = await receiptsResp.json()
      expect(receiptsJson.code).toBe(0)
      expect(receiptsJson.data.read_count).toBeGreaterThanOrEqual(1)

      const lastSeq = items[items.length - 1].seq
      const httpReadResp = await pageB.request.post(`/api/v1/conversations/${convId}/read`, {
        headers: { Authorization: `Bearer ${tokenB}` },
        data: { last_read_seq: lastSeq },
      })
      expect(httpReadResp.ok()).toBeTruthy()
      const httpReadJson = await httpReadResp.json()
      expect(httpReadJson.code).toBe(0)
      expect(httpReadJson.data.unread_count).toBe(0)

      const badDelivered = await pageB.request.post(`/api/v1/conversations/${convId}/delivered`, {
        headers: { Authorization: `Bearer ${tokenB}` },
        data: { last_delivered_seq: String(BigInt(lastSeq) + 100n) },
      })
      expect(badDelivered.status()).toBe(400)
      const badDeliveredJson = await badDelivered.json()
      expect(badDeliveredJson.code).toBe(13006)

      // 4. Recall within window & unauthorized recall error
      const recallTargetText = `Message to be recalled (${browserName})`
      await editorA.fill(recallTargetText)
      await editorA.press('Enter')
      const recallRowA = pageA.locator('.msg-row.is-self', { hasText: recallTargetText })
      await expect(recallRowA).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: recallTargetText })).toBeVisible()

      await recallRowA.getByRole('button', { name: '撤回' }).click()
      const popconfirm = pageA.locator('.el-popconfirm:visible')
      await expect(popconfirm).toBeVisible()
      await popconfirm.getByRole('button', { name: '撤回' }).click()

      await expect(pageA.locator('.msg-bubble__recalled-text').last()).toHaveText('该消息已撤回')
      await expect(pageA.locator('.msg-notice-pill', { hasText: '你撤回了一条消息' })).toBeVisible()
      await expect(pageB.locator('.msg-bubble__recalled-text').last()).toHaveText('该消息已撤回')

      if (process.env.IM_SAVE_SCREENSHOTS === '1' && browserName === 'chromium') {
        fs.mkdirSync(ASSETS_DIR, { recursive: true })
        await pageA.screenshot({ path: path.join(ASSETS_DIR, 'single-chat-sender.png') })
        await pageB.screenshot({ path: path.join(ASSETS_DIR, 'single-chat-receiver.png') })
      }

      // Unauthorized recall: Bob tries to recall Alice's msg1 -> 403 / 14003
      const unauthRecall = await pageB.request.post(
        `/api/v1/conversations/${convId}/messages/${msg1Item!.msg_id}/recall`,
        { headers: { Authorization: `Bearer ${tokenB}` } },
      )
      expect(unauthRecall.status()).toBe(403)
      const unauthRecallJson = await unauthRecall.json()
      expect(unauthRecallJson.code).toBe(14003)

      // Double recall: Alice tries to recall already-recalled message -> 409 / 14004
      const latestMsgsResp = await pageA.request.get(
        `/api/v1/conversations/${convId}/messages?limit=50`,
        { headers: { Authorization: `Bearer ${tokenA}` } },
      )
      const latestItems = (await latestMsgsResp.json()).data.items as Array<{
        msg_id: string
        status: string
      }>
      const recalledOriginal = latestItems.find((m) => m.status === 'recalled')
      expect(recalledOriginal).toBeTruthy()
      const doubleRecall = await pageA.request.post(
        `/api/v1/conversations/${convId}/messages/${recalledOriginal!.msg_id}/recall`,
        { headers: { Authorization: `Bearer ${tokenA}` } },
      )
      expect(doubleRecall.status()).toBe(409)
      expect((await doubleRecall.json()).code).toBe(14004)

      // 5. Refresh both pages and verify full history persistence without blank screen or errors
      await Promise.all([pageA.reload(), pageB.reload()])
      await expect(pageA.locator('.chat-header__title')).toContainText(bob.nickname)
      await expect(pageB.locator('.chat-header__title')).toContainText(alice.nickname)
      await expect(pageA.locator('.msg-row', { hasText: msg1Text })).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: msg1Text })).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: 'sample-doc.txt' })).toBeVisible()
      await expect(pageB.locator('.msg-bubble__recalled-text').last()).toHaveText('该消息已撤回')

      expect(pageErrorsA).toEqual([])
      expect(pageErrorsB).toEqual([])
    } finally {
      await contextA.close()
      await contextB.close()
    }
  })
})
