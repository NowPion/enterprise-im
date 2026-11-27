<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Check, Close, Refresh } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import AppAvatar from '@/components/common/AppAvatar.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import { useContactStore } from '@/stores/contact'
import { isApiError } from '@/types/common'
import type { FriendRequestDirection, FriendRequestStatus } from '@/types/friend'

const router = useRouter()
const contactStore = useContactStore()

const activeDirection = ref<FriendRequestDirection>('received')
const statusFilter = ref<'all' | FriendRequestStatus>('all')
const busyId = ref('')

const currentItems = computed(() =>
  activeDirection.value === 'received'
    ? contactStore.receivedRequests
    : contactStore.sentRequests,
)

const currentHasMore = computed(() =>
  activeDirection.value === 'received'
    ? contactStore.receivedHasMore
    : contactStore.sentHasMore,
)

function statusLabel(status: FriendRequestStatus): string {
  switch (status) {
    case 'pending':
      return '待处理'
    case 'accepted':
      return '已同意'
    case 'rejected':
      return '已拒绝'
    case 'canceled':
      return '已撤销'
  }
}

function statusTagType(status: FriendRequestStatus): 'warning' | 'success' | 'danger' | 'info' {
  switch (status) {
    case 'pending':
      return 'warning'
    case 'accepted':
      return 'success'
    case 'rejected':
      return 'danger'
    case 'canceled':
      return 'info'
  }
}

function formatTime(iso: string | null): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString('zh-CN', { hour12: false })
}

async function loadList(reset = true) {
  const status = statusFilter.value === 'all' ? undefined : statusFilter.value
  try {
    await contactStore.fetchRequests(activeDirection.value, status, reset)
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '加载好友申请失败')
  }
}

async function handleAccept(requestId: string) {
  busyId.value = requestId
  try {
    const res = await contactStore.acceptRequest(requestId)
    ElMessage.success('已添加为好友')
    if (res.conversation_id) {
      contactStore.conversationIdMap[res.friend_id] = res.conversation_id
    }
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '操作失败')
  } finally {
    busyId.value = ''
  }
}

async function handleReject(requestId: string) {
  busyId.value = requestId
  try {
    await contactStore.rejectRequest(requestId)
    ElMessage.success('已拒绝申请')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '操作失败')
  } finally {
    busyId.value = ''
  }
}

async function handleCancel(requestId: string) {
  busyId.value = requestId
  try {
    await contactStore.cancelRequest(requestId)
    ElMessage.success('已撤销申请')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '撤销失败')
  } finally {
    busyId.value = ''
  }
}

function openConversation(friendId: string) {
  const convId = contactStore.conversationIdMap[friendId] || contactStore.findFriend(friendId)?.conversation_id
  if (convId) {
    void router.push(`/chat/${convId}`)
  } else {
    void router.push('/chat')
  }
}

onMounted(() => {
  void Promise.all([
    contactStore.fetchRequests('received', undefined, true),
    contactStore.fetchRequests('sent', undefined, true),
  ]).catch(() => {})
})
</script>

