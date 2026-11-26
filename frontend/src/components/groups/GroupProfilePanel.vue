<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ChatDotRound, Delete, Switch, Upload, UserFilled } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import { useGroupPermissions } from '@/composables/groups/useGroupPermissions'
import { formatUtcTimestamp } from '@/composables/admin/useAdminAccess'
import { useGroupStore } from '@/stores/group'
import type { GroupInfo, GroupMember, GroupRole } from '@/types/group'

const props = defineProps<{
  group: GroupInfo
  myRole: GroupRole | null
  members: GroupMember[]
  memberCount: number
}>()

const emit = defineEmits<{
  (e: 'enter-chat', conversationId: string): void
  (e: 'left-group'): void
}>()

const groupStore = useGroupStore()
const groupRef = computed(() => props.group)
const roleRef = computed(() => props.myRole)

const {
  currentUserId,
  isDisbanded,
  canEditGroupInfo,
  canDisbandGroup,
  canTransferOwnership,
  canLeaveGroup,
  canEditMyNickname,
} = useGroupPermissions(groupRef, roleRef)

const editName = ref('')
const editAnnouncement = ref('')
const myNickname = ref('')
const uploadingAvatar = ref(false)
const fileInputRef = ref<HTMLInputElement | null>(null)

const transferDialogVisible = ref(false)
const transferTargetId = ref('')

const transferCandidates = computed(() =>
  props.members.filter((m) => m.user_id !== currentUserId.value && m.role !== 'owner'),
)

watch(
  () => [props.group.id, props.group.name, props.group.announcement],
  () => {
    editName.value = props.group.name
    editAnnouncement.value = props.group.announcement ?? ''
  },
  { immediate: true },
)

watch(
  () => [props.members, currentUserId.value],
  () => {
    const me = props.members.find((m) => m.user_id === currentUserId.value)
    if (me) {
      myNickname.value = me.nickname_in_group
    }
  },
  { immediate: true },
)

function triggerAvatarUpload() {
  fileInputRef.value?.click()
}

async function handleAvatarSelected(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return

  uploadingAvatar.value = true
  try {
    const asset = await groupStore.uploadAvatar(file)
    await groupStore.updateProfile(props.group.id, {
      avatar_asset_id: asset.asset_id,
    })
    ElMessage.success('群头像已更新')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '更新群头像失败'))
  } finally {
    uploadingAvatar.value = false
  }
}

async function handleClearAvatar() {
  try {
    await groupStore.updateProfile(props.group.id, { avatar_asset_id: '' })
    ElMessage.success('群头像已清除')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '清除群头像失败'))
  }
}

async function handleSaveGroupInfo() {
  const trimmedName = editName.value.trim()
  if (!trimmedName) {
    ElMessage.warning('群名称不能为空')
    return
  }
  const payload: { name?: string; announcement?: string } = {}
  if (trimmedName !== props.group.name) {
    payload.name = trimmedName
  }
  const currentAnn = props.group.announcement ?? ''
  if (editAnnouncement.value !== currentAnn) {
    payload.announcement = editAnnouncement.value
  }
  if (payload.name === undefined && payload.announcement === undefined) {
    ElMessage.info('群名称和公告未发生变化')
    return
  }

  try {
    await groupStore.updateProfile(props.group.id, payload)
    ElMessage.success('群资料已保存')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '保存群资料失败'))
  }
}

async function handleSaveMyNickname() {
  try {
    await groupStore.updateMyNickname(props.group.id, myNickname.value.trim())
    ElMessage.success('我在本群的昵称已更新')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '修改群内昵称失败'))
  }
}

function openTransferDialog() {
  transferTargetId.value = transferCandidates.value[0]?.user_id ?? ''
  transferDialogVisible.value = true
}

async function handleConfirmTransfer() {
  if (!transferTargetId.value) {
    ElMessage.warning('请选择接任群主的成员')
    return
  }
  try {
    await groupStore.transferOwnership(props.group.id, transferTargetId.value)
    transferDialogVisible.value = false
    ElMessage.success('群主已转让')
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '转让群主失败'))
  }
}

