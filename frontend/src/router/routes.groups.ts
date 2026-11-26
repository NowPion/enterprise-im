import type { RouteRecordRaw } from 'vue-router'

export const routes: RouteRecordRaw[] = [
  {
    path: '/groups',
    name: 'Groups',
    component: () => import('@/views/groups/GroupsView.vue'),
    meta: {
      requiresAuth: true,
      title: '群聊管理',
    },
  },
  {
    path: '/groups/:groupId',
    name: 'GroupDetail',
    component: () => import('@/views/groups/GroupsView.vue'),
    meta: {
      requiresAuth: true,
      title: '群聊详情与管理',
    },
  },
]
