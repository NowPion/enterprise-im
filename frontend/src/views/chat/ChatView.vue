<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChatDotRound, Setting } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import AuthorizedAvatar from '@/components/chat/AuthorizedAvatar.vue'
import ChatHeader from '@/components/chat/ChatHeader.vue'
import ConversationList from '@/components/chat/ConversationList.vue'
import MessageComposer from '@/components/chat/MessageComposer.vue'
import MessageList from '@/components/chat/MessageList.vue'
import MessageReceiptsDialog from '@/components/chat/MessageReceiptsDialog.vue'
import { useChatRealtime } from '@/composables/chat/useChatRealtime'
import { isPositiveSeq } from '@/composables/chat/seq'
import { useAuthStore } from '@/stores/auth'
import { useConversationStore } from '@/stores/conversation'
import { useMessageStore } from '@/stores/message'
import type { Conversation, ConversationMember } from '@/types/conversation'
import type { ReceiptFilterStatus } from '@/types/message'

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore() as unknown as {
  user?: { id?: string; nickname?: string; avatar_url?: string } | null
}
const conversationStore = useConversationStore()
const messageStore = useMessageStore()
const { currentUserId, ensureRealtimeConnected } = useChatRealtime()

const membersDrawerVisible = ref(false)
const membersLoading = ref(false)
const detailLoading = ref(false)
const receiptsDialogVisible = ref(false)
const receiptsTargetMsgId = ref<string | null>(null)

const routeConversationId = computed(() => {
  const raw = route.params.conversationId
  if (typeof raw === 'string' && raw.trim()) return raw.trim()
  if (Array.isArray(raw) && raw[0]) return String(raw[0]).trim()
  return null
})

const displayConversations = computed(() =>
  conversationStore.includeHidden
    ? conversationStore.conversations
    : conversationStore.visibleConversations,
)

const activeConversation = computed(() => conversationStore.activeConversation)
const activeMessages = computed(() =>
  routeConversationId.value ? messageStore.getMessages(routeConversationId.value) : [],
)
const activePagination = computed(() =>
  routeConversationId.value
    ? messageStore.getPagination(routeConversationId.value)
    : { hasMoreOlder: false, oldestSeq: null, loadingOlder: false, initialized: false },
)
const activeMembers = computed<ConversationMember[]>(() =>
  routeConversationId.value
    ? conversationStore.membersByConversation[routeConversationId.value] ?? []
    : [],
)
const activeMembersMap = computed<Record<string, ConversationMember>>(() => {
  const map: Record<string, ConversationMember> = {}
  for (const member of activeMembers.value) {
    map[member.user.id] = member
  }
  return map
})

const currentUserName = computed(() => authStore.user?.nickname || '我')
const currentUserAvatar = computed(() => authStore.user?.avatar_url || '')

async function activateConversation(convId: string | null) {
  conversationStore.setActiveConversation(convId)
  if (!convId || !currentUserId.value) {
    conversationStore.setChatVisible(false)
    return
  }

  const visible = typeof document === 'undefined' ? true : document.visibilityState === 'visible'
  conversationStore.setChatVisible(visible)

  detailLoading.value = true
  try {
    await conversationStore.fetchDetail(convId)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '加载会话详情失败')
  } finally {
    detailLoading.value = false
  }

  const pagination = messageStore.getPagination(convId)
  try {
    if (!pagination.initialized) {
      await messageStore.loadInitialMessages(convId, currentUserId.value)
    } else {
      await messageStore.syncConversation(convId, currentUserId.value)
    }
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '加载消息历史失败')
  }

  if (conversationStore.activeDetail?.conversation.type === 'group') {
    void conversationStore.fetchMembers(convId).catch(() => undefined)
  }

  const conv = conversationStore.itemsById[convId]
  if (visible && conv && isPositiveSeq(conv.last_seq)) {
    await messageStore.markRead(convId, conv.last_seq)
  }
}

watch(
  [routeConversationId, currentUserId],
  ([convId, uid]) => {
    if (!uid) return
    void activateConversation(convId)
  },
  { immediate: true },
)

onMounted(async () => {
  ensureRealtimeConnected()
  if (currentUserId.value && conversationStore.conversations.length === 0) {
    try {
      await conversationStore.fetchList()
    } catch (err) {
      ElMessage.error(err instanceof Error ? err.message : '加载会话列表失败')
    }
  }
})

function handleSelectConversation(convId: string) {
  if (routeConversationId.value === convId) return
  void router.push(`/chat/${convId}`)
}

function handleBackToList() {
  void router.push('/chat')
}

async function handleRefreshList() {
  try {
    await conversationStore.fetchList()
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '刷新会话列表失败')
  }
}

