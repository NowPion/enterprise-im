<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  CircleCheck,
  CopyDocument,
  Document,
  Download,
  InfoFilled,
  Loading,
  Picture,
  RefreshLeft,
  VideoPause,
  VideoPlay,
  WarningFilled,
} from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import AuthorizedAvatar from './AuthorizedAvatar.vue'
import { formatDurationMs, formatFileSize, formatMessageTime } from '@/composables/chat/seq'
import { triggerAuthorizedDownload, useAuthorizedMediaSrc } from '@/composables/chat/useAuthorizedMedia'
import { canRecallMessage, RECALL_WINDOW_MS } from '@/composables/chat/useMessageRetry'
import type { ConversationType } from '@/types/conversation'
import type { ChatMessage } from '@/types/message'

const VOICE_PLAY_EVENT = 'enterprise-im:voice-play'

const props = withDefaults(
  defineProps<{
    message: ChatMessage
    currentUserId: string
    conversationType: ConversationType
    senderName?: string
    senderAvatar?: string
    showSenderInfo?: boolean
  }>(),
  {
    senderName: '',
    senderAvatar: '',
    showSenderInfo: false,
  },
)

const emit = defineEmits<{
  retry: [clientMsgId: string]
  recall: [msgId: string]
  'preview-image': [url: string, title?: string]
  'open-receipts': [msgId: string]
}>()

const isSelf = computed(() => Boolean(props.currentUserId) && props.message.sender_id === props.currentUserId)
const isSystem = computed(() => props.message.msg_type === 'system')
const isRecallRecord = computed(() => props.message.msg_type === 'recall')
const isRecalledOriginal = computed(() => props.message.status === 'recalled')

const extra = computed(() => props.message.extra ?? {})

// Image authorized loading (thumbnail first, fallback to local preview during upload/send).
const imageThumbSrc = computed(() => {
  if (props.message.msg_type !== 'image' || isRecalledOriginal.value) return ''
  return (extra.value.thumbnail_url || extra.value.url || props.message.local_preview_url || '') as string
})
const { resolvedUrl: resolvedImageUrl, loading: imageLoading } = useAuthorizedMediaSrc(
  imageThumbSrc,
  () => props.message.local_preview_url,
)

// Voice authorized loading and playback.
const voiceSrc = computed(() => {
  if (props.message.msg_type !== 'voice' || isRecalledOriginal.value) return ''
  return (extra.value.url || props.message.local_preview_url || '') as string
})
const { resolvedUrl: resolvedVoiceUrl, loading: voiceLoading } = useAuthorizedMediaSrc(
  voiceSrc,
  () => props.message.local_preview_url,
)

const isPlayingVoice = ref(false)
const voiceProgress = ref(0)
let audioEl: HTMLAudioElement | null = null

function stopAudioPlayback() {
  if (audioEl) {
    audioEl.pause()
    audioEl.currentTime = 0
  }
  isPlayingVoice.value = false
  voiceProgress.value = 0
}

function handleGlobalVoicePlay(event: Event) {
  const custom = event as CustomEvent<{ id?: string }>
  const identity = props.message.msg_id || props.message.client_msg_id
  if (custom.detail?.id !== identity && isPlayingVoice.value) {
    stopAudioPlayback()
  }
}

if (typeof window !== 'undefined') {
  window.addEventListener(VOICE_PLAY_EVENT, handleGlobalVoicePlay)
}

async function toggleVoicePlay() {
  if (isPlayingVoice.value) {
    stopAudioPlayback()
    return
  }
  if (!resolvedVoiceUrl.value) {
    ElMessage.warning('语音文件尚未就绪')
    return
  }

  const identity = props.message.msg_id || props.message.client_msg_id
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(VOICE_PLAY_EVENT, { detail: { id: identity } }))
  }

  if (!audioEl || audioEl.src !== resolvedVoiceUrl.value) {
    audioEl = new Audio(resolvedVoiceUrl.value)
    audioEl.addEventListener('timeupdate', () => {
      if (!audioEl || !audioEl.duration || Number.isNaN(audioEl.duration)) return
      voiceProgress.value = Math.min(100, Math.round((audioEl.currentTime / audioEl.duration) * 100))
    })
    audioEl.addEventListener('ended', () => {
      isPlayingVoice.value = false
      voiceProgress.value = 0
    })
    audioEl.addEventListener('error', () => {
      isPlayingVoice.value = false
      voiceProgress.value = 0
      ElMessage.error('语音播放失败')
    })
  }

  try {
    await audioEl.play()
    isPlayingVoice.value = true
  } catch {
    isPlayingVoice.value = false
    ElMessage.error('无法播放该语音')
  }
}

