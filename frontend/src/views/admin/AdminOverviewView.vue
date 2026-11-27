<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Connection, Refresh } from '@element-plus/icons-vue'
import {
  formatBytes,
  formatPendingAge,
  formatUtcTimestamp,
} from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'

const adminStore = useAdminStore()
const autoRefresh = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

const overview = computed(() => adminStore.metricsOverview)

const streamPendingSummary = computed(() => {
  const s = overview.value?.streams
  if (typeof s === 'number') return String(s)
  if (s && typeof s === 'object') {
    const pending = s.pending_total ?? adminStore.streamMetrics.reduce((acc, m) => acc + (m.pending_count || 0), 0)
    const dead = s.dead_letter_count
    return dead !== undefined ? `${pending} (死信 ${dead})` : String(pending)
  }
  const totalFromList = adminStore.streamMetrics.reduce((acc, m) => acc + (m.pending_count || 0), 0)
  return String(totalFromList)
})

const dependencyRows = computed(() => {
  const health = adminStore.dependenciesHealth
  if (!health) return []
  return [
    { name: 'MySQL', ...health.mysql },
    { name: 'Redis', ...health.redis },
  ]
})

function refreshNow() {
  void adminStore.fetchMonitoringData()
}

function stopPolling() {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

watch(autoRefresh, (enabled) => {
  stopPolling()
  if (enabled) {
    pollTimer = setInterval(() => {
      void adminStore.fetchMonitoringData()
    }, 10000)
  }
})

onMounted(() => {
  void adminStore.fetchMonitoringData()
})

onBeforeUnmount(() => {
  stopPolling()
})
</script>

<template>
  <div class="overview-page">
    <div class="header-bar ios-grouped-section">
      <div>
        <h2 class="page-title ios-large-title">系统运行监控</h2>
        <p class="page-sub">
          最近刷新：{{ formatUtcTimestamp(adminStore.monitoringLastUpdated) }}
        </p>
      </div>
      <div class="header-controls">
        <el-checkbox v-model="autoRefresh">每 10 秒自动刷新</el-checkbox>
        <el-button
          type="primary"
          :icon="Refresh"
          :loading="adminStore.monitoringLoading"
          @click="refreshNow"
        >
          立即刷新
        </el-button>
      </div>
    </div>

    <el-alert
      v-if="adminStore.monitoringError"
      type="warning"
      show-icon
      :closable="false"
      :title="adminStore.monitoringError"
    />

    <section class="kpi-grid">
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">在线用户数</span>
        <span class="kpi-value">{{ overview?.online_users ?? '—' }}</span>
      </div>
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">WebSocket 连接数</span>
        <span class="kpi-value">{{ overview?.ws_connections ?? '—' }}</span>
      </div>
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">消息吞吐 (条/分)</span>
        <span class="kpi-value">{{ overview?.messages_per_minute ?? '—' }}</span>
      </div>
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">Streams Pending 积压</span>
        <span class="kpi-value">{{ streamPendingSummary }}</span>
      </div>
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">Go Goroutines</span>
        <span class="kpi-value">{{ overview?.goroutines ?? '—' }}</span>
      </div>
      <div class="kpi-card ios-grouped-section">
        <span class="kpi-label">Go 堆内存 (Heap)</span>
        <span class="kpi-value">{{ formatBytes(overview?.heap_bytes) }}</span>
      </div>
    </section>

    <section class="panel-card ios-grouped-section">
      <div class="card-title-row">
        <div class="title-with-icon">
          <el-icon class="title-icon"><Connection /></el-icon>
          <h3 class="card-title">核心依赖健康探活 (MySQL / Redis)</h3>
        </div>
      </div>

      <el-table
        v-loading="adminStore.monitoringLoading"
        :data="dependencyRows"
        border
        size="small"
        empty-text="暂无依赖健康数据"
      >
        <el-table-column prop="name" label="组件" width="120" />
        <el-table-column label="状态" width="120">
          <template #default="{ row }">
            <span :class="row.status === 'ok' ? 'status-ok' : 'status-danger'">
              {{ row.status }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="探测延迟" width="120" align="right">
          <template #default="{ row }">
            <span class="mono">
              {{ row.latency_ms !== null && row.latency_ms !== undefined ? `${row.latency_ms} ms` : '—' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column prop="version" label="服务端版本" width="160" />
        <el-table-column label="最近错误时间" width="180">
          <template #default="{ row }">
            <span class="time-text">{{ formatUtcTimestamp(row.last_error_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="最近错误摘要" min-width="200">
          <template #default="{ row }">
            <span class="err-text">{{ row.last_error || '—' }}</span>
          </template>
        </el-table-column>
      </el-table>
    </section>

    <section class="panel-card ios-grouped-section">
      <div class="card-title-row">
        <h3 class="card-title">Redis Streams 消费组与 Pending 积压</h3>
      </div>

      <el-table
        v-loading="adminStore.monitoringLoading"
        :data="adminStore.streamMetrics"
        border
        size="small"
        empty-text="暂无 Stream 消费组数据"
      >
        <el-table-column prop="stream" label="Stream Key" min-width="180" />
        <el-table-column prop="group" label="Consumer Group" min-width="160" />
        <el-table-column prop="consumer" label="Consumer" min-width="160" />
        <el-table-column prop="last_delivered_id" label="last_delivered_id" min-width="170" />
        <el-table-column label="Pending 数" width="110" align="right">
          <template #default="{ row }">
            <span class="mono" :class="row.pending_count > 0 ? 'status-danger' : 'status-ok'">
              {{ row.pending_count }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="最长未确认耗时" width="150" align="right">
          <template #default="{ row }">
            <span class="mono">{{ formatPendingAge(row.oldest_pending_age) }}</span>
          </template>
        </el-table-column>
      </el-table>
    </section>
  </div>
</template>

<style scoped>
.overview-page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.header-bar {
  padding: 16px 20px;
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}

.page-title {
  font-size: 24px;
}

.page-sub {
  margin: 4px 0 0;
  font-size: 13px;
  color: var(--im-text-secondary);
}

.header-controls {
  display: flex;
  align-items: center;
  gap: 14px;
}

.kpi-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
}

.kpi-card {
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.kpi-label {
  font-size: 13px;
  color: var(--im-text-secondary);
}

.kpi-value {
  font-size: 24px;
  font-weight: 700;
  color: var(--im-text-primary);
  font-variant-numeric: tabular-nums;
}

.panel-card {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.card-title-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.title-with-icon {
  display: flex;
  align-items: center;
  gap: 8px;
}

.title-icon {
  color: var(--im-accent-primary);
}

.card-title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.status-ok {
  color: #248a3d;
  font-weight: 600;
}

.status-danger {
  color: var(--im-status-danger);
  font-weight: 600;
}

.time-text {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.err-text {
  font-size: 12px;
  color: var(--im-text-secondary);
}
</style>
