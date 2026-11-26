<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh, User } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import { useGroupPermissions } from '@/composables/groups/useGroupPermissions'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useGroupStore } from '@/stores/group'
import type { GroupInfo, GroupMember, GroupRole, MutableGroupRole } from '@/types/group'

const props = defineProps<{
  group: GroupInfo
  myRole: GroupRole | null
  members: GroupMember[]
  loading: boolean
  hasMore: boolean
}>()

const emit = defineEmits<{
  (e: 'open-invite'): void
}>()

const groupStore = useGroupStore()
const groupRef = computed(() => props.group)
const roleRef = computed(() => props.myRole)

const { currentUserId, canInviteMembers, canRemoveMember, canChangeMemberRole } =
  useGroupPermissions(groupRef, roleRef)

const roleFilter = ref<GroupRole | ''>('')
const onlineOnly = ref(false)
const keyword = ref('')

const filteredMembers = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  if (!kw) return props.members
  return props.members.filter(
    (m) =>
      m.nickname.toLowerCase().includes(kw) ||
      m.nickname_in_group.toLowerCase().includes(kw) ||
      m.user_id.includes(kw),
  )
})

function roleLabel(role: GroupRole): string {
  if (role === 'owner') return '群主'
  if (role === 'admin') return '管理员'
  return '成员'
}

async function handleFilterChange() {
  try {
    await groupStore.fetchMembers(props.group.id, {
      reset: true,
      role: roleFilter.value,
      online_only: onlineOnly.value,
    })
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '筛选成员失败'))
  }
}

async function handleLoadMore() {
  try {
    await groupStore.fetchMembers(props.group.id, { reset: false })
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载更多成员失败'))
  }
}

async function handleToggleRole(member: GroupMember) {
  const nextRole: MutableGroupRole = member.role === 'admin' ? 'member' : 'admin'
  const label = nextRole === 'admin' ? '设为管理员' : '取消管理员'
  try {
    await ElMessageBox.confirm(
      `确认将成员「${member.nickname_in_group || member.nickname}」${label}？`,
      '变更群角色',
      {
        confirmButtonText: '确认',
        cancelButtonText: '取消',
        type: 'warning',
      },
    )
    await groupStore.updateMemberRole(props.group.id, member.user_id, nextRole)
    ElMessage.success(`已${label}`)
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '更新成员角色失败'))
    }
  }
}

async function handleRemove(member: GroupMember) {
  try {
    await ElMessageBox.confirm(
      `确认将「${member.nickname_in_group || member.nickname}」移出群聊？移出后将立即失去历史与新消息访问权限。`,
      '移出成员',
      {
        confirmButtonText: '确认移出',
        cancelButtonText: '取消',
        type: 'warning',
      },
    )
    await groupStore.removeMember(props.group.id, member.user_id)
    ElMessage.success('成员已移出')
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '移出成员失败'))
    }
  }
}
</script>