const downloadingFile = ref(false)
async function handleDownloadFile() {
  const url = extra.value.url
  if (!url) return
  downloadingFile.value = true
  try {
    await triggerAuthorizedDownload(url, extra.value.filename || 'attachment')
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '文件下载失败')
  } finally {
    downloadingFile.value = false
  }
}

// Keep recall eligibility reactive until the 2-minute window expires.
const nowMs = ref(Date.now())
let recallTimer: ReturnType<typeof setInterval> | undefined

function setupRecallTimer() {
  clearInterval(recallTimer)
  recallTimer = undefined
  if (!isSelf.value || isRecalledOriginal.value || isSystem.value || isRecallRecord.value) return
  const created = Date.parse(props.message.created_at)
  if (Number.isNaN(created)) return
  if (Date.now() - created <= RECALL_WINDOW_MS) {
    recallTimer = setInterval(() => {
      nowMs.value = Date.now()
      if (nowMs.value - created > RECALL_WINDOW_MS) {
        clearInterval(recallTimer)
        recallTimer = undefined
      }
    }, 5000)
  }
}

watch(
  () => [props.message.status, props.message.local_status, props.message.msg_id] as const,
  () => {
    nowMs.value = Date.now()
    setupRecallTimer()
  },
  { immediate: true },
)

onUnmounted(() => {
  clearInterval(recallTimer)
  stopAudioPlayback()
  audioEl = null
  if (typeof window !== 'undefined') {
    window.removeEventListener(VOICE_PLAY_EVENT, handleGlobalVoicePlay)
  }
})

const recallAllowed = computed(() => canRecallMessage(props.message, props.currentUserId, nowMs.value))

const statusDisplay = computed(() => {
  switch (props.message.local_status) {
    case 'sending':
      return { label: '发送中', cls: 'is-sending' }
    case 'sent':
      return { label: '已发送', cls: 'is-sent' }
    case 'delivered':
      return { label: '已送达', cls: 'is-delivered' }
    case 'read':
      return { label: '已读', cls: 'is-read' }
    case 'failed':
      return { label: '发送失败', cls: 'is-failed' }
    default:
      return { label: '已发送', cls: 'is-sent' }
  }
})

const imageBoxStyle = computed(() => {
  const w = extra.value.width
  const h = extra.value.height
  if (!w || !h) return { width: '200px', height: '150px' }
  const maxW = 240
  const maxH = 220
  const ratio = Math.min(maxW / w, maxH / h, 1)
  return {
    width: `${Math.max(80, Math.round(w * ratio))}px`,
    height: `${Math.max(60, Math.round(h * ratio))}px`,
  }
})

function handleImageClick() {
  const targetUrl = (extra.value.url || props.message.local_preview_url || resolvedImageUrl.value) as string
  if (!targetUrl) return
  emit('preview-image', targetUrl, extra.value.filename || '图片预览')
}

// iOS Bubble Context Menu (Copy / Receipts / Recall)
const contextMenu = ref<{ open: boolean; x: number; y: number }>({
  open: false,
  x: 0,
  y: 0,
})

function openBubbleContextMenu(event: MouseEvent) {
  if (isRecalledOriginal.value) return
  const maxX = typeof window !== 'undefined' ? window.innerWidth - 180 : event.clientX
  const maxY = typeof window !== 'undefined' ? window.innerHeight - 160 : event.clientY
  contextMenu.value = {
    open: true,
    x: Math.max(8, Math.min(event.clientX, maxX)),
    y: Math.max(8, Math.min(event.clientY, maxY)),
  }
}

function closeContextMenu() {
  if (contextMenu.value.open) {
    contextMenu.value.open = false
  }
}

