import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { ElMessage } from 'element-plus'
import AppLayout from '@/layouts/AppLayout.vue'
import PlaceholderPanel from '@/components/common/PlaceholderPanel.vue'
import { authAppRoutes, authPublicRoutes } from '@/router/routes.auth'
import { setAuthRedirectHandler, useAuthStore } from '@/stores/auth'

const featureModules = import.meta.glob<{ routes: RouteRecordRaw[] }>(
  './routes.{chat,groups,admin}.ts',
  { eager: true },
)
const featureRoutes = Object.values(featureModules).flatMap((m) => m.routes ?? [])

function hasRoutePrefix(routes: RouteRecordRaw[], prefix: string): boolean {
  const normalized = prefix.replace(/^\/+/, '')
  return routes.some((r) => {
    const p = r.path.replace(/^\/+/, '')
    return p === normalized || p.startsWith(`${normalized}/`)
  })
}

const fallbackFeatureRoutes: RouteRecordRaw[] = []

if (!hasRoutePrefix(featureRoutes, '/chat')) {
  fallbackFeatureRoutes.push(
    {
      path: '/chat',
      name: 'ChatPlaceholder',
      component: PlaceholderPanel,
      props: {
        moduleName: '会话与消息',
        description: '聊天模块路由（routes.chat.ts）尚未挂载，整合聊天分支后即可使用。',
      },
      meta: { title: '消息' },
    },
    {
      path: '/chat/:conversationId',
      name: 'ChatConversationPlaceholder',
      component: PlaceholderPanel,
      props: {
        moduleName: '会话详情',
        description: '聊天模块路由（routes.chat.ts）尚未挂载，整合聊天分支后即可进入此会话。',
      },
      meta: { title: '消息' },
    },
  )
}

if (!hasRoutePrefix(featureRoutes, '/groups')) {
  fallbackFeatureRoutes.push({
    path: '/groups',
    name: 'GroupsPlaceholder',
    component: PlaceholderPanel,
    props: {
      moduleName: '群聊管理',
      description: '群聊模块路由（routes.groups.ts）尚未挂载，整合群聊分支后即可使用。',
    },
    meta: { title: '群聊' },
  })
}

if (!hasRoutePrefix(featureRoutes, '/admin')) {
  fallbackFeatureRoutes.push({
    path: '/admin',
    name: 'AdminPlaceholder',
    component: PlaceholderPanel,
    props: {
      moduleName: '管理后台',
      description: '管理后台路由（routes.admin.ts）尚未挂载，整合管理分支后即可使用。',
    },
    meta: { title: '管理后台', requiresAdmin: true },
  })
}

const routes: RouteRecordRaw[] = [
  ...authPublicRoutes,
  {
    path: '/',
    component: AppLayout,
    redirect: '/chat',
    children: [
      ...authAppRoutes,
      ...featureRoutes,
      ...fallbackFeatureRoutes,
      {
        path: ':pathMatch(.*)*',
        redirect: '/chat',
      },
    ],
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

setAuthRedirectHandler(() => {
  const current = router.currentRoute.value
  if (current.path !== '/login') {
    void router.replace({
      path: '/login',
      query: current.fullPath && current.fullPath !== '/' ? { redirect: current.fullPath } : undefined,
    })
  }
})

router.beforeEach(async (to) => {
  const authStore = useAuthStore()
  if (!authStore.initialized) {
    await authStore.initAuth()
  }

  const isPublic = to.matched.some((record) => record.meta.public === true)
  const requiresAdmin = to.matched.some((record) => record.meta.requiresAdmin === true)

  if (!authStore.isAuthenticated) {
    if (isPublic) return true
    return {
      path: '/login',
      query: to.fullPath && to.fullPath !== '/' ? { redirect: to.fullPath } : undefined,
    }
  }

  if (isPublic) {
    return '/chat'
  }

  if (requiresAdmin && authStore.user?.role !== 'admin') {
    ElMessage.warning('需要平台管理员权限')
    return '/chat'
  }

  return true
})

router.afterEach((to) => {
  const pageTitle = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = pageTitle ? `${pageTitle} - 企业即时通讯` : '企业即时通讯'
})

export default router
