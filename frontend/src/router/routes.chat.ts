import type { RouteRecordRaw } from 'vue-router'

export const routes: RouteRecordRaw[] = [
  {
    path: '/chat',
    name: 'Chat',
    component: () => import('@/views/chat/ChatView.vue'),
    meta: { requiresAuth: true, title: '消息' },
  },
  {
    path: '/chat/:conversationId',
    name: 'ChatConversation',
    component: () => import('@/views/chat/ChatView.vue'),
    meta: { requiresAuth: true, title: '消息' },
  },
]