<template>
  <div class="requests-pane">
    <div class="requests-pane__toolbar">
      <el-radio-group v-model="activeDirection" size="default" @change="loadList(true)">
        <el-radio-button value="received">
          收到的申请
          <span v-if="contactStore.pendingReceivedCount > 0">
            ({{ contactStore.pendingReceivedCount }})
          </span>
        </el-radio-button>
        <el-radio-button value="sent">发出的申请</el-radio-button>
      </el-radio-group>

      <div class="requests-pane__filters">
        <el-select
          v-model="statusFilter"
          size="default"
          style="width: 120px"
          @change="loadList(true)"
        >
          <el-option label="全部状态" value="all" />
          <el-option label="待处理" value="pending" />
          <el-option label="已同意" value="accepted" />
          <el-option label="已拒绝" value="rejected" />
          <el-option label="已撤销" value="canceled" />
        </el-select>
        <el-button :icon="Refresh" :loading="contactStore.requestsLoading" @click="loadList(true)">
          刷新
        </el-button>
      </div>
    </div>

    <div class="requests-pane__list" v-loading="contactStore.requestsLoading">
      <EmptyState
        v-if="!contactStore.requestsLoading && currentItems.length === 0"
        title="暂无好友申请记录"
        description="您可以通过右上角「添加好友」搜索企业成员并发起好友申请。"
      />

      <div v-else class="ios-grouped-section requests-group">
        <div v-for="item in currentItems" :key="item.id" class="request-card">
          <div class="request-card__left">
            <AppAvatar
              :src="item.peer?.avatar_url"
              :name="item.peer?.nickname || item.peer?.username || '用户'"
              :size="46"
              :show-online="true"
              :online="item.peer?.online ?? false"
              rounded="full"
            />
            <div class="request-card__info">
              <div class="request-card__name-row">
                <span class="request-card__name">
                  {{ item.peer?.nickname || `用户 ${activeDirection === 'received' ? item.from_user_id : item.to_user_id}` }}
                </span>
                <span v-if="item.peer?.username" class="request-card__username">
                  @{{ item.peer.username }}
                </span>
                <el-tag size="small" :type="statusTagType(item.status)" effect="plain">
                  {{ statusLabel(item.status) }}
                </el-tag>
              </div>
              <p class="request-card__msg">
                {{ item.message ? `验证消息：${item.message}` : '无附加验证消息' }}
              </p>
              <p class="request-card__time">
                发起于 {{ formatTime(item.created_at) }}
                <template v-if="item.handled_at">
                  · 处理于 {{ formatTime(item.handled_at) }}
                </template>
              </p>
            </div>
          </div>

          <div class="request-card__actions">
            <template v-if="activeDirection === 'received' && item.status === 'pending'">
              <el-button
                type="primary"
                size="small"
                :icon="Check"
                :loading="busyId === item.id"
                @click="handleAccept(item.id)"
              >
                同意
              </el-button>
              <el-button
                size="small"
                :icon="Close"
                :loading="busyId === item.id"
                @click="handleReject(item.id)"
              >
                拒绝
              </el-button>
            </template>

            <template v-else-if="activeDirection === 'sent' && item.status === 'pending'">
              <el-button
                size="small"
                :loading="busyId === item.id"
                @click="handleCancel(item.id)"
              >
                撤销申请
              </el-button>
            </template>

            <template v-else-if="item.status === 'accepted'">
              <el-button
                type="primary"
                plain
                size="small"
                @click="openConversation(activeDirection === 'received' ? item.from_user_id : item.to_user_id)"
              >
                发消息
              </el-button>
            </template>
          </div>
        </div>
      </div>

      <div v-if="currentHasMore" class="requests-pane__more">
        <el-button :loading="contactStore.requestsLoading" @click="loadList(false)">
          加载更多
        </el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.requests-pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}

.requests-pane__toolbar {
  padding: 14px 24px;
  border-bottom: 0.5px solid var(--im-border-light);
  background-color: var(--im-bg-toolbar);
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.requests-pane__filters {
  display: flex;
  align-items: center;
  gap: 8px;
}

.requests-pane__list {
  flex: 1;
  overflow-y: auto;
  padding: 18px 24px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.requests-group {
  display: flex;
  flex-direction: column;
}

.request-card {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 15px 18px;
  background-color: var(--im-bg-surface);
}

.request-card + .request-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 76px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.request-card__left {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  min-width: 0;
}

.request-card__info {
  min-width: 0;
}

.request-card__name-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.request-card__name {
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.request-card__username {
  font-size: 13px;
  color: var(--im-text-secondary);
}

.request-card__msg {
  margin: 5px 0 3px;
  font-size: 14px;
  color: var(--im-text-secondary);
  word-break: break-word;
}

.request-card__time {
  margin: 0;
  font-size: 12px;
  color: var(--im-text-muted);
}

.request-card__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.requests-pane__more {
  display: flex;
  justify-content: center;
  padding: 8px 0 16px;
}

@media (max-width: 640px) {
  .request-card {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>
