<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue'
import {
  Close,
  FolderOpened,
  Microphone,
  Picture,
  Promotion,
  Top,
  Upload,
} from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { uploadAsset } from '@/api/upload'
import type { UploadPurpose } from '@/types/upload'

const MAX_IMAGE_BYTES = 10 * 1024 * 1024
const MAX_VOICE_BYTES = 10 * 1024 * 1024
const MAX_FILE_BYTES = 50 * 1024 * 1024
const MAX_VOICE_SECONDS = 300

const props = withDefaults(
  defineProps<{
    conversationId: string
    canSend: boolean
    disabledReason?: string
    wsConnected: boolean
  }>(),
  {
    disabledReason: '当前会话不可发送消息（已非好友或群已解散），仅可查看历史记录',
  },
)

const emit = defineEmits<{
  'send-text': [content: string]
  'send-media': [
    payload: {
      msg_type: 'image' | 'file' | 'voice'
      content: string
      extra: Record<string, unknown>
      local_preview_url?: string
    },
  ]
}>()

const textInput = ref('')
const isComposing = ref(false)
const uploading = ref(false)
const uploadProgress = ref(0)
const uploadFilename = ref('')

const imageInputRef = ref<HTMLInputElement | null>(null)
const fileInputRef = ref<HTMLInputElement | null>(null)
const voiceInputRef = ref<HTMLInputElement | null>(null)

// Real MediaRecorder state for microphone voice recording.
const recording = ref(false)
const recordingSeconds = ref(0)
let mediaRecorder: MediaRecorder | null = null
let recordingStream: MediaStream | null = null
let recordedChunks: BlobPart[] = []
let recordingTimer: ReturnType<typeof setInterval> | undefined
let cancelRecordingFlag = false

const hasContent = computed(() => textInput.value.trim().length > 0)
const canSubmitText = computed(
  () => props.canSend && !uploading.value && hasContent.value,
)

function handleKeyDown(event: KeyboardEvent) {
  if (event.key !== 'Enter') return
  if (event.shiftKey) return
  if (isComposing.value || event.isComposing) return
  event.preventDefault()
  submitText()
}

function submitText() {
  if (!props.canSend) return
  const content = textInput.value
  if (!content.trim()) return
  emit('send-text', content)
  textInput.value = ''
}

function validateFileSize(file: File | Blob, purpose: UploadPurpose): boolean {
  const maxBytes =
    purpose === 'image'
      ? MAX_IMAGE_BYTES
      : purpose === 'voice'
        ? MAX_VOICE_BYTES
        : MAX_FILE_BYTES
  if (file.size <= 0) {
    ElMessage.warning('不能发送空文件')
    return false
  }
  if (file.size > maxBytes) {
    const mb = Math.round(maxBytes / (1024 * 1024))
    ElMessage.error(`文件超过限制（最大 ${mb} MB）`)
    return false
  }
  return true
}

async function uploadAndEmitMedia(
  file: File | Blob,
  msgType: 'image' | 'file' | 'voice',
  filename?: string,
) {
  if (!props.canSend) return
  if (!validateFileSize(file, msgType)) return

  uploading.value = true
  uploadProgress.value = 0
  uploadFilename.value = filename ?? (file instanceof File ? file.name : 'upload')

  let localPreviewUrl: string | undefined
  if (msgType === 'image' || msgType === 'voice') {
    localPreviewUrl = URL.createObjectURL(file)
  }

  try {
    const asset = await uploadAsset(file, msgType, {
      filename: uploadFilename.value,
      onProgress: (pct) => {
        uploadProgress.value = pct
      },
    })
    emit('send-media', {
      msg_type: msgType,
      content: '',
      extra: {
        asset_id: asset.asset_id,
        purpose: asset.purpose,
        url: asset.url,
        filename: asset.filename,
        mime: asset.mime,
        size_bytes: asset.size_bytes,
        width: asset.width,
        height: asset.height,
        thumbnail_url: asset.thumbnail_url,
        duration_ms: asset.duration_ms,
      },
      local_preview_url: localPreviewUrl,
    })
  } catch (err) {
    if (localPreviewUrl) {
      URL.revokeObjectURL(localPreviewUrl)
    }
    ElMessage.error(err instanceof Error ? err.message : '上传失败，请重试')
  } finally {
    uploading.value = false
    uploadProgress.value = 0
    uploadFilename.value = ''
  }
}

async function handleFileSelected(event: Event, msgType: 'image' | 'file' | 'voice') {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  await uploadAndEmitMedia(file, msgType, file.name)
}

