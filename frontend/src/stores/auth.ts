import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { ElMessage } from 'element-plus'
import * as authApi from '@/api/auth'
import * as userApi from '@/api/user'
import {
  clearStoredTokens,
  getDeviceId,
  getStoredTokens,
  refreshTokensSingleFlight,
  saveStoredTokens,
  setAuthLifecycleCallbacks,
} from '@/composables/useAuthRefresh'
import type { ChangePasswordPayload, LogoutScope, RegisterPayload, Tokens } from '@/types/auth'
import { BIZ_CODE } from '@/types/common'
import type { SelfUser, UpdateMePayload } from '@/types/user'
import {
  createSharedWSClient,
  getSharedWSClient,
  resetSharedWSClient,
  type WSClient,
} from '@/utils/wsClient'

export type WSConnectionState = 'disconnected' | 'connecting' | 'connected'

let navigateToLoginHandler: ((reason?: string) => void) | null = null

export function setAuthRedirectHandler(handler: (reason?: string) => void): void {
  navigateToLoginHandler = handler
}

let activeAccessToken = ''
let activeUserId: string | null = null

export function getWSClient(): WSClient {
  return (
    getSharedWSClient() ??
    createSharedWSClient({
      getAccessToken: () => activeAccessToken || getStoredTokens()?.access_token || '',
      getAccountId: () => activeUserId,
    })
  )
}

