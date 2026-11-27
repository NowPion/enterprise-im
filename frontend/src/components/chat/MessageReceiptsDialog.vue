<script setup lang="ts">
import { ref, watch } from 'vue'
import AuthorizedAvatar from './AuthorizedAvatar.vue'
import { formatDividerTime } from '@/composables/chat/seq'
import type { MessageReceiptList, ReceiptDeliveryStatus, ReceiptFilterStatus } from '@/types/message'

const props = defineProps<{
  visible: boolean
  conversationId: string
  msgId: string | null
  data: MessageReceiptList | null
  loading: boolean
}>()

const emit = defineEmits<{
  'update:visible': [value: boolean]
  load: [params: { status?: ReceiptFilterStatus; cursor?: string; reset?: boolean }]
}>()

const activeFilter = ref<'all' | ReceiptFilterStatus>('all')

watch(
  () => [props.visible, props.msgId] as const,
  ([isVisible, msgId]) => {
    if (isVisible && msgId) {
      activeFilter.value = 'all'
      emit('load', { reset: true })
    }
  },
)

function handleFilterChange(tabName: string | number) {
  const next = String(tabName) as 'all' | ReceiptFilterStatus
  activeFilter.value = next
  emit('load', {
    status: next === 'all' ? undefined : next,
    reset: true,
  })
}

function handleLoadMore() {
  if (!props.data?.next_cursor) return
  emit('load', {
    status: activeFilter.value === 'all' ? undefined : activeFilter.value,
    cursor: props.data.next_cursor,
    reset: false,
  })
}

function statusLabel(status: ReceiptDeliveryStatus): string {
  switch (status) {
    case 'read':
      return '已读'
    case 'delivered':
      return '已送达'
    case 'sent':
      return '已发送'
    default:
      return '未送达'
  }
}

function statusTagType(status: ReceiptDeliveryStatus): 'success' | 'info' | 'warning' {
  if (status === 'read') return 'success'
  if (status === 'delivered') return 'info'
  return 'warning'
}
</script>

<template>
  <el-dialog
    :model-value="visible"
    title="消息回执详情"
    width="480px"
    destroy-on-close
    @update:model-value="(val: boolean) => emit('update:visible', val)"
  >
    <div v-if="data" class="receipts-summary ios-grouped-section">
      <div class="receipts-summary__item">
        <span class="receipts-summary__label">应接收</span>
        <strong class="receipts-summary__value">{{ data.total_members }}</strong>
      </div>
      <div class="receipts-summary__item">
        <span class="receipts-summary__label">已送达</span>
        <strong class="receipts-summary__value">{{ data.delivered_count }}</strong>
      </div>
      <div class="receipts-summary__item">
        <span class="receipts-summary__label">已读</span>
        <strong class="receipts-summary__value is-read">{{ data.read_count }}</strong>
      </div>
      <div class="receipts-summary__item">
        <span class="receipts-summary__label">未读</span>
        <strong class="receipts-summary__value is-unread">{{ data.unread_count }}</strong>
      </div>
    </div>

    <el-tabs :model-value="activeFilter" @tab-change="handleFilterChange">
      <el-tab-pane label="全部" name="all" />
      <el-tab-pane label="已读" name="read" />
      <el-tab-pane label="已送达" name="delivered" />
      <el-tab-pane label="未读" name="unread" />
    </el-tabs>

    <div v-loading="loading" class="receipts-list ios-grouped-section">
      <template v-if="data && data.items.length > 0">
        <div
          v-for="item in data.items"
          :key="item.user.id"
          class="receipts-list__row"
        >
          <div class="receipts-list__user">
            <AuthorizedAvatar
              :src="item.user.avatar_url"
              :name="item.user.nickname"
              :size="34"
              :online="item.user.online"
              show-online
              shape="circle"
            />
            <div class="receipts-list__user-meta">
              <span class="receipts-list__name">{{ item.user.nickname }}</span>
              <span class="receipts-list__sub">
                {{
                  item.read_at
                    ? `已读 ${formatDividerTime(item.read_at)}`
                    : item.delivered_at
                      ? `送达 ${formatDividerTime(item.delivered_at)}`
                      : `已读水位 #${item.last_read_seq}`
                }}
              </span>
            </div>
          </div>

          <el-tag size="small" :type="statusTagType(item.delivery_status)" effect="plain">
            {{ statusLabel(item.delivery_status) }}
          </el-tag>
        </div>

        <div v-if="data.has_more" class="receipts-list__more">
          <el-button text type="primary" size="small" :loading="loading" @click="handleLoadMore">
            加载更多成员
          </el-button>
        </div>
      </template>
      <div v-else-if="!loading" class="receipts-list__empty">暂无匹配的回执记录</div>
    </div>
  </el-dialog>
</template>

<style scoped>
.receipts-summary {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 8px;
  padding: 12px;
  background-color: var(--im-bg-grouped);
  margin-bottom: 10px;
}

.receipts-summary__item {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
}

.receipts-summary__label {
  font-size: 11px;
  color: var(--im-text-secondary);
}

.receipts-summary__value {
  font-size: 18px;
  font-weight: 700;
  color: var(--im-text-primary);
}

.receipts-summary__value.is-read {
  color: var(--im-status-online);
}

.receipts-summary__value.is-unread {
  color: var(--im-status-danger);
}

.receipts-list {
  max-height: 320px;
  overflow-y: auto;
  min-height: 120px;
  background-color: var(--im-bg-grouped);
}

.receipts-list__row {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  background-color: var(--im-bg-surface);
}

.receipts-list__row + .receipts-list__row::before {
  content: '';
  position: absolute;
  top: 0;
  left: 58px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.receipts-list__user {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.receipts-list__user-meta {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.receipts-list__name {
  font-size: 14px;
  font-weight: 500;
  color: var(--im-text-primary);
}

.receipts-list__sub {
  font-size: 12px;
  color: var(--im-text-secondary);
}

.receipts-list__more {
  text-align: center;
  padding: 10px;
}

.receipts-list__empty {
  text-align: center;
  padding: 36px 12px;
  color: var(--im-text-secondary);
  font-size: 14px;
}
</style>
