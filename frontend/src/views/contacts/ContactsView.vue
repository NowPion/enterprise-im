<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ArrowLeft, Bell, Plus, Search, User } from '@element-plus/icons-vue'
import AppAvatar from '@/components/common/AppAvatar.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import { useContactStore } from '@/stores/contact'
import FriendDetailPane from './FriendDetailPane.vue'
import FriendRequestsPane from './FriendRequestsPane.vue'
import UserSearchPane from './UserSearchPane.vue'

type RightPaneMode = 'empty' | 'requests' | 'search' | 'friend'

const contactStore = useContactStore()

const filterKeyword = ref('')
const paneMode = ref<RightPaneMode>('requests')
const selectedFriendId = ref<string>('')
const mobileShowDetail = ref(false)

const filteredFriends = computed(() => {
  const kw = filterKeyword.value.trim().toLowerCase()
  if (!kw) return contactStore.friends
  return contactStore.friends.filter((item) => {
    const remark = item.remark.toLowerCase()
    const nickname = item.user.nickname.toLowerCase()
    const username = item.user.username.toLowerCase()
    return remark.includes(kw) || nickname.includes(kw) || username.includes(kw)
  })
})

const selectedFriend = computed(() =>
  selectedFriendId.value ? contactStore.findFriend(selectedFriendId.value) : undefined,
)

const rightTitle = computed(() => {
  if (paneMode.value === 'requests') return '好友申请'
  if (paneMode.value === 'search') return '添加好友'
  if (paneMode.value === 'friend' && selectedFriend.value) {
    return selectedFriend.value.remark || selectedFriend.value.user.nickname
  }
  return '联系人详情'
})

function openRequests() {
  paneMode.value = 'requests'
  selectedFriendId.value = ''
  mobileShowDetail.value = true
}

function openSearch() {
  paneMode.value = 'search'
  selectedFriendId.value = ''
  mobileShowDetail.value = true
}

function selectFriend(friendId: string) {
  selectedFriendId.value = friendId
  paneMode.value = 'friend'
  mobileShowDetail.value = true
}

function handleFriendDeleted() {
  selectedFriendId.value = ''
  paneMode.value = 'empty'
  mobileShowDetail.value = false
}

onMounted(() => {
  contactStore.bindRealtimeListeners()
  void Promise.all([
    contactStore.fetchFriends(true),
    contactStore.fetchRequests('received', undefined, true),
  ]).catch(() => {})
})
</script>

<template>
  <div class="contacts-page" :class="{ 'is-mobile-detail': mobileShowDetail }">
    <aside class="contacts-list-pane">
      <div class="contacts-list-pane__header">
        <h2 class="contacts-list-pane__title">联系人</h2>
        <el-button size="small" :icon="Plus" @click="openSearch">添加好友</el-button>
      </div>

      <div class="contacts-list-pane__search">
        <el-input
          v-model="filterKeyword"
          size="small"
          clearable
          placeholder="筛选好友昵称/备注/用户名"
          :prefix-icon="Search"
        />
      </div>

      <div class="contacts-list-pane__scroll" v-loading="contactStore.friendsLoading">
        <button
          type="button"
          class="contact-entry contact-entry--special"
          :class="{ 'is-active': paneMode === 'requests' }"
          @click="openRequests"
        >
          <div class="contact-entry__icon">
            <el-icon :size="18"><Bell /></el-icon>
          </div>
          <div class="contact-entry__body">
            <span class="contact-entry__name">新的朋友 / 好友申请</span>
          </div>
          <span
            v-if="contactStore.pendingReceivedCount > 0"
            class="contact-entry__badge"
          >
            {{ contactStore.pendingReceivedCount }}
          </span>
        </button>

        <div class="contacts-section-label">
          我的好友（{{ contactStore.friends.length }}）
        </div>

        <div v-if="filteredFriends.length === 0 && !contactStore.friendsLoading" class="contacts-empty">
          {{ filterKeyword ? '无匹配好友' : '暂无好友，点击上方「添加好友」' }}
        </div>

        <button
          v-for="friend in filteredFriends"
          :key="friend.friend_id"
          type="button"
          class="contact-entry"
          :class="{ 'is-active': paneMode === 'friend' && selectedFriendId === friend.friend_id }"
          @click="selectFriend(friend.friend_id)"
        >
          <AppAvatar
            :src="friend.user.avatar_url"
            :name="friend.remark || friend.user.nickname"
            :size="36"
            :show-online="true"
            :online="friend.online"
          />
          <div class="contact-entry__body">
            <div class="contact-entry__top">
              <span class="contact-entry__name">
                {{ friend.remark || friend.user.nickname }}
              </span>
            </div>
            <p class="contact-entry__sub">
              {{ friend.user.signature || `@${friend.user.username}` }}
            </p>
          </div>
        </button>

        <div v-if="contactStore.friendsHasMore" class="contacts-load-more">
          <el-button
            text
            size="small"
            :loading="contactStore.friendsLoading"
            @click="contactStore.fetchFriends(false)"
          >
            加载更多好友
          </el-button>
        </div>
      </div>
    </aside>

    <section class="contacts-detail-pane">
      <header class="contacts-detail-pane__header">
        <button
          type="button"
          class="mobile-back-btn"
          @click="mobileShowDetail = false"
        >
          <el-icon><ArrowLeft /></el-icon>
          <span>返回</span>
        </button>
        <h3 class="contacts-detail-pane__title">{{ rightTitle }}</h3>
      </header>

      <div class="contacts-detail-pane__body">
        <FriendRequestsPane v-if="paneMode === 'requests'" />
        <UserSearchPane v-else-if="paneMode === 'search'" />
        <FriendDetailPane
          v-else-if="paneMode === 'friend' && selectedFriend"
          :friend="selectedFriend"
          @deleted="handleFriendDeleted"
        />
        <EmptyState
          v-else
          title="选择联系人查看详情"
          description="从左侧选择好友、处理好友申请或搜索新联系人。"
          :icon="User"
        />
      </div>
    </section>
  </div>
