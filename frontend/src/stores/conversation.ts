import { acceptHMRUpdate, defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  fetchConversationDetail,
  fetchConversationMembers,
  fetchConversations,
  updateConversationSettings,
} from '@/api/conversation'
import { compareSeq, maxSeq } from '@/composables/chat/seq'
import type {
  Conversation,
  ConversationDetail,
  ConversationMember,
  UpdateConversationSettingsPayload,
} from '@/types/conversation'
import type { Message, SystemNoticeData } from '@/types/message'

function buildMessagePreview(msg: Message): string {
  if (msg.status === 'recalled' || msg.msg_type === 'recall') {
    return msg.content?.trim() || '撤回了一条消息'
  }
  const trimmed = msg.content?.trim()
  if (trimmed) return trimmed
  switch (msg.msg_type) {
    case 'image':
      return '[图片]'
    case 'file': {
      const filename = typeof msg.extra?.filename === 'string' ? msg.extra.filename.trim() : ''
      return filename ? `[文件] ${filename}` : '[文件]'
    }
    case 'voice':
      return '[语音]'
    case 'system':
      return '[系统通知]'
    default:
      return ''
  }
}

function createPlaceholderConversation(msg: Message): Conversation {
  return {
    id: msg.conversation_id,
    type: 'single',
    peer: null,
    group_id: null,
    title: `会话 ${msg.conversation_id}`,
    avatar_url: '',
    last_msg_id: '0',
    last_seq: '0',
    last_msg_preview: '',
    last_msg_at: null,
    last_delivered_seq: '0',
    last_read_seq: '0',
    unread_count: 0,
    is_pinned: false,
    is_muted: false,
    is_visible: true,
    can_send: true,
  }
}