async function copyMessageText() {
  closeContextMenu()
  const content = props.message.content?.trim()
  if (!content) return
  try {
    await navigator.clipboard.writeText(content)
    ElMessage.success('已复制文本')
  } catch {
    ElMessage.error('复制失败')
  }
}

function triggerRecallFromMenu() {
  closeContextMenu()
  if (recallAllowed.value && props.message.msg_id) {
    emit('recall', props.message.msg_id)
  }
}

function triggerReceiptsFromMenu() {
  closeContextMenu()
  if (props.message.msg_id) {
    emit('open-receipts', props.message.msg_id)
  }
}

onMounted(() => {
  if (typeof window !== 'undefined') {
    window.addEventListener('click', closeContextMenu)
    window.addEventListener('scroll', closeContextMenu, true)
  }
})

onBeforeUnmount(() => {
  if (typeof window !== 'undefined') {
    window.removeEventListener('click', closeContextMenu)
    window.removeEventListener('scroll', closeContextMenu, true)
  }
})
</script>

<template>
  <div v-if="isSystem" class="msg-notice-row">
    <span class="msg-notice-pill">
      <el-icon :size="12"><InfoFilled /></el-icon>
      <span>{{ message.content }}</span>
    </span>
  </div>

  <div v-else-if="isRecallRecord" class="msg-notice-row">
    <span class="msg-notice-pill">
      <el-icon :size="12"><RefreshLeft /></el-icon>
      <span>{{ isSelf ? '你撤回了一条消息' : message.content || '撤回了一条消息' }}</span>
    </span>
  </div>

  <div v-else class="msg-row" :class="{ 'is-self': isSelf }">
    <div v-if="!isSelf" class="msg-row__avatar">
      <AuthorizedAvatar
        :src="senderAvatar"
        :name="senderName"
        :size="32"
        shape="circle"
      />
    </div>

    <div class="msg-row__content-col" :class="{ 'is-self': isSelf }">
      <div v-if="showSenderInfo && !isSelf && senderName" class="msg-row__sender">
        {{ senderName }}
      </div>

      <div class="msg-row__bubble-wrap" :class="{ 'is-self': isSelf }">
        <div
          class="msg-bubble"
          :class="[
            `is-${message.msg_type}`,
            { 'is-self': isSelf, 'is-recalled': isRecalledOriginal, 'is-failed': message.local_status === 'failed' },
          ]"
          @contextmenu.prevent="openBubbleContextMenu"
        >
          <template v-if="isRecalledOriginal">
            <span class="msg-bubble__recalled-text">该消息已撤回</span>
          </template>

          <template v-else-if="message.msg_type === 'text'">
            <div class="msg-bubble__text">{{ message.content }}</div>
          </template>

          <template v-else-if="message.msg_type === 'image'">
            <div class="msg-image" :style="imageBoxStyle" @click="handleImageClick">
              <img
                v-if="resolvedImageUrl"
                :src="resolvedImageUrl"
                :alt="extra.filename || '图片消息'"
                class="msg-image__img"
              />
              <div v-else class="msg-image__placeholder">
                <el-icon :size="20" :class="{ 'is-loading': imageLoading }">
                  <component :is="imageLoading ? Loading : Picture" />
                </el-icon>
                <span>{{ imageLoading ? '加载中...' : '图片' }}</span>
              </div>
            </div>
          </template>

          <template v-else-if="message.msg_type === 'file'">
            <div class="msg-file" :class="{ 'is-self': isSelf }">
              <div class="msg-file__icon">
                <el-icon :size="20"><Document /></el-icon>
              </div>
              <div class="msg-file__meta">
                <div class="msg-file__name" :title="extra.filename || message.content">
                  {{ extra.filename || message.content || '附件文件' }}
                </div>
                <div class="msg-file__sub">
                  {{ formatFileSize(extra.size_bytes) }}
                  <span v-if="extra.mime"> · {{ extra.mime }}</span>
                </div>
              </div>
              <el-button
                v-if="extra.url"
                size="small"
                circle
                plain
                :loading="downloadingFile"
                :icon="Download"
                title="下载文件"
                @click="handleDownloadFile"
              />
            </div>
          </template>

          <template v-else-if="message.msg_type === 'voice'">
            <button
              type="button"
              class="msg-voice"
              :disabled="voiceLoading"
              @click="toggleVoicePlay"
            >
              <el-icon :size="18" :class="{ 'is-loading': voiceLoading }">
                <component :is="voiceLoading ? Loading : isPlayingVoice ? VideoPause : VideoPlay" />
              </el-icon>
              <div class="msg-voice__bar">
                <div class="msg-voice__track">
                  <div class="msg-voice__fill" :style="{ width: `${voiceProgress}%` }" />
                </div>
              </div>
              <span class="msg-voice__duration">{{ formatDurationMs(extra.duration_ms) }}</span>
            </button>
          </template>
        </div>

        <div class="msg-row__actions">
          <span class="msg-row__time">{{ formatMessageTime(message.created_at) }}</span>

          <button
            v-if="message.msg_type === 'text' && !isRecalledOriginal && message.content"
            type="button"
            class="msg-action-link"
            @click="copyMessageText"
          >
            复制
          </button>

          <el-popconfirm
            v-if="recallAllowed"
            title="确认撤回这条消息吗？"
            confirm-button-text="撤回"
            cancel-button-text="取消"
            @confirm="emit('recall', message.msg_id)"
          >
            <template #reference>
              <button type="button" class="msg-action-link is-danger">撤回</button>
            </template>
          </el-popconfirm>

          <button
            v-if="isSelf && !isRecalledOriginal && conversationType === 'group' && message.msg_id"
            type="button"
            class="msg-action-link"
            @click="emit('open-receipts', message.msg_id)"
          >
            回执
          </button>
        </div>
      </div>

      <div v-if="isSelf && !isRecalledOriginal" class="msg-row__status" :class="statusDisplay.cls">
        <el-icon v-if="message.local_status === 'sending'" :size="11" class="is-loading">
          <Loading />
        </el-icon>
        <el-icon v-else-if="message.local_status === 'failed'" :size="11"><WarningFilled /></el-icon>

        <span>{{ statusDisplay.label }}</span>

        <span v-if="message.local_status === 'failed' && message.error_message" class="msg-row__err-text">
          · {{ message.error_message }}
        </span>

        <button
          v-if="message.local_status === 'failed' && message.retryable !== false"
          type="button"
          class="msg-retry-btn"
          @click="emit('retry', message.client_msg_id)"
        >
          重试
        </button>
      </div>
    </div>

    <div v-if="isSelf" class="msg-row__avatar">
      <AuthorizedAvatar
        :src="senderAvatar"
        :name="senderName || '我'"
        :size="32"
        shape="circle"
      />
    </div>

    <Teleport to="body">
      <div
        v-if="contextMenu.open"
        class="ios-context-menu"
        :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }"
        @click.stop
      >
        <button
          v-if="message.msg_type === 'text' && message.content"
          type="button"
          class="ios-context-menu__item"
          @click="copyMessageText"
        >
          <span>复制文本</span>
          <el-icon :size="14"><CopyDocument /></el-icon>
        </button>
        <button
          v-if="isSelf && conversationType === 'group' && message.msg_id"
          type="button"
          class="ios-context-menu__item"
          @click="triggerReceiptsFromMenu"
        >
          <span>查看回执</span>
          <el-icon :size="14"><CircleCheck /></el-icon>
        </button>
        <button
          v-if="recallAllowed"
          type="button"
          class="ios-context-menu__item is-danger"
          @click="triggerRecallFromMenu"
        >
          <span>撤回消息</span>
          <el-icon :size="14"><RefreshLeft /></el-icon>
        </button>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.msg-notice-row {
  display: flex;
  justify-content: center;
  margin: 10px 0;
}