async function handleLeaveGroup() {
  try {
    await ElMessageBox.confirm(
      '退出后将不再接收该群新消息与附件，确认退出群聊？',
      '退出群聊',
      {
        confirmButtonText: '确认退出',
        cancelButtonText: '取消',
        type: 'warning',
      },
    )
    await groupStore.leave(props.group.id)
    ElMessage.success('已退出群聊')
    emit('left-group')
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '退出群聊失败'))
    }
  }
}

async function handleDisbandGroup() {
  try {
    await ElMessageBox.confirm(
      '解散后全员将变为只读会话且不可恢复，确认立即解散群聊？',
      '解散群聊',
      {
        confirmButtonText: '确认解散',
        cancelButtonText: '取消',
        type: 'error',
      },
    )
    await groupStore.disband(props.group.id)
    ElMessage.success('群聊已解散')
  } catch (err) {
    if (err !== 'cancel') {
      ElMessage.error(extractErrorMessage(err, '解散群聊失败'))
    }
  }
}
</script>

<template>
  <div class="profile-panel">
    <section class="section-card">
      <div class="section-header">
        <div>
          <h3 class="section-title">群基础信息</h3>
          <p class="section-desc">群组 ID：{{ group.id }} · 会话 ID：{{ group.conversation_id }}</p>
        </div>
        <el-button
          type="primary"
          :icon="ChatDotRound"
          @click="emit('enter-chat', group.conversation_id)"
        >
          进入群聊
        </el-button>
      </div>

      <div class="avatar-row">
        <div class="group-avatar-box">
          <img v-if="group.avatar_url" :src="group.avatar_url" alt="群头像" class="avatar-img" />
          <el-icon v-else :size="28" class="avatar-icon"><UserFilled /></el-icon>
        </div>
        <div class="avatar-controls">
          <div class="meta-grid">
            <div class="meta-item">
              <span class="meta-label">群状态</span>
              <span class="meta-value" :class="isDisbanded ? 'text-danger' : 'text-ok'">
                {{ isDisbanded ? '已解散（只读）' : '正常' }}
              </span>
            </div>
            <div class="meta-item">
              <span class="meta-label">成员规模</span>
              <span class="meta-value">{{ memberCount }} / {{ group.max_members }}</span>
            </div>
            <div class="meta-item">
              <span class="meta-label">群主 ID</span>
              <span class="meta-value mono">{{ group.owner_id }}</span>
            </div>
            <div class="meta-item">
              <span class="meta-label">创建时间</span>
              <span class="meta-value">{{ formatUtcTimestamp(group.created_at) }}</span>
            </div>
          </div>

          <div v-if="canEditGroupInfo" class="avatar-btns">
            <input
              ref="fileInputRef"
              type="file"
              accept="image/*"
              class="hidden-input"
              @change="handleAvatarSelected"
            />
            <el-button
              size="small"
              :icon="Upload"
              :loading="uploadingAvatar"
              @click="triggerAvatarUpload"
            >
              更换群头像
            </el-button>
            <el-button v-if="group.avatar_url" size="small" @click="handleClearAvatar">
              清除头像
            </el-button>
          </div>
        </div>
      </div>

      <div class="form-grid">
        <div class="form-item">
          <label class="field-label">群名称</label>
          <el-input
            v-model="editName"
            :disabled="!canEditGroupInfo"
            maxlength="100"
            placeholder="群名称"
          />
        </div>
        <div class="form-item">
          <label class="field-label">群公告</label>
          <el-input
            v-model="editAnnouncement"
            :disabled="!canEditGroupInfo"
            type="textarea"
            :rows="3"
            maxlength="2000"
            show-word-limit
            placeholder="暂无群公告"
          />
        </div>
        <div v-if="canEditGroupInfo" class="form-actions">
          <el-button
            type="primary"
            :loading="groupStore.submitting"
            @click="handleSaveGroupInfo"
          >
            保存群资料
          </el-button>
        </div>
      </div>
    </section>

    <section class="section-card">
      <h3 class="section-title">我在本群的设置</h3>
      <div class="nickname-row">
        <el-input
          v-model="myNickname"
          :disabled="!canEditMyNickname"
          maxlength="64"
          placeholder="设置我在本群的群昵称（留空恢复默认昵称）"
        />
        <el-button
          :disabled="!canEditMyNickname"
          :loading="groupStore.submitting"
          @click="handleSaveMyNickname"
        >
          保存群昵称
        </el-button>
      </div>
    </section>

    <section class="section-card danger-card">
      <h3 class="section-title">高危与归属操作</h3>
      <p class="section-desc">
        群主须先转让群主后才可退群，或直接解散群聊；普通成员与管理员可主动退出。
      </p>
      <div class="danger-actions">
        <el-button
          v-if="canTransferOwnership"
          :icon="Switch"
          :disabled="transferCandidates.length === 0"
          @click="openTransferDialog"
        >
          转让群主
        </el-button>
        <el-button
          v-if="canLeaveGroup"
          type="danger"
          plain
          :loading="groupStore.submitting"
          @click="handleLeaveGroup"
        >
          退出群聊
        </el-button>
        <el-button
          v-if="canDisbandGroup"
          type="danger"
          :icon="Delete"
          :loading="groupStore.submitting"
          @click="handleDisbandGroup"
        >
          解散群聊
        </el-button>
      </div>
    </section>

    <el-dialog v-model="transferDialogVisible" title="转让群主" width="440px">
      <div class="transfer-body">
        <p class="section-desc">选择一位活跃成员作为新群主，转让后本人将变为普通成员。</p>
        <el-select v-model="transferTargetId" placeholder="请选择目标成员" class="full-width">
          <el-option
            v-for="member in transferCandidates"
            :key="member.user_id"
            :label="`${member.nickname_in_group || member.nickname} (ID ${member.user_id})`"
            :value="member.user_id"
          />
        </el-select>
      </div>
      <template #footer>
        <el-button @click="transferDialogVisible = false">取消</el-button>
        <el-button
          type="primary"
          :disabled="!transferTargetId"
          :loading="groupStore.submitting"
          @click="handleConfirmTransfer"
        >
          确认转让
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.profile-panel {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.section-card {
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 18px 20px;
}

.section-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
  margin-bottom: 16px;
}

