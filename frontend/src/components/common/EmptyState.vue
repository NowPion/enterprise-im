<script setup lang="ts">
import type { Component } from 'vue'
import { ChatDotRound } from '@element-plus/icons-vue'

interface Props {
  title: string
  description?: string
  icon?: Component
}

withDefaults(defineProps<Props>(), {
  description: '',
  icon: () => ChatDotRound,
})
</script>

<template>
  <div class="empty-state">
    <div class="empty-state__icon">
      <el-icon :size="32">
        <component :is="icon" />
      </el-icon>
    </div>
    <p class="empty-state__title">{{ title }}</p>
    <p v-if="description" class="empty-state__desc">{{ description }}</p>
    <div v-if="$slots.default" class="empty-state__actions">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  text-align: center;
  padding: 40px 24px;
  color: var(--im-text-secondary);
  height: 100%;
}

.empty-state__icon {
  width: 68px;
  height: 68px;
  border-radius: 18px;
  background-color: var(--im-bg-fill);
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--im-accent-primary);
  margin-bottom: 16px;
}

.empty-state__title {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.empty-state__desc {
  margin: 6px 0 0;
  font-size: 14px;
  color: var(--im-text-secondary);
  max-width: 320px;
}

.empty-state__actions {
  margin-top: 18px;
}
</style>
