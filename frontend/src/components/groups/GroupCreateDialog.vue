<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Check, Close, Plus, Search, Upload, UserFilled } from '@element-plus/icons-vue'
import { extractErrorMessage, listFriendCandidates, searchCandidateUsers } from '@/api/group'
import { useAuthorizedMediaSrc } from '@/composables/chat/useAuthorizedMedia'
import { useAuthStore } from '@/stores/auth'
import { useGroupStore } from '@/stores/group'
import type { CandidateUser, CreateGroupResult } from '@/types/group'

const props = defineProps<{
  modelValue: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'created', result: CreateGroupResult): void
}>()

const authStore = useAuthStore()
const groupStore = useGroupStore()

const name = ref('')
const announcement = ref('')
const avatarAssetId = ref<string | null>(null)
const avatarPreviewUrl = ref('')
const { resolvedUrl: resolvedAvatarPreviewUrl } = useAuthorizedMediaSrc(avatarPreviewUrl)
const uploadingAvatar = ref(false)

const searchQuery = ref('')
const friends = ref<CandidateUser[]>([])
const searchResults = ref<CandidateUser[]>([])
const loadingCandidates = ref(false)
const searchingUsers = ref(false)
const selectedUsers = ref<Map<string, CandidateUser>>(new Map())

const fileInputRef = ref<HTMLInputElement | null>(null)

const currentUserId = computed(() => {
  const user = (authStore as { user?: { id?: string } | null }).user
  return user?.id ? String(user.id) : ''
})

const displayCandidates = computed<CandidateUser[]>(() => {
  const list = searchQuery.value.trim() ? searchResults.value : friends.value
  return list.filter((u) => u.id !== currentUserId.value)
})

const selectedList = computed<CandidateUser[]>(() => Array.from(selectedUsers.value.values()))
const canSubmit = computed(
  () => name.value.trim().length > 0 && selectedList.value.length >= 1 && !groupStore.submitting,
)

watch(
  () => props.modelValue,
  (visible) => {
    if (visible) {
      resetForm()
      void loadInitialFriends()
    }
  },
)

function resetForm() {
  name.value = ''
  announcement.value = ''
  avatarAssetId.value = null
  avatarPreviewUrl.value = ''
  searchQuery.value = ''
  searchResults.value = []
  selectedUsers.value = new Map()
}

async function loadInitialFriends() {
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

async function handleSearchUsers() {
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

function toggleCandidate(user: CandidateUser) {
  if (user.id === currentUserId.value) return
  const next = new Map(selectedUsers.value)
  if (next.has(user.id)) {
    next.delete(user.id)
  } else {
    next.set(user.id, user)
  }
  selectedUsers.value = next
}

function removeSelected(userId: string) {
  const next = new Map(selectedUsers.value)
  next.delete(userId)
  selectedUsers.value = next
}

function triggerAvatarPicker() {
  fileInputRef.value?.click()
}

async function handleAvatarChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return

  uploadingAvatar.value = true
  try {
    const asset = await groupStore.uploadAvatar(file)
    avatarAssetId.value = asset.asset_id
    avatarPreviewUrl.value = asset.thumbnail_url || asset.url
    ElMessage.success('群头像上传成功')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '群头像上传失败'))
  } finally {
    uploadingAvatar.value = false
  }
}

function clearAvatar() {
  avatarAssetId.value = null
  avatarPreviewUrl.value = ''
}

async function handleSubmit() {
  const trimmedName = name.value.trim()
  if (!trimmedName) {
    ElMessage.warning('请输入群名称')
    return
  }
  if (selectedList.value.length === 0) {
    ElMessage.warning('除创建者外至少选择 1 位群成员')
    return
  }

  try {
    const result = await groupStore.createGroupAction({
      name: trimmedName,
      member_ids: selectedList.value.map((u) => u.id),
      avatar_asset_id: avatarAssetId.value,
      announcement: announcement.value.trim() ? announcement.value.trim() : null,
    })
    ElMessage.success('群聊创建成功')
    emit('update:modelValue', false)
    emit('created', result)
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '创建群聊失败'))
  }
}

