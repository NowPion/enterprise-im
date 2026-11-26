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
          :size="38"
          :show-online="true"
          :online="authStore.wsState === 'connected'"
          rounded="md"
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
          <el-icon :size="20">
            <ChatDotRound v-if="item.key === 'chat'" />
            <User v-else-if="item.key === 'contacts'" />
            <UserFilled v-else-if="item.key === 'groups'" />
            <Monitor v-else />
          </el-icon>
          <span class="nav-rail__label">{{ item.label }}</span>
          <span v-if="item.badge && item.badge > 0" class="nav-rail__badge">
            {{ item.badge > 99 ? '99+' : item.badge }}
          </span>
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
        <el-icon :size="20"><Setting /></el-icon>
        <span class="nav-rail__label">设置</span>
      </button>

      <el-dropdown trigger="click" placement="right-end" @command="handleLogout">
        <button type="button" class="nav-rail__item" title="退出登录">
          <el-icon :size="20"><SwitchButton /></el-icon>
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
  color: var(--im-text-inverse);
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  align-items: center;
  padding: 16px 0;
  border-right: 1px solid #1f2937;
  user-select: none;
}

.nav-rail__top {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 20px;
  width: 100%;
}

.nav-rail__avatar-btn {
  border: none;
  padding: 0;
  background: transparent;
  cursor: pointer;
  border-radius: 8px;
}

.nav-rail__menu {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  width: 100%;
}

.nav-rail__bottom {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  width: 100%;
}

.nav-rail__item {
  position: relative;
  width: 44px;
  height: 44px;
  border: none;
  border-radius: 8px;
  background: transparent;
  color: #9ca3af;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  transition: background-color 0.15s ease, color 0.15s ease;
}

.nav-rail__item:hover {
  background-color: #1f2937;
  color: #ffffff;
}

.nav-rail__item.is-active {
  background-color: #2563eb;
  color: #ffffff;
}

.nav-rail__label {
  display: none;
  font-size: 11px;
  line-height: 1.2;
}

.nav-rail__badge {
  position: absolute;
  top: 2px;
  right: 2px;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 9999px;
  background-color: var(--im-status-unread);
  color: #ffffff;
  font-size: 10px;
  font-weight: 600;
  line-height: 16px;
  text-align: center;
}

@media (max-width: 767px) {
  .nav-rail {
    width: 100%;
    height: 56px;
    flex-direction: row;
    padding: 0 12px;
    border-right: none;
    border-top: 1px solid #1f2937;
  }

  .nav-rail__top {
    flex-direction: row;
    gap: 8px;
    flex: 1;
  }

  .nav-rail__avatar-btn {
    display: none;
  }

  .nav-rail__menu {
    flex-direction: row;
    justify-content: space-around;
    gap: 4px;
  }

  .nav-rail__bottom {
    width: auto;
    flex-direction: row;
    gap: 4px;
  }

  .nav-rail__item {
    width: 52px;
    height: 44px;
    gap: 2px;
  }

  .nav-rail__label {
    display: block;
  }
}
</style>
