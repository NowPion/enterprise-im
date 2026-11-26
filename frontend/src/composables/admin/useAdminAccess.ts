import { computed } from 'vue'
import { useAuthStore } from '@/stores/auth'

export function useAdminAccess() {
  const authStore = useAuthStore()

  const currentUser = computed(() => {
    const store = authStore as {
      user?: { id?: string; username?: string; nickname?: string; role?: string } | null
    }
    return store.user ?? null
  })

  const currentAdminId = computed(() =>
    currentUser.value?.id ? String(currentUser.value.id) : '',
  )
  const isAdmin = computed(() => currentUser.value?.role === 'admin')

  return {
    currentUser,
    currentAdminId,
    isAdmin,
  }
}

export function formatUtcTimestamp(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(
    d.getHours(),
  )}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || Number.isNaN(bytes)) return '—'
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let idx = 0
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024
    idx += 1
  }
  return `${value.toFixed(1)} ${units[idx]}`
}

export function formatPendingAge(age: number | string | null | undefined): string {
  if (age === null || age === undefined || age === '') return '—'
  if (typeof age === 'string') return age
  if (age < 1000) return `${age} ms`
  const seconds = age / 1000
  if (seconds < 60) return `${seconds.toFixed(1)} s`
  const minutes = Math.floor(seconds / 60)
  const remainSec = Math.round(seconds % 60)
  return `${minutes}m ${remainSec}s`
}
