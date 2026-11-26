import { getActivePinia, type Pinia } from 'pinia'
import { computed, getCurrentInstance, onUnmounted, watch } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { useConversationStore } from '@/stores/conversation'
import { useMessageStore } from '@/stores/message'
import { compareSeq, isPositiveSeq, minSeq } from '@/composables/chat/seq'
import { clearAuthorizedMediaCache } from '@/composables/chat/useAuthorizedMedia'
import {
  createSharedWSClient,
  getSharedWSClient,
  onSharedWSClientReset,
  resetSharedWSClient,
  type WSClient,
} from '@/utils/wsClient'

interface AuthStoreShape {
  user?: { id?: string } | null
  accessToken?: string
  tokens?: { access_token?: string } | null
  getAccessToken?: () => string
}

export interface UseChatRealtimeOptions {
  trackChatVisibility?: boolean
}

let activeClient: WSClient | null = null
let activePiniaRef: Pinia | undefined
let unsubOpen: (() => void) | null = null
let unsubClose: (() => void) | null = null
let unsubReset: (() => void) | null = null
let syncInFlight: Promise<void> | null = null
let reconcileRequestedDuringFlight = false
let hasReconciledCurrentOpen = false
let lastActiveUserId: string | null = null

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

function clearRealtimeBindings(): void {
  unsubOpen?.()
  unsubOpen = null
  unsubClose?.()
  unsubClose = null
  unsubReset?.()
  unsubReset = null
  activeClient = null
  activePiniaRef = undefined
  hasReconciledCurrentOpen = false
  reconcileRequestedDuringFlight = false
}

function resetChatStoresIfAvailable(): void {
  clearAuthorizedMediaCache()
  if (!getActivePinia()) return
  try {
    useMessageStore().reset()
    useConversationStore().reset()
  } catch {
    // Ignore store cleanup errors when Pinia is tearing down.
  }
}

function handleSharedClientReset(): void {
  clearRealtimeBindings()
  lastActiveUserId = null
  resetChatStoresIfAvailable()
}

