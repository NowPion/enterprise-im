import { acceptHMRUpdate, defineStore } from 'pinia'
import { ref } from 'vue'
import {
  fetchMessageReceipts,
  fetchMessages,
  markConversationDelivered,
  markConversationRead,
  recallMessageHttp,
  sendMessageHttp,
  syncMessages,
} from '@/api/message'
import {
  compareSeq,
  generateClientMsgId,
  isPositiveSeq,
  maxSeq,
  minSeq,
} from '@/composables/chat/seq'
import {
  createRetryScheduler,
  isRetryableSendError,
} from '@/composables/chat/useMessageRetry'
import { useConversationStore } from '@/stores/conversation'
import type {
  ChatMessage,
  DeliveryStatus,
  LocalDeliveryStatus,
  Message,
  MessagePushData,
  MessageReceiptList,
  MsgAckData,
  MsgRecallAckData,
  PresenceUpdateData,
  ReceiptFilterStatus,
  ReceiptPushData,
  SystemNoticeData,
  WSErrorData,
} from '@/types/message'
import type { Envelope, WSClient } from '@/utils/wsClient'

export interface ConversationPaginationState {
  hasMoreOlder: boolean
  oldestSeq: string | null
  loadingOlder: boolean
  initialized: boolean
}

export interface SendChatMessageInput {
  msg_type: 'text' | 'image' | 'file' | 'voice'
  content: string
  extra?: Record<string, unknown>
  local_preview_url?: string
}

const DELIVERY_RANK: Record<LocalDeliveryStatus, number> = {
  failed: -1,
  sending: 0,
  sent: 1,
  delivered: 2,
  read: 3,
}

function mergeDeliveryStatus(
  current: LocalDeliveryStatus | undefined,
  incoming: LocalDeliveryStatus | undefined,
): LocalDeliveryStatus {
  if (!current) return incoming ?? 'sent'
  if (!incoming) return current
  if (current === 'failed') return incoming
  if (incoming === 'failed') return current
  return DELIVERY_RANK[incoming] >= DELIVERY_RANK[current] ? incoming : current
}

function sortMessages(items: ChatMessage[]): ChatMessage[] {
  return [...items].sort((a, b) => {
    const aHasSeq = isPositiveSeq(a.seq)
    const bHasSeq = isPositiveSeq(b.seq)
    if (aHasSeq && bHasSeq) {
      return compareSeq(a.seq, b.seq)
    }
    if (aHasSeq !== bHasSeq) {
      return aHasSeq ? -1 : 1
    }
    const ta = Date.parse(a.created_at) || 0
    const tb = Date.parse(b.created_at) || 0
    if (ta !== tb) return ta - tb
    return a.client_msg_id.localeCompare(b.client_msg_id)
  })
}

function createReqId(): string {
  if (typeof globalThis.crypto?.getRandomValues === 'function') {
    const bytes = new Uint8Array(16)
    globalThis.crypto.getRandomValues(bytes)
    return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  }
  return `req_${Date.now()}_${Math.random().toString(16).slice(2, 10)}`
}

