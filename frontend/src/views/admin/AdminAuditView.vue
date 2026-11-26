<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Refresh, Search } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'

const route = useRoute()
const adminStore = useAdminStore()

const filters = reactive({
  conversation_id: '',
  sender_id: '',
  keyword: '',
})
const dateRange = ref<[Date, Date] | null>(null)

function buildQuery() {
  const params: Record<string, string> = {}
  if (filters.conversation_id.trim()) params.conversation_id = filters.conversation_id.trim()
  if (filters.sender_id.trim()) params.sender_id = filters.sender_id.trim()
  if (filters.keyword.trim()) params.keyword = filters.keyword.trim()
  if (dateRange.value && dateRange.value.length === 2) {
    params.from = dateRange.value[0].toISOString()
    params.to = dateRange.value[1].toISOString()
  }
  return params
}

async function handleSearch() {
  try {
    await adminStore.fetchMessageAudit(buildQuery(), true)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '查询消息审计失败'))
  }
}

function handleReset() {
  filters.conversation_id = ''
  filters.sender_id = ''
  filters.keyword = ''
  dateRange.value = null
  void handleSearch()
}

async function handleLoadMore() {
  try {
    await adminStore.fetchMessageAudit(buildQuery(), false)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载更多消息审计失败'))
  }
}

function formatExtra(extra: Record<string, unknown> | null): string {
  if (!extra) return ''
  try {
    return JSON.stringify(extra)
  } catch {
    return ''
  }
}

onMounted(() => {
  const convId = route.query.conversation_id
  if (typeof convId === 'string' && convId.trim()) {
    filters.conversation_id = convId.trim()
  }
  void handleSearch()
})
</script>

<template>
  <div class="admin-page">
    <section class="panel-card">
      <div class="panel-header">
        <div>
          <h2 class="panel-title">消息审计查询</h2>
          <p class="panel-desc">
            按会话 ID、发送者 ID、内容关键词与 UTC 时间范围组合检索不可变消息审计记录。
          </p>
        </div>
        <el-button :icon="Refresh" :loading="adminStore.auditLoading" @click="handleSearch">
          刷新
        </el-button>
      </div>

      <div class="filter-bar">
        <el-input
          v-model="filters.conversation_id"
          placeholder="会话 ID (conversation_id)"
          clearable
          class="id-input"
          @keyup.enter="handleSearch"
        />
        <el-input
          v-model="filters.sender_id"
          placeholder="发送者 ID (sender_id)"
          clearable
          class="id-input"
          @keyup.enter="handleSearch"
        />
        <el-input
          v-model="filters.keyword"
          placeholder="消息关键词"
          clearable
          class="kw-input"
          @keyup.enter="handleSearch"
        >
          <template #prefix>
            <el-icon><Search /></el-icon>
          </template>
        </el-input>

        <el-date-picker
          v-model="dateRange"
          type="datetimerange"
          range-separator="至"
          start-placeholder="起始时间"
          end-placeholder="结束时间"
        />

        <el-button type="primary" @click="handleSearch">查询</el-button>
        <el-button @click="handleReset">重置</el-button>
      </div>

      <el-table
        v-loading="adminStore.auditLoading"
        :data="adminStore.auditMessages"
        border
        size="small"
        empty-text="暂无符合条件的消息审计记录"
      >
        <el-table-column prop="msg_id" label="msg_id" width="175" />
        <el-table-column prop="conversation_id" label="会话 ID" width="95" />
        <el-table-column prop="seq" label="seq" width="75" align="right" />
        <el-table-column label="发送者" width="140">
          <template #default="{ row }">
            <div class="sender-cell">
              <span class="mono">{{ row.sender_id }}</span>
              <span v-if="row.sender_nickname || row.sender_username" class="sender-sub">
                {{ row.sender_nickname || row.sender_username }}
              </span>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="msg_type" label="类型" width="85" />
        <el-table-column label="状态" width="105">
          <template #default="{ row }">
            <span :class="row.status === 'recalled' ? 'status-recalled' : 'status-normal'">
              {{ row.status === 'recalled' ? '已撤回' : '正常' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="正文 / 扩展" min-width="260">
          <template #default="{ row }">
            <div class="content-cell">{{ row.content || '—' }}</div>
            <div v-if="row.extra" class="extra-text">{{ formatExtra(row.extra) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="发送时间 / 撤回时间" width="180">
          <template #default="{ row }">
            <div class="time-text">{{ formatUtcTimestamp(row.created_at) }}</div>
            <div v-if="row.recalled_at" class="recall-time">
              撤回：{{ formatUtcTimestamp(row.recalled_at) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="client_msg_id" label="client_msg_id" min-width="180" />
      </el-table>

      <div v-if="adminStore.auditHasMore" class="load-more">
        <el-button :loading="adminStore.auditLoading" @click="handleLoadMore">加载更多</el-button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.admin-page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.panel-card {
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.panel-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
}

.panel-title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: #111827;
}

.panel-desc {
  margin: 4px 0 0;
  font-size: 12px;
  color: #6b7280;
}

.filter-bar {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  align-items: center;
}

.id-input {
  width: 170px;
}

.kw-input {
  width: 210px;
}

.sender-cell {
  display: flex;
  flex-direction: column;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.sender-sub {
  font-size: 11px;
  color: #6b7280;
}

.status-normal {
  color: #111827;
  font-size: 12px;
}

.status-recalled {
  color: #dc2626;
  font-size: 12px;
  font-weight: 500;
}

.content-cell {
  font-size: 12px;
  color: #111827;
  word-break: break-word;
}

.extra-text {
  margin-top: 2px;
  font-size: 11px;
  color: #6b7280;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  word-break: break-all;
}

.time-text {
  font-size: 12px;
  color: #4b5563;
}

.recall-time {
  font-size: 11px;
  color: #dc2626;
}

.load-more {
  display: flex;
  justify-content: center;
}
</style>
