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
        <span class="conv-list__title">会话列表</span>
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
            :title="includeHidden ? '仅看可见会话' : '显示隐藏会话'"
            @click="emit('toggle-include-hidden')"
          >
            <el-icon :size="15"><component :is="includeHidden ? View : Hide" /></el-icon>
          </button>
          <button
            type="button"
            class="conv-list__icon-btn"
            title="刷新会话"
            :disabled="loading"
            @click="emit('refresh')"
          >
            <el-icon :size="15"><RefreshRight /></el-icon>
          </button>
        </div>
      </div>

      <el-input
        v-model="keyword"
        placeholder="搜索会话、联系人或消息预览"
        clearable
        size="small"
        :prefix-icon="Search"
      />
    </div>

    <div v-loading="loading && conversations.length === 0" class="conv-list__scroll">
      <template v-if="filteredConversations.length > 0">
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
        <div v-if="hasMore" class="conv-list__load-more">
          <el-button
            text
            size="small"
            :loading="loadingMore"
            @click="emit('load-more')"
          >
            加载更多会话
          </el-button>
        </div>
      </template>

      <div v-else-if="!loading" class="conv-list__empty">
        <el-icon :size="28" class="conv-list__empty-icon"><ChatDotRound /></el-icon>
        <p>{{ keyword ? '未找到匹配的会话' : '暂无会话，前往联系人发起聊天' }}</p>
      </div>
    </div>
  </aside>
</template>

<style scoped>
.conv-list {
  width: 280px;
  flex-shrink: 0;
  height: 100%;
  display: flex;
  flex-direction: column;
  background-color: #ffffff;
  border-right: 1px solid #e5e7eb;
}

.conv-list__header {
  padding: 12px;
  border-bottom: 1px solid #e5e7eb;
  background-color: #f5f6f8;
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
  font-size: 15px;
  font-weight: 600;
  color: #111827;
}

.conv-list__status-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.conv-list__ws-pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11px;
  padding: 2px 6px;
  border-radius: 4px;
  border: 1px solid #e5e7eb;
  background-color: #ffffff;
  color: #4b5563;
}

.conv-list__ws-dot {
  width: 6px;
  height: 6px;
  border-radius: 9999px;
  background-color: #9ca3af;
}

.conv-list__ws-pill.is-connected .conv-list__ws-dot {
  background-color: #10b981;
}

.conv-list__icon-btn {
  border: 1px solid #e5e7eb;
  background-color: #ffffff;
  color: #4b5563;
  width: 26px;
  height: 26px;
  border-radius: 4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}

.conv-list__icon-btn:hover {
  background-color: #eceef2;
  color: #111827;
}

.conv-list__scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}

.conv-list__load-more {
  padding: 10px;
  text-align: center;
}

.conv-list__empty {
  padding: 48px 16px;
  text-align: center;
  color: #6b7280;
  font-size: 13px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}

.conv-list__empty-icon {
  color: #9ca3af;
}

@media (max-width: 768px) {
  .conv-list {
    width: 100%;
    border-right: none;
  }
}
</style>
