import assert from 'node:assert/strict'
import fs from 'node:fs/promises'
import path from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(__dirname, '../../..')
const assetsDir = path.join(repoRoot, 'docs/stage9/assets/reconnect')

async function loadPlaywright() {
  try {
    return await import('playwright')
  } catch {
    const fallbackPath = process.env.PW_PATH || '/tmp/pw-stage9/node_modules/playwright/index.mjs'
    const require = createRequire(import.meta.url)
    try {
      return require(fallbackPath.replace(/index\.mjs$/, 'index.js'))
    } catch {
      return await import(fallbackPath)
    }
  }
}

const BASE_URL = (process.env.IM_FRONTEND_ORIGIN || 'http://127.0.0.1:5176').replace(/\/+$/, '')
const BROWSER_ARG = (process.env.PW_BROWSERS || 'chromium,webkit').split(',').map(s => s.trim()).filter(Boolean)
const RUN_TAG = `${Date.now().toString(36)}`
const PASSWORD = 'Password@123'

function log(msg) {
  console.log(`[e2e-reconnect] ${msg}`)
}

async function waitForWsStatus(page, label, timeoutMs = 15000) {
  const pill = page.locator('.conv-list__ws-pill')
  await pill.waitFor({ state: 'visible', timeout: timeoutMs })
  await page.waitForFunction(
    (expected) => {
      const el = document.querySelector('.conv-list__ws-pill')
      return Boolean(el && el.textContent && el.textContent.includes(expected))
    },
    label,
    { timeout: timeoutMs },
  )
}

async function registerUser(page, username, nickname) {
  await page.goto(`${BASE_URL}/register`, { waitUntil: 'networkidle' })
  await page.getByPlaceholder('1–64 个字符，用于登录').fill(username)
  await page.getByPlaceholder('1–64 个字符，用于对外展示').fill(nickname)
  await page.getByPlaceholder('8–72 字节').fill(PASSWORD)
  await page.getByPlaceholder('请再次输入密码').fill(PASSWORD)
  await page.getByRole('button', { name: '注册' }).click()
  await page.waitForURL(/\/login/, { timeout: 15000 })
}

async function loginUser(page, username) {
  await page.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle' })
  await page.getByPlaceholder('请输入用户名').fill(username)
  await page.getByPlaceholder('请输入密码').fill(PASSWORD)
  await page.getByRole('button', { name: '登录' }).click()
  await page.waitForURL(/\/chat/, { timeout: 15000 })
  await waitForWsStatus(page, '实时在线', 15000)
}

async function addFriendAndOpenChat(alicePage, bobPage, bobUsername, aliceNickname) {
  // Alice navigates to contacts and searches for Bob
  await alicePage.getByTitle('联系人').click()
  await alicePage.waitForURL(/\/contacts/, { timeout: 10000 })
  await alicePage.getByRole('button', { name: '添加好友' }).click()
  await alicePage.getByPlaceholder('输入用户名或昵称搜索').fill(bobUsername)
  await alicePage.getByRole('button', { name: '搜索' }).click()

  const addBtn = alicePage.locator('.user-card').filter({ hasText: `@${bobUsername}` }).getByRole('button', { name: '添加好友' })
  await addBtn.waitFor({ state: 'visible', timeout: 10000 })
  await addBtn.click()
  await alicePage.getByRole('button', { name: '发送申请' }).click()
  await alicePage.locator('.el-dialog').waitFor({ state: 'hidden', timeout: 10000 })

  // Bob navigates to contacts and accepts Alice's request
  await bobPage.getByTitle('联系人').click()
  await bobPage.waitForURL(/\/contacts/, { timeout: 10000 })
  await bobPage.getByRole('button', { name: /新的朋友/ }).click()
  const reqCard = bobPage.locator('.request-card').filter({ hasText: aliceNickname }).first()
  await reqCard.waitFor({ state: 'visible', timeout: 10000 })
  await reqCard.getByRole('button', { name: '同意' }).click()
  const sendMsgBtn = reqCard.getByRole('button', { name: '发消息' })
  await sendMsgBtn.waitFor({ state: 'visible', timeout: 10000 })
  await sendMsgBtn.click()
  await bobPage.waitForURL(/\/chat\/\d+/, { timeout: 10000 })

  const convUrl = bobPage.url()
  const convIdMatch = convUrl.match(/\/chat\/(\d+)/)
  assert.ok(convIdMatch, `Expected conversation ID in URL, got ${convUrl}`)
  const convId = convIdMatch[1]

  // Alice returns to chat and opens the conversation
  await alicePage.getByTitle('消息').click()
  await alicePage.waitForURL(/\/chat/, { timeout: 10000 })
  const aliceConvItem = alicePage.locator('.conv-item').filter({ hasText: bobUsername }).or(
    alicePage.locator('.conv-item').first(),
  )
  await aliceConvItem.first().waitFor({ state: 'visible', timeout: 10000 })
  await aliceConvItem.first().click()
  await alicePage.waitForURL(new RegExp(`/chat/${convId}`), { timeout: 10000 })

  await waitForWsStatus(alicePage, '实时在线', 15000)
  await waitForWsStatus(bobPage, '实时在线', 15000)
  return convId
}

