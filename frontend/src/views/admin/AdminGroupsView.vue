<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Search } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import AdminGroupDetailDrawer from '@/components/admin/AdminGroupDetailDrawer.vue'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'
import type { AdminGroupSummary } from '@/types/admin'
import type { GroupStatus } from '@/types/group'

const router = useRouter()
const adminStore = useAdminStore()

const filters = reactive<{
  q: string
  status: GroupStatus | ''
}>({
  q: '',
  status: '',
})

const drawerVisible = ref(false)

function buildQuery() {
  return {
    q: filters.q.trim() || undefined,
    status: filters.status || undefined,
  }
}

async function handleSearch() {
  try {
    await adminStore.fetchGroups(buildQuery(), true)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载群组监管列表失败'))
  }
}

function handleReset() {
  filters.q = ''
  filters.status = ''
  void handleSearch()
}

async function handleLoadMore() {
  try {
    await adminStore.fetchGroups(buildQuery(), false)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载更多群组失败'))
  }
}

async function openDetail(group: AdminGroupSummary) {
  drawerVisible.value = true
  try {
    await adminStore.fetchGroupDetail(group.id)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载群组详情失败'))
  }
}

async function handleForceDisband(group: AdminGroupSummary) {
  try {
    const { value: reason } = await ElMessageBox.prompt(
      `确认强制解散群聊「${group.name}」(ID ${group.id})？请输入审计原因：`,
      '强制解散群聊',
      {
        confirmButtonText: '确认解散',
        cancelButtonText: '取消',
        inputPlaceholder: '例如：涉敏或违规内容治理',
        inputValidator: (val) => (val && val.trim().length > 0 ? true : '请填写解散原因'),
      },
    )
    await adminStore.forceDisbandGroup(group.id, reason.trim())
    ElMessage.success('群聊已强制解散')
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '强制解散群聊失败'))
    }
  }
}

function jumpToMessageAudit(conversationId: string) {
  void router.push({
    path: '/admin/audit',
    query: { conversation_id: conversationId },
  })
}

onMounted(() => {
  void handleSearch()
})
</script>

<template>
  <div class="admin-page">
    <section class="panel-card">
      <div class="panel-header">
        <div>
          <h2 class="panel-title">群组监管</h2>
          <p class="panel-desc">检索全平台群聊、核查群成员与会话序号、执行合规强制解散。</p>
        </div>
        <el-button :icon="Refresh" :loading="adminStore.groupsLoading" @click="handleSearch">
          刷新
        </el-button>
      </div>

      <div class="filter-bar">
        <el-input
          v-model="filters.q"
          placeholder="搜索群名称或群 ID"
          clearable
          class="query-input"
          @keyup.enter="handleSearch"
        >
          <template #prefix>
            <el-icon><Search /></el-icon>
          </template>
        </el-input>

        <el-select v-model="filters.status" placeholder="群状态" clearable class="filter-select">
          <el-option label="正常 (active)" value="active" />
          <el-option label="已解散 (disbanded)" value="disbanded" />
        </el-select>

        <el-button type="primary" @click="handleSearch">查询</el-button>
        <el-button @click="handleReset">重置</el-button>
      </div>

      <el-table
        v-loading="adminStore.groupsLoading"
        :data="adminStore.groups"
        border
        size="small"
        empty-text="暂无符合条件的群聊"
      >
        <el-table-column prop="id" label="群 ID" width="100" />
        <el-table-column prop="conversation_id" label="会话 ID" width="100" />
        <el-table-column prop="name" label="群名称" min-width="160" />
        <el-table-column label="群主" min-width="130">
          <template #default="{ row }">
            <span>{{ row.owner?.nickname || `ID ${row.owner_id}` }}</span>
          </template>
        </el-table-column>
        <el-table-column label="成员数" width="100" align="right">
          <template #default="{ row }">
            <span>{{ row.member_count }} / {{ row.max_members }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="last_seq" label="末序号" width="90" align="right" />
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <span :class="row.status === 'active' ? 'status-ok' : 'status-danger'">
              {{ row.status === 'active' ? '正常' : '已解散' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="165">
          <template #default="{ row }">
            <span class="time-text">{{ formatUtcTimestamp(row.created_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="210" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <el-button link size="small" @click="openDetail(row)">详情</el-button>
              <el-button
                link
                size="small"
                @click="jumpToMessageAudit(row.conversation_id)"
              >
                消息审计
              </el-button>
              <el-button
                v-if="row.status === 'active'"
                link
                type="danger"
                size="small"
                @click="handleForceDisband(row)"
              >
                强制解散
              </el-button>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <div v-if="adminStore.groupsHasMore" class="load-more">
        <el-button :loading="adminStore.groupsLoading" @click="handleLoadMore">加载更多</el-button>
      </div>
    </section>

    <AdminGroupDetailDrawer
      v-model="drawerVisible"
      :detail="adminStore.selectedGroupDetail"
      :loading="adminStore.groupDetailLoading"
    />
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

.query-input {
  width: 240px;
}

.filter-select {
  width: 170px;
}

.status-ok {
  color: #16a34a;
  font-weight: 500;
}

.status-danger {
  color: #dc2626;
  font-weight: 500;
}

.time-text {
  font-size: 12px;
  color: #4b5563;
}

.row-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.load-more {
  display: flex;
  justify-content: center;
}
</style>