.section-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: #111827;
}

.section-desc {
  margin: 4px 0 0;
  font-size: 12px;
  color: #6b7280;
}

.avatar-row {
  display: flex;
  gap: 16px;
  align-items: flex-start;
  padding-bottom: 16px;
  border-bottom: 1px solid #eceef2;
  margin-bottom: 16px;
}

.group-avatar-box {
  width: 72px;
  height: 72px;
  border-radius: 8px;
  border: 1px solid #e5e7eb;
  background-color: #f5f6f8;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
  flex-shrink: 0;
}

.avatar-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.avatar-icon {
  color: #6b7280;
}

.avatar-controls {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.meta-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 10px 16px;
}

.meta-item {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.meta-label {
  font-size: 12px;
  color: #6b7280;
}

.meta-value {
  font-size: 13px;
  color: #111827;
  font-weight: 500;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}

.text-ok {
  color: #16a34a;
}

.text-danger {
  color: #dc2626;
}

.avatar-btns {
  display: flex;
  gap: 8px;
}

.hidden-input {
  display: none;
}

.form-grid {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.form-item {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.field-label {
  font-size: 13px;
  font-weight: 500;
  color: #111827;
}

.form-actions {
  display: flex;
  justify-content: flex-end;
}

.nickname-row {
  margin-top: 12px;
  display: flex;
  gap: 10px;
}

.danger-card {
  border-color: #e5e7eb;
}

.danger-actions {
  margin-top: 12px;
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
}

.transfer-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.full-width {
  width: 100%;
}
</style>
