import type { RouteRecordRaw } from 'vue-router'

export const routes: RouteRecordRaw[] = [
  {
    path: '/admin',
    name: 'AdminLayout',
    component: () => import('@/views/admin/AdminLayoutView.vue'),
    redirect: '/admin/overview',
    meta: {
      requiresAuth: true,
      requiresAdmin: true,
      title: '管理后台',
    },
    children: [
      {
        path: 'overview',
        name: 'AdminOverview',
        component: () => import('@/views/admin/AdminOverviewView.vue'),
        meta: {
          requiresAuth: true,
          requiresAdmin: true,
          title: '运行监控',
        },
      },
      {
        path: 'users',
        name: 'AdminUsers',
        component: () => import('@/views/admin/AdminUsersView.vue'),
        meta: {
          requiresAuth: true,
          requiresAdmin: true,
          title: '用户管理',
        },
      },
      {
        path: 'groups',
        name: 'AdminGroups',
        component: () => import('@/views/admin/AdminGroupsView.vue'),
        meta: {
          requiresAuth: true,
          requiresAdmin: true,
          title: '群组监管',
        },
      },
      {
        path: 'audit',
        name: 'AdminAudit',
        component: () => import('@/views/admin/AdminAuditView.vue'),
        meta: {
          requiresAuth: true,
          requiresAdmin: true,
          title: '消息审计',
        },
      },
    ],
  },
]