<template>
  <div class="member-panel">
    <div class="toolbar">
      <div class="filters">
        <el-input
          v-model="keyword"
          placeholder="当前页筛选昵称或 User ID"
          clearable
          class="filter-input"
        />
        <el-select
          v-model="roleFilter"
          placeholder="全部角色"
          clearable
          class="role-select"
          @change="handleFilterChange"
        >
          <el-option label="全部角色" value="" />
          <el-option label="群主" value="owner" />
          <el-option label="管理员" value="admin" />
          <el-option label="普通成员" value="member" />
        </el-select>
        <el-checkbox v-model="onlineOnly" @change="handleFilterChange">仅看在线</el-checkbox>
      </div>

      <div class="actions">
        <el-button :icon="Refresh" :loading="loading" @click="handleFilterChange">刷新</el-button>
        <el-button
          v-if="canInviteMembers"
          type="primary"
          :icon="Plus"
          @click="emit('open-invite')"
        >
          邀请成员
        </el-button>
      </div>
    </div>

    <el-table
      v-loading="loading"
      :data="filteredMembers"
      border
      size="small"
      class="member-table"
      empty-text="暂无符合条件的群成员"
    >
      <el-table-column label="成员" min-width="200">
        <template #default="{ row }">
          <div class="member-cell">
            <div class="avatar-mini">
              <img v-if="row.avatar_url" :src="row.avatar_url" alt="" class="avatar-mini-img" />
              <el-icon v-else :size="14"><User /></el-icon>
            </div>
            <div class="member-names">
              <div class="primary-name">
                {{ row.nickname_in_group || row.nickname }}
                <span v-if="row.user_id === currentUserId" class="self-tag">我</span>
              </div>
              <div class="sub-name">
                原昵称：{{ row.nickname }} · ID {{ row.user_id }}
              </div>
            </div>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="群角色" width="110">
        <template #default="{ row }">
          <span class="role-badge" :class="`role-${row.role}`">
            {{ roleLabel(row.role) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column label="在线状态" width="100">
        <template #default="{ row }">
          <span class="online-cell">
            <span class="dot" :class="row.online ? 'online' : 'offline'" />
            {{ row.online ? '在线' : '离线' }}
          </span>
        </template>
      </el-table-column>

      <el-table-column label="入群时间" width="170">
        <template #default="{ row }">
          <span class="time-text">{{ formatUtcTimestamp(row.joined_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="180" fixed="right">
        <template #default="{ row }">
          <div class="row-ops">
            <el-button
              v-if="canChangeMemberRole(row)"
              link
              size="small"
              @click="handleToggleRole(row)"
            >
              {{ row.role === 'admin' ? '取消管理员' : '设为管理员' }}
            </el-button>
            <el-button
              v-if="canRemoveMember(row)"
              link
              type="danger"
              size="small"
              @click="handleRemove(row)"
            >
              移出
            </el-button>
            <span v-if="!canChangeMemberRole(row) && !canRemoveMember(row)" class="muted-op">—</span>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <div v-if="hasMore" class="load-more-bar">
      <el-button :loading="loading" @click="handleLoadMore">加载更多成员</el-button>
    </div>
  </div>
</template>

<style scoped>
.member-panel {
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.toolbar {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: 10px;
  align-items: center;
}

.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
}

.filter-input {
  width: 210px;
}

.role-select {
  width: 130px;
}

.actions {
  display: flex;
  gap: 8px;
}

.member-table {
  width: 100%;
}

.member-cell {
  display: flex;
  align-items: center;
  gap: 10px;
}

.avatar-mini {
  width: 32px;
  height: 32px;
  border-radius: 6px;
  background-color: #f5f6f8;
  border: 1px solid #e5e7eb;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #6b7280;
  overflow: hidden;
  flex-shrink: 0;
}

.avatar-mini-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.member-names {
  min-width: 0;
}

.primary-name {
  font-size: 13px;
  font-weight: 500;
  color: #111827;
  display: flex;
  align-items: center;
  gap: 6px;
}

.self-tag {
  font-size: 11px;
  padding: 0 5px;
  border-radius: 3px;
  background-color: #111827;
  color: #ffffff;
}

.sub-name {
  font-size: 12px;
  color: #6b7280;
}

.role-badge {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 4px;
  font-size: 12px;
  font-weight: 500;
}

.role-owner {
  background-color: #111827;
  color: #ffffff;
}

.role-admin {
  background-color: #eceef2;
  color: #111827;
  border: 1px solid #d1d5db;
}

.role-member {
  background-color: #f5f6f8;
  color: #4b5563;
}

.online-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #4b5563;
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

.row-ops {
  display: flex;
  align-items: center;
  gap: 8px;
}

.muted-op {
  color: #9ca3af;
  font-size: 12px;
}

.load-more-bar {
  display: flex;
  justify-content: center;
  padding-top: 4px;
}
</style>
