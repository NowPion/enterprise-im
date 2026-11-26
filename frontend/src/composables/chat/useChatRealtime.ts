import { computed, onUnmounted, watch } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { useConversationStore } from '@/stores/conversation'
import { useMessageStore } from '@/stores/message'
import { compareSeq, isPositiveSeq } from '@/composables/chat/seq'
import { clearAuthorizedMediaCache } from '@/composables/chat/useAuthorizedMedia'
import { createSharedWSClient, resetSharedWSClient, type WSClient } from '@/utils/wsClient'

interface AuthStoreShape {
  user?: { id?: string } | null
  accessToken?: string
  tokens?: { access_token?: string } | null
  getAccessToken?: () => string
}

let activeClient: WSClient | null = null
let unsubOpen: (() => void) | null = null
let syncInFlight: Promise<void> | null = null

export function resolveAuthAccessToken(authStore: AuthStoreShape): string {
  if (typeof authStore.getAccessToken === 'function') {
    return authStore.getAccessToken() || ''
  }
  if (typeof authStore.accessToken === 'string' && authStore.accessToken) {
    return authStore.accessToken
  }
  if (typeof authStore.tokens?.access_token === 'string' && authStore.tokens.access_token) {
    return authStore.tokens.access_token
  }
  return ''
}

export function resolveAuthUserId(authStore: AuthStoreShape): string {
  return authStore.user?.id ? String(authStore.user.id) : ''
}

export function useChatRealtime() {
  const authStore = useAuthStore() as unknown as AuthStoreShape
  const conversationStore = useConversationStore()
  const messageStore = useMessageStore()

  const currentUserId = computed(() => resolveAuthUserId(authStore))
  const accessToken = computed(() => resolveAuthAccessToken(authStore))

  async function reconcileOnAuthenticatedOpen(): Promise<void> {
    if (syncInFlight) return syncInFlight
    const userId = resolveAuthUserId(authStore)
    if (!userId) return

    syncInFlight = (async () => {
      // Snapshot local last_seq before fetchList overwrites conversation watermarks.
      const prevSeqMap: Record<string, string> = { ...conversationStore.lastSeqMap }

      try {
        await conversationStore.fetchList()
      } catch {
        return
      }

      const activeId = conversationStore.activeConversationId
      const syncTargets = new Set<string>()
      if (activeId) {
        syncTargets.add(activeId)
      }

      for (const conv of conversationStore.conversations) {
        const prevSeq = prevSeqMap[conv.id] ?? '0'
        const hasLocalMessages = (messageStore.messagesByConversation[conv.id]?.length ?? 0) > 0
        if (hasLocalMessages && compareSeq(conv.last_seq, prevSeq) > 0) {
          syncTargets.add(conv.id)
        }
      }

      for (const convId of syncTargets) {
        const afterSeq =
          prevSeqMap[convId] ?? messageStore.getHighestCommittedSeq(convId) ?? '0'
        try {
          const pagination = messageStore.getPagination(convId)
          if (convId === activeId && !pagination.initialized) {
            await messageStore.loadInitialMessages(convId, userId)
          } else {
            await messageStore.syncConversation(convId, userId, afterSeq)
          }
        } catch {
          // Individual conversation sync failure does not block other conversations.
        }
      }

      if (activeId && conversationStore.isChatVisible) {
        const activeConv = conversationStore.itemsById[activeId]
        if (activeConv && isPositiveSeq(activeConv.last_seq)) {
          await messageStore.markRead(activeId, activeConv.last_seq)
        }
      }
    })().finally(() => {
      syncInFlight = null
    })

    return syncInFlight
  }

  function ensureRealtimeConnected(): void {
    const token = resolveAuthAccessToken(authStore)
    const userId = resolveAuthUserId(authStore)
    if (!token || !userId) return

    const client = createSharedWSClient({
      getAccessToken: () => resolveAuthAccessToken(authStore),
      getAccountId: () => resolveAuthUserId(authStore),
      getLastSeq: () => conversationStore.lastSeqMap,
    })

    if (activeClient !== client) {
      unsubOpen?.()
      activeClient = client
      unsubOpen = client.on('open', () => {
        void reconcileOnAuthenticatedOpen()
      })
    }

    messageStore.bindWSClient(client, () => resolveAuthUserId(authStore))
    client.connect()
  }

  function stopRealtime(): void {
    unsubOpen?.()
    unsubOpen = null
    activeClient = null
    resetSharedWSClient()
    messageStore.reset()
    conversationStore.reset()
    clearAuthorizedMediaCache()
  }

  function syncVisibilityState(): void {
    const visible = typeof document === 'undefined' ? true : document.visibilityState === 'visible'
    conversationStore.setChatVisible(visible)
    const activeId = conversationStore.activeConversationId
    if (visible && activeId) {
      const conv = conversationStore.itemsById[activeId]
      if (conv && isPositiveSeq(conv.last_seq)) {
        void messageStore.markRead(activeId, conv.last_seq)
      }
    }
  }

  const stopAuthWatch = watch(
    [currentUserId, accessToken],
    ([uid, token], [prevUid]) => {
      if (!uid || !token) {
        if (prevUid) {
          stopRealtime()
        }
        return
      }
      if (prevUid && prevUid !== uid) {
        messageStore.reset()
        conversationStore.reset()
        clearAuthorizedMediaCache()
      }
      ensureRealtimeConnected()
    },
    { immediate: true },
  )

  if (typeof window !== 'undefined' && typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', syncVisibilityState)
    window.addEventListener('focus', syncVisibilityState)
  }

  onUnmounted(() => {
    stopAuthWatch()
    conversationStore.setChatVisible(false)
    if (typeof window !== 'undefined' && typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', syncVisibilityState)
      window.removeEventListener('focus', syncVisibilityState)
    }
  })

  return {
    currentUserId,
    ensureRealtimeConnected,
    reconcileOnAuthenticatedOpen,
    stopRealtime,
    syncVisibilityState,
  }
}
