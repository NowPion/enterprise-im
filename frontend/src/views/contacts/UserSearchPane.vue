<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ChatDotRound, Plus, Search } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import AppAvatar from '@/components/common/AppAvatar.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import { BIZ_CODE, isApiError } from '@/types/common'
import type { User, UserSearchField, UserSearchMatch } from '@/types/user'

const router = useRouter()
const authStore = useAuthStore()
const contactStore = useContactStore()

const query = ref('')
const field = ref<UserSearchField>('username')
const match = ref<UserSearchMatch>('prefix')
const hasSearched = ref(false)

const requestDialogVisible = ref(false)
const sendingRequest = ref(false)
const targetUser = ref<User | null>(null)
const verifyForm = reactive({
  message: '',
})

async function handleSearch(reset = true) {
  const q = query.value.trim()
  if (!q) {
    ElMessage.warning('请输入搜索关键字')
    return
  }
  hasSearched.value = true
  try {
    await contactStore.searchUsers(
      {
        q,
        field: field.value,
        match: match.value,
        limit: 20,
      },
      reset,
    )
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '搜索用户失败')
  }
}

function openRequestDialog(user: User) {
  targetUser.value = user
  verifyForm.message = ''
  requestDialogVisible.value = true
}

async function submitFriendRequest() {
  if (!targetUser.value) return
  sendingRequest.value = true
  try {
    await contactStore.sendFriendRequest(targetUser.value.id, verifyForm.message.trim())
    ElMessage.success('好友申请已发送')
    requestDialogVisible.value = false
  } catch (err) {
    if (isApiError(err)) {
      if (err.code === BIZ_CODE.ALREADY_FRIENDS) {
        ElMessage.info('你们已经是好友了')
      } else if (err.code === BIZ_CODE.FRIEND_REQUEST_PENDING) {
        ElMessage.info('已有待处理的好友申请，请勿重复发送')
      } else if (err.code === BIZ_CODE.FRIEND_REQUEST_SELF) {
        ElMessage.warning('不能向自己发送好友申请')
      } else {
        ElMessage.error(err.message || '发送失败')
      }
    } else {
      ElMessage.error('发送失败')
    }
  } finally {
    sendingRequest.value = false
  }
}

function goToChat(userId: string) {
  const friend = contactStore.findFriend(userId)
  if (friend?.conversation_id) {
    void router.push(`/chat/${friend.conversation_id}`)
  } else {
    void router.push('/chat')
  }
}
</script>

<template>
  <div class="search-pane">
    <form class="search-pane__bar" @submit.prevent="handleSearch(true)">
      <el-input
        v-model="query"
        placeholder="输入用户名或昵称搜索"
        clearable
        :prefix-icon="Search"
        class="search-pane__input"
      />
      <el-select v-model="field" style="width: 110px">
        <el-option label="用户名" value="username" />
        <el-option label="昵称" value="nickname" />
      </el-select>
      <el-select v-model="match" style="width: 110px">
        <el-option label="前缀匹配" value="prefix" />
        <el-option label="精确匹配" value="exact" />
      </el-select>
      <el-button type="primary" native-type="submit" :loading="contactStore.searchLoading">
        搜索
      </el-button>
    </form>

    <div class="search-pane__results" v-loading="contactStore.searchLoading">
      <EmptyState
        v-if="!hasSearched"
        title="查找企业成员"
        description="支持按用户名或昵称进行精确或前缀搜索，向对方发送好友申请。"
        :icon="Search"
      />
      <EmptyState
        v-else-if="!contactStore.searchLoading && contactStore.searchResults.length === 0"
        title="未找到匹配用户"
        description="请尝试调整搜索关键词或切换匹配方式。"
        :icon="Search"
      />

      <div v-else class="ios-grouped-section search-group">
        <div v-for="user in contactStore.searchResults" :key="user.id" class="user-card">
          <div class="user-card__left">
            <AppAvatar
              :src="user.avatar_url"
              :name="user.nickname || user.username"
              :size="46"
              :show-online="true"
              :online="user.online"
              rounded="full"
            />
            <div class="user-card__info">
              <div class="user-card__title">
                <span class="user-card__nickname">{{ user.nickname }}</span>
                <span class="user-card__username">@{{ user.username }}</span>
                <el-tag size="small" :type="user.online ? 'success' : 'info'" effect="plain">
                  {{ user.online ? '在线' : '离线' }}
                </el-tag>
              </div>
              <p class="user-card__sig">{{ user.signature || '暂无个性签名' }}</p>
            </div>
          </div>

          <div class="user-card__actions">
            <el-tag v-if="user.id === authStore.user?.id" type="info" effect="plain">本人</el-tag>
            <el-button
              v-else-if="contactStore.friendIdsSet.has(user.id)"
              type="primary"
              plain
              size="small"
              :icon="ChatDotRound"
              @click="goToChat(user.id)"
            >
              发消息
            </el-button>
            <el-button
              v-else
              type="primary"
              size="small"
              :icon="Plus"
              @click="openRequestDialog(user)"
            >
              添加好友
            </el-button>
          </div>
        </div>
      </div>

      <div v-if="contactStore.searchHasMore" class="search-pane__more">
        <el-button :loading="contactStore.searchLoading" @click="handleSearch(false)">
          加载更多
        </el-button>
      </div>
    </div>

    <el-dialog
      v-model="requestDialogVisible"
      title="发送好友申请"
      width="420px"
      destroy-on-close
    >
      <div v-if="targetUser" class="target-preview ios-grouped-section">
        <AppAvatar
          :src="targetUser.avatar_url"
          :name="targetUser.nickname || targetUser.username"
          :size="42"
          rounded="full"
        />
        <div>
          <div class="target-preview__name">{{ targetUser.nickname }}</div>
          <div class="target-preview__sub">@{{ targetUser.username }}</div>
        </div>
      </div>

      <el-form label-position="top">
        <el-form-item label="验证消息（可选）">
          <el-input
            v-model="verifyForm.message"
            type="textarea"
            :rows="3"
            maxlength="255"
            show-word-limit
            placeholder="例如：你好，我是产品部的..."
          />
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="requestDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="sendingRequest" @click="submitFriendRequest">
          发送申请
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.search-pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}

.search-pane__bar {
  padding: 14px 24px;
  border-bottom: 0.5px solid var(--im-border-light);
  background-color: var(--im-bg-toolbar);
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
}

.search-pane__input {
  flex: 1;
  min-width: 180px;
}

.search-pane__results {
  flex: 1;
  overflow-y: auto;
  padding: 18px 24px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.search-group {
  display: flex;
  flex-direction: column;
}

.user-card {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 14px 18px;
  background-color: var(--im-bg-surface);
}

.user-card + .user-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 76px;
  right: 0;
  height: 0.5px;
  background-color: var(--im-border-light);
}

.user-card__left {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}

.user-card__info {
  min-width: 0;
}

.user-card__title {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.user-card__nickname {
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.user-card__username {
  font-size: 13px;
  color: var(--im-text-secondary);
}

.user-card__sig {
  margin: 3px 0 0;
  font-size: 13px;
  color: var(--im-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.user-card__actions {
  flex-shrink: 0;
}

.search-pane__more {
  display: flex;
  justify-content: center;
  padding: 8px 0 16px;
}

.target-preview {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 14px;
  margin-bottom: 16px;
  background-color: var(--im-bg-grouped);
}

.target-preview__name {
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.target-preview__sub {
  font-size: 13px;
  color: var(--im-text-secondary);
}
</style>