async function sendTextMessage(page, text) {
  const textarea = page.locator('.composer__textarea')
  await textarea.waitFor({ state: 'visible', timeout: 10000 })
  await textarea.fill(text)
  await page.getByRole('button', { name: '发送', exact: true }).click()
}

async function getMessageTexts(page) {
  return await page.evaluate(() => {
    const bubbles = Array.from(document.querySelectorAll('.msg-bubble__text'))
    return bubbles.map(el => (el.textContent || '').trim())
  })
}

async function countMessageOccurrences(page, text) {
  return await page.evaluate((target) => {
    const bubbles = Array.from(document.querySelectorAll('.msg-bubble__text'))
    return bubbles.filter(el => (el.textContent || '').trim() === target).length
  }, text)
}

async function waitForMessagePresent(page, text, timeoutMs = 15000) {
  await page.waitForFunction(
    (target) => {
      const bubbles = Array.from(document.querySelectorAll('.msg-bubble__text'))
      return bubbles.some(el => (el.textContent || '').trim() === target)
    },
    text,
    { timeout: timeoutMs },
  )
}

async function waitForBubbleStatus(page, text, expectedStatuses, timeoutMs = 15000) {
  await page.waitForFunction(
    ({ target, statuses }) => {
      const rows = Array.from(document.querySelectorAll('.msg-row'))
      for (const row of rows) {
        const body = row.querySelector('.msg-bubble__text')?.textContent?.trim()
        if (body !== target) continue
        const statusText = row.querySelector('.msg-row__status')?.textContent?.trim() || ''
        if (statuses.some(s => statusText.includes(s))) return true
      }
      return false
    },
    { target: text, statuses: expectedStatuses },
    { timeout: timeoutMs },
  )
}

function attachWsRecorder(page) {
  const sentFrames = []
  const receivedFrames = []
  page.on('websocket', (ws) => {
    ws.on('framesent', (event) => {
      if (typeof event.payload === 'string') {
        try {
          sentFrames.push(JSON.parse(event.payload))
        } catch {
          // ignore non-json
        }
      }
    })
    ws.on('framereceived', (event) => {
      if (typeof event.payload === 'string') {
        try {
          receivedFrames.push(JSON.parse(event.payload))
        } catch {
          // ignore non-json
        }
      }
    })
  })
  return { sentFrames, receivedFrames }
}