async function handleToggleIncludeHidden() {
  try {
    await conversationStore.fetchList({
      includeHidden: !conversationStore.includeHidden,
      reset: true,
    })
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '切换会话筛选失败')
  }
}

async function handleTogglePin(conv: Conversation) {
  try {
    await conversationStore.updateSettings(conv.id, { is_pinned: !conv.is_pinned })
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '更新置顶状态失败')
  }
}

async function handleToggleMute(conv: Conversation) {
  try {
    await conversationStore.updateSettings(conv.id, { is_muted: !conv.is_muted })
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '更新免打扰状态失败')
  }
}

async function handleToggleHide(conv: Conversation) {
  try {
    const wasVisible = conv.is_visible
    await conversationStore.updateSettings(conv.id, { is_visible: !wasVisible })
    if (wasVisible && routeConversationId.value === conv.id) {
      void router.push('/chat')
    }
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '更新会话可见性失败')
  }
}

async function handleLoadOlder() {
  if (!routeConversationId.value || !currentUserId.value) return
  try {
    await messageStore.loadOlderMessages(routeConversationId.value, currentUserId.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '加载更早消息失败')
  }
}

async function handleSendText(content: string) {
  if (!routeConversationId.value || !currentUserId.value) return
  try {
    await messageStore.sendMessage(
      routeConversationId.value,
      { msg_type: 'text', content, extra: {} },
      currentUserId.value,
    )
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '发送消息失败')
  }
}

async function handleSendMedia(payload: {
  msg_type: 'image' | 'file' | 'voice'
  content: string
  extra: Record<string, unknown>
  local_preview_url?: string
}) {
  if (!routeConversationId.value || !currentUserId.value) return
  try {
    await messageStore.sendMessage(routeConversationId.value, payload, currentUserId.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '发送媒体消息失败')
  }
}

async function handleRetryMessage(clientMsgId: string) {
  if (!routeConversationId.value || !currentUserId.value) return
  try {
    await messageStore.retryMessage(routeConversationId.value, clientMsgId, currentUserId.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '重发消息失败')
  }
}

async function handleRecallMessage(msgId: string) {
  if (!routeConversationId.value || !currentUserId.value) return
  try {
    await messageStore.recallMessage(routeConversationId.value, msgId, currentUserId.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '撤回消息失败')
  }
}

async function handleReachBottom() {
  if (!routeConversationId.value) return
  const conv = activeConversation.value
  if (conv && isPositiveSeq(conv.last_seq) && conv.unread_count > 0) {
    await messageStore.markRead(routeConversationId.value, conv.last_seq)
  }
}

async function handleOpenMembers() {
  if (!routeConversationId.value) return
  membersDrawerVisible.value = true
  membersLoading.value = true
  try {
    await conversationStore.fetchMembers(routeConversationId.value)
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '加载成员列表失败')
  } finally {
    membersLoading.value = false
  }
}

function handleOpenGroup(groupId: string) {
  void router.push(`/groups/${groupId}`)
}

function handleOpenReceipts(msgId: string) {
  receiptsTargetMsgId.value = msgId
  receiptsDialogVisible.value = true
}

async function handleLoadReceipts(params: {
  status?: ReceiptFilterStatus
  cursor?: string
  reset?: boolean
}) {
  if (!routeConversationId.value || !receiptsTargetMsgId.value) return
  try {
    await messageStore.loadMessageReceipts(
      routeConversationId.value,
      receiptsTargetMsgId.value,
      params,
    )
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '加载回执详情失败')
  }
}

function roleLabel(role: string | null): string {
  if (role === 'owner') return '群主'
  if (role === 'admin') return '管理员'
  return '成员'
}
</script>

