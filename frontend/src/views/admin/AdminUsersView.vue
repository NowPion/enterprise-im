<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Search } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import AdminUserDetailDrawer from '@/components/admin/AdminUserDetailDrawer.vue'
import { formatUtcTimestamp, useAdminAccess } from '@/composables/admin/useAdminAccess'
import { useAdminStore } from '@/stores/admin'
import type { AdminUserSummary, PlatformRole, UserAccountStatus } from '@/types/admin'

const adminStore = useAdminStore()
const { currentAdminId } = useAdminAccess()

const filters = reactive<{
  q: string
  status: UserAccountStatus | ''
  role: PlatformRole | ''
}>({
  q: '',
  status: '',
  role: '',
})

const drawerVisible = ref(false)

function buildQuery() {
  return {
    q: filters.q.trim() || undefined,
    status: filters.status === '' ? undefined : filters.status,
    role: filters.role || undefined,
  }
}

async function handleSearch() {
  try {
    await adminStore.fetchUsers(buildQuery(), true)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '查询用户列表失败'))
  }
}

function handleReset() {
  filters.q = ''
  filters.status = ''
  filters.role = ''
  void handleSearch()
}

async function handleLoadMore() {
  try {
    await adminStore.fetchUsers(buildQuery(), false)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载更多用户失败'))
  }
}

async function openDetail(user: AdminUserSummary) {
  drawerVisible.value = true
  try {
    await adminStore.fetchUserDetail(user.id)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载用户详情失败'))
  }
}

async function handleToggleStatus(user: AdminUserSummary) {
  const nextStatus: UserAccountStatus = user.status === 1 ? 0 : 1
  const actionLabel = nextStatus === 0 ? '禁用' : '启用'
  const extraHint =
    nextStatus === 0
      ? '禁用后将立即撤销该用户全部设备会话并关闭所有 WebSocket 连接。'
      : '启用后仅恢复登录资格，需用户重新登录。'

  try {
    const { value: reason } = await ElMessageBox.prompt(
      `确认${actionLabel}账号「${user.username}」(ID ${user.id})？${extraHint}`,
      `${actionLabel}账号`,
      {
        confirmButtonText: `确认${actionLabel}`,
        cancelButtonText: '取消',
        inputPlaceholder: `请输入${actionLabel}原因`,
        inputValidator: (val) => (val && val.trim().length > 0 ? true : '请填写审计原因'),
      },
    )
    const res = await adminStore.toggleUserStatus(user.id, nextStatus, reason.trim())
    if (nextStatus === 0) {
      ElMessage.success(
        `已禁用账号，撤销 ${res.revoked_session_count} 个会话、关闭 ${res.kicked_connection_count} 条连接`,
      )
    } else {
      ElMessage.success('账号已启用')
    }
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, `${actionLabel}账号失败`))
    }
  }
}

async function handleKickAll(user: AdminUserSummary) {
  try {
    const { value: reason } = await ElMessageBox.prompt(
      `确认强制踢出「${user.username}」(ID ${user.id}) 全部设备并下线？`,
      '强制踢下线',
      {
        confirmButtonText: '确认踢下线',
        cancelButtonText: '取消',
        inputPlaceholder: '例如：安全会话清理',
        inputValidator: (val) => (val && val.trim().length > 0 ? true : '请填写操作原因'),
      },
    )
    const res = await adminStore.kickUser(user.id, { reason: reason.trim() })
    ElMessage.success(
      `已撤销 ${res.revoked_session_count} 个会话，关闭 ${res.kicked_connection_count} 条 WS 连接`,
    )
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '强制踢下线失败'))
    }
  }
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
          <h2 class="panel-title">用户管理</h2>
          <p class="panel-desc">检索平台账号、查看多端设备会话、禁用/启用账号及强制踢下线。</p>
        </div>
        <el-button :icon="Refresh" :loading="adminStore.usersLoading" @click="handleSearch">
          刷新
        </el-button>
      </div>

      <div class="filter-bar">
        <el-input
          v-model="filters.q"
          placeholder="搜索用户名、昵称或 ID"
          clearable
          class="query-input"
          @keyup.enter="handleSearch"
        >
          <template #prefix>
            <el-icon><Search /></el-icon>
          </template>
        </el-input>

        <el-select v-model="filters.status" placeholder="账号状态" clearable class="filter-select">
          <el-option label="正常 (1)" :value="1" />
          <el-option label="已禁用 (0)" :value="0" />
        </el-select>

        <el-select v-model="filters.role" placeholder="平台角色" clearable class="filter-select">
          <el-option label="普通用户" value="user" />
          <el-option label="平台管理员" value="admin" />
        </el-select>

        <el-button type="primary" @click="handleSearch">查询</el-button>
        <el-button @click="handleReset">重置</el-button>
      </div>

      <el-table
        v-loading="adminStore.usersLoading"
        :data="adminStore.users"
        border
        size="small"
        empty-text="暂无符合条件的用户"
      >
        <el-table-column prop="id" label="User ID" width="110" />
        <el-table-column prop="username" label="用户名" min-width="130" />
        <el-table-column prop="nickname" label="昵称" min-width="130" />
        <el-table-column label="角色" width="90">
          <template #default="{ row }">
            <span class="role-badge" :class="row.role === 'admin' ? 'is-admin' : ''">
              {{ row.role === 'admin' ? 'admin' : 'user' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="95">
          <template #default="{ row }">
            <span :class="row.status === 1 ? 'status-ok' : 'status-danger'">
              {{ row.status === 1 ? '正常' : '已禁用' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="在线态" width="85">
          <template #default="{ row }">
            <span class="online-cell">
              <span class="dot" :class="row.online ? 'online' : 'offline'" />
              {{ row.online ? '在线' : '离线' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column prop="session_count" label="活跃会话" width="85" align="right" />
        <el-table-column label="最后在线" width="165">
          <template #default="{ row }">
            <span class="time-text">{{ formatUtcTimestamp(row.last_online_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="210" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
              <el-button link size="small" @click="openDetail(row)">详情</el-button>
              <el-button
                link
                size="small"
                :type="row.status === 1 ? 'danger' : 'primary'"
                :disabled="row.id === currentAdminId && row.status === 1"
                @click="handleToggleStatus(row)"
              >
                {{ row.status === 1 ? '禁用' : '启用' }}
              </el-button>
              <el-button
                link
                size="small"
                :disabled="row.session_count === 0 && !row.online"
                @click="handleKickAll(row)"
              >
                踢下线
              </el-button>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <div v-if="adminStore.usersHasMore" class="load-more">
        <el-button :loading="adminStore.usersLoading" @click="handleLoadMore">加载更多</el-button>
      </div>
    </section>

    <AdminUserDetailDrawer
      v-model="drawerVisible"
      :detail="adminStore.selectedUserDetail"
      :loading="adminStore.userDetailLoading"
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
  width: 140px;
}

.role-badge {
  font-size: 11px;
  padding: 2px 6px;
  border-radius: 3px;
  background-color: #f5f6f8;
  color: #4b5563;
}

.role-badge.is-admin {
  background-color: #111827;
  color: #ffffff;
}

.status-ok {
  color: #16a34a;
  font-weight: 500;
}

.status-danger {
  color: #dc2626;
  font-weight: 500;
}

.online-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
}

.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
}

.dot.online {
  background-color: #16a34a;
}

.dot.offline {
  background-color: #9ca3af;
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