</template>

<style scoped>
.contacts-page {
  display: flex;
  width: 100%;
  height: 100%;
  min-height: 0;
}

.contacts-list-pane {
  width: var(--im-list-width);
  flex-shrink: 0;
  background-color: var(--im-bg-sidebar);
  border-right: 1px solid var(--im-border-base);
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.contacts-list-pane__header {
  height: var(--im-header-height);
  padding: 0 16px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid var(--im-border-light);
}

.contacts-list-pane__title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.contacts-list-pane__search {
  padding: 10px 12px;
  border-bottom: 1px solid var(--im-border-light);
}

.contacts-list-pane__scroll {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}

.contacts-section-label {
  padding: 12px 8px 6px;
  font-size: 12px;
  font-weight: 500;
  color: var(--im-text-muted);
}

.contacts-empty {
  padding: 20px 12px;
  text-align: center;
  font-size: 12px;
  color: var(--im-text-muted);
}

.contact-entry {
  width: 100%;
  border: none;
  background: transparent;
  padding: 10px;
  border-radius: var(--im-radius-md);
  display: flex;
  align-items: center;
  gap: 10px;
  text-align: left;
  cursor: pointer;
  transition: background-color 0.12s ease;
}

.contact-entry:hover {
  background-color: var(--im-bg-hover);
}

.contact-entry.is-active {
  background-color: var(--im-bg-active);
}

.contact-entry__icon {
  width: 36px;
  height: 36px;
  border-radius: 8px;
  background-color: var(--im-bg-rail);
  color: var(--im-text-inverse);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.contact-entry__body {
  flex: 1;
  min-width: 0;
}

.contact-entry__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.contact-entry__name {
  font-size: 14px;
  font-weight: 500;
  color: var(--im-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: block;
}

.contact-entry__sub {
  margin: 2px 0 0;
  font-size: 12px;
  color: var(--im-text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.contact-entry__badge {
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
}

.contacts-load-more {
  display: flex;
  justify-content: center;
  padding: 8px 0;
}

.contacts-detail-pane {
  flex: 1;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background-color: var(--im-bg-surface);
}

.contacts-detail-pane__header {
  height: var(--im-header-height);
  padding: 0 24px;
  display: flex;
  align-items: center;
  gap: 12px;
  border-bottom: 1px solid var(--im-border-light);
  flex-shrink: 0;
}

.contacts-detail-pane__title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.contacts-detail-pane__body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}

.mobile-back-btn {
  display: none;
  border: none;
  background: transparent;
  padding: 4px 8px;
  border-radius: 6px;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  color: var(--im-text-secondary);
  cursor: pointer;
}

.mobile-back-btn:hover {
  background-color: var(--im-bg-hover);
}

@media (max-width: 767px) {
  .contacts-list-pane {
    width: 100%;
    border-right: none;
  }

  .contacts-detail-pane {
    display: none;
  }

  .contacts-page.is-mobile-detail .contacts-list-pane {
    display: none;
  }

  .contacts-page.is-mobile-detail .contacts-detail-pane {
    display: flex;
  }

  .mobile-back-btn {
    display: inline-flex;
  }
}
</style>