export async function reconcileChatOnReconnect(): Promise<void> {
  if (syncInFlight) {
    reconcileRequestedDuringFlight = true
    return syncInFlight
  }
  if (!getActivePinia()) return

  const authStore = useAuthStore() as unknown as AuthStoreShape
  const userId = resolveAuthUserId(authStore)
  const token = resolveAuthAccessToken(authStore)
  if (!userId || !token) return

  const conversationStore = useConversationStore()
  const messageStore = useMessageStore()
  hasReconciledCurrentOpen = true

  syncInFlight = (async () => {
    // 1. Ensure any unconfirmed local 'sending' messages are queued/resent with original client_msg_id.
    messageStore.resendPendingMessages()

    // 2. Buffer live realtime pushes until conversation list & per-conversation /sync finish.
    messageStore.beginRealtimeBuffering()
    try {
      const prevSeqMap: Record<string, string> = { ...conversationStore.lastSeqMap }

      try {
        await conversationStore.fetchList()
      } catch {
        return
      }

      const activeId = conversationStore.activeConversationId
      const syncPlans = new Map<string, { mode: 'initial' | 'sync'; afterSeq: string }>()

      if (activeId) {
        const pagination = messageStore.getPagination(activeId)
        if (!pagination.initialized) {
          syncPlans.set(activeId, { mode: 'initial', afterSeq: '0' })
        } else {
          syncPlans.set(activeId, {
            mode: 'sync',
            afterSeq: messageStore.getHighestCommittedSeq(activeId),
          })
        }
      }

      for (const conv of conversationStore.conversations) {
        if (conv.id === activeId) continue

        const localHighestSeq = messageStore.getHighestCommittedSeq(conv.id)
        const prevSeq = prevSeqMap[conv.id]
        const pagination = messageStore.getPagination(conv.id)

        const hasSeqGapWithLocal =
          isPositiveSeq(localHighestSeq) && compareSeq(conv.last_seq, localHighestSeq) > 0
        const advancedSinceSnapshot =
          prevSeq !== undefined && compareSeq(conv.last_seq, prevSeq) > 0
        const hasUndeliveredOnServer =
          isPositiveSeq(conv.last_seq) && compareSeq(conv.last_seq, conv.last_delivered_seq) > 0
        const hasUnreadWithoutLocal =
          conv.unread_count > 0 && isPositiveSeq(conv.last_seq) && !isPositiveSeq(localHighestSeq)
        const initializedGap =
          pagination.initialized && compareSeq(conv.last_seq, localHighestSeq) > 0

        if (
          !hasSeqGapWithLocal &&
          !advancedSinceSnapshot &&
          !hasUndeliveredOnServer &&
          !hasUnreadWithoutLocal &&
          !initializedGap
        ) {
          continue
        }

        let afterSeq: string
        if (isPositiveSeq(localHighestSeq)) {
          afterSeq = localHighestSeq
        } else if (prevSeq !== undefined && isPositiveSeq(prevSeq)) {
          afterSeq = isPositiveSeq(conv.last_delivered_seq)
            ? minSeq(prevSeq, conv.last_delivered_seq)
            : prevSeq
        } else if (compareSeq(conv.last_seq, conv.last_delivered_seq) > 0) {
          afterSeq = conv.last_delivered_seq || '0'
        } else if (conv.unread_count > 0) {
          afterSeq = conv.last_read_seq || '0'
        } else {
          afterSeq = '0'
        }

        syncPlans.set(conv.id, { mode: 'sync', afterSeq })
      }

      for (const [convId, plan] of syncPlans.entries()) {
        try {
          if (plan.mode === 'initial') {
            await messageStore.loadInitialMessages(convId, userId)
            const conv = conversationStore.itemsById[convId]
            const afterInitialSeq = messageStore.getHighestCommittedSeq(convId)
            if (conv && compareSeq(conv.last_seq, afterInitialSeq) > 0) {
              await messageStore.syncConversation(convId, userId, afterInitialSeq)
            }
          } else {
            await messageStore.syncConversation(convId, userId, plan.afterSeq)
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
    } finally {
      // 3. Flush buffered live subscription frames strictly after /sync completes.
      messageStore.flushRealtimeBuffer()
    }
  })().finally(() => {
    syncInFlight = null
    if (reconcileRequestedDuringFlight) {
      reconcileRequestedDuringFlight = false
      void reconcileChatOnReconnect()
    }
  })

  return syncInFlight
}

export function ensureChatRealtime(): WSClient | null {
  if (!getActivePinia()) return getSharedWSClient()

  const authStore = useAuthStore() as unknown as AuthStoreShape
  const token = resolveAuthAccessToken(authStore)
  const userId = resolveAuthUserId(authStore)
  if (!token || !userId) return null

  const conversationStore = useConversationStore()
  const messageStore = useMessageStore()
  const currentPinia = getActivePinia()

  if (lastActiveUserId && lastActiveUserId !== userId) {
    resetChatStoresIfAvailable()
    hasReconciledCurrentOpen = false
  }
  lastActiveUserId = userId

  const client = createSharedWSClient({
    getAccessToken: () => resolveAuthAccessToken(useAuthStore() as unknown as AuthStoreShape),
    getAccountId: () => resolveAuthUserId(useAuthStore() as unknown as AuthStoreShape),
    getLastSeq: () => useConversationStore().lastSeqMap,
  })

  if (activeClient !== client || activePiniaRef !== currentPinia) {
    clearRealtimeBindings()
    activeClient = client
    activePiniaRef = currentPinia
    unsubReset = onSharedWSClientReset(() => {
      handleSharedClientReset()
    })
    unsubOpen = client.on('open', () => {
      hasReconciledCurrentOpen = false
      if (syncInFlight) {
        reconcileRequestedDuringFlight = true
      } else {
        void reconcileChatOnReconnect()
      }
    })
    unsubClose = client.on('close', () => {
      hasReconciledCurrentOpen = false
    })
  }

  messageStore.bindWSClient(client, () =>
    resolveAuthUserId(useAuthStore() as unknown as AuthStoreShape),
  )
  client.connect()

  // If the shared WS connection was already authenticated before chat mounted, reconcile immediately.
  if (client.isConnected && !hasReconciledCurrentOpen) {
    void reconcileChatOnReconnect()
  }

  return client
}

export function stopChatRealtime(): void {
  clearRealtimeBindings()
  lastActiveUserId = null
  resetSharedWSClient()
  resetChatStoresIfAvailable()
}

export function useChatRealtime(options: UseChatRealtimeOptions = {}) {
  const authStore = useAuthStore() as unknown as AuthStoreShape
  const conversationStore = useConversationStore()
  const messageStore = useMessageStore()
  const trackChatVisibility = options.trackChatVisibility ?? true

  const currentUserId = computed(() => resolveAuthUserId(authStore))
  const accessToken = computed(() => resolveAuthAccessToken(authStore))

  function ensureRealtimeConnected(): void {
    ensureChatRealtime()
  }

  function reconcileOnAuthenticatedOpen(): Promise<void> {
    return reconcileChatOnReconnect()
  }

  function stopRealtime(): void {
    stopChatRealtime()
  }

  function syncVisibilityState(): void {
    if (!trackChatVisibility) return
    const activeId = conversationStore.activeConversationId
    if (!activeId) {
      conversationStore.setChatVisible(false)
      return
    }
    const visible = typeof document === 'undefined' ? true : document.visibilityState === 'visible'
    conversationStore.setChatVisible(visible)
    if (visible) {
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
        if (prevUid || lastActiveUserId) {
          stopChatRealtime()
        }
        return
      }
      if (prevUid && prevUid !== uid) {
        resetChatStoresIfAvailable()
        hasReconciledCurrentOpen = false
      }
      ensureChatRealtime()
    },
    { immediate: true },
  )

  if (trackChatVisibility && typeof window !== 'undefined' && typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', syncVisibilityState)
    window.addEventListener('focus', syncVisibilityState)
  }

  if (getCurrentInstance()) {
    onUnmounted(() => {
      stopAuthWatch()
      if (trackChatVisibility) {
        conversationStore.setChatVisible(false)
        if (typeof window !== 'undefined' && typeof document !== 'undefined') {
          document.removeEventListener('visibilitychange', syncVisibilityState)
          window.removeEventListener('focus', syncVisibilityState)
        }
      }
    })
  }

  return {
    currentUserId,
    ensureRealtimeConnected,
    reconcileOnAuthenticatedOpen,
    stopRealtime,
    syncVisibilityState,
  }
}