export const useConversationStore = defineStore('conversation', () => {
  const itemsById = ref<Record<string, Conversation>>({})
  const activeConversationId = ref<string | null>(null)
  const isChatVisible = ref(false)
  const nextCursor = ref<string | null>(null)
  const hasMore = ref(false)
  const loading = ref(false)
  const loadingMore = ref(false)
  const includeHidden = ref(false)
  const activeDetail = ref<ConversationDetail | null>(null)
  const membersByConversation = ref<Record<string, ConversationMember[]>>({})
  const error = ref<string | null>(null)

  // Tracks server snapshot last_seq and seen push message IDs to prevent double-counting unread on replay.
  const baselineSeqByConv = new Map<string, string>()
  const seenPushIdsByConv = new Map<string, Set<string>>()
  const placeholderConvIds = new Set<string>()
  const hydratingConvIds = new Set<string>()

  const conversations = computed<Conversation[]>(() => {
    return Object.values(itemsById.value).sort((a, b) => {
      if (a.is_pinned !== b.is_pinned) {
        return a.is_pinned ? -1 : 1
      }
      const timeA = a.last_msg_at ? Date.parse(a.last_msg_at) : 0
      const timeB = b.last_msg_at ? Date.parse(b.last_msg_at) : 0
      if (timeA !== timeB) {
        return timeB - timeA
      }
      return compareSeq(b.id, a.id)
    })
  })

  const visibleConversations = computed<Conversation[]>(() => {
    return conversations.value.filter((conv) => conv.is_visible)
  })

  const activeConversation = computed<Conversation | null>(() => {
    if (!activeConversationId.value) return null
    return itemsById.value[activeConversationId.value] ?? null
  })

  const totalUnreadCount = computed<number>(() => {
    return visibleConversations.value.reduce((sum, conv) => sum + (conv.unread_count || 0), 0)
  })

  const unmutedUnreadCount = computed<number>(() => {
    return visibleConversations.value.reduce(
      (sum, conv) => sum + (conv.is_muted ? 0 : conv.unread_count || 0),
      0,
    )
  })

  const lastSeqMap = computed<Record<string, string>>(() => {
    const map: Record<string, string> = {}
    for (const conv of Object.values(itemsById.value)) {
      map[conv.id] = conv.last_seq || '0'
    }
    return map
  })

  function markPushSeen(conversationId: string, msgId: string): boolean {
    if (!msgId) return false
    let set = seenPushIdsByConv.get(conversationId)
    if (!set) {
      set = new Set<string>()
      seenPushIdsByConv.set(conversationId, set)
    }
    if (set.has(msgId)) return false
    set.add(msgId)
    return true
  }

  function upsertConversation(conv: Conversation): Conversation {
    const existing = itemsById.value[conv.id]
    placeholderConvIds.delete(conv.id)
    const prevBaseline = baselineSeqByConv.get(conv.id) ?? '0'
    baselineSeqByConv.set(conv.id, maxSeq(prevBaseline, conv.last_seq || '0'))

    if (!existing) {
      const normalized: Conversation = {
        ...conv,
        last_msg_id: conv.last_msg_id || '0',
        last_seq: conv.last_seq || '0',
        last_delivered_seq: conv.last_delivered_seq || '0',
        last_read_seq: conv.last_read_seq || '0',
        unread_count: Math.max(0, conv.unread_count || 0),
      }
      itemsById.value[conv.id] = normalized
      return normalized
    }

    const mergedLastSeq = maxSeq(existing.last_seq, conv.last_seq)
    const mergedDeliveredSeq = maxSeq(existing.last_delivered_seq, conv.last_delivered_seq)
    const mergedReadSeq = maxSeq(existing.last_read_seq, conv.last_read_seq)
    const serverAdvanced = compareSeq(conv.last_seq, existing.last_seq) >= 0

    let unreadCount = existing.unread_count
    if (compareSeq(mergedReadSeq, mergedLastSeq) >= 0) {
      unreadCount = 0
    } else if (serverAdvanced) {
      unreadCount = Math.max(0, conv.unread_count || 0)
    }

    const merged: Conversation = {
      ...existing,
      ...conv,
      last_msg_id: serverAdvanced ? conv.last_msg_id : existing.last_msg_id,
      last_seq: mergedLastSeq,
      last_msg_preview: serverAdvanced ? conv.last_msg_preview : existing.last_msg_preview,
      last_msg_at: serverAdvanced ? conv.last_msg_at : existing.last_msg_at,
      last_delivered_seq: mergedDeliveredSeq,
      last_read_seq: mergedReadSeq,
      unread_count: unreadCount,
    }
    itemsById.value[conv.id] = merged
    return merged
  }

  async function fetchList(options: { includeHidden?: boolean; reset?: boolean } = {}): Promise<void> {
    const nextIncludeHidden = options.includeHidden ?? includeHidden.value
    includeHidden.value = nextIncludeHidden
    loading.value = true
    error.value = null
    try {
      const page = await fetchConversations({
        include_hidden: nextIncludeHidden || undefined,
        limit: 50,
      })
      if (options.reset) {
        const fetchedIds = new Set(page.items.map((item) => item.id))
        for (const existingId of Object.keys(itemsById.value)) {
          if (!fetchedIds.has(existingId) && existingId !== activeConversationId.value) {
            delete itemsById.value[existingId]
          }
        }
      }
      for (const item of page.items) {
        upsertConversation(item)
      }
      nextCursor.value = page.next_cursor
      hasMore.value = page.has_more
    } catch (err) {
      error.value = err instanceof Error ? err.message : '加载会话列表失败'
      throw err
    } finally {
      loading.value = false
    }
  }

  async function loadMore(): Promise<void> {
    if (loadingMore.value || !hasMore.value || !nextCursor.value) return
    loadingMore.value = true
    error.value = null
    try {
      const page = await fetchConversations({
        cursor: nextCursor.value,
        include_hidden: includeHidden.value || undefined,
        limit: 50,
      })
      for (const item of page.items) {
        upsertConversation(item)
      }
      nextCursor.value = page.next_cursor
      hasMore.value = page.has_more
    } catch (err) {
      error.value = err instanceof Error ? err.message : '加载更多会话失败'
      throw err
    } finally {
      loadingMore.value = false
    }
  }

  async function fetchDetail(conversationId: string): Promise<ConversationDetail> {
    const detail = await fetchConversationDetail(conversationId)
    if (detail?.conversation) {
      upsertConversation(detail.conversation)
      if (activeConversationId.value === conversationId) {
        activeDetail.value = detail
      }
    }
    return detail
  }

  function hydrateConversation(conversationId: string): void {
    if (!conversationId || hydratingConvIds.has(conversationId)) return
    hydratingConvIds.add(conversationId)
    void fetchDetail(conversationId)
      .catch(() => undefined)
      .finally(() => {
        hydratingConvIds.delete(conversationId)
      })
  }

  async function fetchMembers(conversationId: string): Promise<ConversationMember[]> {
    const all: ConversationMember[] = []
    let cursor: string | undefined
    for (let pageCount = 0; pageCount < 10; pageCount += 1) {
      const page = await fetchConversationMembers(conversationId, { cursor, limit: 100 })
      all.push(...page.items)
      if (!page.has_more || !page.next_cursor) break
      cursor = page.next_cursor
    }
    membersByConversation.value[conversationId] = all
    return all
  }

  function setActiveConversation(conversationId: string | null): void {
    activeConversationId.value = conversationId
    if (!conversationId) {
      activeDetail.value = null
      return
    }
    if (activeDetail.value?.conversation.id !== conversationId) {
      activeDetail.value = null
    }
  }

  function setChatVisible(visible: boolean): void {
    isChatVisible.value = visible
  }

  async function updateSettings(
    conversationId: string,
    payload: UpdateConversationSettingsPayload,
  ): Promise<void> {
    const updated = await updateConversationSettings(conversationId, payload)
    const existing = itemsById.value[conversationId]
    if (existing) {
      itemsById.value[conversationId] = {
        ...existing,
        is_pinned: updated.is_pinned,
        is_muted: updated.is_muted,
        is_visible: updated.is_visible,
      }
    }
    if (activeDetail.value?.conversation.id === conversationId) {
      activeDetail.value = {
        ...activeDetail.value,
        conversation: {
          ...activeDetail.value.conversation,
          is_pinned: updated.is_pinned,
          is_muted: updated.is_muted,
          is_visible: updated.is_visible,
        },
      }
    }
  }

  function applyIncomingMessage(
    msg: Message,
    currentUserId: string,
    options: { isReplay?: boolean } = {},
  ): Conversation {
    const hadExisting = Boolean(itemsById.value[msg.conversation_id])
    const existing = itemsById.value[msg.conversation_id] ?? createPlaceholderConversation(msg)
    if (!hadExisting) {
      placeholderConvIds.add(msg.conversation_id)
      hydrateConversation(msg.conversation_id)
    } else if (placeholderConvIds.has(msg.conversation_id)) {
      hydrateConversation(msg.conversation_id)
    }
    const prevLastSeq = existing.last_seq || '0'
    const baselineSeq = baselineSeqByConv.get(msg.conversation_id) ?? '0'

    const isFirstSeenPush = msg.msg_id ? markPushSeen(msg.conversation_id, msg.msg_id) : true
    const isReplay = options.isReplay === true || (msg.msg_id !== '' && !isFirstSeenPush)
    const isLatestOrSame = compareSeq(msg.seq, prevLastSeq) >= 0

    let nextUnread = existing.unread_count
    const isActiveVisible =
      activeConversationId.value === msg.conversation_id && isChatVisible.value

    if (!isReplay && compareSeq(msg.seq, baselineSeq) > 0) {
      if (msg.msg_type === 'recall') {
        const recalledSeq =
          typeof msg.extra?.recalled_seq === 'string' ? msg.extra.recalled_seq : ''
        if (
          msg.sender_id !== currentUserId &&
          recalledSeq &&
          compareSeq(recalledSeq, existing.last_read_seq) > 0 &&
          nextUnread > 0
        ) {
          nextUnread = Math.max(0, nextUnread - 1)
        }
      } else if (
        msg.sender_id !== currentUserId &&
        compareSeq(msg.seq, existing.last_read_seq) > 0 &&
        !isActiveVisible
      ) {
        nextUnread += 1
      }
    }

    const updated: Conversation = {
      ...existing,
      last_seq: maxSeq(prevLastSeq, msg.seq),
      last_msg_id: isLatestOrSame && msg.msg_id ? msg.msg_id : existing.last_msg_id,
      last_msg_preview: isLatestOrSame ? buildMessagePreview(msg) : existing.last_msg_preview,
      last_msg_at: isLatestOrSame ? msg.created_at : existing.last_msg_at,
      is_visible: true,
      unread_count: nextUnread,
    }

    itemsById.value[msg.conversation_id] = updated
    if (activeDetail.value?.conversation.id === msg.conversation_id) {
      activeDetail.value = {
        ...activeDetail.value,
        conversation: updated,
      }
    }
    return updated
  }

  function applyConversationPreviewOnRecall(
    conversationId: string,
    recalledMsgId: string,
    recallText = '撤回了一条消息',
  ): void {
    const existing = itemsById.value[conversationId]
    if (!existing) return
    if (existing.last_msg_id === recalledMsgId) {
      itemsById.value[conversationId] = {
        ...existing,
        last_msg_preview: recallText,
      }
    }
  }

  function applyReadWatermark(
    conversationId: string,
    lastReadSeq: string,
    lastDeliveredSeq?: string,
    unreadCount?: number,
  ): void {
    const existing = itemsById.value[conversationId]
    if (!existing) return

    const nextReadSeq = maxSeq(existing.last_read_seq, lastReadSeq)
    const nextDeliveredSeq = maxSeq(
      existing.last_delivered_seq,
      maxSeq(lastDeliveredSeq ?? '0', nextReadSeq),
    )

    let nextUnread = existing.unread_count
    if (unreadCount === 0 || compareSeq(nextReadSeq, existing.last_seq) >= 0) {
      nextUnread = 0
    } else if (typeof unreadCount === 'number') {
      nextUnread = Math.max(0, unreadCount)
    }

    const updated: Conversation = {
      ...existing,
      last_read_seq: nextReadSeq,
      last_delivered_seq: nextDeliveredSeq,
      unread_count: nextUnread,
    }
    itemsById.value[conversationId] = updated
    if (activeDetail.value?.conversation.id === conversationId) {
      activeDetail.value = {
        ...activeDetail.value,
        conversation: updated,
      }
    }
  }

  function applyDeliveredWatermark(conversationId: string, lastDeliveredSeq: string): void {
    const existing = itemsById.value[conversationId]
    if (!existing) return
    const nextDeliveredSeq = maxSeq(existing.last_delivered_seq, lastDeliveredSeq)
    if (nextDeliveredSeq === existing.last_delivered_seq) return
    const updated: Conversation = {
      ...existing,
      last_delivered_seq: nextDeliveredSeq,
    }
    itemsById.value[conversationId] = updated
    if (activeDetail.value?.conversation.id === conversationId) {
      activeDetail.value = {
        ...activeDetail.value,
        conversation: updated,
      }
    }
  }

  function applyPeerPresence(
    userId: string,
    online: boolean,
    lastOnlineAt: string | null,
  ): void {
    for (const [convId, conv] of Object.entries(itemsById.value)) {
      if (conv.peer && conv.peer.id === userId) {
        itemsById.value[convId] = {
          ...conv,
          peer: {
            ...conv.peer,
            online,
            last_online_at: online ? conv.peer.last_online_at : lastOnlineAt,
          },
        }
      }
    }

    if (activeDetail.value?.peer && activeDetail.value.peer.id === userId) {
      activeDetail.value = {
        ...activeDetail.value,
        peer: {
          ...activeDetail.value.peer,
          online,
          last_online_at: online ? activeDetail.value.peer.last_online_at : lastOnlineAt,
        },
        conversation: itemsById.value[activeDetail.value.conversation.id] ?? activeDetail.value.conversation,
      }
    }

    for (const [convId, members] of Object.entries(membersByConversation.value)) {
      let changed = false
      const updatedMembers = members.map((member) => {
        if (member.user.id !== userId) return member
        changed = true
        return {
          ...member,
          online,
          user: {
            ...member.user,
            online,
            last_online_at: online ? member.user.last_online_at : lastOnlineAt,
          },
        }
      })
      if (changed) {
        membersByConversation.value[convId] = updatedMembers
      }
    }
  }

  function applySystemNotice(notice: SystemNoticeData, currentUserId: string): void {
    const convId = notice.conversation_id
    if (!convId) return
    const existing = itemsById.value[convId]

    switch (notice.event) {
      case 'group.disbanded': {
        if (existing) {
          itemsById.value[convId] = { ...existing, can_send: false }
        }
        if (activeDetail.value?.conversation.id === convId && activeDetail.value.group) {
          activeDetail.value = {
            ...activeDetail.value,
            group: { ...activeDetail.value.group, status: 'disbanded' },
            conversation: itemsById.value[convId] ?? activeDetail.value.conversation,
          }
        }
        break
      }
      case 'group.member_removed': {
        const targetId =
          notice.target_user_id ??
          (typeof notice.payload?.user_id === 'string' ? notice.payload.user_id : null)
        if (targetId && targetId === currentUserId && existing) {
          itemsById.value[convId] = { ...existing, can_send: false }
        }
        break
      }
      case 'group.created':
      case 'group.member_added': {
        if (!existing || placeholderConvIds.has(convId)) {
          hydrateConversation(convId)
        }
        break
      }
      case 'group.updated': {
        if (!existing || placeholderConvIds.has(convId)) {
          hydrateConversation(convId)
        }
        if (existing) {
          const nextTitle =
            typeof notice.payload?.name === 'string' && notice.payload.name.trim()
              ? notice.payload.name
              : existing.title
          const nextAvatar =
            typeof notice.payload?.avatar_url === 'string'
              ? notice.payload.avatar_url
              : existing.avatar_url
          itemsById.value[convId] = {
            ...existing,
            title: nextTitle,
            avatar_url: nextAvatar,
          }
        }
        if (activeDetail.value?.conversation.id === convId && activeDetail.value.group) {
          activeDetail.value = {
            ...activeDetail.value,
            group: {
              ...activeDetail.value.group,
              name:
                typeof notice.payload?.name === 'string'
                  ? notice.payload.name
                  : activeDetail.value.group.name,
              avatar_url:
                typeof notice.payload?.avatar_url === 'string'
                  ? notice.payload.avatar_url
                  : activeDetail.value.group.avatar_url,
              announcement:
                notice.payload?.announcement === null || typeof notice.payload?.announcement === 'string'
                  ? (notice.payload.announcement as string | null)
                  : activeDetail.value.group.announcement,
            },
            conversation: itemsById.value[convId] ?? activeDetail.value.conversation,
          }
        }
        break
      }
      case 'friend.accepted': {
        if (!existing || placeholderConvIds.has(convId)) {
          hydrateConversation(convId)
        }
        if (existing) {
          itemsById.value[convId] = { ...existing, can_send: true, is_visible: true }
        }
        break
      }
      default:
        break
    }
  }

  function reset(): void {
    itemsById.value = {}
    activeConversationId.value = null
    isChatVisible.value = false
    nextCursor.value = null
    hasMore.value = false
    loading.value = false
    loadingMore.value = false
    includeHidden.value = false
    activeDetail.value = null
    membersByConversation.value = {}
    error.value = null
    baselineSeqByConv.clear()
    seenPushIdsByConv.clear()
    placeholderConvIds.clear()
    hydratingConvIds.clear()
  }

  return {
    itemsById,
    activeConversationId,
    isChatVisible,
    nextCursor,
    hasMore,
    loading,
    loadingMore,
    includeHidden,
    activeDetail,
    membersByConversation,
    error,
    conversations,
    visibleConversations,
    activeConversation,
    totalUnreadCount,
    unmutedUnreadCount,
    lastSeqMap,
    upsertConversation,
    fetchList,
    loadMore,
    fetchDetail,
    fetchMembers,
    setActiveConversation,
    setChatVisible,
    updateSettings,
    applyIncomingMessage,
    applyConversationPreviewOnRecall,
    applyReadWatermark,
    applyDeliveredWatermark,
    applyPeerPresence,
    applySystemNotice,
    reset,
  }
})

if (import.meta.hot) {
  import.meta.hot.accept(acceptHMRUpdate(useConversationStore, import.meta.hot))
}