function pickSupportedRecorderMime(): { mime: string; ext: string } | null {
  if (typeof MediaRecorder === 'undefined') return null
  const candidates = [
    { mime: 'audio/ogg;codecs=opus', ext: '.ogg' },
    { mime: 'audio/ogg', ext: '.ogg' },
    { mime: 'audio/webm;codecs=opus', ext: '.webm' },
    { mime: 'audio/webm', ext: '.webm' },
    { mime: 'audio/mp4', ext: '.m4a' },
  ]
  for (const item of candidates) {
    if (MediaRecorder.isTypeSupported(item.mime)) {
      return item
    }
  }
  return { mime: '', ext: '.webm' }
}

function cleanupRecorderResources() {
  clearInterval(recordingTimer)
  recordingTimer = undefined
  if (recordingStream) {
    for (const track of recordingStream.getTracks()) {
      track.stop()
    }
    recordingStream = null
  }
  mediaRecorder = null
  recording.value = false
  recordingSeconds.value = 0
}

async function startVoiceRecording() {
  if (!props.canSend || uploading.value || recording.value) return
  if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === 'undefined') {
    ElMessage.info('当前浏览器不支持直接录音，请选择本地音频文件发送')
    voiceInputRef.value?.click()
    return
  }

  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
    const format = pickSupportedRecorderMime()
    const recorder = format?.mime
      ? new MediaRecorder(stream, { mimeType: format.mime })
      : new MediaRecorder(stream)

    recordingStream = stream
    mediaRecorder = recorder
    recordedChunks = []
    cancelRecordingFlag = false
    recordingSeconds.value = 0
    recording.value = true

    recorder.ondataavailable = (ev) => {
      if (ev.data && ev.data.size > 0) {
        recordedChunks.push(ev.data)
      }
    }

    recorder.onstop = () => {
      const shouldCancel = cancelRecordingFlag
      const chunks = [...recordedChunks]
      const mimeType = (recorder.mimeType || format?.mime || 'audio/webm').split(';')[0]
      const ext = mimeType.includes('ogg') ? '.ogg' : mimeType.includes('mp4') ? '.m4a' : '.webm'
      cleanupRecorderResources()
      if (shouldCancel || chunks.length === 0) return
      const blob = new Blob(chunks, { type: mimeType })
      const file = new File([blob], `voice-${Date.now()}${ext}`, { type: mimeType })
      void uploadAndEmitMedia(file, 'voice', file.name)
    }

    recorder.start(250)
    recordingTimer = setInterval(() => {
      recordingSeconds.value += 1
      if (recordingSeconds.value >= MAX_VOICE_SECONDS) {
        finishVoiceRecording()
      }
    }, 1000)
  } catch {
    cleanupRecorderResources()
    ElMessage.warning('无法访问麦克风，可改用本地音频文件上传')
  }
}

function finishVoiceRecording() {
  if (!mediaRecorder || mediaRecorder.state === 'inactive') return
  cancelRecordingFlag = false
  mediaRecorder.stop()
}

function cancelVoiceRecording() {
  if (!mediaRecorder) {
    cleanupRecorderResources()
    return
  }
  cancelRecordingFlag = true
  if (mediaRecorder.state !== 'inactive') {
    mediaRecorder.stop()
  } else {
    cleanupRecorderResources()
  }
}

onUnmounted(() => {
  cancelVoiceRecording()
})
</script>