.msg-notice-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 3px 10px;
  border-radius: 9999px;
  color: var(--im-text-secondary);
  font-size: 12px;
}

.msg-row {
  display: flex;
  align-items: flex-end;
  gap: 8px;
  margin-bottom: 10px;
  animation: ios-bubble-in 0.16s var(--im-ease-ios);
}

.msg-row.is-self {
  justify-content: flex-end;
}

.msg-row__avatar {
  flex-shrink: 0;
  margin-bottom: 2px;
}

.msg-row__content-col {
  max-width: min(68%, 520px);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
}

.msg-row__content-col.is-self {
  align-items: flex-end;
}

.msg-row__sender {
  font-size: 12px;
  color: var(--im-text-secondary);
  padding-left: 10px;
}

.msg-row__bubble-wrap {
  display: flex;
  align-items: flex-end;
  gap: 6px;
}

.msg-row__bubble-wrap.is-self {
  flex-direction: row-reverse;
}

.msg-bubble {
  padding: 9px 14px;
  border-radius: 18px 18px 18px 5px;
  background-color: var(--im-bubble-peer);
  color: var(--im-bubble-peer-text);
  font-size: 15px;
  line-height: 1.42;
  transition: filter 0.12s ease;
}

.msg-bubble.is-self {
  border-radius: 18px 18px 5px 18px;
  background-color: var(--im-bubble-self);
  color: var(--im-bubble-self-text);
}

