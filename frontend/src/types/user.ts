import type { Asset } from './common'

export type UserRole = 'user' | 'admin'

export interface User {
  id: string
  username: string
  nickname: string
  avatar_url: string
  signature: string
  online: boolean
  last_online_at: string | null
}

export interface SelfUser extends User {
  role: UserRole
  status: number
}

export type UserSearchField = 'username' | 'nickname'
export type UserSearchMatch = 'exact' | 'prefix'

export interface UserSearchParams {
  q: string
  field?: UserSearchField
  match?: UserSearchMatch
  cursor?: string
  limit?: number
}

export interface GetMeResponse {
  user: SelfUser
  session_id: string
  device_id: string
}

export interface UpdateMePayload {
  nickname?: string
  signature?: string
}

export interface UpdateMeResponse {
  user: SelfUser
}

export interface UploadAvatarResponse {
  user: SelfUser
  asset: Asset
}
