import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import * as friendApi from '@/api/friend'
import * as userApi from '@/api/user'
import { getWSClient } from '@/stores/auth'
import type {
  AcceptFriendRequestResponse,
  FriendItem,
  FriendRequestDirection,
  FriendRequestItem,
  FriendRequestStatus,
} from '@/types/friend'
import type { User, UserSearchParams } from '@/types/user'

export const useContactStore = defineStore('contact', () => {
  const friends = ref<FriendItem[]>([])
  const friendsNextCursor = ref<string | null>(null)
  const friendsHasMore = ref(false)
  const friendsLoading = ref(false)

  const receivedRequests = ref<FriendRequestItem[]>([])
  const receivedNextCursor = ref<string | null>(null)
  const receivedHasMore = ref(false)

  const sentRequests = ref<FriendRequestItem[]>([])
  const sentNextCursor = ref<string | null>(null)
  const sentHasMore = ref(false)
  const requestsLoading = ref(false)

  const searchResults = ref<User[]>([])
  const searchNextCursor = ref<string | null>(null)
  const searchHasMore = ref(false)
  const searchLoading = ref(false)

  const conversationIdMap = ref<Record<string, string>>({})
  let lastSearchParams: UserSearchParams | null = null
  let realtimeUnbinders: Array<() => void> = []

  const pendingReceivedCount = computed(
    () => receivedRequests.value.filter((item) => item.status === 'pending').length,
  )

  const friendIdsSet = computed(() => new Set(friends.value.map((f) => f.friend_id)))

  function findFriend(friendId: string): FriendItem | undefined {
    return friends.value.find((item) => item.friend_id === friendId)
  }

  function applyPresenceUpdate(userId: string, online: boolean, lastOnlineAt: string | null) {
    friends.value.forEach((friend) => {
      if (friend.friend_id === userId || friend.user.id === userId) {
        friend.online = online
        friend.user.online = online
        friend.user.last_online_at = lastOnlineAt
      }
    })

    searchResults.value.forEach((u) => {
      if (u.id === userId) {
        u.online = online
        u.last_online_at = lastOnlineAt
      }
    })

    receivedRequests.value.forEach((req) => {
      if (req.peer && req.peer.id === userId) {
        req.peer.online = online
        req.peer.last_online_at = lastOnlineAt
      }
    })

    sentRequests.value.forEach((req) => {
      if (req.peer && req.peer.id === userId) {
        req.peer.online = online
        req.peer.last_online_at = lastOnlineAt
      }
    })
  }

  function bindRealtimeListeners() {
    if (realtimeUnbinders.length > 0) return
    const wsClient = getWSClient()

    const unbindPresence = wsClient.on('presence.update', (frame) => {
      const data = frame.data
      if (typeof data?.user_id !== 'string') return
      const online = Boolean(data.online)
      const lastOnlineAt =
        typeof data.last_online_at === 'string' ? data.last_online_at : null
      applyPresenceUpdate(data.user_id, online, lastOnlineAt)
    })

    const unbindNotice = wsClient.on('system.notice', (frame) => {
      const event = frame.data?.event
      if (event === 'friend.requested') {
        void fetchRequests('received', undefined, true).catch(() => {})
      } else if (event === 'friend.accepted') {
        void Promise.all([
          fetchFriends(true),
          fetchRequests('received', undefined, true),
          fetchRequests('sent', undefined, true),
        ]).catch(() => {})
      } else if (event === 'friend.rejected') {
        void fetchRequests('sent', undefined, true).catch(() => {})
      }
    })

    realtimeUnbinders.push(unbindPresence, unbindNotice)
  }

  function unbindRealtimeListeners() {
    realtimeUnbinders.forEach((fn) => fn())
    realtimeUnbinders = []
  }

  async function fetchFriends(reset = true): Promise<void> {
    if (!reset && (!friendsHasMore.value || !friendsNextCursor.value)) return
    friendsLoading.value = true
    try {
      const page = await friendApi.listFriends({
        cursor: reset ? undefined : friendsNextCursor.value ?? undefined,
        limit: 50,
      })
      const items = page.items ?? []
      items.forEach((item) => {
        if (item.conversation_id) {
          conversationIdMap.value[item.friend_id] = item.conversation_id
        }
      })
      if (reset) {
        friends.value = items
      } else {
        const existingIds = new Set(friends.value.map((f) => f.friend_id))
        friends.value.push(...items.filter((i) => !existingIds.has(i.friend_id)))
      }
      friendsNextCursor.value = page.next_cursor
      friendsHasMore.value = page.has_more
    } finally {
      friendsLoading.value = false
    }
  }

  async function fetchRequests(
    direction: FriendRequestDirection,
    status?: FriendRequestStatus,
    reset = true,
  ): Promise<void> {
    const cursorRef = direction === 'received' ? receivedNextCursor : sentNextCursor
    const hasMoreRef = direction === 'received' ? receivedHasMore : sentHasMore
    const listRef = direction === 'received' ? receivedRequests : sentRequests

    if (!reset && (!hasMoreRef.value || !cursorRef.value)) return
    requestsLoading.value = true
    try {
      const page = await friendApi.listFriendRequests({
        direction,
        status,
        cursor: reset ? undefined : cursorRef.value ?? undefined,
        limit: 20,
      })
      const items = page.items ?? []
      if (reset) {
        listRef.value = items
      } else {
        const existingIds = new Set(listRef.value.map((r) => r.id))
        listRef.value.push(...items.filter((i) => !existingIds.has(i.id)))
      }
      cursorRef.value = page.next_cursor
      hasMoreRef.value = page.has_more
    } finally {
      requestsLoading.value = false
    }
  }

  async function searchUsers(params: UserSearchParams, reset = true): Promise<void> {
    if (reset) {
      lastSearchParams = { ...params }
    } else if (!lastSearchParams || !searchHasMore.value || !searchNextCursor.value) {
      return
    }

    const queryParams: UserSearchParams = reset
      ? params
      : { ...lastSearchParams!, cursor: searchNextCursor.value ?? undefined }

    searchLoading.value = true
    try {
      const page = await userApi.searchUsers(queryParams)
      const items = page.items ?? []
      if (reset) {
        searchResults.value = items
      } else {
        const existingIds = new Set(searchResults.value.map((u) => u.id))
        searchResults.value.push(...items.filter((u) => !existingIds.has(u.id)))
      }
      searchNextCursor.value = page.next_cursor
      searchHasMore.value = page.has_more
    } finally {
      searchLoading.value = false
    }
  }

  async function sendFriendRequest(toUserId: string, message = ''): Promise<void> {
    await friendApi.createFriendRequest({ to_user_id: toUserId, message })
    await fetchRequests('sent', undefined, true)
  }

  async function acceptRequest(requestId: string): Promise<AcceptFriendRequestResponse> {
    const res = await friendApi.acceptFriendRequest(requestId)
    if (res.conversation_id) {
      conversationIdMap.value[res.friend_id] = res.conversation_id
    }
    const target = receivedRequests.value.find((r) => r.id === requestId)
    if (target) {
      target.status = res.request.status
      target.handled_at = res.request.handled_at
    }
    await fetchFriends(true)
    return res
  }

  async function rejectRequest(requestId: string): Promise<void> {
    const res = await friendApi.rejectFriendRequest(requestId)
    const target = receivedRequests.value.find((r) => r.id === requestId)
    if (target) {
      target.status = res.request.status
      target.handled_at = res.request.handled_at
    }
  }

  async function cancelRequest(requestId: string): Promise<void> {
    const res = await friendApi.cancelFriendRequest(requestId)
    const target = sentRequests.value.find((r) => r.id === requestId)
    if (target) {
      target.status = res.request.status
      target.handled_at = res.request.handled_at
    }
  }

  async function updateRemark(friendId: string, remark: string): Promise<void> {
    const res = await friendApi.updateFriendRemark(friendId, remark)
    const target = findFriend(friendId)
    if (target) {
      target.remark = res.remark
    }
  }

  async function removeFriend(friendId: string): Promise<void> {
    await friendApi.deleteFriend(friendId)
    friends.value = friends.value.filter((f) => f.friend_id !== friendId)
    delete conversationIdMap.value[friendId]
  }

  function reset() {
    unbindRealtimeListeners()
    friends.value = []
    friendsNextCursor.value = null
    friendsHasMore.value = false
    receivedRequests.value = []
    receivedNextCursor.value = null
    receivedHasMore.value = false
    sentRequests.value = []
    sentNextCursor.value = null
    sentHasMore.value = false
    searchResults.value = []
    searchNextCursor.value = null
    searchHasMore.value = false
    conversationIdMap.value = {}
    lastSearchParams = null
  }

  return {
    friends,
    friendsNextCursor,
    friendsHasMore,
    friendsLoading,
    receivedRequests,
    receivedNextCursor,
    receivedHasMore,
    sentRequests,
    sentNextCursor,
    sentHasMore,
    requestsLoading,
    searchResults,
    searchNextCursor,
    searchHasMore,
    searchLoading,
    conversationIdMap,
    pendingReceivedCount,
    friendIdsSet,
    findFriend,
    bindRealtimeListeners,
    unbindRealtimeListeners,
    fetchFriends,
    fetchRequests,
    searchUsers,
    sendFriendRequest,
    acceptRequest,
    rejectRequest,
    cancelRequest,
    updateRemark,
    removeFriend,
    reset,
  }
})
