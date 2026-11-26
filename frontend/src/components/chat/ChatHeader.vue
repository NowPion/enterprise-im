<script setup lang="ts">
import { computed } from 'vue'
import { ArrowLeft, Bell, MuteNotification, Setting, Top, UserFilled } from '@element-plus/icons-vue'
import AuthorizedAvatar from './AuthorizedAvatar.vue'
import { formatConversationTime } from '@/composables/chat/seq'
import type { Conversation, ConversationDetail } from '@/types/conversation'

const props = withDefaults(
  defineProps<{
    conversation: Conversation
    detail: ConversationDetail | null
    showBack?: boolean
    memberCount?: number
  }>(),
  {
    showBack: false,
    memberCount: 0,
  },
)

const emit = defineEmits<{
  back: []
  'toggle-pin': []
  'toggle-mute': []
  'open-members': []
  'open-group': [groupId: string]
}>()

const subtitleText = computed(() => {
  if (props.conversation.type === 'group') {
    const count = props.detail?.group?.member_count ?? props.memberCount
    const statusLabel = props.detail?.group?.status === 'disbanded' ? ' · 群已解散' : ''
    return count > 0 ? `群成员 ${count} 人${statusLabel}` : `群聊${statusLabel}`
  }
  const peer = props.detail?.peer ?? props.conversation.peer
  if (!peer) return '单聊'
  if (peer.online) return '在线'
  if (peer.last_online_at) {
    return `离线 · 最近在线 ${formatConversationTime(peer.last_online_at)}`
  }
  return '离线'
})

const isGroupDisbanded = computed(
  () => props.conversation.type === 'group' && props.detail?.group?.status === 'disbanded',
)
</script>

<template>
  <header class="chat-header">
    <div class="chat-header__left">
      <button
        v-if="showBack"
        type="button"
        class="chat-header__back-btn"
        title="返回会话列表"
        @click="emit('back')"
      >
        <el-icon :size="16"><ArrowLeft /></el-icon>
      </button>

      <AuthorizedAvatar
        :src="conversation.avatar_url"
        :name="conversation.title"
        :size="38"
        :online="Boolean((detail?.peer ?? conversation.peer)?.online)"
        :show-online="conversation.type === 'single'"
      />

      <div class="chat-header__info">
        <div class="chat-header__title-row">
          <h2 class="chat-header__title">{{ conversation.title || `会话 ${conversation.id}` }}</h2>
          <span v-if="!conversation.can_send || isGroupDisbanded" class="chat-header__readonly-tag">
            仅可查看历史
          </span>
        </div>
        <div class="chat-header__subtitle">
          <span
            v-if="conversation.type === 'single'"
            class="chat-header__dot"
            :class="(detail?.peer ?? conversation.peer)?.online ? 'is-online' : 'is-offline'"
          />
          <span>{{ subtitleText }}</span>
        </div>
      </div>
    </div>

    <div class="chat-header__actions">
      <el-tooltip :content="conversation.is_pinned ? '取消置顶' : '置顶会话'" placement="bottom">
        <el-button
          size="small"
          :type="conversation.is_pinned ? 'primary' : 'default'"
          plain
          :icon="Top"
          @click="emit('toggle-pin')"
        />
      </el-tooltip>

      <el-tooltip :content="conversation.is_muted ? '取消免打扰' : '消息免打扰'" placement="bottom">
        <el-button
          size="small"
          :type="conversation.is_muted ? 'info' : 'default'"
          plain
          :icon="conversation.is_muted ? MuteNotification : Bell"
          @click="emit('toggle-mute')"
        />
      </el-tooltip>

      <el-button
        size="small"
        plain
        :icon="UserFilled"
        @click="emit('open-members')"
      >
        成员
      </el-button>

      <el-button
        v-if="conversation.type === 'group' && conversation.group_id"
        size="small"
        plain
        :icon="Setting"
        @click="emit('open-group', conversation.group_id)"
      >
        群资料
      </el-button>
    </div>
  </header>
</template>

<style scoped>
.chat-header {
  height: 60px;
  flex-shrink: 0;
  padding: 0 16px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  background-color: #ffffff;
  border-bottom: 1px solid #e5e7eb;
}

.chat-header__left {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.chat-header__back-btn {
  border: 1px solid #e5e7eb;
  background-color: #ffffff;
  color: #111827;
  width: 30px;
  height: 30px;
  border-radius: 4px;
  display: none;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}

@media (max-width: 768px) {
  .chat-header__back-btn {
    display: inline-flex;
  }
}

.chat-header__info {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.chat-header__title-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.chat-header__title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: #111827;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chat-header__readonly-tag {
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 4px;
  background-color: #f5f6f8;
  color: #4b5563;
  border: 1px solid #e5e7eb;
  flex-shrink: 0;
}

.chat-header__subtitle {
  display: flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: #6b7280;
}

.chat-header__dot {
  width: 7px;
  height: 7px;
  border-radius: 9999px;
}

.chat-header__dot.is-online {
  background-color: #10b981;
}

.chat-header__dot.is-offline {
  background-color: #9ca3af;
}

.chat-header__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
</style>
