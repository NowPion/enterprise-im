<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useAuthorizedMediaSrc } from '@/composables/chat/useAuthorizedMedia'

interface Props {
  src?: string | null
  name?: string
  size?: number
  online?: boolean
  showOnline?: boolean
  rounded?: 'sm' | 'md' | 'full'
}

const props = withDefaults(defineProps<Props>(), {
  src: '',
  name: '',
  size: 40,
  online: false,
  showOnline: false,
  rounded: 'md',
})

const { resolvedUrl } = useAuthorizedMediaSrc(() => props.src)
const imgFailed = ref(false)

watch(
  () => [props.src, resolvedUrl.value] as const,
  () => {
    imgFailed.value = false
  },
)

const displaySrc = computed(() => {
  if (imgFailed.value) return ''
  return resolvedUrl.value
})

const initial = computed(() => {
  const clean = props.name?.trim()
  if (!clean) return 'IM'
  return clean.slice(0, 1).toUpperCase()
})

const radiusClass = computed(() => {
  if (props.rounded === 'full') return 'is-full'
  if (props.rounded === 'sm') return 'is-sm'
  return 'is-md'
})

const sizeStyle = computed(() => ({
  width: `${props.size}px`,
  height: `${props.size}px`,
  fontSize: `${Math.max(12, Math.floor(props.size * 0.4))}px`,
}))
</script>

<template>
  <div class="app-avatar" :class="radiusClass" :style="sizeStyle">
    <img
      v-if="displaySrc"
      :src="displaySrc"
      :alt="name || '头像'"
      class="app-avatar__img"
      @error="imgFailed = true"
    />
    <span v-else class="app-avatar__fallback">{{ initial }}</span>
    <span
      v-if="showOnline"
      class="app-avatar__status"
      :class="{ 'is-online': online }"
      :title="online ? '在线' : '离线'"
    />
  </div>
</template>

<style scoped>
.app-avatar {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  background-color: #1f2937;
  color: #ffffff;
  font-weight: 600;
  user-select: none;
  overflow: visible;
}

.app-avatar.is-sm {
  border-radius: 4px;
}

.app-avatar.is-md {
  border-radius: 8px;
}

.app-avatar.is-full {
  border-radius: 9999px;
}

.app-avatar__img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  border-radius: inherit;
  display: block;
}

.app-avatar__fallback {
  line-height: 1;
}

.app-avatar__status {
  position: absolute;
  right: -2px;
  bottom: -2px;
  width: 10px;
  height: 10px;
  border-radius: 9999px;
  background-color: var(--im-status-offline);
  border: 2px solid var(--im-bg-surface);
}

.app-avatar__status.is-online {
  background-color: var(--im-status-online);
}
</style>