<template>
  <div class="chat-workspace" :class="{ 'has-active': Boolean(routeConversationId) }">
    <div class="chat-workspace__list">
      <ConversationList
        :conversations="displayConversations"
        :active-conversation-id="routeConversationId"
        :loading="conversationStore.loading"
        :loading-more="conversationStore.loadingMore"
        :has-more="conversationStore.hasMore"
        :include-hidden="conversationStore.includeHidden"
        :ws-connected="messageStore.wsConnected"
        @select="handleSelectConversation"
        @load-more="conversationStore.loadMore()"
        @toggle-include-hidden="handleToggleIncludeHidden"
        @toggle-pin="handleTogglePin"
        @toggle-mute="handleToggleMute"
        @toggle-hide="handleToggleHide"
        @refresh="handleRefreshList"
      />
    </div>

    <main class="chat-workspace__detail">
      <template v-if="routeConversationId && activeConversation">
        <ChatHeader
          :conversation="activeConversation"
          :detail="conversationStore.activeDetail"
          :member-count="activeMembers.length"
          :show-back="true"
          @back="handleBackToList"
          @toggle-pin="handleTogglePin(activeConversation)"
          @toggle-mute="handleToggleMute(activeConversation)"
          @open-members="handleOpenMembers"
          @open-group="handleOpenGroup"
        />

        <MessageList
          :conversation-id="routeConversationId"
          :conversation-type="activeConversation.type"
          :messages="activeMessages"
          :current-user-id="currentUserId"
          :current-user-name="currentUserName"
          :current-user-avatar="currentUserAvatar"
          :peer-user="conversationStore.activeDetail?.peer ?? activeConversation.peer"
          :members-map="activeMembersMap"
          :has-more-older="activePagination.hasMoreOlder"
          :loading-older="activePagination.loadingOlder"
          :syncing="Boolean(messageStore.syncingByConversation[routeConversationId])"
          @load-older="handleLoadOlder"
          @retry="handleRetryMessage"
          @recall="handleRecallMessage"
          @open-receipts="handleOpenReceipts"
          @reach-bottom="handleReachBottom"
        />

        <MessageComposer
          :conversation-id="routeConversationId"
          :can-send="activeConversation.can_send"
          :ws-connected="messageStore.wsConnected"
          @send-text="handleSendText"
          @send-media="handleSendMedia"
        />
      </template>

      <div v-else-if="routeConversationId" v-loading="detailLoading" class="chat-workspace__empty">
        <h3>正在加载会话...</h3>
      </div>

      <div v-else class="chat-workspace__empty">
        <el-icon :size="44" class="chat-workspace__empty-icon"><ChatDotRound /></el-icon>
        <h3>选择一个会话开始沟通</h3>
        <p>支持单聊与群聊实时消息、媒体上传、四阶段回执及断线自动补偿</p>
      </div>
    </main>

    <el-drawer
      v-model="membersDrawerVisible"
      title="会话成员"
      size="320px"
      destroy-on-close
    >
      <div v-if="activeConversation?.type === 'group' && activeConversation.group_id" class="members-drawer__top">
        <el-button
          type="primary"
          plain
          size="small"
          :icon="Setting"
          @click="handleOpenGroup(activeConversation.group_id)"
        >
          进入群资料与管理
        </el-button>
      </div>

      <div v-loading="membersLoading" class="members-drawer__list">
        <div
          v-for="member in activeMembers"
          :key="member.user.id"
          class="members-drawer__row"
        >
          <AuthorizedAvatar
            :src="member.user.avatar_url"
            :name="member.nickname_in_group || member.user.nickname"
            :size="34"
            :online="member.online"
            show-online
          />
          <div class="members-drawer__info">
            <div class="members-drawer__name-row">
              <span class="members-drawer__name">
                {{ member.nickname_in_group || member.user.nickname }}
              </span>
              <el-tag v-if="member.role" size="small" effect="plain">
                {{ roleLabel(member.role) }}
              </el-tag>
            </div>
            <div class="members-drawer__sub">
              @{{ member.user.username }} · 已读 #{{ member.last_read_seq }}
            </div>
          </div>
        </div>
      </div>
    </el-drawer>

    <MessageReceiptsDialog
      v-model:visible="receiptsDialogVisible"
      :conversation-id="routeConversationId || ''"
      :msg-id="receiptsTargetMsgId"
      :data="messageStore.receiptsModalData"
      :loading="messageStore.receiptsLoading"
      @load="handleLoadReceipts"
    />
  </div>
</template>

<style scoped>
.chat-workspace {
  display: flex;
  width: 100%;
  height: 100%;
  min-height: 0;
  background-color: #ffffff;
  overflow: hidden;
}

.chat-workspace__list {
  display: flex;
  height: 100%;
  min-height: 0;
}

.chat-workspace__detail {
  flex: 1;
  min-width: 0;
  height: 100%;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background-color: #f5f6f8;
}

.chat-workspace__empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 24px;
  color: #6b7280;
  text-align: center;
}

.chat-workspace__empty h3 {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: #111827;
}

.chat-workspace__empty p {
  margin: 0;
  font-size: 13px;
}

.chat-workspace__empty-icon {
  color: #9ca3af;
}

.members-drawer__top {
  margin-bottom: 12px;
}

.members-drawer__list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.members-drawer__row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 10px;
  border-bottom: 1px solid #f0f2f5;
}

.members-drawer__info {
  flex: 1;
  min-width: 0;
}

.members-drawer__name-row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.members-drawer__name {
  font-size: 13px;
  font-weight: 500;
  color: #111827;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.members-drawer__sub {
  font-size: 11px;
  color: #6b7280;
  margin-top: 2px;
}

@media (max-width: 768px) {
  .chat-workspace__list {
    width: 100%;
  }

  .chat-workspace.has-active .chat-workspace__list {
    display: none;
  }

  .chat-workspace:not(.has-active) .chat-workspace__detail {
    display: none;
  }
}
</style>
