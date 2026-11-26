import { onUnmounted, ref, toValue, watch, type MaybeRefOrGetter, type Ref } from 'vue'
import { fetchAssetBlob } from '@/api/upload'

const MAX_OBJECT_URL_CACHE = 120
const objectUrlCache = new Map<string, string>()
const inflightRequests = new Map<string, Promise<string>>()

function isPassthroughUrl(url: string): boolean {
  return (
    url.startsWith('blob:') ||
    url.startsWith('data:') ||
    url.startsWith('http://') ||
    url.startsWith('https://')
  )
}

function isProtectedAssetUrl(url: string): boolean {
  return url.startsWith('/api/v1/uploads/') || url.startsWith('/uploads/')
}

function cacheObjectUrl(key: string, objectUrl: string): void {
  if (objectUrlCache.has(key)) {
    objectUrlCache.delete(key)
  }
  objectUrlCache.set(key, objectUrl)
  if (objectUrlCache.size > MAX_OBJECT_URL_CACHE) {
    const oldestKey = objectUrlCache.keys().next().value
    if (oldestKey !== undefined) {
      const oldestUrl = objectUrlCache.get(oldestKey)
      objectUrlCache.delete(oldestKey)
      if (oldestUrl) {
        URL.revokeObjectURL(oldestUrl)
      }
    }
  }
}

export async function resolveAuthorizedMediaUrl(url: string | null | undefined): Promise<string> {
  const trimmed = (url ?? '').trim()
  if (!trimmed) return ''
  if (isPassthroughUrl(trimmed)) return trimmed
  if (!isProtectedAssetUrl(trimmed)) return trimmed

  const cached = objectUrlCache.get(trimmed)
  if (cached) {
    // Refresh insertion order for simple LRU eviction.
    objectUrlCache.delete(trimmed)
    objectUrlCache.set(trimmed, cached)
    return cached
  }

  const inflight = inflightRequests.get(trimmed)
  if (inflight) {
    return inflight
  }

  const requestPromise = (async () => {
    try {
      const parsed = new URL(trimmed, 'http://localhost')
      const variant = parsed.searchParams.get('variant') === 'thumbnail' ? 'thumbnail' : undefined
      const blob = await fetchAssetBlob(parsed.pathname, { variant })
      const objectUrl = URL.createObjectURL(blob)
      cacheObjectUrl(trimmed, objectUrl)
      return objectUrl
    } finally {
      inflightRequests.delete(trimmed)
    }
  })()

  inflightRequests.set(trimmed, requestPromise)
  return requestPromise
}

export async function triggerAuthorizedDownload(url: string, filename = 'download'): Promise<void> {
  const trimmed = url.trim()
  if (!trimmed) return

  if (isPassthroughUrl(trimmed)) {
    const link = document.createElement('a')
    link.href = trimmed
    link.download = filename
    link.rel = 'noopener'
    document.body.appendChild(link)
    link.click()
    link.remove()
    return
  }

  const parsed = new URL(trimmed, 'http://localhost')
  const blob = await fetchAssetBlob(parsed.pathname, { download: true })
  const objectUrl = URL.createObjectURL(blob)
  try {
    const link = document.createElement('a')
    link.href = objectUrl
    link.download = filename || 'download'
    document.body.appendChild(link)
    link.click()
    link.remove()
  } finally {
    setTimeout(() => URL.revokeObjectURL(objectUrl), 1000)
  }
}

export function clearAuthorizedMediaCache(): void {
  for (const url of objectUrlCache.values()) {
    URL.revokeObjectURL(url)
  }
  objectUrlCache.clear()
}

export function useAuthorizedMediaSrc(
  source: MaybeRefOrGetter<string | null | undefined>,
  fallback?: MaybeRefOrGetter<string | null | undefined>,
): {
  resolvedUrl: Ref<string>
  loading: Ref<boolean>
  error: Ref<string | null>
} {
  const resolvedUrl = ref('')
  const loading = ref(false)
  const error = ref<string | null>(null)
  let seqToken = 0

  watch(
    () => [toValue(source), toValue(fallback)] as const,
    async ([rawSource, rawFallback]) => {
      const token = ++seqToken
      const target = (rawSource ?? '').trim() || (rawFallback ?? '').trim()
      if (!target) {
        resolvedUrl.value = ''
        loading.value = false
        error.value = null
        return
      }
      if (isPassthroughUrl(target) || !isProtectedAssetUrl(target)) {
        resolvedUrl.value = target
        loading.value = false
        error.value = null
        return
      }

      const cached = objectUrlCache.get(target)
      if (cached) {
        resolvedUrl.value = cached
        loading.value = false
        error.value = null
        return
      }

      if (rawFallback && rawFallback.trim()) {
        resolvedUrl.value = rawFallback.trim()
      }
      loading.value = true
      error.value = null
      try {
        const url = await resolveAuthorizedMediaUrl(target)
        if (token === seqToken) {
          resolvedUrl.value = url
        }
      } catch (err) {
        if (token === seqToken) {
          error.value = err instanceof Error ? err.message : '媒体资源加载失败'
          resolvedUrl.value = (rawFallback ?? '').trim()
        }
      } finally {
        if (token === seqToken) {
          loading.value = false
        }
      }
    },
    { immediate: true },
  )

  onUnmounted(() => {
    seqToken += 1
  })

  return {
    resolvedUrl,
    loading,
    error,
  }
}
