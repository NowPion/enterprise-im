<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useGroupStore } from '@/stores/group'
import type { GroupAuditLog } from '@/types/group'

const props = defineProps<{
  groupId: string
  logs: GroupAuditLog[]
  loading: boolean
  hasMore: boolean
}>()

const groupStore = useGroupStore()
const dateRange = ref<[Date, Date] | null>(null)

function buildRangeParams() {
  if (!dateRange.value || dateRange.value.length !== 2) {
    return {}
  }
  return {
    from: dateRange.value[0].toISOString(),
    to: dateRange.value[1].toISOString(),
  }
}

async function refreshAudit() {
  try {
    await groupStore.fetchAuditLogs(props.groupId, {
      reset: true,
      ...buildRangeParams(),
    })
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载群审计日志失败'))
  }
}

async function loadMore() {
  try {
    await groupStore.fetchAuditLogs(props.groupId, {
      reset: false,
      ...buildRangeParams(),
    })
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载更多审计记录失败'))
  }
}

function formatDetail(detail: Record<string, unknown> | null): string {
  if (!detail) return '—'
  try {
    return JSON.stringify(detail)
  } catch {
    return '—'
  }
}

onMounted(() => {
  if (props.logs.length === 0) {
    void refreshAudit()
  }
})

watch(
  () => props.groupId,
  () => {
    void refreshAudit()
  },
)
</script>

<template>
  <div class="audit-panel ios-grouped-section">
    <div class="audit-toolbar">
      <el-date-picker
        v-model="dateRange"
        type="datetimerange"
        range-separator="至"
        start-placeholder="开始时间"
        end-placeholder="结束时间"
        size="small"
        @change="refreshAudit"
      />
      <el-button size="small" type="primary" plain :icon="Refresh" :loading="loading" @click="refreshAudit">
        查询审计日志
      </el-button>
    </div>

    <el-table
      v-loading="loading"
      :data="logs"
      border
      size="small"
      empty-text="暂无群管理审计记录"
    >
      <el-table-column prop="action" label="动作" width="150" />
      <el-table-column label="操作人" width="150">
        <template #default="{ row }">
          <span class="mono">{{ row.actor_id }}</span>
          <span class="role-hint">({{ row.actor_role || '—' }})</span>
        </template>
      </el-table-column>
      <el-table-column label="目标" width="150">
        <template #default="{ row }">
          <span>{{ row.target_type }}:</span>
          <span class="mono">{{ row.target_id }}</span>
        </template>
      </el-table-column>
      <el-table-column label="详情" min-width="220">
        <template #default="{ row }">
          <span class="detail-text">{{ row.reason || formatDetail(row.detail) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="时间" width="170">
        <template #default="{ row }">
          <span class="time-text">{{ formatUtcTimestamp(row.created_at) }}</span>
        </template>
      </el-table-column>
      <el-table-column prop="request_id" label="Request ID" width="140" />
    </el-table>

    <div v-if="hasMore" class="load-more">
      <el-button size="small" :loading="loading" @click="loadMore">加载更多</el-button>
    </div>
  </div>
</template>

<style scoped>
.audit-panel {
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.audit-toolbar {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.role-hint {
  margin-left: 4px;
  font-size: 12px;
  color: var(--im-text-secondary);
}

.detail-text {
  font-size: 12px;
  color: var(--im-text-secondary);
  word-break: break-all;
}

.time-text {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.load-more {
  display: flex;
  justify-content: center;
}
</style>