export const useAuthStore = defineStore('auth', () => {
  const user = ref<SelfUser | null>(null)
  const tokens = ref<Tokens | null>(getStoredTokens())
  const sessionId = ref<string>(tokens.value?.session_id ?? '')
  const deviceId = ref<string>(getDeviceId())
  const initialized = ref(false)
  const loading = ref(false)
  const wsState = ref<WSConnectionState>('disconnected')

  activeAccessToken = tokens.value?.access_token ?? ''
  activeUserId = user.value?.id ?? null

  let boundWSClient: WSClient | null = null
  let wsUnsubs: Array<() => void> = []
  let initPromise: Promise<boolean> | null = null

  const isAuthenticated = computed(() => Boolean(tokens.value?.access_token && user.value))
  const isAdmin = computed(() => user.value?.role === 'admin')
  const wsClient = computed(() => getWSClient())

  function getAccessToken(): string {
    return activeAccessToken || tokens.value?.access_token || getStoredTokens()?.access_token || ''
  }

  function handleSessionTerminated(message: string, redirect = true) {
    clearLocalSession()
    if (message) {
      ElMessage.warning(message)
    }
    if (redirect) {
      navigateToLoginHandler?.(message)
    }
  }

  function unbindWSListeners() {
    for (const unsub of wsUnsubs) {
      unsub()
    }
    wsUnsubs = []
    boundWSClient = null
  }

  function bindWSListeners(client: WSClient = getWSClient()) {
    if (boundWSClient === client && wsUnsubs.length > 0) return
    unbindWSListeners()
    boundWSClient = client
    if (client.isConnected) {
      wsState.value = 'connected'
    }

    wsUnsubs = [
      client.on('open', () => {
        wsState.value = 'connected'
      }),

      client.on('close', () => {
        wsState.value = 'disconnected'
      }),

      client.on('error', (err) => {
        if (err.kind === 'frame') {
          const code = err.frame.data?.code
          if (code === BIZ_CODE.WS_AUTH_FAILED && tokens.value?.refresh_token) {
            void refreshTokensSingleFlight(tokens.value.access_token).catch(() => {
              // Expiry redirect handled inside refreshTokensSingleFlight callback
            })
          } else if (code === BIZ_CODE.WS_SESSION_REVOKED) {
            handleSessionTerminated('当前会话已被下线或禁用，请重新登录')
          }
        } else if (err.kind === 'protocol' && tokens.value?.refresh_token) {
          void refreshTokensSingleFlight(tokens.value.access_token).catch(() => {
            // Expiry redirect handled inside refreshTokensSingleFlight callback
          })
        }
      }),

      client.on('system.notice', (frame) => {
        const event = frame.data?.event
        if (event === 'session.revoked') {
          handleSessionTerminated('您的会话已被注销，请重新登录')
        } else if (event === 'account.kicked') {
          handleSessionTerminated('您的账号已被强制下线，请重新登录')
        } else if (event === 'account.disabled') {
          handleSessionTerminated('您的账号已被禁用，请联系管理员')
        }
      }),
    ]
  }

  setAuthLifecycleCallbacks({
    onTokensRotated(nextTokens) {
      tokens.value = nextTokens
      activeAccessToken = nextTokens.access_token
      sessionId.value = nextTokens.session_id
      deviceId.value = nextTokens.device_id || getDeviceId()
    },
    onSessionExpired(code, message) {
      const displayMsg =
        code === BIZ_CODE.ACCOUNT_DISABLED
          ? '账号已被禁用，请重新登录'
          : message || '登录已过期，请重新登录'
      handleSessionTerminated(displayMsg)
    },
  })

  function connectWS() {
    if (!tokens.value?.access_token || !user.value) return
    activeAccessToken = tokens.value.access_token
    activeUserId = user.value.id
    const client = getWSClient()
    bindWSListeners(client)
    if (client.isConnected) {
      wsState.value = 'connected'
      return
    }
    if (wsState.value === 'disconnected') {
      wsState.value = 'connecting'
    }
    client.connect()
  }

  function disconnectWS() {
    unbindWSListeners()
    resetSharedWSClient()
    wsState.value = 'disconnected'
  }

  function setSession(nextUser: SelfUser, nextTokens: Tokens) {
    user.value = nextUser
    tokens.value = nextTokens
    activeAccessToken = nextTokens.access_token
    activeUserId = nextUser.id
    sessionId.value = nextTokens.session_id
    deviceId.value = nextTokens.device_id || getDeviceId()
    saveStoredTokens(nextTokens)
    initialized.value = true
  }

  function clearLocalSession() {
    disconnectWS()
    user.value = null
    tokens.value = null
    activeAccessToken = ''
    activeUserId = null
    sessionId.value = ''
    clearStoredTokens()
  }

  async function initAuth(): Promise<boolean> {
    if (initialized.value && user.value && tokens.value?.access_token) {
      return true
    }
    if (initPromise) {
      return initPromise
    }

    initPromise = (async () => {
      const stored = getStoredTokens()
      tokens.value = stored
      activeAccessToken = stored?.access_token ?? ''
      if (!stored?.access_token) {
        initialized.value = true
        return false
      }

      try {
        const res = await userApi.getMe()
        user.value = res.user
        activeUserId = res.user.id
        sessionId.value = res.session_id || stored.session_id
        deviceId.value = res.device_id || stored.device_id || getDeviceId()
        tokens.value = getStoredTokens() ?? stored
        activeAccessToken = tokens.value?.access_token ?? ''
        initialized.value = true
        connectWS()
        return true
      } catch {
        clearLocalSession()
        initialized.value = true
        return false
      } finally {
        initPromise = null
      }
    })()

    return initPromise
  }

  async function register(payload: RegisterPayload): Promise<SelfUser> {
    loading.value = true
    try {
      const res = await authApi.register(payload)
      return res.user
    } finally {
      loading.value = false
    }
  }

  async function login(username: string, password: string): Promise<SelfUser> {
    loading.value = true
    try {
      const devId = getDeviceId()
      const res = await authApi.login({
        username,
        password,
        device_id: devId,
        device_type: 'web',
      })
      setSession(res.user, res.tokens)
      connectWS()
      return res.user
    } finally {
      loading.value = false
    }
  }

  async function fetchMe(): Promise<SelfUser> {
    const res = await userApi.getMe()
    user.value = res.user
    activeUserId = res.user.id
    sessionId.value = res.session_id || sessionId.value
    deviceId.value = res.device_id || deviceId.value
    return res.user
  }

  async function updateProfile(payload: UpdateMePayload): Promise<SelfUser> {
    const res = await userApi.updateMe(payload)
    user.value = res.user
    return res.user
  }

  async function uploadAvatar(file: File): Promise<SelfUser> {
    const res = await userApi.uploadAvatar(file)
    user.value = res.user
    return res.user
  }

  async function changePassword(payload: ChangePasswordPayload): Promise<number> {
    const res = await authApi.changePassword(payload)
    clearLocalSession()
    return res.revoked_count
  }

  async function logout(scope: LogoutScope = 'current'): Promise<void> {
    try {
      if (tokens.value?.access_token) {
        await authApi.logout({ scope })
      }
    } catch {
      // Ensure client session is still cleared even if token already expired
    } finally {
      clearLocalSession()
    }
  }

  return {
    user,
    tokens,
    sessionId,
    deviceId,
    initialized,
    loading,
    wsState,
    wsClient,
    getAccessToken,
    isAuthenticated,
    isAdmin,
    initAuth,
    register,
    login,
    fetchMe,
    updateProfile,
    uploadAvatar,
    changePassword,
    logout,
    clearLocalSession,
    connectWS,
    disconnectWS,
  }
})
