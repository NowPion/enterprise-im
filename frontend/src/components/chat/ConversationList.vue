<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChatDotRound, Hide, RefreshRight, Search, View } from '@element-plus/icons-vue'
import ConversationItem from './ConversationItem.vue'
import type { Conversation } from '@/types/conversation'

const props = defineProps<{
  conversations: Conversation[]
  activeConversationId: string | null
  loading: boolean
  loadingMore: boolean
  hasMore: boolean
  includeHidden: boolean
  wsConnected: boolean
}>()

const emit = defineEmits<{
  select: [conversationId: string]
  'load-more': []
  'toggle-include-hidden': []
  'toggle-pin': [conversation: Conversation]
  'toggle-mute': [conversation: Conversation]
  'toggle-hide': [conversation: Conversation]
  refresh: []
}>()

const keyword = ref('')

const filteredConversations = computed(() => {
  const q = keyword.value.trim().toLowerCase()
  if (!q) return props.conversations
  return props.conversations.filter((conv) => {
    const title = conv.title?.toLowerCase() ?? ''
    const preview = conv.last_msg_preview?.toLowerCase() ?? ''
    const username = conv.peer?.username?.toLowerCase() ?? ''
    return title.includes(q) || preview.includes(q) || username.includes(q)
  })
})
</script>

<template>
  <aside class="conv-list">
    <div class="conv-list__header">
      <div class="conv-list__title-row">
        <span class="conv-list__title ios-large-title">消息</span>
        <div class="conv-list__status-actions">
          <span
            class="conv-list__ws-pill"
            :class="wsConnected ? 'is-connected' : 'is-disconnected'"
            :title="wsConnected ? '实时通道已连接' : '实时通道重连中'"
          >
            <span class="conv-list__ws-dot" />
            {{ wsConnected ? '实时在线' : '连接中' }}
          </span>
          <button
            type="button"
            class="conv-list__icon-btn"
            :class="{ 'is-active': includeHidden }"
            :title="includeHidden ? '仅看可见会话' : '显示隐藏会话'"
            @click="emit('toggle-include-hidden')"
          >
            <el-icon :size="16"><component :is="includeHidden ? View : Hide" /></el-icon>
          </button>
          <button
            type="button"
            class="conv-list__icon-btn"
            title="刷新会话"
            :disabled="loading"
            @click="emit('refresh')"
          >
            <el-icon :size="16"><RefreshRight /></el-icon>
          </button>
        </div>
      </div>

      <el-input
        v-model="keyword"
        placeholder="搜索会话、联系人或消息预览"
        clearable
        :prefix-icon="Search"
      />
    </div>

    <div v-loading="loading && conversations.length === 0" class="conv-list__scroll">
      <template v-if="filteredConversations.length > 0">
        <div class="ios-grouped-section conv-list__group">
          <ConversationItem
            v-for="conv in filteredConversations"
            :key="conv.id"
            :conversation="conv"
            :active="conv.id === activeConversationId"
            @select="(id) => emit('select', id)"
            @toggle-pin="(c) => emit('toggle-pin', c)"
            @toggle-mute="(c) => emit('toggle-mute', c)"
            @toggle-hide="(c) => emit('toggle-hide', c)"
          />
        </div>
        <div v-if="hasMore" class="conv-list__load-more">
          <el-button
            text
            type="primary"
            size="small"
            :loading="loadingMore"
            @click="emit('load-more')"
          >
            加载更多会话
          </el-button>
        </div>
      </template>

      <div v-else-if="!loading" class="conv-list__empty">
        <el-icon :size="30" class="conv-list__empty-icon"><ChatDotRound /></el-icon>
        <p>{{ keyword ? '未找到匹配的会话' : '暂无会话，前往联系人发起聊天' }}</p>
      </div>
    </div>
  </aside>
</template>

<style scoped>
.conv-list {
  width: var(--im-list-width);
  flex-shrink: 0;
  height: 100%;
  display: flex;
  flex-direction: column;
  background-color: var(--im-bg-grouped);
  border-right: 0.5px solid var(--im-separator);
}

.conv-list__header {
  padding: 16px 14px 12px;
  background-color: var(--im-bg-sidebar);
  backdrop-filter: blur(18px) saturate(180%);
  -webkit-backdrop-filter: blur(18px) saturate(180%);
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.conv-list__title-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.conv-list__title {
  font-size: 26px;
}

.conv-list__status-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.conv-list__ws-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 11px;
  font-weight: 500;
  padding: 3px 8px;
  border-radius: 9999px;
  background-color: var(--im-bg-surface);
  border: 0.5px solid var(--im-border-light);
  color: var(--im-text-secondary);
}

.conv-list__ws-dot {
  width: 6px;
  height: 6px;
  border-radius: 9999px;
  background-color: var(--im-status-offline);
}

.conv-list__ws-pill.is-connected .conv-list__ws-dot {
  background-color: var(--im-status-online);
}

.conv-list__icon-btn {
  border: none;
  background-color: var(--im-bg-fill);
  color: var(--im-accent-primary);
  width: 30px;
  height: 30px;
  border-radius: 9999px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  transition: background-color 0.14s ease, transform 0.12s ease;
}

.conv-list__icon-btn:hover {
  background-color: var(--im-accent-primary-soft);
}

.conv-list__icon-btn.is-active {
  background-color: var(--im-accent-primary);
  color: #ffffff;
}

.conv-list__icon-btn:active {
  transform: scale(0.94);
}

.conv-list__scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 2px 12px 16px;
}

.conv-list__group {
  margin-top: 2px;
}

.conv-list__load-more {
  padding: 12px 10px 4px;
  text-align: center;
}

.conv-list__empty {
  padding: 56px 16px;
  text-align: center;
  color: var(--im-text-secondary);
  font-size: 14px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}

.conv-list__empty-icon {
  color: var(--im-accent-primary);
}

@media (max-width: 768px) {
  .conv-list {
    width: 100%;
    border-right: none;
  }
}
</style>
