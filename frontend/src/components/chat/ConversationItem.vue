<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  ArrowRight,
  Bell,
  Hide,
  MoreFilled,
  MuteNotification,
  Top,
  View,
} from '@element-plus/icons-vue'
import AuthorizedAvatar from './AuthorizedAvatar.vue'
import { formatConversationTime } from '@/composables/chat/seq'
import type { Conversation } from '@/types/conversation'

const props = defineProps<{
  conversation: Conversation
  active: boolean
}>()

const emit = defineEmits<{
  select: [conversationId: string]
  'toggle-pin': [conversation: Conversation]
  'toggle-mute': [conversation: Conversation]
  'toggle-hide': [conversation: Conversation]
}>()

const formattedTime = computed(() => formatConversationTime(props.conversation.last_msg_at))

const unreadText = computed(() => {
  const count = props.conversation.unread_count || 0
  if (count <= 0) return ''
  return count > 99 ? '99+' : String(count)
})

const previewText = computed(() => {
  const preview = props.conversation.last_msg_preview?.trim()
  if (preview) return preview
  return props.conversation.type === 'group' ? '群聊已就绪' : '暂无消息'
})

function handleCommand(command: string) {
  if (command === 'pin') emit('toggle-pin', props.conversation)
  if (command === 'mute') emit('toggle-mute', props.conversation)
  if (command === 'hide') emit('toggle-hide', props.conversation)
}

// Desktop iOS context menu + swipe-reveal action drawer
const contextMenu = ref<{ open: boolean; x: number; y: number }>({
  open: false,
  x: 0,
  y: 0,
})
const swipedOpen = ref(false)
let pointerStartX = 0
let pointerTracking = false

