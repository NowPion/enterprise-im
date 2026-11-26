<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import {
  Check,
  CircleCheck,
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
</script>

<template>
  <div v-if="isSystem" class="msg-notice-row">
    <span class="msg-notice-pill">
      <el-icon :size="13"><InfoFilled /></el-icon>
      <span>{{ message.content }}</span>
    </span>
  </div>

  <div v-else-if="isRecallRecord" class="msg-notice-row">
    <span class="msg-notice-pill">
      <el-icon :size="13"><RefreshLeft /></el-icon>
      <span>{{ isSelf ? '你撤回了一条消息' : message.content || '撤回了一条消息' }}</span>
    </span>
  </div>

  <div v-else class="msg-row" :class="{ 'is-self': isSelf }">
    <div v-if="!isSelf" class="msg-row__avatar">
      <AuthorizedAvatar
        :src="senderAvatar"
        :name="senderName"
        :size="34"
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
            <div class="msg-file">
              <div class="msg-file__icon">
                <el-icon :size="22"><Document /></el-icon>
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

          <el-popconfirm
            v-if="recallAllowed"
            title="确认撤回这条消息吗？"
            confirm-button-text="撤回"
            cancel-button-text="取消"
            @confirm="emit('recall', message.msg_id)"
          >
            <template #reference>
              <button type="button" class="msg-action-link">撤回</button>
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
        <el-icon v-if="message.local_status === 'sending'" :size="12" class="is-loading">
          <Loading />
        </el-icon>
        <el-icon v-else-if="message.local_status === 'sent'" :size="12"><Check /></el-icon>
        <el-icon v-else-if="message.local_status === 'delivered' || message.local_status === 'read'" :size="12">
          <CircleCheck />
        </el-icon>
        <el-icon v-else-if="message.local_status === 'failed'" :size="12"><WarningFilled /></el-icon>

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
        :size="34"
      />
    </div>
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
  padding: 4px 10px;
  border-radius: 9999px;
  background-color: #f5f6f8;
  border: 1px solid #e5e7eb;
  color: #6b7280;
  font-size: 12px;
}

.msg-row {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  margin-bottom: 14px;
}

.msg-row.is-self {
  justify-content: flex-end;
}

.msg-row__avatar {
  flex-shrink: 0;
}

.msg-row__content-col {
  max-width: min(70%, 540px);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 4px;
}

.msg-row__content-col.is-self {
  align-items: flex-end;
}

.msg-row__sender {
  font-size: 12px;
  color: #6b7280;
  padding-left: 2px;
}

.msg-row__bubble-wrap {
  display: flex;
  align-items: flex-end;
  gap: 8px;
}

.msg-row__bubble-wrap.is-self {
  flex-direction: row-reverse;
}

.msg-bubble {
  padding: 9px 12px;
  border-radius: 8px;
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  color: #111827;
  font-size: 14px;
  line-height: 1.5;
}

.msg-bubble.is-self {
  background-color: #111827;
  border-color: #111827;
  color: #ffffff;
}

.msg-bubble.is-image {
  padding: 4px;
  background-color: #ffffff;
  border-color: #e5e7eb;
}

.msg-bubble.is-file {
  background-color: #ffffff;
  border-color: #e5e7eb;
  color: #111827;
}

.msg-bubble.is-recalled {
  background-color: #f5f6f8;
  border-color: #e5e7eb;
  color: #6b7280;
  font-style: italic;
}

.msg-bubble.is-failed {
  border-color: #fca5a5;
}

.msg-bubble__text {
  white-space: pre-wrap;
  word-break: break-word;
}

.msg-bubble__recalled-text {
  font-size: 13px;
}

.msg-image {
  border-radius: 6px;
  overflow: hidden;
  background-color: #f5f6f8;
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
  color: #6b7280;
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
  border-radius: 6px;
  background-color: #f5f6f8;
  border: 1px solid #e5e7eb;
  color: #374151;
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
  font-size: 13px;
  font-weight: 500;
  color: #111827;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.msg-file__sub {
  font-size: 11px;
  color: #6b7280;
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
  min-width: 64px;
}

.msg-voice__track {
  height: 4px;
  border-radius: 2px;
  background-color: rgba(156, 163, 175, 0.35);
  overflow: hidden;
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
  color: #9ca3af;
}

.msg-action-link {
  border: none;
  background: transparent;
  color: #6b7280;
  font-size: 11px;
  padding: 0 2px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s ease;
}

.msg-row:hover .msg-action-link {
  opacity: 1;
}

.msg-action-link:hover {
  color: #111827;
  text-decoration: underline;
}

.msg-row__status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11px;
  color: #6b7280;
}

.msg-row__status.is-read {
  color: #10b981;
}

.msg-row__status.is-delivered {
  color: #2563eb;
}

.msg-row__status.is-failed {
  color: #ef4444;
}

.msg-row__err-text {
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.msg-retry-btn {
  border: 1px solid #fca5a5;
  background-color: #ffffff;
  color: #ef4444;
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 4px;
  cursor: pointer;
}

.msg-retry-btn:hover {
  background-color: #fef2f2;
}
</style>