async function runFullSuiteOnChromium(pw) {
  log('=== Running full reconnect/offline-sync/reload-persistence/multi-device suite on Chromium ===')
  const browser = await pw.chromium.launch({ headless: true })
  const aliceCtx = await browser.newContext()
  const bobCtx = await browser.newContext()
  const aliceDev2Ctx = await browser.newContext()

  const alicePage = await aliceCtx.newPage()
  const bobPage = await bobCtx.newPage()
  const aliceDev2Page = await aliceDev2Ctx.newPage()

  const aliceWs = attachWsRecorder(alicePage)

  const aliceUser = `rec_alice_${RUN_TAG}`
  const bobUser = `rec_bob_${RUN_TAG}`
  const aliceNick = `Alice_${RUN_TAG}`
  const bobNick = `Bob_${RUN_TAG}`

  try {
    // Setup accounts & friendship
    await registerUser(alicePage, aliceUser, aliceNick)
    await registerUser(bobPage, bobUser, bobNick)
    await loginUser(alicePage, aliceUser)
    await loginUser(bobPage, bobUser)
    const convId = await addFriendAndOpenChat(alicePage, bobPage, bobUser, aliceNick)
    log(`Setup complete: convId=${convId}, alice=${aliceUser}, bob=${bobUser}`)

    // Scenario 1: Baseline online text message
    const baselineText = `baseline-online-${RUN_TAG}`
    await sendTextMessage(alicePage, baselineText)
    await waitForMessagePresent(bobPage, baselineText)
    await waitForBubbleStatus(alicePage, baselineText, ['已送达', '已读'])
    log('Scenario 1 PASS: baseline online send/receive and delivery status confirmed')

    // Scenario 2 & 3: Bob offline -> Alice sends 4 messages + recalls 1 -> Bob reconnects -> zero loss/dup, strict order
    await bobCtx.setOffline(true)
    await waitForWsStatus(bobPage, '连接中', 10000)

    const offlineMsgs = [
      `offline-1-${RUN_TAG}`,
      `offline-2-${RUN_TAG}`,
      `offline-3-${RUN_TAG}`,
      `offline-4-${RUN_TAG}`,
    ]
    for (const msg of offlineMsgs) {
      await sendTextMessage(alicePage, msg)
      await waitForBubbleStatus(alicePage, msg, ['已发送', '已送达', '已读'])
    }

    // Also send & recall a message while Bob is offline
    const recallTarget = `offline-recall-target-${RUN_TAG}`
    await sendTextMessage(alicePage, recallTarget)
    await waitForBubbleStatus(alicePage, recallTarget, ['已发送', '已送达', '已读'])
    const targetRow = alicePage.locator('.msg-row').filter({ hasText: recallTarget })
    await targetRow.getByRole('button', { name: '撤回' }).click()
    await alicePage.locator('.el-popconfirm__action').getByRole('button', { name: '撤回' }).click()
    await alicePage.locator('.msg-notice-pill').filter({ hasText: '你撤回了一条消息' }).waitFor({ state: 'visible', timeout: 10000 })

    // Give Streams a brief moment to settle before Bob reconnects
    await alicePage.waitForTimeout(300)

    // Bring Bob back online
    await bobCtx.setOffline(false)
    await waitForWsStatus(bobPage, '实时在线', 15000)
    for (const msg of offlineMsgs) {
      await waitForMessagePresent(bobPage, msg, 15000)
      assert.equal(await countMessageOccurrences(bobPage, msg), 1, `Duplicate found for ${msg}`)
    }

    // Verify recall synced while offline
    assert.equal(await countMessageOccurrences(bobPage, recallTarget), 0, 'Recalled message text should not appear as normal bubble')
    await bobPage.locator('.msg-notice-pill').filter({ hasText: '撤回了一条消息' }).waitFor({ state: 'visible', timeout: 10000 })

    // Verify strict ascending order
    const bobTexts = await getMessageTexts(bobPage)
    const expectedOrder = [baselineText, ...offlineMsgs]
    const indices = expectedOrder.map(t => bobTexts.indexOf(t))
    for (let i = 0; i < indices.length; i += 1) {
      assert.ok(indices[i] >= 0, `Missing message ${expectedOrder[i]}`)
      if (i > 0) {
        assert.ok(indices[i] > indices[i - 1], `Out of order messages: ${JSON.stringify(bobTexts)}`)
      }
    }
    await waitForBubbleStatus(alicePage, offlineMsgs[3], ['已读'])
    log('Scenario 2 & 3 PASS: offline messages and recall synced with zero loss, zero dup, strict order')

    await fs.mkdir(assetsDir, { recursive: true })
    await bobPage.screenshot({ path: path.join(assetsDir, '01-offline-sync-chromium.png') })

    // Scenario 4: Sender offline unconfirmed message -> reconnect retries with original client_msg_id -> zero duplicate bubbles
    await aliceCtx.setOffline(true)
    await waitForWsStatus(alicePage, '连接中', 10000)
    await alicePage.locator('.composer__offline-hint').waitFor({ state: 'visible', timeout: 5000 })

    const retryText = `sender-offline-retry-${RUN_TAG}`
    const sentCountBefore = aliceWs.sentFrames.length
    await sendTextMessage(alicePage, retryText)
    await waitForMessagePresent(alicePage, retryText, 5000)
    await waitForBubbleStatus(alicePage, retryText, ['发送中'], 5000)

    await aliceCtx.setOffline(false)
    await waitForWsStatus(alicePage, '实时在线', 15000)
    await waitForBubbleStatus(alicePage, retryText, ['已发送', '已送达', '已读'], 15000)
    await waitForMessagePresent(bobPage, retryText, 15000)

    assert.equal(await countMessageOccurrences(alicePage, retryText), 1, 'Sender has duplicate bubble after reconnect retry')
    assert.equal(await countMessageOccurrences(bobPage, retryText), 1, 'Receiver has duplicate bubble after sender reconnect retry')

    const retrySendFrames = aliceWs.sentFrames
      .slice(sentCountBefore)
      .filter(f => f.type === 'msg.send' && f.data?.content === retryText)
    assert.ok(retrySendFrames.length >= 1, 'Expected msg.send frame after reconnect')
    const clientIds = new Set(retrySendFrames.map(f => f.data.client_msg_id))
    assert.equal(clientIds.size, 1, `Expected single stable client_msg_id across retries, got ${JSON.stringify([...clientIds])}`)
    log(`Scenario 4 PASS: offline sender retry preserved client_msg_id=${[...clientIds][0]} with zero duplicates`)

    // Scenario 5: Full page reload persistence + incremental sync does not duplicate old messages
    await bobPage.reload({ waitUntil: 'networkidle' })
    assert.ok(bobPage.url().includes(`/chat/${convId}`), `Reload lost route: ${bobPage.url()}`)
    await waitForWsStatus(bobPage, '实时在线', 15000)
    for (const msg of [...expectedOrder, retryText]) {
      await waitForMessagePresent(bobPage, msg, 15000)
      assert.equal(await countMessageOccurrences(bobPage, msg), 1, `Duplicate after reload for ${msg}`)
    }

    // Verify unread badge persists across reload when Bob is away from the conversation
    await bobPage.getByTitle('联系人').click()
    await bobPage.waitForURL(/\/contacts/, { timeout: 10000 })
    const unreadMsg1 = `unread-persist-1-${RUN_TAG}`
    const unreadMsg2 = `unread-persist-2-${RUN_TAG}`
    await sendTextMessage(alicePage, unreadMsg1)
    await sendTextMessage(alicePage, unreadMsg2)
    await waitForBubbleStatus(alicePage, unreadMsg2, ['已发送', '已送达'])

    await bobPage.goto(`${BASE_URL}/chat`, { waitUntil: 'networkidle' })
    await bobPage.reload({ waitUntil: 'networkidle' })
    await waitForWsStatus(bobPage, '实时在线', 15000)
    const bobBadge = bobPage.locator('.conv-item').first().locator('.conv-item__badge')
    await bobBadge.waitFor({ state: 'visible', timeout: 10000 })
    assert.equal((await bobBadge.textContent()).trim(), '2', 'Expected unread badge = 2 persisted after reload')

    await bobPage.locator('.conv-item').first().click()
    await bobPage.waitForURL(new RegExp(`/chat/${convId}`), { timeout: 10000 })
    await waitForMessagePresent(bobPage, unreadMsg1, 15000)
    await waitForMessagePresent(bobPage, unreadMsg2, 15000)
    await bobBadge.waitFor({ state: 'hidden', timeout: 10000 })
    await waitForBubbleStatus(alicePage, unreadMsg2, ['已读'], 15000)

    // Verify new live message after reload does not duplicate old messages
    const postReloadLive = `post-reload-live-${RUN_TAG}`
    await sendTextMessage(alicePage, postReloadLive)
    await waitForMessagePresent(bobPage, postReloadLive, 15000)
    for (const msg of [...expectedOrder, retryText, unreadMsg1, unreadMsg2, postReloadLive]) {
      assert.equal(await countMessageOccurrences(bobPage, msg), 1, `Unexpected count for ${msg}`)
    }
    await bobPage.screenshot({ path: path.join(assetsDir, '02-reload-persistence-chromium.png') })
    log('Scenario 5 PASS: full reload persisted auth, unread badge, read watermarks, and history without duplicates')

    // Scenario 6: Multi-device same account (aliceDev1 online, aliceDev2 goes offline -> reconnects)
    await loginUser(aliceDev2Page, aliceUser)
    await aliceDev2Page.locator('.conv-item').first().click()
    await aliceDev2Page.waitForURL(new RegExp(`/chat/${convId}`), { timeout: 10000 })
    await waitForMessagePresent(aliceDev2Page, postReloadLive, 15000)

    await aliceDev2Ctx.setOffline(true)
    await waitForWsStatus(aliceDev2Page, '连接中', 10000)

    const multiDevSent = `multi-dev-sent-${RUN_TAG}`
    const multiDevReply = `multi-dev-reply-${RUN_TAG}`
    await sendTextMessage(alicePage, multiDevSent)
    await waitForMessagePresent(bobPage, multiDevSent, 15000)
    await sendTextMessage(bobPage, multiDevReply)
    await waitForMessagePresent(alicePage, multiDevReply, 15000)

    await aliceDev2Ctx.setOffline(false)
    await waitForWsStatus(aliceDev2Page, '实时在线', 15000)
    await waitForMessagePresent(aliceDev2Page, multiDevSent, 15000)
    await waitForMessagePresent(aliceDev2Page, multiDevReply, 15000)
    assert.equal(await countMessageOccurrences(aliceDev2Page, multiDevSent), 1)
    assert.equal(await countMessageOccurrences(aliceDev2Page, multiDevReply), 1)
    log('Scenario 6 PASS: multi-device offline/online sync confirmed with zero loss/duplication')
  } finally {
    await aliceCtx.close()
    await bobCtx.close()
    await aliceDev2Ctx.close()
    await browser.close()
  }
}

