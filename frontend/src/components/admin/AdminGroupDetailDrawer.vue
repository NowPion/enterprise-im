<script setup lang="ts">
import { ElMessage, ElMessageBox } from 'element-plus'
import { extractErrorMessage } from '@/api/group'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'
import type { AdminGroupDetail } from '@/types/admin'

const props = defineProps<{
  modelValue: boolean
  detail: AdminGroupDetail | null
  loading: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
}>()

const adminStore = useAdminStore()

async function handleForceDisband() {
  if (!props.detail) return
  try {
    const { value: reason } = await ElMessageBox.prompt(
      `确认强制解散群「${props.detail.group.name}」？解散后将不可恢复并写入不可变审计日志。请输入强制解散原因：`,
      '强制解散群聊',
      {
        confirmButtonText: '确认解散',
        cancelButtonText: '取消',
        inputPlaceholder: '例如：违规群聊处置',
        inputValidator: (val) => (val && val.trim().length > 0 ? true : '请填写处置原因'),
      },
    )
    await adminStore.forceDisbandGroup(props.detail.group.id, reason.trim())
    ElMessage.success('群聊已强制解散')
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '强制解散失败'))
    }
  }
}
</script>

<template>
  <el-drawer
    :model-value="modelValue"
    title="群组详情与成员监管"
    size="660px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div v-loading="loading" class="drawer-body">
      <template v-if="detail">
        <section class="info-section">
          <div class="sec-header">
            <h4 class="sec-title">群基础属性</h4>
            <el-button
              v-if="detail.group.status === 'active'"
              type="danger"
              size="small"
              :loading="adminStore.actionSubmitting"
              @click="handleForceDisband"
            >
              强制解散群聊
            </el-button>
          </div>
          <el-descriptions :column="2" border size="small">
            <el-descriptions-item label="群 ID">
              <span class="mono">{{ detail.group.id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="会话 ID">
              <span class="mono">{{ detail.group.conversation_id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="群名称">{{ detail.group.name }}</el-descriptions-item>
            <el-descriptions-item label="群主 ID">
              <span class="mono">{{ detail.group.owner_id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="状态">
              <span :class="detail.group.status === 'active' ? 'status-ok' : 'status-danger'">
                {{ detail.group.status === 'active' ? '正常' : '已解散' }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item label="成员数">
              {{ detail.group.member_count }} / {{ detail.group.max_members }}
            </el-descriptions-item>
            <el-descriptions-item label="末序号 last_seq">
              <span class="mono">{{ detail.group.last_seq ?? '0' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="创建时间">
              {{ formatUtcTimestamp(detail.group.created_at) }}
            </el-descriptions-item>
            <el-descriptions-item label="群公告" :span="2">
              {{ detail.group.announcement || '—' }}
            </el-descriptions-item>
          </el-descriptions>
        </section>

        <section class="info-section">
          <h4 class="sec-title">群成员 ({{ detail.members.length }})</h4>
          <el-table :data="detail.members" border size="small" empty-text="暂无成员记录">
            <el-table-column prop="user_id" label="User ID" width="110" />
            <el-table-column prop="nickname" label="用户昵称" min-width="120" />
            <el-table-column prop="nickname_in_group" label="群昵称" min-width="120" />
            <el-table-column prop="role" label="角色" width="90" />
            <el-table-column label="在线" width="70">
              <template #default="{ row }">
                <span :class="row.online ? 'status-ok' : 'status-muted'">
                  {{ row.online ? '在线' : '离线' }}
                </span>
              </template>
            </el-table-column>
            <el-table-column label="入群时间" width="150">
              <template #default="{ row }">
                <span class="time-text">{{ formatUtcTimestamp(row.joined_at) }}</span>
              </template>
            </el-table-column>
          </el-table>
        </section>

        <section class="info-section">
          <h4 class="sec-title">系统事件摘要</h4>
          <el-table
            :data="detail.system_events"
            border
            size="small"
            empty-text="暂无系统事件记录"
          >
            <el-table-column prop="action" label="事件" width="140" />
            <el-table-column prop="actor_id" label="操作者" width="100" />
            <el-table-column prop="reason" label="原因/说明" min-width="160" />
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

.sec-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
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
