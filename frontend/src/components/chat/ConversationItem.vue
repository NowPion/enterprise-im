<script setup lang="ts">
import { computed } from 'vue'
import { Bell, Hide, MoreFilled, MuteNotification, Top, View } from '@element-plus/icons-vue'
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
</script>

<template>
  <div
    class="conv-item"
    :class="{
      'is-active': active,
      'is-pinned': conversation.is_pinned,
      'is-hidden': !conversation.is_visible,
    }"
    role="button"
    tabindex="0"
    @click="emit('select', conversation.id)"
    @keydown.enter.prevent="emit('select', conversation.id)"
  >
    <div class="conv-item__avatar">
      <AuthorizedAvatar
        :src="conversation.avatar_url"
        :name="conversation.title"
        :size="42"
        :online="Boolean(conversation.peer?.online)"
        :show-online="conversation.type === 'single'"
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
        <span class="conv-item__time">{{ formattedTime }}</span>
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
</template>

<style scoped>
.conv-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  cursor: pointer;
  border-bottom: 1px solid #f0f2f5;
  background-color: #ffffff;
  transition: background-color 0.15s ease;
}

.conv-item.is-pinned {
  background-color: #f9fafb;
}

.conv-item:hover {
  background-color: #f3f4f6;
}

.conv-item.is-active {
  background-color: #eceef2;
}

.conv-item.is-hidden {
  opacity: 0.72;
}

.conv-item__avatar {
  position: relative;
  flex-shrink: 0;
}

.conv-item__badge {
  position: absolute;
  top: -5px;
  right: -6px;
  min-width: 18px;
  height: 18px;
  padding: 0 5px;
  border-radius: 9999px;
  background-color: #ef4444;
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
  top: -2px;
  right: -2px;
}

.conv-item__body {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.conv-item__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.conv-item__title-wrap {
  display: flex;
  align-items: center;
  gap: 4px;
  min-width: 0;
}

.conv-item__pin-icon {
  color: #4b5563;
  flex-shrink: 0;
}

.conv-item__title {
  font-size: 14px;
  font-weight: 600;
  color: #111827;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.conv-item__tag {
  flex-shrink: 0;
  font-size: 10px;
  line-height: 14px;
  padding: 0 4px;
  border-radius: 3px;
  background-color: #eceef2;
  color: #4b5563;
  border: 1px solid #e5e7eb;
}

.conv-item__tag.is-muted {
  color: #6b7280;
}

.conv-item__time {
  flex-shrink: 0;
  font-size: 11px;
  color: #6b7280;
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
  font-size: 12px;
  color: #4b5563;
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
  color: #9ca3af;
}

.conv-item__more-btn {
  border: none;
  background: transparent;
  color: #6b7280;
  padding: 2px;
  border-radius: 4px;
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
  background-color: #e5e7eb;
  color: #111827;
}
</style>