.msg-bubble.is-image {
  padding: 3px;
  border-radius: 16px;
  background-color: var(--im-bg-grouped);
}

.msg-bubble.is-file {
  background-color: #f2f2f7;
  color: var(--im-text-primary);
  border: 0.5px solid var(--im-border-light);
}

.msg-bubble.is-recalled {
  background-color: var(--im-bg-fill);
  color: var(--im-text-secondary);
  font-style: italic;
}

.msg-bubble.is-failed {
  box-shadow: 0 0 0 1.5px var(--im-status-danger);
}

.msg-bubble__text {
  white-space: pre-wrap;
  word-break: break-word;
}

.msg-bubble__recalled-text {
  font-size: 13px;
}

.msg-image {
  border-radius: 14px;
  overflow: hidden;
  background-color: var(--im-bg-grouped);
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
}

.msg-image__img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.msg-image__placeholder {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--im-text-secondary);
}

.msg-file {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 220px;
  max-width: 300px;
}

.msg-file__icon {
  width: 38px;
  height: 38px;
  border-radius: 10px;
  background-color: var(--im-accent-primary-soft);
  color: var(--im-accent-primary);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.msg-file__meta {
  flex: 1;
  min-width: 0;
}

.msg-file__name {
  font-size: 14px;
  font-weight: 500;
  color: var(--im-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.msg-file__sub {
  font-size: 12px;
  color: var(--im-text-secondary);
  margin-top: 2px;
}

.msg-voice {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 150px;
  background: transparent;
  border: none;
  padding: 2px 0;
  color: inherit;
  cursor: pointer;
}

.msg-voice__bar {
  flex: 1;
  min-width: 68px;
}

.msg-voice__track {
  height: 4px;
  border-radius: 9999px;
  background-color: rgba(120, 120, 128, 0.25);
  overflow: hidden;
}

.msg-bubble.is-self .msg-voice__track {
  background-color: rgba(255, 255, 255, 0.35);
}

.msg-voice__fill {
  height: 100%;
  background-color: currentColor;
  transition: width 0.1s linear;
}

.msg-voice__duration {
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.msg-row__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}

.msg-row__time {
  font-size: 11px;
  color: var(--im-text-muted);
}

.msg-action-link {
  border: none;
  background: transparent;
  color: var(--im-accent-primary);
  font-size: 11px;
  font-weight: 500;
  padding: 0 3px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s ease;
}

.msg-action-link.is-danger {
  color: var(--im-status-danger);
}

.msg-row:hover .msg-action-link {
  opacity: 1;
}

.msg-action-link:hover {
  text-decoration: underline;
}

/* iOS thin delivery/read caption below self bubble */
.msg-row__status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding-right: 4px;
  font-size: 11px;
  font-weight: 500;
  color: var(--im-text-muted);
}

.msg-row__status.is-read {
  color: var(--im-accent-primary);
}

.msg-row__status.is-delivered {
  color: var(--im-text-secondary);
}

.msg-row__status.is-failed {
  color: var(--im-status-danger);
}

.msg-row__err-text {
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.msg-retry-btn {
  border: none;
  background-color: rgba(255, 59, 48, 0.12);
  color: var(--im-status-danger);
  font-size: 11px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 9999px;
  cursor: pointer;
}

.msg-retry-btn:hover {
  background-color: rgba(255, 59, 48, 0.2);
}
</style>
