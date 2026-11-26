<script setup lang="ts">
import { ElMessage, ElMessageBox } from 'element-plus'
import { extractErrorMessage } from '@/api/group'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'
import type { AdminUserDetail, AdminUserSession } from '@/types/admin'

const props = defineProps<{
  modelValue: boolean
  detail: AdminUserDetail | null
  loading: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
}>()

const adminStore = useAdminStore()

async function handleKickSession(session: AdminUserSession) {
  if (!props.detail) return
  try {
    const { value: reason } = await ElMessageBox.prompt(
      `确认撤销设备「${session.device_type} / ${session.device_id}」的会话并关闭关联 WebSocket？请输入审计原因：`,
      '踢出单端会话',
      {
        confirmButtonText: '确认踢出',
        cancelButtonText: '取消',
        inputPlaceholder: '例如：异常设备登录排查',
        inputValidator: (val) => (val && val.trim().length > 0 ? true : '请填写操作原因'),
      },
    )
    const res = await adminStore.kickUser(props.detail.user.id, {
      session_id: session.id,
      reason: reason.trim(),
    })
    ElMessage.success(
      `已撤销 ${res.revoked_session_count} 个会话，关闭 ${res.kicked_connection_count} 条连接`,
    )
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '踢出会话失败'))
    }
  }
}
</script>

<template>
  <el-drawer
    :model-value="modelValue"
    title="用户详情与设备会话"
    size="640px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div v-loading="loading" class="drawer-body">
      <template v-if="detail">
        <section class="info-section">
          <h4 class="sec-title">账号档案</h4>
          <el-descriptions :column="2" border size="small">
            <el-descriptions-item label="用户 ID">
              <span class="mono">{{ detail.user.id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="用户名">{{ detail.user.username }}</el-descriptions-item>
            <el-descriptions-item label="昵称">{{ detail.user.nickname }}</el-descriptions-item>
            <el-descriptions-item label="平台角色">{{ detail.user.role }}</el-descriptions-item>
            <el-descriptions-item label="账号状态">
              <span :class="detail.user.status === 1 ? 'status-ok' : 'status-danger'">
                {{ detail.user.status === 1 ? '正常 (1)' : '已禁用 (0)' }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item label="在线状态">
              {{ detail.user.online ? '在线' : '离线' }}
            </el-descriptions-item>
            <el-descriptions-item label="好友数">{{ detail.friend_count }}</el-descriptions-item>
            <el-descriptions-item label="加群数">{{ detail.group_count }}</el-descriptions-item>
            <el-descriptions-item label="最后在线" :span="2">
              {{ formatUtcTimestamp(detail.user.last_online_at) }}
            </el-descriptions-item>
            <el-descriptions-item label="个性签名" :span="2">
              {{ detail.user.signature || '—' }}
            </el-descriptions-item>
          </el-descriptions>
        </section>

        <section class="info-section">
          <h4 class="sec-title">设备会话列表 ({{ detail.sessions.length }})</h4>
          <el-table :data="detail.sessions" border size="small" empty-text="暂无会话记录">
            <el-table-column prop="id" label="Session ID" width="100" />
            <el-table-column prop="device_type" label="终端" width="80" />
            <el-table-column prop="ip_address" label="IP" width="120" />
            <el-table-column label="状态" width="90">
              <template #default="{ row }">
                <span :class="row.revoked_at ? 'status-muted' : 'status-ok'">
                  {{ row.revoked_at ? '已撤销' : '活跃' }}
                </span>
              </template>
            </el-table-column>
            <el-table-column label="最近活跃" width="150">
              <template #default="{ row }">
                <span class="time-text">{{ formatUtcTimestamp(row.last_active_at) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="80" fixed="right">
              <template #default="{ row }">
                <el-button
                  v-if="!row.revoked_at"
                  link
                  type="danger"
                  size="small"
                  :loading="adminStore.actionSubmitting"
                  @click="handleKickSession(row)"
                >
                  踢出
                </el-button>
                <span v-else class="status-muted">—</span>
              </template>
            </el-table-column>
          </el-table>
        </section>

        <section class="info-section">
          <h4 class="sec-title">近期审计摘要</h4>
          <el-table :data="detail.audit_summary" border size="small" empty-text="暂无相关审计记录">
            <el-table-column prop="action" label="动作" width="140" />
            <el-table-column prop="actor_id" label="操作者 ID" width="100" />
            <el-table-column prop="reason" label="原因" min-width="160" />
            <el-table-column label="时间" width="150">
              <template #default="{ row }">
                <span class="time-text">{{ formatUtcTimestamp(row.created_at) }}</span>
              </template>
            </el-table-column>
          </el-table>
        </section>
      </template>
    </div>
  </el-drawer>
</template>

<style scoped>
.drawer-body {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.info-section {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.sec-title {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
  color: #111827;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}

.status-ok {
  color: #16a34a;
  font-weight: 500;
}

.status-danger {
  color: #dc2626;
  font-weight: 500;
}

.status-muted {
  color: #9ca3af;
}

.time-text {
  font-size: 12px;
  color: #4b5563;
}
</style>
