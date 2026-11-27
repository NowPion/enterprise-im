<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ChatDotRound, Check, Close, Delete, Edit } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import AppAvatar from '@/components/common/AppAvatar.vue'
import { useContactStore } from '@/stores/contact'
import { isApiError } from '@/types/common'
import type { FriendItem } from '@/types/friend'

interface Props {
  friend: FriendItem
}

const props = defineProps<Props>()
const emit = defineEmits<{
  deleted: [friendId: string]
}>()

const router = useRouter()
const contactStore = useContactStore()

const editingRemark = ref(false)
const remarkDraft = ref('')
const savingRemark = ref(false)
const deleting = ref(false)

watch(
  () => props.friend.friend_id,
  () => {
    editingRemark.value = false
    remarkDraft.value = props.friend.remark
  },
  { immediate: true },
)

const displayName = computed(() => props.friend.remark || props.friend.user.nickname)

const formattedLastOnline = computed(() => {
  const raw = props.friend.user.last_online_at
  if (!raw) return '暂无记录'
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return raw
  return date.toLocaleString('zh-CN', { hour12: false })
})

function startEditRemark() {
  remarkDraft.value = props.friend.remark
  editingRemark.value = true
}

async function saveRemark() {
  const nextRemark = remarkDraft.value.trim()
  if (nextRemark.length > 64) {
    ElMessage.warning('备注最多 64 个字符')
    return
  }
  savingRemark.value = true
  try {
    await contactStore.updateRemark(props.friend.friend_id, nextRemark)
    editingRemark.value = false
    ElMessage.success('备注已更新')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '更新备注失败')
  } finally {
    savingRemark.value = false
  }
}

function handleStartChat() {
  const convId =
    props.friend.conversation_id || contactStore.conversationIdMap[props.friend.friend_id]
  if (convId) {
    void router.push(`/chat/${convId}`)
  } else {
    void router.push('/chat')
  }
}

async function handleDeleteFriend() {
  try {
    await ElMessageBox.confirm(
      `确定要删除好友「${displayName.value}」吗？删除后将解除好友关系并隐藏会话。`,
      '删除好友',
      {
        confirmButtonText: '确认删除',
        cancelButtonText: '取消',
        type: 'warning',
      },
    )
  } catch {
    return
  }

  deleting.value = true
  try {
    await contactStore.removeFriend(props.friend.friend_id)
    ElMessage.success('已删除好友')
    emit('deleted', props.friend.friend_id)
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '删除好友失败')
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <div class="friend-detail">
    <div class="friend-detail__stack">
      <div class="ios-grouped-section friend-detail__card">
        <div class="friend-detail__header">
          <AppAvatar
            :src="friend.user.avatar_url"
            :name="displayName"
            :size="72"
            :show-online="true"
            :online="friend.online"
            rounded="full"
          />
          <div class="friend-detail__identity">
            <div class="friend-detail__name-row">
              <h2 class="friend-detail__name">{{ displayName }}</h2>
              <el-tag size="small" :type="friend.online ? 'success' : 'info'" effect="plain">
                {{ friend.online ? '在线' : '离线' }}
              </el-tag>
            </div>
            <p class="friend-detail__sub">昵称：{{ friend.user.nickname }}</p>
            <p class="friend-detail__sub">用户名：@{{ friend.user.username }}</p>
          </div>
        </div>

        <div class="friend-detail__actions">
          <el-button type="primary" :icon="ChatDotRound" @click="handleStartChat">
            发消息
          </el-button>
          <el-button
            type="danger"
            plain
            :icon="Delete"
            :loading="deleting"
            @click="handleDeleteFriend"
          >
            删除好友
          </el-button>
        </div>
      </div>

      <div class="ios-grouped-section friend-detail__meta">
        <div class="meta-row">
          <span class="meta-row__label">好友备注</span>
          <div class="meta-row__value">
            <template v-if="editingRemark">
              <div class="remark-editor">
                <el-input
                  v-model="remarkDraft"
                  size="small"
                  maxlength="64"
                  placeholder="输入备注，留空则清除"
                  @keyup.enter="saveRemark"
                />
                <el-button
                  type="primary"
                  size="small"
                  :icon="Check"
                  :loading="savingRemark"
                  @click="saveRemark"
                >
                  保存
                </el-button>
                <el-button size="small" :icon="Close" @click="editingRemark = false">
                  取消
                </el-button>
              </div>
            </template>
            <template v-else>
              <span>{{ friend.remark || '未设置备注' }}</span>
              <el-button link type="primary" :icon="Edit" @click="startEditRemark">
                修改备注
              </el-button>
            </template>
          </div>
        </div>

        <div class="meta-row">
          <span class="meta-row__label">个性签名</span>
          <span class="meta-row__value">{{ friend.user.signature || '暂无个性签名' }}</span>
        </div>

        <div class="meta-row">
          <span class="meta-row__label">最近在线</span>
          <span class="meta-row__value">
            {{ friend.online ? '当前在线' : formattedLastOnline }}
          </span>
        </div>

        <div class="meta-row">
          <span class="meta-row__label">单聊会话 ID</span>
          <span class="meta-row__value mono">{{ friend.conversation_id || '暂无' }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.friend-detail {
  padding: 28px 24px;
  display: flex;
  justify-content: center;
  overflow-y: auto;
  height: 100%;
}

.friend-detail__stack {
  width: 100%;
  max-width: 560px;
  align-self: flex-start;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.friend-detail__card {
  padding: 22px 24px;
}

.friend-detail__header {
  display: flex;
  align-items: center;
  gap: 16px;
}

.friend-detail__identity {
  min-width: 0;
}

.friend-detail__name-row {
  display: flex;
  align-items: center;
  gap: 10px;
}

.friend-detail__name {
  margin: 0;
  font-size: 22px;
  font-weight: 700;
  letter-spacing: -0.01em;
  color: var(--im-text-primary);
}

.friend-detail__sub {
  margin: 3px 0 0;
  font-size: 14px;
  color: var(--im-text-secondary);
}

.friend-detail__actions {
  margin-top: 20px;
  display: flex;
  gap: 10px;
}

.friend-detail__meta {
  display: flex;
  flex-direction: column;
}

.meta-row {
  position: relative;
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding: 13px 18px;
}

.meta-row + .meta-row::before {
  content: '';
  position: absolute;
  top: 0;
  left: 18px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.meta-row__label {
  width: 96px;
  flex-shrink: 0;
  font-size: 14px;
  color: var(--im-text-secondary);
  line-height: 24px;
}

.meta-row__value {
  flex: 1;
  min-width: 0;
  font-size: 15px;
  color: var(--im-text-primary);
  line-height: 24px;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  word-break: break-word;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 13px;
}

.remark-editor {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}
</style>
