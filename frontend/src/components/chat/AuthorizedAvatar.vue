<script setup lang="ts">
import { computed } from 'vue'
import { useAuthorizedMediaSrc } from '@/composables/chat/useAuthorizedMedia'

const props = withDefaults(
  defineProps<{
    src?: string | null
    name?: string
    size?: number
    online?: boolean
    showOnline?: boolean
    shape?: 'square' | 'circle'
  }>(),
  {
    src: '',
    name: '',
    size: 40,
    online: false,
    showOnline: false,
    shape: 'circle',
  },
)

const { resolvedUrl } = useAuthorizedMediaSrc(() => props.src)

const initialText = computed(() => {
  const trimmed = (props.name ?? '').trim()
  if (!trimmed) return '?'
  return trimmed.slice(0, 1).toUpperCase()
})

const boxStyle = computed(() => ({
  width: `${props.size}px`,
  height: `${props.size}px`,
  borderRadius: props.shape === 'circle' ? '9999px' : '11px',
  fontSize: `${Math.max(12, Math.round(props.size * 0.4))}px`,
}))
</script>

<template>
  <div class="authorized-avatar" :style="{ width: `${size}px`, height: `${size}px` }">
    <div class="authorized-avatar__frame" :style="boxStyle">
      <img
        v-if="resolvedUrl"
        :src="resolvedUrl"
        :alt="name || '头像'"
        class="authorized-avatar__img"
      />
      <span v-else class="authorized-avatar__fallback">{{ initialText }}</span>
    </div>
    <span
      v-if="showOnline"
      class="authorized-avatar__status"
      :class="online ? 'is-online' : 'is-offline'"
      :title="online ? '在线' : '离线'"
    />
  </div>
</template>

<style scoped>
.authorized-avatar {
  position: relative;
  display: inline-flex;
  flex-shrink: 0;
}

.authorized-avatar__frame {
  width: 100%;
  height: 100%;
  overflow: hidden;
  display: flex;
  align-items: center;
  justify-content: center;
  background-color: #8e8e93;
  color: #ffffff;
  font-weight: 600;
  user-select: none;
}

.authorized-avatar__img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.authorized-avatar__fallback {
  line-height: 1;
}

.authorized-avatar__status {
  position: absolute;
  right: -1px;
  bottom: -1px;
  width: 11px;
  height: 11px;
  border-radius: 9999px;
  border: 2px solid var(--im-bg-surface);
}

.authorized-avatar__status.is-online {
  background-color: var(--im-status-online);
}

.authorized-avatar__status.is-offline {
  background-color: var(--im-status-offline);
}
</style>
