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
      <div class="forbidden-card">
        <el-icon :size="36" class="forbidden-icon"><Lock /></el-icon>
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
          <el-button size="small" :icon="ArrowLeft" @click="backToChat">返回聊天</el-button>
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
  height: 100%;
  min-height: 0;
  background-color: #f5f6f8;
  color: #111827;
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
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 32px 24px;
  text-align: center;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
}

.forbidden-icon {
  color: #dc2626;
}

.forbidden-title {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: #111827;
}

.forbidden-desc {
  margin: 0 0 8px;
  font-size: 13px;
  color: #4b5563;
  line-height: 1.6;
}

.admin-topbar {
  background-color: #ffffff;
  border-bottom: 1px solid #e5e7eb;
  padding: 0 20px;
  height: 52px;
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
  font-size: 15px;
  font-weight: 600;
  color: #111827;
}

.role-chip {
  font-size: 11px;
  padding: 2px 6px;
  background-color: #111827;
  color: #ffffff;
  border-radius: 3px;
}

.admin-nav {
  display: flex;
  align-items: center;
  gap: 4px;
  overflow-x: auto;
}

.nav-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border: 1px solid transparent;
  background: transparent;
  border-radius: 5px;
  font-size: 13px;
  font-weight: 500;
  color: #4b5563;
  cursor: pointer;
  white-space: nowrap;
}

.nav-item:hover {
  background-color: #f5f6f8;
  color: #111827;
}

.nav-item.is-active {
  background-color: #eceef2;
  color: #111827;
  border-color: #d1d5db;
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}

.admin-user {
  font-size: 12px;
  color: #4b5563;
}

.admin-content {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 16px 20px;
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
