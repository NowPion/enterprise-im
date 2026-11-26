<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChatDotSquare, Monitor, UserFilled } from '@element-plus/icons-vue'
import EmptyState from './EmptyState.vue'

interface Props {
  moduleName: string
  description?: string
}

const props = withDefaults(defineProps<Props>(), {
  description: '该业务模块路由尚未挂载，整合对应功能分支后将自动启用。',
})

const route = useRoute()
const router = useRouter()

const icon = computed(() => {
  if (route.path.startsWith('/groups')) return UserFilled
  if (route.path.startsWith('/admin')) return Monitor
  return ChatDotSquare
})
</script>

<template>
  <div class="placeholder-panel">
    <aside class="placeholder-panel__list">
      <div class="placeholder-panel__header">
        <span>{{ moduleName }}</span>
      </div>
      <div class="placeholder-panel__list-body">
        <p class="placeholder-panel__hint">当前路径：{{ route.fullPath }}</p>
      </div>
    </aside>
    <section class="placeholder-panel__main">
      <EmptyState :title="moduleName" :description="props.description" :icon="icon">
        <el-button @click="router.push('/contacts')">前往联系人</el-button>
      </EmptyState>
    </section>
  </div>
</template>

<style scoped>
.placeholder-panel {
  display: flex;
  width: 100%;
  height: 100%;
  min-height: 0;
}

.placeholder-panel__list {
  width: var(--im-list-width);
  flex-shrink: 0;
  background-color: var(--im-bg-sidebar);
  border-right: 1px solid var(--im-border-base);
  display: flex;
  flex-direction: column;
}

.placeholder-panel__header {
  height: var(--im-header-height);
  padding: 0 16px;
  display: flex;
  align-items: center;
  font-size: 16px;
  font-weight: 600;
  color: var(--im-text-primary);
  border-bottom: 1px solid var(--im-border-light);
}

.placeholder-panel__list-body {
  padding: 16px;
}

.placeholder-panel__hint {
  margin: 0;
  font-size: 12px;
  color: var(--im-text-muted);
  word-break: break-all;
}

.placeholder-panel__main {
  flex: 1;
  background-color: var(--im-bg-surface);
  min-width: 0;
}

@media (max-width: 767px) {
  .placeholder-panel__list {
    display: none;
  }
}
</style>
