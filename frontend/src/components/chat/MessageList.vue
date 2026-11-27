<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ArrowDown, Download, Loading, RefreshRight } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import MessageBubble from './MessageBubble.vue'
import { formatDividerTime } from '@/composables/chat/seq'
import { triggerAuthorizedDownload, useAuthorizedMediaSrc } from '@/composables/chat/useAuthorizedMedia'
import { useScrollPagination } from '@/composables/chat/useScrollPagination'
import type { ChatUser, ConversationMember, ConversationType } from '@/types/conversation'
import type { ChatMessage } from '@/types/message'

const props = defineProps<{
  conversationId: string
  conversationType: ConversationType
  messages: ChatMessage[]
  currentUserId: string
  currentUserName?: string
  currentUserAvatar?: string
  peerUser: ChatUser | null
  membersMap: Record<string, ConversationMember>
  hasMoreOlder: boolean
  loadingOlder: boolean
  syncing: boolean
}>()

const emit = defineEmits<{
  'load-older': []
  retry: [clientMsgId: string]
  recall: [msgId: string]
  'open-receipts': [msgId: string]
  'reach-bottom': []
}>()

const scrollContainerRef = ref<HTMLElement | null>(null)

const {
  unreadBelowCount,
  scrollToBottom,
  handleScroll,
  notifyIncomingBottomOrHold,
  resetScrollState,
} = useScrollPagination(scrollContainerRef, {
  hasMoreOlder: () => props.hasMoreOlder,
  isLoadingOlder: () => props.loadingOlder,
  onLoadOlder: async () => {
    emit('load-older')
  },
  onReachBottom: () => {
    emit('reach-bottom')
  },
})

let prependSnapshot: { scrollHeight: number; scrollTop: number } | null = null

watch(
  () => props.loadingOlder,
  (isLoading, wasLoading) => {
    const el = scrollContainerRef.value
    if (!el) return
    if (isLoading && !wasLoading) {
      prependSnapshot = {
        scrollHeight: el.scrollHeight,
        scrollTop: el.scrollTop,
      }
    }
  },
  { flush: 'sync' },
)

watch(
  () => props.loadingOlder,
  (isLoading, wasLoading) => {
    const el = scrollContainerRef.value
    if (!el || !wasLoading || isLoading || !prependSnapshot) return
    const delta = el.scrollHeight - prependSnapshot.scrollHeight
    el.scrollTop = Math.max(0, prependSnapshot.scrollTop + delta)
    prependSnapshot = null
  },
  { flush: 'post' },
)

function handleManualLoadOlder() {
  if (props.loadingOlder || !props.hasMoreOlder) return
  emit('load-older')
}

watch(
  () => props.conversationId,
  () => {
    resetScrollState()
    void scrollToBottom(false)
  },
  { immediate: true },
)

watch(
  () => {
    const last = props.messages[props.messages.length - 1]
    return [props.messages.length, last?.msg_id ?? '', last?.client_msg_id ?? ''] as const
  },
  ([nextLen, nextMsgId, nextClientId], [prevLen, prevMsgId, prevClientId]) => {
    if (nextLen === 0) return
    if (!prevLen) {
      void scrollToBottom(false)
      return
    }
    const appendedAtTail =
      nextLen > prevLen && (nextMsgId !== prevMsgId || nextClientId !== prevClientId)
    if (appendedAtTail) {
      const last = props.messages[props.messages.length - 1]
      notifyIncomingBottomOrHold(last?.sender_id === props.currentUserId)
    }
  },
)

const DIVIDER_GAP_MS = 5 * 60 * 1000

const renderedRows = computed(() => {
  let lastShownTs = 0
  return props.messages.map((msg) => {
    const ts = Date.parse(msg.created_at) || 0
    const showDivider = ts > 0 && (lastShownTs === 0 || ts - lastShownTs >= DIVIDER_GAP_MS)
    if (showDivider) {
      lastShownTs = ts
    }
    return {
      key: msg.msg_id || msg.client_msg_id,
      msg,
      dividerLabel: showDivider ? formatDividerTime(msg.created_at) : '',
    }
  })
})

function resolveSenderName(msg: ChatMessage): string {
  if (msg.sender_id === props.currentUserId) {
    return props.currentUserName || '我'
  }
  const member = props.membersMap[msg.sender_id]
  if (member) {
    return member.nickname_in_group || member.user.nickname || member.user.username
  }
  if (props.peerUser && props.peerUser.id === msg.sender_id) {
    return props.peerUser.nickname || props.peerUser.username
  }
  return `成员 ${msg.sender_id}`
}

function resolveSenderAvatar(msg: ChatMessage): string {
  if (msg.sender_id === props.currentUserId) {
    return props.currentUserAvatar || ''
  }
  const member = props.membersMap[msg.sender_id]
  if (member) {
    return member.user.avatar_url
  }
  if (props.peerUser && props.peerUser.id === msg.sender_id) {
    return props.peerUser.avatar_url
  }
  return ''
}

