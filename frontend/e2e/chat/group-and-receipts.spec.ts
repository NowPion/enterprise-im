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

test.describe('Group chat, broadcast, member management, and group receipts', () => {
  test('create group, invite member, rename, broadcast text+image, receipts dialog, role/remove, refresh', async ({
    browser,
  }) => {
    const [alice, bob, carol] = makeUniqueAccounts('grp', 3)
    const contextA = await openCleanContext(browser)
    const contextB = await openCleanContext(browser)
    const contextC = await openCleanContext(browser)
    const pageA = await contextA.newPage()
    const pageB = await contextB.newPage()
    const pageC = await contextC.newPage()

    const errors: string[] = []
    for (const p of [pageA, pageB, pageC]) {
      p.on('pageerror', (e) => errors.push(e.message))
    }

    try {
      await registerAndLoginViaUI(pageA, alice)
      const bobId = await registerAndLoginViaUI(pageB, bob)
      const carolId = await registerAndLoginViaUI(pageC, carol)
      await Promise.all([waitForWsConnected(pageA), waitForWsConnected(pageB), waitForWsConnected(pageC)])

      // Alice becomes friends with Bob and Carol
      await becomeFriendsViaUI(pageA, pageB, alice, bob)
      await becomeFriendsViaUI(pageA, pageC, alice, carol)

      // Alice goes to /groups and creates group with Bob
      await pageA.locator('.nav-rail__item', { hasText: '群聊' }).click()
      await pageA.waitForURL(/\/groups/)
      await pageA.getByRole('button', { name: '新建群聊' }).click()

      const createDialog = pageA.getByRole('dialog', { name: '创建群聊' })
      await expect(createDialog).toBeVisible()
      const initialGroupName = `Stage9测试群_${Date.now().toString(36)}`
      await createDialog.getByPlaceholder(/请输入群名称/).fill(initialGroupName)
      await createDialog.locator('.candidate-row', { hasText: bob.username }).click()
      await createDialog.getByRole('button', { name: '创建群聊' }).click()
      await expect(createDialog).toBeHidden()
      await expect(pageA.locator('.detail-title')).toHaveText(initialGroupName)

      // Alice invites Carol via 邀请成员
      await pageA.locator('.detail-header-actions').getByRole('button', { name: '邀请成员' }).click()
      const inviteDialog = pageA.getByRole('dialog', { name: '邀请成员入群' })
      await expect(inviteDialog).toBeVisible()
      await inviteDialog.locator('.candidate-item', { hasText: carol.username }).click()
      await inviteDialog.getByRole('button', { name: '确认邀请' }).click()
      await expect(inviteDialog).toBeHidden()
      await expect(pageA.locator('.detail-sub')).toContainText('成员 3 /')

      // Alice renames group in 群资料与设置
      await pageA.getByRole('tab', { name: '群资料与设置' }).click()
      const renamedGroupName = `${initialGroupName}-Renamed`
      const groupNameInput = pageA.locator('.profile-panel input[maxlength="100"]')
      await groupNameInput.fill(renamedGroupName)
      await pageA.getByRole('button', { name: '保存群资料' }).click()
      await expect(pageA.locator('.detail-title')).toHaveText(renamedGroupName)

      // Bob and Carol go to /chat (conversation list) - should automatically show the new group without manual refresh
      await pageB.locator('.nav-rail__item', { hasText: '消息' }).click()
      await pageB.waitForURL(/\/chat/)

      await pageC.locator('.nav-rail__item', { hasText: '消息' }).click()
      await pageC.waitForURL(/\/chat/)

      // Bob opens the group chat; Carol stays on the /chat list (so Carol is delivered, not yet read)
      await openConversationByTitle(pageB, renamedGroupName)

      // Alice enters the group chat
      await pageA.locator('.detail-header-actions').getByRole('button', { name: '进入聊天' }).click()
      await pageA.waitForURL(/\/chat\/\d+/)
      await expect(pageA.locator('.chat-header__title')).toContainText(renamedGroupName)

      // Alice sends a group text message and an image
      const groupMsgText = `Group broadcast msg ${Date.now()}`
      await pageA.locator('.composer__textarea').fill(groupMsgText)
      await pageA.getByRole('button', { name: '发送', exact: true }).click()

      const groupMsgRowA = pageA.locator('.msg-row.is-self', { hasText: groupMsgText })
      await expect(groupMsgRowA).toBeVisible()
      // Bob is in the group chat (reads it), Carol is on /chat list (delivered) -> Alice sees 已读 or 已送达
      await expect(groupMsgRowA.locator('.msg-row__status')).toContainText(/已送达|已读/, {
        timeout: 15_000,
      })

      // Bob sees the group text message immediately
      await expect(pageB.locator('.msg-row', { hasText: groupMsgText })).toBeVisible({ timeout: 15_000 })

      // Alice sends an image to the group
      await pageA
        .locator('input[type="file"][accept*="image/png"]')
        .setInputFiles(path.join(FIXTURES_DIR, 'sample.png'))
      await expect(pageB.locator('.msg-row:not(.is-self) .msg-image img').first()).toBeVisible({
        timeout: 20_000,
      })

      // Alice opens group receipts dialog on groupMsgRowA
      await groupMsgRowA.getByRole('button', { name: '回执' }).click()
      const receiptsDialog = pageA.getByRole('dialog', { name: '消息回执详情' })
      await expect(receiptsDialog).toBeVisible()
      await expect(receiptsDialog.locator('.receipts-summary')).toContainText('应接收2')
      await expect(receiptsDialog.locator('.receipts-summary')).toContainText('已读1')
      await expect(receiptsDialog.locator('.receipts-summary')).toContainText('未读1')
      await receiptsDialog.locator('.el-dialog__headerbtn').click()
      await expect(receiptsDialog).toBeHidden()

      // Now Carol opens the group chat -> receives both text & image and marks read
      await openConversationByTitle(pageC, renamedGroupName)
      await expect(pageC.locator('.msg-row', { hasText: groupMsgText })).toBeVisible({ timeout: 15_000 })
      await expect(pageC.locator('.msg-row:not(.is-self) .msg-image img').first()).toBeVisible({
        timeout: 20_000,
      })

      // Re-open receipts dialog on Alice -> should show 已读2 未读0
      await groupMsgRowA.getByRole('button', { name: '回执' }).click()
      await expect(receiptsDialog).toBeVisible()
      await expect(receiptsDialog.locator('.receipts-summary')).toContainText('已读2')
      await expect(receiptsDialog.locator('.receipts-summary')).toContainText('未读0')
      await expect(receiptsDialog.locator('.receipts-list__row')).toHaveCount(2)
      if (process.env.IM_SAVE_SCREENSHOTS === '1') {
        await pageA.waitForTimeout(350)
        fs.mkdirSync(ASSETS_DIR, { recursive: true })
        await pageA.screenshot({ path: path.join(ASSETS_DIR, 'group-chat-receipts-dialog.png') })
      }
      await receiptsDialog.locator('.el-dialog__headerbtn').click()

      // Alice goes back to /groups, sets Bob as admin, and removes Carol
      const groupIdMatch = await pageA.locator('.chat-header').innerText()
      void groupIdMatch
      await pageA.locator('.chat-header__actions').getByRole('button', { name: '群资料' }).click()
      await pageA.waitForURL(/\/groups\/\d+/)
      await expect(pageA.locator('.member-table')).toBeVisible()

      const bobRow = pageA.locator('.member-table .el-table__row', { hasText: `ID ${bobId}` })
      await bobRow.getByRole('button', { name: '设为管理员' }).click()
      await pageA.locator('.el-message-box:visible').getByRole('button', { name: '确认' }).click()
      await expect(bobRow).toContainText('管理员')

      const carolRow = pageA.locator('.member-table .el-table__row', { hasText: `ID ${carolId}` })
      await carolRow.getByRole('button', { name: '移出' }).click()
      await pageA.locator('.el-message-box:visible').getByRole('button', { name: '确认移出' }).click()
      await expect(carolRow).toBeHidden()

      // Carol's group chat should become read-only / sending blocked
      const convIdMatch = pageC.url().match(/\/chat\/(\d+)/)
      expect(convIdMatch).toBeTruthy()
      const convId = convIdMatch![1]
      const tokenC = await getAccessTokenFromPage(pageC)
      const sendAfterRemoved = await pageC.request.post(`/api/v1/conversations/${convId}/messages`, {
        headers: { Authorization: `Bearer ${tokenC}` },
        data: {
          client_msg_id: `removed-${Date.now()}`,
          msg_type: 'text',
          content: 'should fail',
          extra: {},
        },
      })
      expect(sendAfterRemoved.ok()).toBeFalsy()
      const errJson = await sendAfterRemoved.json()
      expect([13002, 13003, 15002]).toContain(errJson.code)

      // Alice & Bob return to group chat and reload -> history & renamed title persist
      await pageA.locator('.detail-header-actions').getByRole('button', { name: '进入聊天' }).click()
      await pageA.waitForURL(/\/chat\/\d+/)
      await Promise.all([pageA.reload(), pageB.reload()])
      await expect(pageA.locator('.chat-header__title')).toContainText(renamedGroupName)
      await expect(pageB.locator('.chat-header__title')).toContainText(renamedGroupName)
      await expect(pageA.locator('.msg-row', { hasText: groupMsgText })).toBeVisible()
      await expect(pageB.locator('.msg-row', { hasText: groupMsgText })).toBeVisible()

      expect(errors).toEqual([])
    } finally {
      await contextA.close()
      await contextB.close()
      await contextC.close()
    }
  })
})
