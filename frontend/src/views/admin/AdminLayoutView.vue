<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ArrowLeft,
  ChatDotSquare,
  Document,
  Lock,
  Monitor,
  User,
} from '@element-plus/icons-vue'
import { useAdminAccess } from '@/composables/admin/useAdminAccess'

const route = useRoute()
const router = useRouter()
const { currentUser, isAdmin } = useAdminAccess()

const navItems = [
  { path: '/admin/overview', label: '运行监控', icon: Monitor },
  { path: '/admin/users', label: '用户管理', icon: User },
  { path: '/admin/groups', label: '群组监管', icon: ChatDotSquare },
  { path: '/admin/audit', label: '消息审计', icon: Document },
]

const activePath = computed(() => route.path)

function navigateTo(path: string) {
  if (route.path !== path) {
    void router.push(path)
  }
}

function backToChat() {
  void router.push('/chat')
}
</script>

<template>
  <div class="admin-shell">
    <div v-if="!isAdmin" class="forbidden-view">
      <div class="forbidden-card ios-grouped-section">
        <div class="forbidden-icon-wrap">
          <el-icon :size="30" class="forbidden-icon"><Lock /></el-icon>
        </div>
        <h2 class="forbidden-title">无权访问管理后台</h2>
        <p class="forbidden-desc">
          管理后台仅限平台管理员（role = admin）访问。当前账号暂无管理员权限。
        </p>
        <el-button type="primary" :icon="ArrowLeft" @click="backToChat">返回聊天</el-button>
      </div>
    </div>

    <template v-else>
      <header class="admin-topbar">
        <div class="topbar-left">
          <span class="brand-title">管理控制台</span>
          <span class="role-chip">平台管理员</span>
        </div>

        <nav class="admin-nav">
          <button
            v-for="item in navItems"
            :key="item.path"
            type="button"
            class="nav-item"
            :class="{ 'is-active': activePath.startsWith(item.path) }"
            @click="navigateTo(item.path)"
          >
            <el-icon :size="15"><component :is="item.icon" /></el-icon>
            <span>{{ item.label }}</span>
          </button>
        </nav>

        <div class="topbar-right">
          <span v-if="currentUser?.username" class="admin-user">
            {{ currentUser.nickname || currentUser.username }}
          </span>
          <button type="button" class="ios-chevron-back topbar-back" @click="backToChat">
            <el-icon :size="16"><ArrowLeft /></el-icon>
            <span>返回聊天</span>
          </button>
        </div>
      </header>

      <main class="admin-content">
        <router-view />
      </main>
    </template>
  </div>
</template>

<style scoped>
.admin-shell {
  display: flex;
  flex-direction: column;
  width: 100%;
  height: 100%;
  min-height: 0;
  background-color: var(--im-bg-grouped);
  color: var(--im-text-primary);
}

.forbidden-view {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

.forbidden-card {
  width: 420px;
  max-width: 100%;
  padding: 32px 26px;
  text-align: center;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
}

.forbidden-icon-wrap {
  width: 60px;
  height: 60px;
  border-radius: 16px;
  background-color: rgba(255, 59, 48, 0.12);
  display: flex;
  align-items: center;
  justify-content: center;
}

.forbidden-icon {
  color: var(--im-status-danger);
}

.forbidden-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.forbidden-desc {
  margin: 0 0 8px;
  font-size: 14px;
  color: var(--im-text-secondary);
  line-height: 1.55;
}

.admin-topbar {
  background-color: var(--im-bg-toolbar);
  backdrop-filter: blur(20px) saturate(180%);
  -webkit-backdrop-filter: blur(20px) saturate(180%);
  border-bottom: 0.5px solid var(--im-separator);
  padding: 0 20px;
  height: 58px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.topbar-left {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.brand-title {
  font-size: 17px;
  font-weight: 700;
  color: var(--im-text-primary);
}

.role-chip {
  font-size: 11px;
  font-weight: 600;
  padding: 2px 8px;
  background-color: var(--im-accent-primary-soft);
  color: var(--im-accent-primary);
  border-radius: 9999px;
}

.admin-nav {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 3px;
  border-radius: 10px;
  background-color: var(--im-bg-fill);
  overflow-x: auto;
}

.nav-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 13px;
  border: none;
  background: transparent;
  border-radius: 8px;
  font-size: 13px;
  font-weight: 500;
  color: var(--im-text-secondary);
  cursor: pointer;
  white-space: nowrap;
  transition: background-color 0.16s var(--im-ease-ios), color 0.16s ease, box-shadow 0.16s ease;
}

.nav-item:hover {
  color: var(--im-text-primary);
}

.nav-item.is-active {
  background-color: var(--im-bg-surface);
  color: var(--im-accent-primary);
  font-weight: 600;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.1);
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-shrink: 0;
}

.admin-user {
  font-size: 13px;
  color: var(--im-text-secondary);
}

.topbar-back {
  font-size: 14px;
}

.admin-content {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 20px 24px;
}

@media (max-width: 768px) {
  .admin-topbar {
    height: auto;
    padding: 10px 12px;
    flex-wrap: wrap;
  }

  .admin-content {
    padding: 12px;
  }
}
</style>