function openContextMenu(event: MouseEvent) {
  const maxX = typeof window !== 'undefined' ? window.innerWidth - 188 : event.clientX
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

function runMenuAction(command: 'pin' | 'mute' | 'hide') {
  closeContextMenu()
  swipedOpen.value = false
  handleCommand(command)
}

function onPointerDown(event: PointerEvent) {
  if (event.button !== 0) return
  pointerStartX = event.clientX
  pointerTracking = true
}

function onPointerUp(event: PointerEvent) {
  if (!pointerTracking) return
  pointerTracking = false
  const dx = event.clientX - pointerStartX
  if (dx < -44) {
    swipedOpen.value = true
  } else if (dx > 36) {
    swipedOpen.value = false
  }
}

function handleClickRow() {
  if (swipedOpen.value) {
    swipedOpen.value = false
    return
  }
  emit('select', props.conversation.id)
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
  <div class="conv-item-shell" :class="{ 'is-swiped': swipedOpen }">
    <div
      class="conv-item"
      :class="{
        'is-active': active,
        'is-pinned': conversation.is_pinned,
        'is-hidden': !conversation.is_visible,
      }"
      role="button"
      tabindex="0"
      @click="handleClickRow"
      @keydown.enter.prevent="emit('select', conversation.id)"
      @contextmenu.prevent="openContextMenu"
      @pointerdown="onPointerDown"
      @pointerup="onPointerUp"
    >
      <div class="conv-item__unread-col">
        <span
          v-if="conversation.unread_count > 0 && !conversation.is_muted"
          class="conv-item__dot-indicator"
          aria-hidden="true"
        />
      </div>

      <div class="conv-item__avatar">
        <AuthorizedAvatar
          :src="conversation.avatar_url"
          :name="conversation.title"
          :size="46"
          :online="Boolean(conversation.peer?.online)"
          :show-online="conversation.type === 'single'"
          shape="circle"
        />
        <span
          v-if="conversation.unread_count > 0"
          class="conv-item__badge"
          :class="{ 'is-muted': conversation.is_muted }"
        >
          {{ conversation.is_muted ? '' : unreadText }}
        </span>
      </div>

      <div class="conv-item__body">
        <div class="conv-item__top">
          <div class="conv-item__title-wrap">
            <el-icon v-if="conversation.is_pinned" class="conv-item__pin-icon" :size="12">
              <Top />
            </el-icon>
            <span class="conv-item__title">{{ conversation.title || `会话 ${conversation.id}` }}</span>
            <span v-if="conversation.type === 'group'" class="conv-item__tag">群</span>
            <span v-if="!conversation.is_visible" class="conv-item__tag is-muted">已隐藏</span>
          </div>
          <div class="conv-item__time-row">
            <span class="conv-item__time">{{ formattedTime }}</span>
            <el-icon class="conv-item__chevron" :size="12"><ArrowRight /></el-icon>
          </div>
        </div>

        <div class="conv-item__bottom">
          <span class="conv-item__preview">{{ previewText }}</span>
          <div class="conv-item__meta-icons">
            <el-icon v-if="conversation.is_muted" class="conv-item__mute-icon" :size="13">
              <MuteNotification />
            </el-icon>
            <el-dropdown trigger="click" @command="handleCommand" @click.stop>
              <button
                type="button"
                class="conv-item__more-btn"
                title="会话操作"
                @click.stop
              >
                <el-icon :size="14"><MoreFilled /></el-icon>
              </button>
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item command="pin" :icon="Top">
                    {{ conversation.is_pinned ? '取消置顶' : '置顶会话' }}
                  </el-dropdown-item>
                  <el-dropdown-item command="mute" :icon="conversation.is_muted ? Bell : MuteNotification">
                    {{ conversation.is_muted ? '取消免打扰' : '消息免打扰' }}
                  </el-dropdown-item>
                  <el-dropdown-item command="hide" :icon="conversation.is_visible ? Hide : View">
                    {{ conversation.is_visible ? '隐藏会话' : '恢复显示' }}
                  </el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </div>
        </div>
      </div>
    </div>

    <div v-if="swipedOpen" class="conv-item__swipe-actions">
      <button type="button" class="swipe-btn is-pin" @click.stop="runMenuAction('pin')">
        <el-icon :size="15"><Top /></el-icon>
        <span>{{ conversation.is_pinned ? '取消' : '置顶' }}</span>
      </button>
      <button type="button" class="swipe-btn is-mute" @click.stop="runMenuAction('mute')">
        <el-icon :size="15"><MuteNotification /></el-icon>
        <span>{{ conversation.is_muted ? '提醒' : '静音' }}</span>
      </button>
      <button type="button" class="swipe-btn is-hide" @click.stop="runMenuAction('hide')">
        <el-icon :size="15"><Hide /></el-icon>
        <span>{{ conversation.is_visible ? '隐藏' : '恢复' }}</span>
      </button>
    </div>

    <Teleport to="body">
      <div
        v-if="contextMenu.open"
        class="ios-context-menu"
        :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }"
        @click.stop
      >
        <button type="button" class="ios-context-menu__item" @click="runMenuAction('pin')">
          <span>{{ conversation.is_pinned ? '取消置顶' : '置顶会话' }}</span>
          <el-icon :size="14"><Top /></el-icon>
        </button>
        <button type="button" class="ios-context-menu__item" @click="runMenuAction('mute')">
          <span>{{ conversation.is_muted ? '取消免打扰' : '消息免打扰' }}</span>
          <el-icon :size="14"><component :is="conversation.is_muted ? Bell : MuteNotification" /></el-icon>
        </button>
        <div class="ios-context-menu__divider" />
        <button
          type="button"
          class="ios-context-menu__item"
          :class="{ 'is-danger': conversation.is_visible }"
          @click="runMenuAction('hide')"
        >
          <span>{{ conversation.is_visible ? '隐藏会话' : '恢复显示' }}</span>
          <el-icon :size="14"><component :is="conversation.is_visible ? Hide : View" /></el-icon>
        </button>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.conv-item-shell {
  position: relative;
  display: flex;
  overflow: hidden;
  background-color: var(--im-bg-surface);
}

