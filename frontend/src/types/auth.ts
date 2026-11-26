import type { SelfUser } from './user'

export type DeviceType = 'web' | 'desktop' | 'mobile'

export interface Tokens {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
  refresh_expires_in: number
  session_id: string
  device_id: string
}

export interface RegisterPayload {
  username: string
  password: string
  nickname: string
}

export interface RegisterResponse {
  user: SelfUser
}

export interface LoginPayload {
  username: string
  password: string
  device_id: string
  device_type: DeviceType
}

export interface LoginResponse {
  user: SelfUser
  tokens: Tokens
}

export interface RefreshPayload {
  refresh_token: string
  device_id: string
}

export interface RefreshResponse {
  tokens: Tokens
}

export type LogoutScope = 'current' | 'all'

export interface LogoutPayload {
  scope?: LogoutScope
}

export interface LogoutResponse {
  scope: LogoutScope
  revoked_count: number
}

export interface ChangePasswordPayload {
  old_password: string
  new_password: string
}

export interface ChangePasswordResponse {
  revoked_count: number
}
