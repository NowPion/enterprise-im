<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ChatDotRound,
  Monitor,
  Setting,
  SwitchButton,
  User,
  UserFilled,
} from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import AppAvatar from '@/components/common/AppAvatar.vue'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import type { LogoutScope } from '@/types/auth'

const emit = defineEmits<{
  openProfile: []
}>()

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const contactStore = useContactStore()

interface NavItem {
  key: string
  label: string
  path: string
  matchPrefix: string
  badge?: number
  adminOnly?: boolean
}

const navItems = computed<NavItem[]>(() => {
  const items: NavItem[] = [
    {
      key: 'chat',
      label: '消息',
      path: '/chat',
      matchPrefix: '/chat',
    },
    {
      key: 'contacts',
      label: '联系人',
      path: '/contacts',
      matchPrefix: '/contacts',
      badge: contactStore.pendingReceivedCount,
    },
    {
      key: 'groups',
      label: '群聊',
      path: '/groups',
      matchPrefix: '/groups',
    },
  ]

  if (authStore.isAdmin) {
    items.push({
      key: 'admin',
      label: '后台',
      path: '/admin',
      matchPrefix: '/admin',
      adminOnly: true,
    })
  }
  return items
})

function isActive(prefix: string): boolean {
  return route.path === prefix || route.path.startsWith(`${prefix}/`)
}

function navigateTo(path: string) {
  if (route.path !== path) {
    void router.push(path)
  }
}

async function handleLogout(scope: LogoutScope) {
  const title = scope === 'all' ? '下线全部设备' : '退出当前登录'
  const message =
    scope === 'all'
      ? '确定要注销当前账号在所有设备上的登录会话吗？'
      : '确定要退出当前设备登录吗？'

  try {
    await ElMessageBox.confirm(message, title, {
      confirmButtonText: '确认退出',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }

  await authStore.logout(scope)
  contactStore.reset()
  ElMessage.success(scope === 'all' ? '已注销全部设备会话' : '已退出登录')
  await router.replace('/login')
}
</script>

<template>
  <nav class="nav-rail" aria-label="主导航">
    <div class="nav-rail__top">
      <button
        type="button"
        class="nav-rail__avatar-btn"
        :title="`${authStore.user?.nickname || '个人中心'} (${authStore.wsState === 'connected' ? '实时连接正常' : '连接中/离线'})`"
        @click="emit('openProfile')"
      >
        <AppAvatar
          :src="authStore.user?.avatar_url"
          :name="authStore.user?.nickname || authStore.user?.username"
          :size="40"
          :show-online="true"
          :online="authStore.wsState === 'connected'"
          rounded="full"
        />
      </button>

      <div class="nav-rail__menu">
        <button
          v-for="item in navItems"
          :key="item.key"
          type="button"
          class="nav-rail__item"
          :class="{ 'is-active': isActive(item.matchPrefix) }"
          :title="item.label"
          @click="navigateTo(item.path)"
        >
          <span class="nav-rail__icon-wrap">
            <el-icon :size="20">
              <ChatDotRound v-if="item.key === 'chat'" />
              <User v-else-if="item.key === 'contacts'" />
              <UserFilled v-else-if="item.key === 'groups'" />
              <Monitor v-else />
            </el-icon>
            <span v-if="item.badge && item.badge > 0" class="nav-rail__badge">
              {{ item.badge > 99 ? '99+' : item.badge }}
            </span>
          </span>
          <span class="nav-rail__label">{{ item.label }}</span>
        </button>
      </div>
    </div>

    <div class="nav-rail__bottom">
      <button
        type="button"
        class="nav-rail__item"
        title="个人资料与安全"
        @click="emit('openProfile')"
      >
        <span class="nav-rail__icon-wrap">
          <el-icon :size="20"><Setting /></el-icon>
        </span>
        <span class="nav-rail__label">设置</span>
      </button>

      <el-dropdown trigger="click" placement="right-end" @command="handleLogout">
        <button type="button" class="nav-rail__item" title="退出登录">
          <span class="nav-rail__icon-wrap">
            <el-icon :size="20"><SwitchButton /></el-icon>
          </span>
          <span class="nav-rail__label">退出</span>
        </button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="current">退出当前设备</el-dropdown-item>
            <el-dropdown-item command="all" divided>下线全部设备</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </div>
  </nav>
</template>

<style scoped>
.nav-rail {
  width: var(--im-rail-width);
  flex-shrink: 0;
  background-color: var(--im-bg-rail);
  backdrop-filter: blur(22px) saturate(180%);
  -webkit-backdrop-filter: blur(22px) saturate(180%);
  color: var(--im-text-primary);
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  align-items: center;
  padding: 14px 6px;
  border-right: 0.5px solid var(--im-separator);
  user-select: none;
}

.nav-rail__top {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 18px;
  width: 100%;
}

.nav-rail__avatar-btn {
  border: none;
  padding: 2px;
  background: transparent;
  cursor: pointer;
  border-radius: 9999px;
  transition: transform 0.14s var(--im-ease-ios), background-color 0.14s ease;
}

.nav-rail__avatar-btn:hover {
  background-color: var(--im-bg-hover);
}

.nav-rail__avatar-btn:active {
  transform: scale(0.95);
}

.nav-rail__menu {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  width: 100%;
}

.nav-rail__bottom {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  width: 100%;
}

.nav-rail__item {
  position: relative;
  width: 58px;
  padding: 6px 4px;
  border: none;
  border-radius: 12px;
  background: transparent;
  color: var(--im-text-secondary);
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  cursor: pointer;
  transition: background-color 0.16s var(--im-ease-ios),
    color 0.16s var(--im-ease-ios),
    transform 0.12s var(--im-ease-ios);
}

.nav-rail__item:hover {
  background-color: var(--im-bg-hover);
  color: var(--im-text-primary);
}

.nav-rail__item:active {
  transform: scale(0.96);
}

.nav-rail__item.is-active {
  background-color: var(--im-accent-primary-soft);
  color: var(--im-accent-primary);
}

.nav-rail__icon-wrap {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.nav-rail__label {
  display: block;
  font-size: 11px;
  font-weight: 500;
  line-height: 1.15;
}

.nav-rail__item.is-active .nav-rail__label {
  font-weight: 600;
}

.nav-rail__badge {
  position: absolute;
  top: -5px;
  right: -10px;
  min-width: 17px;
  height: 17px;
  padding: 0 5px;
  border-radius: 9999px;
  background-color: var(--im-status-unread);
  color: #ffffff;
  font-size: 10px;
  font-weight: 600;
  line-height: 17px;
  text-align: center;
  box-shadow: 0 0 0 1.5px #f2f2f7;
}

@media (max-width: 767px) {
  .nav-rail {
    width: 100%;
    height: 58px;
    flex-direction: row;
    padding: 4px 10px;
    border-right: none;
    border-top: 0.5px solid var(--im-separator);
  }

  .nav-rail__top {
    flex-direction: row;
    gap: 4px;
    flex: 1;
  }

  .nav-rail__avatar-btn {
    display: none;
  }

  .nav-rail__menu {
    flex-direction: row;
    justify-content: space-around;
    gap: 2px;
  }

  .nav-rail__bottom {
    width: auto;
    flex-direction: row;
    gap: 2px;
  }

  .nav-rail__item {
    width: 52px;
    padding: 4px 2px;
    gap: 2px;
  }
}
</style>