.conv-item-shell + .conv-item-shell::before {
  content: '';
  position: absolute;
  top: 0;
  left: 72px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
  z-index: 2;
}

.conv-item {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 11px 12px 11px 6px;
  cursor: pointer;
  background-color: var(--im-bg-surface);
  transition: background-color 0.14s var(--im-ease-ios), transform 0.12s var(--im-ease-ios);
}

.conv-item.is-pinned {
  background-color: rgba(0, 122, 255, 0.03);
}

.conv-item:hover {
  background-color: var(--im-bg-hover);
}

.conv-item:active {
  background-color: rgba(120, 120, 128, 0.18);
}

.conv-item.is-active {
  background-color: var(--im-bg-active);
}

.conv-item.is-hidden {
  opacity: 0.72;
}

.conv-item__unread-col {
  width: 10px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.conv-item__dot-indicator {
  width: 8px;
  height: 8px;
  border-radius: 9999px;
  background-color: var(--im-accent-primary);
}

.conv-item__avatar {
  position: relative;
  flex-shrink: 0;
}

.conv-item__badge {
  position: absolute;
  top: -4px;
  right: -5px;
  min-width: 18px;
  height: 18px;
  padding: 0 5px;
  border-radius: 9999px;
  background-color: var(--im-status-unread);
  color: #ffffff;
  font-size: 11px;
  font-weight: 600;
  line-height: 18px;
  text-align: center;
  border: 1.5px solid #ffffff;
}

.conv-item__badge.is-muted {
  min-width: 9px;
  width: 9px;
  height: 9px;
  padding: 0;
  top: -1px;
  right: -1px;
  background-color: var(--im-status-offline);
}

.conv-item__body {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.conv-item__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
}

.conv-item__title-wrap {
  display: flex;
  align-items: center;
  gap: 4px;
  min-width: 0;
}

.conv-item__pin-icon {
  color: var(--im-accent-primary);
  flex-shrink: 0;
}

.conv-item__title {
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.conv-item__tag {
  flex-shrink: 0;
  font-size: 10px;
  font-weight: 600;
  line-height: 15px;
  padding: 0 6px;
  border-radius: 9999px;
  background-color: var(--im-accent-primary-soft);
  color: var(--im-accent-primary);
}

.conv-item__tag.is-muted {
  background-color: var(--im-bg-fill);
  color: var(--im-text-secondary);
}

.conv-item__time-row {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  flex-shrink: 0;
}

.conv-item__time {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.conv-item__chevron {
  color: var(--im-text-tertiary);
}

.conv-item__bottom {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
}

.conv-item__preview {
  flex: 1;
  min-width: 0;
  font-size: 14px;
  color: var(--im-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.conv-item__meta-icons {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}

.conv-item__mute-icon {
  color: var(--im-text-muted);
}

.conv-item__more-btn {
  border: none;
  background: transparent;
  color: var(--im-text-secondary);
  padding: 3px;
  border-radius: 8px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  opacity: 0;
  transition: opacity 0.15s ease, background-color 0.15s ease;
}

.conv-item:hover .conv-item__more-btn,
.conv-item.is-active .conv-item__more-btn {
  opacity: 1;
}

.conv-item__more-btn:hover {
  background-color: var(--im-bg-fill-secondary);
  color: var(--im-accent-primary);
}

.conv-item__swipe-actions {
  display: flex;
  flex-shrink: 0;
  animation: ios-bubble-in 0.15s var(--im-ease-ios);
}

.swipe-btn {
  border: none;
  width: 52px;
  color: #ffffff;
  font-size: 11px;
  font-weight: 600;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  cursor: pointer;
}

.swipe-btn.is-pin {
  background-color: #007aff;
}

.swipe-btn.is-mute {
  background-color: #5856d6;
}

.swipe-btn.is-hide {
  background-color: #ff3b30;
}
</style>
