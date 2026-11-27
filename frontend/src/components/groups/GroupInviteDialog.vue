<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Check, Plus, Search } from '@element-plus/icons-vue'
import { extractErrorMessage, listFriendCandidates, searchCandidateUsers } from '@/api/group'
import { useGroupStore } from '@/stores/group'
import type { AddGroupMembersResult, CandidateUser } from '@/types/group'

const props = defineProps<{
  modelValue: boolean
  groupId: string
  existingMemberIds: string[]
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'invited', result: AddGroupMembersResult): void
}>()

const groupStore = useGroupStore()

const searchQuery = ref('')
const friends = ref<CandidateUser[]>([])
const searchResults = ref<CandidateUser[]>([])
const loadingCandidates = ref(false)
const searchingUsers = ref(false)
const selectedIds = ref<Set<string>>(new Set())

const existingSet = computed(() => new Set(props.existingMemberIds))

const displayCandidates = computed<CandidateUser[]>(() =>
  searchQuery.value.trim() ? searchResults.value : friends.value,
)

watch(
  () => props.modelValue,
  (visible) => {
    if (visible) {
      searchQuery.value = ''
      searchResults.value = []
      selectedIds.value = new Set()
      void loadFriends()
    }
  },
)

async function loadFriends() {
  loadingCandidates.value = true
  try {
    const page = await listFriendCandidates(undefined, 50)
    friends.value = page.items.map((item) => ({
      ...item.user,
      remark: item.remark,
    }))
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载好友列表失败'))
  } finally {
    loadingCandidates.value = false
  }
}

async function handleSearch() {
  const q = searchQuery.value.trim()
  if (!q) {
    searchResults.value = []
    return
  }
  searchingUsers.value = true
  try {
    const page = await searchCandidateUsers({ q, field: 'nickname', match: 'prefix', limit: 30 })
    searchResults.value = page.items
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '搜索用户失败'))
  } finally {
    searchingUsers.value = false
  }
}

function toggleSelect(user: CandidateUser) {
  if (existingSet.value.has(user.id)) return
  const next = new Set(selectedIds.value)
  if (next.has(user.id)) {
    next.delete(user.id)
  } else {
    next.add(user.id)
  }
  selectedIds.value = next
}

async function handleSubmit() {
  const userIds = Array.from(selectedIds.value)
  if (userIds.length === 0) {
    ElMessage.warning('请至少选择 1 位新成员')
    return
  }
  try {
    const res = await groupStore.addMembers(props.groupId, userIds)
    ElMessage.success(`已成功邀请 ${res.members.length} 位成员`)
    emit('update:modelValue', false)
    emit('invited', res)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '邀请成员失败'))
  }
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    title="邀请成员入群"
    width="520px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div class="invite-body">
      <div class="search-row">
        <el-input
          v-model="searchQuery"
          placeholder="搜索平台用户昵称，留空展示好友"
          clearable
          @keyup.enter="handleSearch"
          @clear="searchResults = []"
        >
          <template #prefix>
            <el-icon><Search /></el-icon>
          </template>
        </el-input>
        <el-button :loading="searchingUsers" @click="handleSearch">搜索</el-button>
      </div>

      <div class="summary-bar">
        <span>点击用户加入本次邀请列表</span>
        <span class="summary-count">已选择 {{ selectedIds.size }} 人</span>
      </div>

      <div v-loading="loadingCandidates || searchingUsers" class="candidate-list ios-grouped-section">
        <div
          v-for="user in displayCandidates"
          :key="user.id"
          class="candidate-item"
          :class="{
            'is-existing': existingSet.has(user.id),
            'is-selected': selectedIds.has(user.id),
          }"
          @click="toggleSelect(user)"
        >
          <div class="candidate-left">
            <span class="status-dot" :class="user.online ? 'online' : 'offline'" />
            <div>
              <div class="candidate-name">
                {{ user.remark || user.nickname || user.username }}
              </div>
              <div class="candidate-meta">@{{ user.username }} · ID {{ user.id }}</div>
            </div>
          </div>

          <span v-if="existingSet.has(user.id)" class="member-badge">已在群中</span>
          <el-icon v-else-if="selectedIds.has(user.id)" class="icon-active"><Check /></el-icon>
          <el-icon v-else class="icon-muted"><Plus /></el-icon>
        </div>
        <div v-if="!loadingCandidates && displayCandidates.length === 0" class="empty-box">
          暂无可选用户
        </div>
      </div>
    </div>

    <template #footer>
      <div class="footer-actions">
        <el-button @click="emit('update:modelValue', false)">取消</el-button>
        <el-button
          type="primary"
          :disabled="selectedIds.size === 0"
          :loading="groupStore.submitting"
          @click="handleSubmit"
        >
          确认邀请
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.invite-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.search-row {
  display: flex;
  gap: 8px;
}

.summary-bar {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  color: var(--im-text-secondary);
}

.summary-count {
  font-weight: 600;
  color: var(--im-accent-primary);
}

.candidate-list {
  max-height: 280px;
  overflow-y: auto;
  background-color: var(--im-bg-grouped);
}

.candidate-item {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  background-color: var(--im-bg-surface);
  cursor: pointer;
  transition: background-color 0.12s ease;
}

.candidate-item + .candidate-item::before {
  content: '';
  position: absolute;
  top: 0;
  left: 32px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.candidate-item:hover:not(.is-existing) {
  background-color: var(--im-bg-hover);
}

.candidate-item.is-selected {
  background-color: var(--im-bg-active);
}

.candidate-item.is-existing {
  opacity: 0.55;
  cursor: not-allowed;
}

.candidate-left {
  display: flex;
  align-items: center;
  gap: 10px;
}

.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
}

.status-dot.online {
  background-color: var(--im-status-online);
}

.status-dot.offline {
  background-color: var(--im-status-offline);
}

.candidate-name {
  font-size: 14px;
  font-weight: 500;
  color: var(--im-text-primary);
}

.candidate-meta {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.member-badge {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.icon-active {
  color: var(--im-accent-primary);
}

.icon-muted {
  color: var(--im-text-tertiary);
}

.empty-box {
  padding: 24px;
  text-align: center;
  color: var(--im-text-secondary);
  font-size: 13px;
}

.footer-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
