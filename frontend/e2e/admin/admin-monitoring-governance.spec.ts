import crypto from 'node:crypto'
import { expect, test } from '../fixtures/imFixtures'
import { assertEnvelopeOK, loginUserViaAPI, registerUserViaAPI } from '../helpers/api'
import { expectRedirectedToLogin } from '../helpers/auth'
import { getBaseUrl, makeTestUser } from '../helpers/env'

test.describe('Admin session kick, group governance, and message audit', () => {
  test('admin inspects user detail drawer and kicks active session', async ({
    createUserSession,
    adminSession,
  }) => {
    const userSession = await createUserSession({ prefix: 'kickone' })
    const adminPage = adminSession.page

    await adminPage.goto('/admin/users')
    await adminPage.getByPlaceholder('搜索用户名、昵称或 ID').fill(userSession.user.username)
    await adminPage.getByRole('button', { name: '查询' }).click()

    const row = adminPage.locator('.el-table__body-wrapper tr', { hasText: userSession.user.username }).first()
    await expect(row).toBeVisible()
    await row.getByRole('button', { name: '详情' }).click()

    const drawer = adminPage.locator('.el-drawer', { hasText: '用户详情与设备会话' })
    await expect(drawer).toBeVisible()
    await expect(drawer).toContainText(userSession.user.username)
    await expect(drawer.locator('span.status-ok', { hasText: '活跃' }).first()).toBeVisible()

    await drawer.getByRole('button', { name: '踢出' }).first().click()
    const prompt = adminPage.locator('.el-message-box')
    await expect(prompt).toBeVisible()
    await prompt.locator('input').fill('E2E 单端踢下线验证')
    await prompt.getByRole('button', { name: '确认踢出' }).click()

    await expect(adminPage.locator('.el-message--success')).toContainText('撤销 1 个会话')
    await expect(drawer).toContainText('已撤销')
    await expectRedirectedToLogin(userSession.page, 15_000)
  })

  test('admin inspects group, force disbands group, and queries message audit', async ({
    adminSession,
    request,
  }) => {
    const owner = makeTestUser('grpowner')
    const member = makeTestUser('grpmem')
    const ownerReg = await registerUserViaAPI(request, owner)
    const memberReg = await registerUserViaAPI(request, member)
    const ownerLogin = await loginUserViaAPI(request, owner)
    await loginUserViaAPI(request, member)

    const groupName = `QA监管群_${Date.now().toString(36)}`
    const createGroupRes = await request.post(`${getBaseUrl()}/api/v1/groups`, {
      headers: { Authorization: `Bearer ${ownerLogin.tokens.access_token}` },
      data: {
        name: groupName,
        member_ids: [memberReg.user.id],
        announcement: 'Stage 9 admin governance test',
      },
    })
    const createdGroup = await assertEnvelopeOK<{
      group: { id: string; conversation_id: string; name: string }
    }>(createGroupRes)

    const auditKeyword = `audit_probe_${Date.now().toString(36)}`
    const sendRes = await request.post(
      `${getBaseUrl()}/api/v1/conversations/${createdGroup.group.conversation_id}/messages`,
      {
        headers: { Authorization: `Bearer ${ownerLogin.tokens.access_token}` },
        data: {
          client_msg_id: crypto.randomUUID(),
          msg_type: 'text',
          content: `Hello ${auditKeyword} message`,
          extra: {},
        },
      },
    )
    await assertEnvelopeOK(sendRes)

    const adminPage = adminSession.page
    await adminPage.goto('/admin/groups')
    await adminPage.getByPlaceholder('搜索群名称或群 ID').fill(groupName)
    await adminPage.getByRole('button', { name: '查询' }).click()

    const groupRow = adminPage.locator('.el-table__body-wrapper tr', { hasText: groupName }).first()
    await expect(groupRow).toBeVisible()
    await expect(groupRow).toContainText('正常')
    await expect(groupRow).toContainText('2 / 500')

    await groupRow.getByRole('button', { name: '详情' }).click()
    const drawer = adminPage.locator('.el-drawer', { hasText: '群组详情与成员监管' })
    await expect(drawer).toBeVisible()
    await expect(drawer).toContainText(ownerReg.user.nickname)
    await expect(drawer).toContainText(memberReg.user.nickname)
    await drawer.locator('.el-drawer__close-btn').click()

    await groupRow.getByRole('button', { name: '强制解散' }).click()
    const prompt = adminPage.locator('.el-message-box')
    await expect(prompt).toBeVisible()
    await prompt.locator('input').fill('E2E 群组合规处置')
    await prompt.getByRole('button', { name: '确认解散' }).click()
    await expect(adminPage.locator('.el-message--success')).toContainText('群聊已强制解散')
    await expect(groupRow).toContainText('已解散')

    await groupRow.getByRole('button', { name: '消息审计' }).click()
    await expect(adminPage).toHaveURL(new RegExp(`/admin/audit\\?conversation_id=${createdGroup.group.conversation_id}`))
    await adminPage.getByPlaceholder('消息关键词').fill(auditKeyword)
    await adminPage.getByRole('button', { name: '查询' }).click()

    const auditRow = adminPage.locator('.el-table__body-wrapper tr', { hasText: auditKeyword }).first()
    await expect(auditRow).toBeVisible()
    await expect(auditRow).toContainText(owner.nickname)
  })
})