function closeDialog() {
  emit('update:modelValue', false)
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    title="创建群聊"
    width="620px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div class="create-group-form">
      <div class="form-top-row">
        <div class="avatar-uploader">
          <div class="avatar-preview">
            <img
              v-if="resolvedAvatarPreviewUrl"
              :src="resolvedAvatarPreviewUrl"
              alt="群头像"
              class="avatar-img"
            />
            <el-icon v-else :size="24" class="avatar-placeholder-icon"><UserFilled /></el-icon>
          </div>
          <div class="avatar-actions">
            <input
              ref="fileInputRef"
              type="file"
              accept="image/*"
              class="hidden-file-input"
              @change="handleAvatarChange"
            />
            <el-button
              size="small"
              type="primary"
              plain
              :loading="uploadingAvatar"
              :icon="Upload"
              @click="triggerAvatarPicker"
            >
              上传群头像
            </el-button>
            <el-button v-if="avatarAssetId" size="small" link @click="clearAvatar">移除</el-button>
          </div>
        </div>

        <div class="basic-fields">
          <div class="field-label">群名称 <span class="required-mark">*</span></div>
          <el-input
            v-model="name"
            maxlength="100"
            show-word-limit
            placeholder="请输入群名称（1-100 字）"
          />
          <div class="field-label mt-12">群公告（可选）</div>
          <el-input
            v-model="announcement"
            type="textarea"
            :rows="2"
            maxlength="2000"
            placeholder="输入初始群公告"
          />
        </div>
      </div>

      <div class="member-picker">
        <div class="picker-header">
          <span class="field-label">选择初始成员（除本人外至少 1 人）</span>
          <span class="selected-counter">已选 {{ selectedList.length }} 人</span>
        </div>

        <div v-if="selectedList.length > 0" class="selected-tags">
          <span v-for="user in selectedList" :key="user.id" class="selected-chip">
            {{ user.remark || user.nickname || user.username }}
            <button type="button" class="chip-remove" @click="removeSelected(user.id)">
              <el-icon :size="12"><Close /></el-icon>
            </button>
          </span>
        </div>

        <div class="search-bar">
          <el-input
            v-model="searchQuery"
            placeholder="按昵称搜索平台用户，留空显示好友列表"
            clearable
            @keyup.enter="handleSearchUsers"
            @clear="searchResults = []"
          >
            <template #prefix>
              <el-icon><Search /></el-icon>
            </template>
          </el-input>
          <el-button :loading="searchingUsers" @click="handleSearchUsers">搜索</el-button>
        </div>

        <div v-loading="loadingCandidates || searchingUsers" class="candidate-list ios-grouped-section">
          <div
            v-for="user in displayCandidates"
            :key="user.id"
            class="candidate-row"
            :class="{ 'is-selected': selectedUsers.has(user.id) }"
            @click="toggleCandidate(user)"
          >
            <div class="candidate-info">
              <span class="status-dot" :class="user.online ? 'online' : 'offline'" />
              <div class="candidate-text">
                <div class="candidate-name">
                  {{ user.remark || user.nickname || user.username }}
                </div>
                <div class="candidate-sub">@{{ user.username }} · ID {{ user.id }}</div>
              </div>
            </div>
            <el-icon v-if="selectedUsers.has(user.id)" class="check-icon"><Check /></el-icon>
            <el-icon v-else class="add-icon"><Plus /></el-icon>
          </div>
          <div v-if="!loadingCandidates && displayCandidates.length === 0" class="empty-hint">
            {{ searchQuery.trim() ? '未找到匹配用户' : '暂无可选好友，可使用上方搜索框查找用户' }}
          </div>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="dialog-footer">
        <el-button @click="closeDialog">取消</el-button>
        <el-button
          type="primary"
          :disabled="!canSubmit"
          :loading="groupStore.submitting"
          @click="handleSubmit"
        >
          创建群聊
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.create-group-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.form-top-row {
  display: flex;
  gap: 16px;
  align-items: flex-start;
}

.avatar-uploader {
  width: 112px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}

.avatar-preview {
  width: 72px;
  height: 72px;
  border-radius: 16px;
  background-color: var(--im-bg-grouped);
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
}

.avatar-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.avatar-placeholder-icon {
  color: var(--im-accent-primary);
}

.avatar-actions {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
}

.hidden-file-input {
  display: none;
}

.basic-fields {
  flex: 1;
  min-width: 0;
}

.field-label {
  font-size: 13px;
  font-weight: 500;
  color: var(--im-text-secondary);
  margin-bottom: 6px;
}

.required-mark {
  color: var(--im-status-danger);
}

.mt-12 {
  margin-top: 12px;
}

.member-picker {
  border-top: 0.5px solid var(--im-border-light);
  padding-top: 14px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.picker-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.selected-counter {
  font-size: 12px;
  font-weight: 600;
  color: var(--im-accent-primary);
}

.selected-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.selected-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 4px 10px;
  font-size: 12px;
  font-weight: 500;
  color: var(--im-accent-primary);
  background-color: var(--im-accent-primary-soft);
  border-radius: 9999px;
}

.chip-remove {
  border: none;
  background: transparent;
  padding: 0;
  cursor: pointer;
  color: var(--im-accent-primary);
  display: inline-flex;
  align-items: center;
}

.search-bar {
  display: flex;
  gap: 8px;
}

.candidate-list {
  max-height: 220px;
  overflow-y: auto;
  background-color: var(--im-bg-grouped);
}

.candidate-row {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  background-color: var(--im-bg-surface);
  cursor: pointer;
  transition: background-color 0.12s ease;
}

.candidate-row + .candidate-row::before {
  content: '';
  position: absolute;
  top: 0;
  left: 32px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.candidate-row:hover {
  background-color: var(--im-bg-hover);
}

.candidate-row.is-selected {
  background-color: var(--im-bg-active);
}

.candidate-info {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex-shrink: 0;
}

.status-dot.online {
  background-color: var(--im-status-online);
}

.status-dot.offline {
  background-color: var(--im-status-offline);
}

.candidate-text {
  min-width: 0;
}

.candidate-name {
  font-size: 14px;
  font-weight: 500;
  color: var(--im-text-primary);
}

.candidate-sub {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.check-icon {
  color: var(--im-accent-primary);
}

.add-icon {
  color: var(--im-text-tertiary);
}

.empty-hint {
  padding: 20px;
  text-align: center;
  font-size: 13px;
  color: var(--im-text-secondary);
}

.dialog-footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