async function runCoreOfflineSyncOnSecondEngine(pw, engineName) {
  log(`=== Running core offline-receive -> reconnect-sync -> reload test on ${engineName} ===`)
  const browserType = pw[engineName]
  assert.ok(browserType, `Unsupported Playwright browser engine: ${engineName}`)
  const browser = await browserType.launch({ headless: true })
  const aliceCtx = await browser.newContext()
  const bobCtx = await browser.newContext()
  const alicePage = await aliceCtx.newPage()
  const bobPage = await bobCtx.newPage()

  const tag = `${RUN_TAG}_${engineName}`
  const aliceUser = `rec_a_${tag}`
  const bobUser = `rec_b_${tag}`
  const aliceNick = `Alice_${tag}`
  const bobNick = `Bob_${tag}`

  try {
    await registerUser(alicePage, aliceUser, aliceNick)
    await registerUser(bobPage, bobUser, bobNick)
    await loginUser(alicePage, aliceUser)
    await loginUser(bobPage, bobUser)
    const convId = await addFriendAndOpenChat(alicePage, bobPage, bobUser, aliceNick)

    const baseMsg = `engine-base-${tag}`
    await sendTextMessage(alicePage, baseMsg)
    await waitForMessagePresent(bobPage, baseMsg, 15000)

    // Receiver offline
    await bobCtx.setOffline(true)
    await waitForWsStatus(bobPage, '连接中', 10000)

    const off1 = `engine-off1-${tag}`
    const off2 = `engine-off2-${tag}`
    await sendTextMessage(alicePage, off1)
    await waitForBubbleStatus(alicePage, off1, ['已发送', '已送达', '已读'])
    await sendTextMessage(alicePage, off2)
    await waitForBubbleStatus(alicePage, off2, ['已发送', '已送达', '已读'])

    await alicePage.waitForTimeout(300)

    // Receiver reconnects
    await bobCtx.setOffline(false)
    await waitForWsStatus(bobPage, '实时在线', 15000)
    await waitForMessagePresent(bobPage, off1, 15000)
    await waitForMessagePresent(bobPage, off2, 15000)
    assert.equal(await countMessageOccurrences(bobPage, off1), 1)
    assert.equal(await countMessageOccurrences(bobPage, off2), 1)

    // Reload persistence on second browser engine
    await bobPage.reload({ waitUntil: 'networkidle' })
    assert.ok(bobPage.url().includes(`/chat/${convId}`))
    await waitForWsStatus(bobPage, '实时在线', 15000)
    await waitForMessagePresent(bobPage, baseMsg, 15000)
    await waitForMessagePresent(bobPage, off1, 15000)
    await waitForMessagePresent(bobPage, off2, 15000)
    assert.equal(await countMessageOccurrences(bobPage, baseMsg), 1)
    assert.equal(await countMessageOccurrences(bobPage, off1), 1)
    assert.equal(await countMessageOccurrences(bobPage, off2), 1)
    log(`${engineName} core offline-sync & reload persistence PASS`)
  } finally {
    await aliceCtx.close()
    await bobCtx.close()
    await browser.close()
  }
}

async function main() {
  const pw = await loadPlaywright()
  for (const engine of BROWSER_ARG) {
    if (engine === 'chromium') {
      await runFullSuiteOnChromium(pw)
    } else {
      await runCoreOfflineSyncOnSecondEngine(pw, engine)
    }
  }
  log('ALL RECONNECT E2E SCENARIOS PASSED')
}

main().catch((err) => {
  console.error('[e2e-reconnect] FAILED:', err)
  process.exit(1)
})