// Authorized image lightbox state.
const previewVisible = ref(false)
const previewRawUrl = ref('')
const previewTitle = ref('图片预览')
const { resolvedUrl: previewResolvedUrl, loading: previewLoading } = useAuthorizedMediaSrc(previewRawUrl)

function openImagePreview(url: string, title?: string) {
  previewRawUrl.value = url
  previewTitle.value = title || '图片预览'
  previewVisible.value = true
}

async function downloadPreviewImage() {
  if (!previewRawUrl.value) return
  try {
    await triggerAuthorizedDownload(previewRawUrl.value, previewTitle.value || 'image')
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '图片下载失败')
  }
}
</script>

<template>
  <div class="msg-list-wrap">
    <div
      ref="scrollContainerRef"
      class="msg-list"
      @scroll="handleScroll"
    >
      <div class="msg-list__top-bar">
        <el-button
          v-if="hasMoreOlder"
          text
          type="primary"
          size="small"
          :loading="loadingOlder"
          @click="handleManualLoadOlder"
        >
          {{ loadingOlder ? '加载历史消息中...' : '加载更早消息' }}
        </el-button>
        <span v-else-if="messages.length > 0" class="msg-list__no-more">已显示全部历史消息</span>

        <span v-if="syncing" class="msg-list__syncing">
          <el-icon :size="12" class="is-loading"><RefreshRight /></el-icon>
          正在同步增量消息...
        </span>
      </div>

      <div v-if="messages.length === 0 && !loadingOlder" class="msg-list__empty">
        暂无消息记录，发送第一条消息开始对话
      </div>

      <template v-for="row in renderedRows" :key="row.key">
        <div v-if="row.dividerLabel" class="msg-list__divider">
          <span>{{ row.dividerLabel }}</span>
        </div>

        <MessageBubble
          :message="row.msg"
          :current-user-id="currentUserId"
          :conversation-type="conversationType"
          :sender-name="resolveSenderName(row.msg)"
          :sender-avatar="resolveSenderAvatar(row.msg)"
          :show-sender-info="conversationType === 'group'"
          @retry="(cid) => emit('retry', cid)"
          @recall="(mid) => emit('recall', mid)"
          @preview-image="openImagePreview"
          @open-receipts="(mid) => emit('open-receipts', mid)"
        />
      </template>
    </div>

    <button
      v-if="unreadBelowCount > 0"
      type="button"
      class="msg-list__jump-btn"
      @click="scrollToBottom(true)"
    >
      <el-icon :size="13"><ArrowDown /></el-icon>
      <span>{{ unreadBelowCount }} 条新消息</span>
    </button>

    <el-dialog
      v-model="previewVisible"
      :title="previewTitle"
      width="720px"
      align-center
      destroy-on-close
    >
      <div v-loading="previewLoading" class="msg-preview-box">
        <img
          v-if="previewResolvedUrl"
          :src="previewResolvedUrl"
          :alt="previewTitle"
          class="msg-preview-box__img"
        />
        <div v-else class="msg-preview-box__loading">
          <el-icon :size="24" class="is-loading"><Loading /></el-icon>
        </div>
      </div>
      <template #footer>
        <el-button :icon="Download" @click="downloadPreviewImage">下载原图</el-button>
        <el-button type="primary" @click="previewVisible = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.msg-list-wrap {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background-color: var(--im-bg-surface);
}

.msg-list {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 16px 20px;
}

.msg-list__top-bar {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  margin-bottom: 10px;
}

.msg-list__no-more {
  font-size: 12px;
  color: var(--im-text-muted);
}

.msg-list__syncing {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: var(--im-text-secondary);
  background-color: var(--im-bg-grouped);
  border-radius: 9999px;
  padding: 3px 12px;
}

.msg-list__empty {
  text-align: center;
  color: var(--im-text-secondary);
  font-size: 14px;
  padding: 72px 16px;
}

.msg-list__divider {
  display: flex;
  justify-content: center;
  margin: 14px 0 10px;
}

.msg-list__divider span {
  font-size: 11px;
  font-weight: 500;
  color: var(--im-text-secondary);
}

.msg-list__jump-btn {
  position: absolute;
  right: 20px;
  bottom: 16px;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 7px 14px;
  border-radius: 9999px;
  border: 0.5px solid var(--im-border-light);
  background-color: rgba(255, 255, 255, 0.92);
  backdrop-filter: blur(18px);
  color: var(--im-accent-primary);
  font-size: 13px;
  font-weight: 600;
  box-shadow: var(--im-shadow-md);
  cursor: pointer;
}

.msg-list__jump-btn:hover {
  background-color: #ffffff;
}

.msg-preview-box {
  min-height: 240px;
  max-height: 70vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background-color: var(--im-bg-grouped);
  border-radius: 12px;
  overflow: hidden;
}

.msg-preview-box__img {
  max-width: 100%;
  max-height: 68vh;
  object-fit: contain;
  display: block;
}

.msg-preview-box__loading {
  padding: 48px;
  color: var(--im-text-secondary);
}
</style>