export const useMessageStore = defineStore('message', () => {
  const messagesByConversation = ref<Record<string, ChatMessage[]>>({})
  const paginationByConversation = ref<Record<string, ConversationPaginationState>>({})
  const syncingByConversation = ref<Record<string, boolean>>({})
  const receiptsModalData = ref<MessageReceiptList | null>(null)
  const receiptsLoading = ref(false)
  const wsConnected = ref(false)

  // Non-reactive internal bookkeeping for WS transport & retry timers.
  let boundWSClient: WSClient | null = null
  let wsUnsubs: Array<() => void> = []
  let resolveCurrentUserId: () => string = () => ''
  const peerWatermarksByConv = new Map<string, { deliveredSeq: string; readSeq: string }>()
  const pendingRecalls = new Map<
    string,
    {
      resolve: (ack: MsgRecallAckData) => void
      reject: (err: Error) => void
      timer: ReturnType<typeof setTimeout>
    }
  >()

  const retryScheduler = createRetryScheduler(
    (clientMsgId, reqId, conversationId) => {
      const list = messagesByConversation.value[conversationId]
      const target = list?.find((m) => m.client_msg_id === clientMsgId)
      if (!target || target.local_status !== 'sending') return

      if (boundWSClient && boundWSClient.hasPending(reqId)) {
        boundWSClient.retryPending(reqId)
      } else if (boundWSClient) {
        const nextReqId = createReqId()
        target.req_id = nextReqId
        boundWSClient.enqueue(
          {
            conversation_id: conversationId,
            client_msg_id: target.client_msg_id,
            msg_type: target.msg_type,
            content: target.content,
            extra: (target.extra ?? {}) as Record<string, never>,
          },
          nextReqId,
        )
      }
    },
    (clientMsgId, _reqId, conversationId) => {
      const list = messagesByConversation.value[conversationId]
      const target = list?.find((m) => m.client_msg_id === clientMsgId)
      if (!target || target.local_status !== 'sending') return
      target.local_status = 'failed'
      target.retryable = true
      if (!target.error_message) {
        target.error_message = '发送超时，请点击重试'
      }
    },
  )

  function ensurePagination(conversationId: string): ConversationPaginationState {
    if (!paginationByConversation.value[conversationId]) {
      paginationByConversation.value[conversationId] = {
        hasMoreOlder: false,
        oldestSeq: null,
        loadingOlder: false,
        initialized: false,
      }
    }
    return paginationByConversation.value[conversationId]
  }

  function getMessages(conversationId: string): ChatMessage[] {
    return messagesByConversation.value[conversationId] ?? []
  }

  function getPagination(conversationId: string): ConversationPaginationState {
    return (
      paginationByConversation.value[conversationId] ?? {
        hasMoreOlder: false,
        oldestSeq: null,
        loadingOlder: false,
        initialized: false,
      }
    )
  }

  function getHighestCommittedSeq(conversationId: string): string {
    const list = messagesByConversation.value[conversationId]
    if (!list || list.length === 0) return '0'
    let max = '0'
    for (const msg of list) {
      if (isPositiveSeq(msg.seq)) {
        max = maxSeq(max, msg.seq)
      }
    }
    return max
  }

  function getOldestCommittedSeq(conversationId: string): string | null {
    const list = messagesByConversation.value[conversationId]
    if (!list) return null
    for (const msg of list) {
      if (isPositiveSeq(msg.seq)) {
        return msg.seq
      }
    }
    return null
  }

  function applyPeerWatermarksToStatus(
    conversationId: string,
    seq: string,
    senderId: string,
    currentUserId: string,
    baseStatus: LocalDeliveryStatus,
  ): LocalDeliveryStatus {
    if (!currentUserId || senderId !== currentUserId || !isPositiveSeq(seq)) {
      return baseStatus
    }
    const watermarks = peerWatermarksByConv.get(conversationId)
    if (!watermarks) return baseStatus
    let next = baseStatus
    if (isPositiveSeq(watermarks.deliveredSeq) && compareSeq(seq, watermarks.deliveredSeq) <= 0) {
      next = mergeDeliveryStatus(next, 'delivered')
    }
    if (isPositiveSeq(watermarks.readSeq) && compareSeq(seq, watermarks.readSeq) <= 0) {
      next = mergeDeliveryStatus(next, 'read')
    }
    return next
  }

  function upsertServerMessage(
    conversationId: string,
    serverMsg: Message,
    currentUserId: string,
  ): { inserted: boolean; message: ChatMessage } {
    const currentList = messagesByConversation.value[conversationId] ?? []
    const existingIdx = currentList.findIndex(
      (item) =>
        (serverMsg.msg_id && item.msg_id === serverMsg.msg_id) ||
        (serverMsg.client_msg_id && item.client_msg_id === serverMsg.client_msg_id),
    )

    const incomingDelivery: LocalDeliveryStatus = serverMsg.delivery_status ?? 'sent'

    if (existingIdx >= 0) {
      const existing = currentList[existingIdx]
      const mergedStatus =
        existing.status === 'recalled' || serverMsg.status === 'recalled'
          ? 'recalled'
          : 'normal'
      const baseLocal = mergeDeliveryStatus(existing.local_status, incomingDelivery)
      const nextLocal = applyPeerWatermarksToStatus(
        conversationId,
        serverMsg.seq,
        serverMsg.sender_id,
        currentUserId,
        baseLocal,
      )

      const updated: ChatMessage = {
        ...existing,
        ...serverMsg,
        extra: serverMsg.extra ?? existing.extra ?? {},
        status: mergedStatus,
        recalled_at: serverMsg.recalled_at ?? existing.recalled_at,
        local_status: nextLocal,
        optimistic: false,
        error_code: undefined,
        error_message: undefined,
        retryable: undefined,
      }

      const nextList = [...currentList]
      nextList[existingIdx] = updated
      messagesByConversation.value[conversationId] = sortMessages(nextList)
      if (updated.client_msg_id) {
        retryScheduler.cancel(updated.client_msg_id)
      }
      return { inserted: false, message: updated }
    }

    const nextLocal = applyPeerWatermarksToStatus(
      conversationId,
      serverMsg.seq,
      serverMsg.sender_id,
      currentUserId,
      incomingDelivery,
    )

    const insertedMsg: ChatMessage = {
      ...serverMsg,
      extra: serverMsg.extra ?? {},
      local_status: nextLocal,
      optimistic: false,
    }
    messagesByConversation.value[conversationId] = sortMessages([...currentList, insertedMsg])
    if (insertedMsg.client_msg_id) {
      retryScheduler.cancel(insertedMsg.client_msg_id)
    }
    return { inserted: true, message: insertedMsg }
  }

  function applyRecallInPlace(
    conversationId: string,
    recalledMsgId: string,
    recalledAt: string | null,
  ): void {
    const list = messagesByConversation.value[conversationId]
    if (!list || !recalledMsgId) return
    const idx = list.findIndex((m) => m.msg_id === recalledMsgId)
    if (idx < 0) return
    const nextList = [...list]
    nextList[idx] = {
      ...nextList[idx],
      status: 'recalled',
      recalled_at: recalledAt ?? nextList[idx].recalled_at ?? new Date().toISOString(),
    }
    messagesByConversation.value[conversationId] = nextList
  }

  function setWsConnected(connected: boolean): void {
    wsConnected.value = connected
  }

  function bindWSClient(client: WSClient, getCurrentUserId: () => string): void {
    if (boundWSClient === client && wsUnsubs.length > 0) {
      resolveCurrentUserId = getCurrentUserId
      return
    }
    unbindWSClient()
    boundWSClient = client
    resolveCurrentUserId = getCurrentUserId
    wsConnected.value = client.isConnected

    wsUnsubs = [
      client.on('open', () => {
        wsConnected.value = true
      }),
      client.on('close', () => {
        wsConnected.value = false
      }),
      client.on('msg.ack', (frame: Envelope) => {
        handleMessageAck(frame.data as unknown as MsgAckData, resolveCurrentUserId())
      }),
      client.on('msg.push', (frame: Envelope) => {
        const data = frame.data as unknown as MessagePushData | ReceiptPushData
        if (data.push_type === 'message') {
          handleMessagePush(data, resolveCurrentUserId())
        } else if (data.push_type === 'receipt') {
          handleReceiptPush(data, resolveCurrentUserId())
        }
      }),
      client.on('msg.recall', (frame: Envelope) => {
        const data = frame.data as unknown as MsgRecallAckData
        handleRecallAck(data)
        const pending = pendingRecalls.get(frame.req_id)
        if (pending) {
          clearTimeout(pending.timer)
          pendingRecalls.delete(frame.req_id)
          pending.resolve(data)
        }
      }),
      client.on('presence.update', (frame: Envelope) => {
        const data = frame.data as unknown as PresenceUpdateData
        const conversationStore = useConversationStore()
        conversationStore.applyPeerPresence(data.user_id, data.online, data.last_online_at)
      }),
      client.on('system.notice', (frame: Envelope) => {
        handleSystemNotice(frame.data as unknown as SystemNoticeData, resolveCurrentUserId())
      }),
      client.on('error', (err) => {
        if (err.kind === 'frame') {
          const data = err.frame.data as unknown as WSErrorData
          if (data.related_type === 'msg.recall' && data.related_req_id) {
            const pending = pendingRecalls.get(data.related_req_id)
            if (pending) {
              clearTimeout(pending.timer)
              pendingRecalls.delete(data.related_req_id)
              pending.reject(new Error(data.message || `撤回失败 (${data.code})`))
            }
          }
          handleWsError(data, resolveCurrentUserId())
        }
      }),
    ]
  }

  function unbindWSClient(): void {
    for (const unsub of wsUnsubs) {
      unsub()
    }
    wsUnsubs = []
    boundWSClient = null
    wsConnected.value = false
    for (const pending of pendingRecalls.values()) {
      clearTimeout(pending.timer)
    }
    pendingRecalls.clear()
  }

  async function loadInitialMessages(conversationId: string, currentUserId: string): Promise<void> {
    const pagination = ensurePagination(conversationId)
    pagination.loadingOlder = true
    try {
      const page = await fetchMessages(conversationId, { limit: 30 })
      for (const msg of page.items) {
        upsertServerMessage(conversationId, msg, currentUserId)
      }
      pagination.hasMoreOlder = page.has_more
      pagination.oldestSeq = getOldestCommittedSeq(conversationId)
      pagination.initialized = true

      const conversationStore = useConversationStore()
      const highestSeq = getHighestCommittedSeq(conversationId)
      const conv = conversationStore.itemsById[conversationId]
      if (isPositiveSeq(highestSeq) && conv) {
        if (compareSeq(highestSeq, conv.last_delivered_seq) > 0) {
          void markDelivered(conversationId, minSeq(highestSeq, maxSeq(conv.last_seq, highestSeq)))
        }
        if (
          conversationStore.activeConversationId === conversationId &&
          conversationStore.isChatVisible
        ) {
          await markRead(conversationId, minSeq(highestSeq, maxSeq(conv.last_seq, highestSeq)))
        }
      }
    } finally {
      pagination.loadingOlder = false
    }
  }

  async function loadOlderMessages(conversationId: string, currentUserId: string): Promise<boolean> {
    const pagination = ensurePagination(conversationId)
    if (pagination.loadingOlder || !pagination.hasMoreOlder) return false
    const beforeSeq = pagination.oldestSeq ?? getOldestCommittedSeq(conversationId)
    if (!beforeSeq || !isPositiveSeq(beforeSeq)) {
      pagination.hasMoreOlder = false
      return false
    }

    pagination.loadingOlder = true
    try {
      const page = await fetchMessages(conversationId, {
        before_seq: beforeSeq,
        limit: 30,
      })
      for (const msg of page.items) {
        upsertServerMessage(conversationId, msg, currentUserId)
      }
      pagination.hasMoreOlder = page.has_more
      pagination.oldestSeq = getOldestCommittedSeq(conversationId)
      return page.items.length > 0
    } finally {
      pagination.loadingOlder = false
    }
  }

  async function syncConversation(
    conversationId: string,
    currentUserId: string,
    explicitAfterSeq?: string,
  ): Promise<number> {
    if (syncingByConversation.value[conversationId]) return 0
    syncingByConversation.value[conversationId] = true

    const conversationStore = useConversationStore()
    let cursorSeq = explicitAfterSeq ?? getHighestCommittedSeq(conversationId)
    let syncedCount = 0
    let highestSyncedSeq = cursorSeq

    try {
      for (let pageIndex = 0; pageIndex < 20; pageIndex += 1) {
        const res = await syncMessages(conversationId, { after_seq: cursorSeq, limit: 100 })
        for (const msg of res.items) {
          const { inserted } = upsertServerMessage(conversationId, msg, currentUserId)
          if (msg.msg_type === 'recall' && typeof msg.extra?.recalled_msg_id === 'string') {
            applyRecallInPlace(
              conversationId,
              msg.extra.recalled_msg_id,
              typeof msg.extra.recalled_at === 'string' ? msg.extra.recalled_at : msg.created_at,
            )
          }
          if (inserted) {
            syncedCount += 1
          }
          highestSyncedSeq = maxSeq(highestSyncedSeq, msg.seq)
        }
        if (isPositiveSeq(res.to_seq)) {
          highestSyncedSeq = maxSeq(highestSyncedSeq, res.to_seq)
          cursorSeq = res.to_seq
        }
        if (!res.has_more || res.items.length === 0) {
          break
        }
      }

      const conv = conversationStore.itemsById[conversationId]
      if (conv && isPositiveSeq(highestSyncedSeq)) {
        if (compareSeq(highestSyncedSeq, conv.last_seq) > 0) {
          const list = messagesByConversation.value[conversationId] ?? []
          const latestMsg = list.find((m) => m.seq === highestSyncedSeq)
          if (latestMsg) {
            conversationStore.applyIncomingMessage(latestMsg, currentUserId, { isReplay: true })
          }
        }
        if (compareSeq(highestSyncedSeq, conv.last_delivered_seq) > 0) {
          await markDelivered(conversationId, highestSyncedSeq)
        }
        if (
          conversationStore.activeConversationId === conversationId &&
          conversationStore.isChatVisible
        ) {
          await markRead(conversationId, highestSyncedSeq)
        }
      }

      const pagination = ensurePagination(conversationId)
      if (!pagination.oldestSeq) {
        pagination.oldestSeq = getOldestCommittedSeq(conversationId)
      }
      pagination.initialized = true
      return syncedCount
    } finally {
      syncingByConversation.value[conversationId] = false
    }
  }

  async function sendMessage(
    conversationId: string,
    input: SendChatMessageInput,
    currentUserId: string,
  ): Promise<ChatMessage> {
    const clientMsgId = generateClientMsgId()
    const reqId = createReqId()
    const extra = input.extra ?? {}
    const optimisticMsg: ChatMessage = {
      msg_id: '',
      client_msg_id: clientMsgId,
      conversation_id: conversationId,
      seq: '0',
      sender_id: currentUserId,
      msg_type: input.msg_type,
      content: input.content,
      extra,
      status: 'normal',
      created_at: new Date().toISOString(),
      recalled_at: null,
      delivery_status: null,
      local_status: 'sending',
      req_id: reqId,
      optimistic: true,
      local_preview_url: input.local_preview_url,
    }

    const currentList = messagesByConversation.value[conversationId] ?? []
    messagesByConversation.value[conversationId] = sortMessages([...currentList, optimisticMsg])

    if (boundWSClient) {
      boundWSClient.enqueue(
        {
          conversation_id: conversationId,
          client_msg_id: clientMsgId,
          msg_type: input.msg_type,
          content: input.content,
          extra: extra as Record<string, never>,
        },
        reqId,
      )
      return optimisticMsg
    }

    try {
      const res = await sendMessageHttp(conversationId, {
        client_msg_id: clientMsgId,
        msg_type: input.msg_type,
        content: input.content,
        extra,
      })
      const { message } = upsertServerMessage(conversationId, res.message, currentUserId)
      const conversationStore = useConversationStore()
      conversationStore.applyIncomingMessage(res.message, currentUserId, { isReplay: false })
      return message
    } catch (err) {
      const code =
        typeof err === 'object' && err !== null && 'code' in err
          ? Number((err as { code: unknown }).code)
          : -1
      const messageText = err instanceof Error ? err.message : '发送失败'
      markLocalMessageFailed(conversationId, clientMsgId, code, messageText, isRetryableSendError(code, true))
      throw err
    }
  }

  function markLocalMessageFailed(
    conversationId: string,
    clientMsgId: string,
    errorCode: number,
    errorMessage: string,
    retryable: boolean,
  ): void {
    const list = messagesByConversation.value[conversationId]
    if (!list) return
    const idx = list.findIndex((m) => m.client_msg_id === clientMsgId)
    if (idx < 0) return
    const nextList = [...list]
    nextList[idx] = {
      ...nextList[idx],
      local_status: 'failed',
      error_code: errorCode,
      error_message: errorMessage,
      retryable,
    }
    messagesByConversation.value[conversationId] = nextList
  }

  async function retryMessage(
    conversationId: string,
    clientMsgId: string,
    currentUserId: string,
  ): Promise<void> {
    const list = messagesByConversation.value[conversationId]
    if (!list) return
    const idx = list.findIndex((m) => m.client_msg_id === clientMsgId)
    if (idx < 0) return

    const existing = list[idx]
    if (existing.local_status !== 'failed' && existing.local_status !== 'sending') {
      return
    }
    retryScheduler.cancel(clientMsgId)

    const nextList = [...list]
    const nextReqId = existing.req_id && boundWSClient?.hasPending(existing.req_id)
      ? existing.req_id
      : createReqId()
    nextList[idx] = {
      ...existing,
      local_status: 'sending',
      req_id: nextReqId,
      error_code: undefined,
      error_message: undefined,
    }
    messagesByConversation.value[conversationId] = nextList

    if (boundWSClient) {
      if (boundWSClient.hasPending(nextReqId)) {
        boundWSClient.retryPending(nextReqId)
      } else {
        boundWSClient.enqueue(
          {
            conversation_id: conversationId,
            client_msg_id: existing.client_msg_id,
            msg_type: existing.msg_type,
            content: existing.content,
            extra: (existing.extra ?? {}) as Record<string, never>,
          },
          nextReqId,
        )
      }
      return
    }

    try {
      const payloadType =
        existing.msg_type === 'image' ||
        existing.msg_type === 'file' ||
        existing.msg_type === 'voice' ||
        existing.msg_type === 'text'
          ? existing.msg_type
          : 'text'
      const res = await sendMessageHttp(conversationId, {
        client_msg_id: existing.client_msg_id,
        msg_type: payloadType,
        content: existing.content,
        extra: existing.extra ?? {},
      })
      upsertServerMessage(conversationId, res.message, currentUserId)
      const conversationStore = useConversationStore()
      conversationStore.applyIncomingMessage(res.message, currentUserId, { isReplay: false })
    } catch (err) {
      const code =
        typeof err === 'object' && err !== null && 'code' in err
          ? Number((err as { code: unknown }).code)
          : -1
      markLocalMessageFailed(
        conversationId,
        clientMsgId,
        code,
        err instanceof Error ? err.message : '重试发送失败',
        isRetryableSendError(code, true),
      )
      throw err
    }
  }

  async function recallMessage(
    conversationId: string,
    msgId: string,
    currentUserId: string,
  ): Promise<void> {
    if (boundWSClient?.isConnected) {
      const reqId = boundWSClient.sendRecall({ conversation_id: conversationId, msg_id: msgId })
      if (reqId) {
        try {
          await new Promise<MsgRecallAckData>((resolve, reject) => {
            const timer = setTimeout(() => {
              pendingRecalls.delete(reqId)
              reject(new Error('撤回响应超时'))
            }, 6000)
            pendingRecalls.set(reqId, { resolve, reject, timer })
          })
          return
        } catch (err) {
          // If server rejected explicitly (not timeout), rethrow instead of duplicating HTTP call.
          if (err instanceof Error && err.message !== '撤回响应超时') {
            throw err
          }
        }
      }
    }

    const res = await recallMessageHttp(conversationId, msgId)
    upsertServerMessage(conversationId, res.recalled_message, currentUserId)
    upsertServerMessage(conversationId, res.recall_message, currentUserId)
    const conversationStore = useConversationStore()
    conversationStore.applyIncomingMessage(res.recall_message, currentUserId, { isReplay: true })
  }

  async function markRead(conversationId: string, requestedSeq?: string): Promise<void> {
    const conversationStore = useConversationStore()
    const conv = conversationStore.itemsById[conversationId]
    const highestLocalSeq = getHighestCommittedSeq(conversationId)
    const maxKnownSeq = maxSeq(conv?.last_seq ?? '0', highestLocalSeq)
    if (!isPositiveSeq(maxKnownSeq)) return

    const candidateSeq = requestedSeq ? minSeq(requestedSeq, maxKnownSeq) : maxKnownSeq
    if (!isPositiveSeq(candidateSeq)) return

    if (
      conv &&
      compareSeq(candidateSeq, conv.last_read_seq) <= 0 &&
      conv.unread_count === 0
    ) {
      return
    }

    // Optimistic local monotonic advance so unread red dot clears immediately.
    conversationStore.applyReadWatermark(conversationId, candidateSeq, candidateSeq, 0)

    if (boundWSClient?.isConnected) {
      const sentReq = boundWSClient.sendReadAck({
        conversation_id: conversationId,
        last_read_seq: candidateSeq,
      })
      if (sentReq) return
    }

    try {
      const res = await markConversationRead(conversationId, candidateSeq)
      conversationStore.applyReadWatermark(
        conversationId,
        res.last_read_seq,
        res.last_delivered_seq,
        res.unread_count,
      )
    } catch {
      // Watermark errors (e.g. 13005/13006) are safely ignored without regressing local state.
    }
  }

  async function markDelivered(conversationId: string, requestedSeq: string): Promise<void> {
    const conversationStore = useConversationStore()
    const conv = conversationStore.itemsById[conversationId]
    const maxKnownSeq = maxSeq(conv?.last_seq ?? '0', getHighestCommittedSeq(conversationId))
    const targetSeq = isPositiveSeq(maxKnownSeq) ? minSeq(requestedSeq, maxKnownSeq) : requestedSeq
    if (!isPositiveSeq(targetSeq)) return
    if (conv && compareSeq(targetSeq, conv.last_delivered_seq) <= 0) return

    conversationStore.applyDeliveredWatermark(conversationId, targetSeq)
    try {
      const res = await markConversationDelivered(conversationId, targetSeq)
      conversationStore.applyDeliveredWatermark(conversationId, res.last_delivered_seq)
    } catch {
      // Best-effort watermark compensation; next sync retries cleanly.
    }
  }

  async function loadMessageReceipts(
    conversationId: string,
    msgId: string,
    params: { status?: ReceiptFilterStatus; cursor?: string; reset?: boolean } = {},
  ): Promise<MessageReceiptList> {
    receiptsLoading.value = true
    try {
      const page = await fetchMessageReceipts(conversationId, msgId, {
        status: params.status,
        cursor: params.cursor,
        limit: 50,
      })
      if (params.reset || !params.cursor || !receiptsModalData.value) {
        receiptsModalData.value = page
      } else {
        receiptsModalData.value = {
          ...page,
          items: [...receiptsModalData.value.items, ...page.items],
        }
      }
      return receiptsModalData.value
    } finally {
      receiptsLoading.value = false
    }
  }

  function handleMessageAck(ack: MsgAckData, currentUserId: string): void {
    const list = messagesByConversation.value[ack.conversation_id] ?? []
    const existing = list.find((m) => m.client_msg_id === ack.client_msg_id)

    retryScheduler.cancel(ack.client_msg_id)

    const mergedMessage: Message = existing
      ? {
          msg_id: ack.msg_id,
          client_msg_id: ack.client_msg_id,
          conversation_id: ack.conversation_id,
          seq: ack.seq,
          sender_id: existing.sender_id || currentUserId,
          msg_type: existing.msg_type,
          content: existing.content,
          extra: existing.extra,
          status: ack.status,
          created_at: existing.created_at,
          recalled_at: existing.recalled_at,
          delivery_status: ack.delivery_status,
        }
      : {
          msg_id: ack.msg_id,
          client_msg_id: ack.client_msg_id,
          conversation_id: ack.conversation_id,
          seq: ack.seq,
          sender_id: currentUserId,
          msg_type: 'text',
          content: '',
          extra: {},
          status: ack.status,
          created_at: new Date().toISOString(),
          recalled_at: null,
          delivery_status: ack.delivery_status,
        }

    const { message } = upsertServerMessage(ack.conversation_id, mergedMessage, currentUserId)
    const conversationStore = useConversationStore()
    conversationStore.applyIncomingMessage(message, currentUserId, { isReplay: true })
  }

  function handleMessagePush(push: MessagePushData, currentUserId: string): void {
    const serverMsg: Message = {
      ...push.message,
      delivery_status: push.message.delivery_status ?? push.delivery_status,
    }
    const { inserted, message } = upsertServerMessage(
      serverMsg.conversation_id,
      serverMsg,
      currentUserId,
    )

    if (serverMsg.msg_type === 'recall' && typeof serverMsg.extra?.recalled_msg_id === 'string') {
      applyRecallInPlace(
        serverMsg.conversation_id,
        serverMsg.extra.recalled_msg_id,
        typeof serverMsg.extra.recalled_at === 'string'
          ? serverMsg.extra.recalled_at
          : serverMsg.created_at,
      )
    }

    const conversationStore = useConversationStore()
    conversationStore.applyIncomingMessage(serverMsg, currentUserId, { isReplay: !inserted })

    // Send delivered_ack once persisted into local Pinia store.
    if (currentUserId && serverMsg.sender_id !== currentUserId && isPositiveSeq(serverMsg.seq)) {
      if (boundWSClient?.isConnected) {
        boundWSClient.sendDeliveredAck({
          msg_id: serverMsg.msg_id,
          conversation_id: serverMsg.conversation_id,
          seq: serverMsg.seq,
        })
      }
      conversationStore.applyDeliveredWatermark(serverMsg.conversation_id, serverMsg.seq)

      if (
        conversationStore.activeConversationId === serverMsg.conversation_id &&
        conversationStore.isChatVisible
      ) {
        void markRead(serverMsg.conversation_id, serverMsg.seq)
      }
    }

    // Keep local variable used in strict TS builds.
    void message
  }

  function handleReceiptPush(push: ReceiptPushData, currentUserId: string): void {
    const conversationStore = useConversationStore()

    // Same-account multi-device read/delivered sync.
    if (currentUserId && push.user_id === currentUserId) {
      if (push.delivery_status === 'read') {
        conversationStore.applyReadWatermark(
          push.conversation_id,
          push.last_read_seq,
          push.last_delivered_seq,
          0,
        )
      } else {
        conversationStore.applyDeliveredWatermark(push.conversation_id, push.last_delivered_seq)
      }
      return
    }

    // Peer receipt: monotonically advance sender's outgoing messages without ever downgrading.
    const prevWatermark = peerWatermarksByConv.get(push.conversation_id) ?? {
      deliveredSeq: '0',
      readSeq: '0',
    }
    const nextDeliveredSeq = maxSeq(prevWatermark.deliveredSeq, push.last_delivered_seq)
    const nextReadSeq = maxSeq(prevWatermark.readSeq, push.last_read_seq)
    peerWatermarksByConv.set(push.conversation_id, {
      deliveredSeq: nextDeliveredSeq,
      readSeq: nextReadSeq,
    })

    const list = messagesByConversation.value[push.conversation_id]
    if (!list || list.length === 0) return

    let changed = false
    const updatedList = list.map((msg) => {
      if (currentUserId && msg.sender_id !== currentUserId) return msg

      let targetStatus: DeliveryStatus | null = null
      if (push.msg_id && msg.msg_id === push.msg_id) {
        targetStatus = push.delivery_status
      }
      if (isPositiveSeq(msg.seq)) {
        if (isPositiveSeq(nextReadSeq) && compareSeq(msg.seq, nextReadSeq) <= 0) {
          targetStatus = 'read'
        } else if (
          isPositiveSeq(nextDeliveredSeq) &&
          compareSeq(msg.seq, nextDeliveredSeq) <= 0
        ) {
          targetStatus = targetStatus === 'read' ? 'read' : 'delivered'
        }
      }

      if (!targetStatus) return msg
      const merged = mergeDeliveryStatus(msg.local_status, targetStatus)
      if (merged === msg.local_status) return msg
      changed = true
      return {
        ...msg,
        local_status: merged,
        delivery_status: merged as DeliveryStatus,
      }
    })

    if (changed) {
      messagesByConversation.value[push.conversation_id] = updatedList
    }
  }

  function handleRecallAck(ack: MsgRecallAckData): void {
    applyRecallInPlace(ack.conversation_id, ack.recalled_msg_id, ack.recalled_at)
    const conversationStore = useConversationStore()
    conversationStore.applyConversationPreviewOnRecall(ack.conversation_id, ack.recalled_msg_id)
  }

  function handleSystemNotice(notice: SystemNoticeData, currentUserId: string): void {
    const conversationStore = useConversationStore()
    if (notice.event === 'message.recalled' && notice.conversation_id) {
      const recalledMsgId =
        typeof notice.payload?.recalled_msg_id === 'string'
          ? notice.payload.recalled_msg_id
          : ''
      const recalledAt =
        typeof notice.payload?.recalled_at === 'string' ? notice.payload.recalled_at : null
      if (recalledMsgId) {
        applyRecallInPlace(notice.conversation_id, recalledMsgId, recalledAt)
        conversationStore.applyConversationPreviewOnRecall(notice.conversation_id, recalledMsgId)
      }
    }
    conversationStore.applySystemNotice(notice, currentUserId)
  }

  function handleWsError(err: WSErrorData, _currentUserId: string): void {
    if (err.related_type !== 'msg.send' || !err.related_req_id) return
    const reqId = err.related_req_id

    for (const [convId, list] of Object.entries(messagesByConversation.value)) {
      const msg = list.find((item) => item.req_id === reqId)
      if (!msg) continue

      const canRetry = isRetryableSendError(err.code, err.retryable)
      if (canRetry) {
        const scheduled = retryScheduler.schedule(msg.client_msg_id, reqId, convId)
        if (!scheduled) {
          markLocalMessageFailed(
            convId,
            msg.client_msg_id,
            err.code,
            err.message || '消息发送失败，请手动重试',
            true,
          )
        }
      } else {
        retryScheduler.cancel(msg.client_msg_id)
        boundWSClient?.removePending(reqId)
        markLocalMessageFailed(
          convId,
          msg.client_msg_id,
          err.code,
          err.message || '消息发送被拒绝',
          false,
        )
      }
      break
    }
  }

  function reset(): void {
    retryScheduler.cancelAll()
    unbindWSClient()
    for (const list of Object.values(messagesByConversation.value)) {
      for (const msg of list) {
        if (msg.local_preview_url && msg.local_preview_url.startsWith('blob:') && typeof URL !== 'undefined') {
          URL.revokeObjectURL(msg.local_preview_url)
        }
      }
    }
    messagesByConversation.value = {}
    paginationByConversation.value = {}
    syncingByConversation.value = {}
    receiptsModalData.value = null
    receiptsLoading.value = false
    peerWatermarksByConv.clear()
  }

  return {
    messagesByConversation,
    paginationByConversation,
    syncingByConversation,
    receiptsModalData,
    receiptsLoading,
    wsConnected,
    getMessages,
    getPagination,
    getHighestCommittedSeq,
    upsertServerMessage,
    applyRecallInPlace,
    setWsConnected,
    bindWSClient,
    unbindWSClient,
    loadInitialMessages,
    loadOlderMessages,
    syncConversation,
    sendMessage,
    retryMessage,
    recallMessage,
    markRead,
    markDelivered,
    loadMessageReceipts,
    handleMessageAck,
    handleMessagePush,
    handleReceiptPush,
    handleRecallAck,
    handleSystemNotice,
    handleWsError,
    reset,
  }
})

if (import.meta.hot) {
  import.meta.hot.accept(acceptHMRUpdate(useMessageStore, import.meta.hot))
}
