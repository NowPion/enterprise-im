<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ArrowLeft, ArrowRight, Bell, Plus, Search, User } from '@element-plus/icons-vue'
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
        <h2 class="contacts-list-pane__title ios-large-title">联系人</h2>
        <el-button type="primary" plain size="small" :icon="Plus" @click="openSearch">
          添加好友
        </el-button>
      </div>

      <div class="contacts-list-pane__search">
        <el-input
          v-model="filterKeyword"
          clearable
          placeholder="筛选好友昵称/备注/用户名"
          :prefix-icon="Search"
        />
      </div>

      <div class="contacts-list-pane__scroll" v-loading="contactStore.friendsLoading">
        <div class="ios-grouped-section contacts-group">
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
            <el-icon class="contact-entry__chevron" :size="14"><ArrowRight /></el-icon>
          </button>
        </div>

        <div class="contacts-section-label ios-section-caption">
          我的好友（{{ contactStore.friends.length }}）
        </div>

        <div v-if="filteredFriends.length === 0 && !contactStore.friendsLoading" class="ios-grouped-section contacts-empty">
          {{ filterKeyword ? '无匹配好友' : '暂无好友，点击上方「添加好友」' }}
        </div>

        <div v-else class="ios-grouped-section contacts-group">
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
              :size="40"
              :show-online="true"
              :online="friend.online"
              rounded="full"
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
            <el-icon class="contact-entry__chevron" :size="14"><ArrowRight /></el-icon>
          </button>
        </div>

        <div v-if="contactStore.friendsHasMore" class="contacts-load-more">
          <el-button
            text
            type="primary"
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
          class="mobile-back-btn ios-chevron-back"
          @click="mobileShowDetail = false"
        >
          <el-icon :size="18"><ArrowLeft /></el-icon>
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
  background-color: var(--im-bg-grouped);
}

.contacts-list-pane {
  width: var(--im-list-width);
  flex-shrink: 0;
  background-color: var(--im-bg-grouped);
  border-right: 0.5px solid var(--im-separator);
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.contacts-list-pane__header {
  padding: 16px 14px 10px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  background-color: var(--im-bg-sidebar);
  backdrop-filter: blur(18px);
}

.contacts-list-pane__title {
  font-size: 26px;
}

.contacts-list-pane__search {
  padding: 0 14px 12px;
  background-color: var(--im-bg-sidebar);
}

.contacts-list-pane__scroll {
  flex: 1;
  overflow-y: auto;
  padding: 2px 12px 16px;
}

.contacts-group {
  margin-bottom: 4px;
}

.contacts-section-label {
  padding: 14px 6px 6px;
}

.contacts-empty {
  padding: 24px 14px;
  text-align: center;
  font-size: 13px;
  color: var(--im-text-secondary);
}

.contact-entry {
  position: relative;
  width: 100%;
  border: none;
  background: var(--im-bg-surface);
  padding: 10px 12px;
  display: flex;
  align-items: center;
  gap: 11px;
  text-align: left;
  cursor: pointer;
  transition: background-color 0.14s var(--im-ease-ios);
}

.contact-entry + .contact-entry::before {
  content: '';
  position: absolute;
  top: 0;
  left: 63px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.contact-entry:hover {
  background-color: var(--im-bg-hover);
}

.contact-entry:active {
  background-color: rgba(120, 120, 128, 0.18);
}

.contact-entry.is-active {
  background-color: var(--im-bg-active);
}

.contact-entry__icon {
  width: 40px;
  height: 40px;
  border-radius: 11px;
  background-color: var(--im-status-warning);
  color: #ffffff;
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
  font-size: 16px;
  font-weight: 500;
  color: var(--im-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: block;
}

.contact-entry__sub {
  margin: 2px 0 0;
  font-size: 13px;
  color: var(--im-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.contact-entry__badge {
  min-width: 19px;
  height: 19px;
  padding: 0 6px;
  border-radius: 9999px;
  background-color: var(--im-status-unread);
  color: #ffffff;
  font-size: 11px;
  font-weight: 600;
  line-height: 19px;
  text-align: center;
}

.contact-entry__chevron {
  color: var(--im-text-tertiary);
  flex-shrink: 0;
}

.contacts-load-more {
  display: flex;
  justify-content: center;
  padding: 10px 0;
}

.contacts-detail-pane {
  flex: 1;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background-color: var(--im-bg-grouped);
}

.contacts-detail-pane__header {
  height: 58px;
  padding: 0 24px;
  display: flex;
  align-items: center;
  gap: 12px;
  background-color: var(--im-bg-toolbar);
  backdrop-filter: blur(20px) saturate(180%);
  -webkit-backdrop-filter: blur(20px) saturate(180%);
  border-bottom: 0.5px solid var(--im-separator);
  flex-shrink: 0;
}

.contacts-detail-pane__title {
  margin: 0;
  font-size: 17px;
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
