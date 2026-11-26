import type { RouteRecordRaw } from 'vue-router'

export const authPublicRoutes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'Login',
    component: () => import('@/views/login/LoginView.vue'),
    meta: { public: true, title: '登录' },
  },
  {
    path: '/register',
    name: 'Register',
    component: () => import('@/views/register/RegisterView.vue'),
    meta: { public: true, title: '注册' },
  },
]

export const authAppRoutes: RouteRecordRaw[] = [
  {
    path: '/contacts',
    name: 'Contacts',
    component: () => import('@/views/contacts/ContactsView.vue'),
    meta: { requiresAuth: true, title: '联系人' },
  },
]