<template>
  <footer class="composer">
    <div v-if="!canSend" class="composer__disabled-banner">
      {{ disabledReason }}
    </div>

    <div v-if="uploading" class="composer__progress">
      <span class="composer__progress-name">正在上传：{{ uploadFilename }}</span>
      <el-progress :percentage="uploadProgress" :stroke-width="4" />
    </div>

    <div v-if="recording" class="composer__recording-bar">
      <span class="composer__recording-dot" />
      <span>正在录音 {{ recordingSeconds }}s / {{ MAX_VOICE_SECONDS }}s</span>
      <div class="composer__recording-actions">
        <el-button size="small" :icon="Close" @click="cancelVoiceRecording">取消</el-button>
        <el-button size="small" type="primary" :icon="Promotion" @click="finishVoiceRecording">
          停止并发送
        </el-button>
      </div>
    </div>

    <div class="composer__toolbar">
      <div class="composer__tools">
        <button
          type="button"
          class="composer__tool-pill"
          :disabled="!canSend || uploading || recording"
          @click="imageInputRef?.click()"
        >
          <el-icon :size="15"><Picture /></el-icon>
          <span>图片</span>
        </button>

        <button
          type="button"
          class="composer__tool-pill"
          :disabled="!canSend || uploading || recording"
          @click="fileInputRef?.click()"
        >
          <el-icon :size="15"><FolderOpened /></el-icon>
          <span>文件</span>
        </button>

        <button
          type="button"
          class="composer__tool-pill"
          :disabled="!canSend || uploading || recording"
          @click="startVoiceRecording"
        >
          <el-icon :size="15"><Microphone /></el-icon>
          <span>录制语音</span>
        </button>

        <button
          type="button"
          class="composer__tool-pill"
          :disabled="!canSend || uploading || recording"
          @click="voiceInputRef?.click()"
        >
          <el-icon :size="15"><Upload /></el-icon>
          <span>音频文件</span>
        </button>
      </div>

      <span v-if="!wsConnected" class="composer__offline-hint">
        当前连接断开，发送的消息将在重连后自动补发
      </span>
    </div>

    <div class="composer__pill" :class="{ 'is-disabled': !canSend }">
      <textarea
        v-model="textInput"
        class="composer__textarea"
        :disabled="!canSend"
        :placeholder="canSend ? '信息 · Enter 发送，Shift + Enter 换行' : '当前会话不可发送消息'"
        rows="1"
        @compositionstart="isComposing = true"
        @compositionend="isComposing = false"
        @keydown="handleKeyDown"
      />
      <button
        v-if="hasContent"
        type="button"
        class="composer__send-btn"
        :disabled="!canSubmitText"
        aria-label="发送"
        title="发送"
        @click="submitText"
      >
        <el-icon :size="16"><Top /></el-icon>
      </button>
    </div>

    <input
      ref="imageInputRef"
      type="file"
      accept="image/jpeg,image/png,image/gif,image/webp"
      hidden
      @change="(e) => handleFileSelected(e, 'image')"
    />
    <input
      ref="fileInputRef"
      type="file"
      hidden
      @change="(e) => handleFileSelected(e, 'file')"
    />
    <input
      ref="voiceInputRef"
      type="file"
      accept="audio/wav,audio/mpeg,audio/ogg,audio/mp4,audio/aac,audio/webm"
      hidden
      @change="(e) => handleFileSelected(e, 'voice')"
    />
  </footer>
</template>

<style scoped>
.composer {
  flex-shrink: 0;
  background-color: var(--im-bg-toolbar);
  backdrop-filter: blur(20px) saturate(180%);
  -webkit-backdrop-filter: blur(20px) saturate(180%);
  border-top: 0.5px solid var(--im-separator);
  padding: 8px 16px 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.composer__disabled-banner {
  padding: 8px 14px;
  border-radius: 12px;
  background-color: var(--im-bg-fill);
  color: var(--im-text-secondary);
  font-size: 13px;
}

.composer__progress {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.composer__progress-name {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.composer__recording-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  border-radius: 14px;
  background-color: rgba(255, 59, 48, 0.08);
  font-size: 13px;
  color: var(--im-text-primary);
}

.composer__recording-dot {
  width: 8px;
  height: 8px;
  border-radius: 9999px;
  background-color: var(--im-status-danger);
}

.composer__recording-actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 6px;
}

.composer__toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  flex-wrap: wrap;
}

.composer__tools {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.composer__tool-pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: none;
  background-color: var(--im-bg-fill);
  color: var(--im-text-secondary);
  padding: 4px 10px;
  border-radius: 9999px;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: background-color 0.14s ease, color 0.14s ease;
}

.composer__tool-pill:hover:not(:disabled) {
  background-color: var(--im-accent-primary-soft);
  color: var(--im-accent-primary);
}

.composer__tool-pill:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.composer__offline-hint {
  font-size: 12px;
  color: var(--im-status-warning);
}

.composer__pill {
  display: flex;
  align-items: flex-end;
  gap: 8px;
  padding: 6px 8px 6px 16px;
  border-radius: 22px;
  background-color: var(--im-bg-surface);
  border: 0.5px solid var(--im-border-base);
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}

.composer__pill:focus-within {
  border-color: var(--im-accent-primary);
  box-shadow: 0 0 0 2px rgba(0, 122, 255, 0.12);
}

.composer__pill.is-disabled {
  background-color: var(--im-bg-fill);
}

.composer__textarea {
  flex: 1;
  min-height: 24px;
  max-height: 112px;
  resize: none;
  border: none;
  padding: 4px 0;
  font-size: 15px;
  line-height: 1.45;
  color: var(--im-text-primary);
  background: transparent;
  font-family: inherit;
  outline: none;
}

.composer__textarea::placeholder {
  color: var(--im-text-muted);
}

.composer__textarea:disabled {
  color: var(--im-text-muted);
  cursor: not-allowed;
}

.composer__send-btn {
  width: 32px;
  height: 32px;
  border-radius: 9999px;
  border: none;
  background-color: var(--im-accent-primary);
  color: #ffffff;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  cursor: pointer;
  animation: ios-bubble-in 0.14s var(--im-ease-ios);
  transition: background-color 0.14s ease, transform 0.12s ease;
}

.composer__send-btn:hover:not(:disabled) {
  background-color: var(--im-accent-primary-hover);
}

.composer__send-btn:active:not(:disabled) {
  transform: scale(0.92);
}

.composer__send-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
</style>
