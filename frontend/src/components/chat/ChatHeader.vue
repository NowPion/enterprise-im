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
        class="chat-header__back-btn ios-chevron-back"
        title="返回会话列表"
        @click="emit('back')"
      >
        <el-icon :size="18"><ArrowLeft /></el-icon>
        <span>消息</span>
      </button>

      <AuthorizedAvatar
        :src="conversation.avatar_url"
        :name="conversation.title"
        :size="38"
        :online="Boolean((detail?.peer ?? conversation.peer)?.online)"
        :show-online="conversation.type === 'single'"
        shape="circle"
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
          :type="conversation.is_muted ? 'primary' : 'default'"
          plain
          :icon="conversation.is_muted ? MuteNotification : Bell"
          @click="emit('toggle-mute')"
        />
      </el-tooltip>

      <el-button
        size="small"
        type="primary"
        plain
        :icon="UserFilled"
        @click="emit('open-members')"
      >
        成员
      </el-button>

      <el-button
        v-if="conversation.type === 'group' && conversation.group_id"
        size="small"
        type="primary"
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
  height: 58px;
  flex-shrink: 0;
  padding: 0 18px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  background-color: var(--im-bg-toolbar);
  backdrop-filter: blur(20px) saturate(180%);
  -webkit-backdrop-filter: blur(20px) saturate(180%);
  border-bottom: 0.5px solid var(--im-separator);
}

.chat-header__left {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.chat-header__back-btn {
  display: none;
  flex-shrink: 0;
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
  gap: 1px;
}

.chat-header__title-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.chat-header__title {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--im-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chat-header__readonly-tag {
  font-size: 11px;
  font-weight: 500;
  padding: 2px 8px;
  border-radius: 9999px;
  background-color: var(--im-bg-fill);
  color: var(--im-text-secondary);
  flex-shrink: 0;
}

.chat-header__subtitle {
  display: flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: var(--im-text-secondary);
}

.chat-header__dot {
  width: 7px;
  height: 7px;
  border-radius: 9999px;
}

.chat-header__dot.is-online {
  background-color: var(--im-status-online);
}

.chat-header__dot.is-offline {
  background-color: var(--im-status-offline);
}

.chat-header__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
</style>
