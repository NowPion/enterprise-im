import { apiGet, apiPatch, apiPost } from './request'
import type { CursorPage } from '@/types/common'
import type {
  GetMeResponse,
  UpdateMePayload,
  UpdateMeResponse,
  UploadAvatarResponse,
  User,
  UserSearchParams,
} from '@/types/user'

export function getMe(): Promise<GetMeResponse> {
  return apiGet<GetMeResponse>('/me')
}

export function updateMe(payload: UpdateMePayload): Promise<UpdateMeResponse> {
  return apiPatch<UpdateMeResponse>('/me', payload)
}

export function uploadAvatar(file: File): Promise<UploadAvatarResponse> {
  const formData = new FormData()
  formData.append('file', file)
  return apiPost<UploadAvatarResponse>('/me/avatar', formData)
}

export function searchUsers(params: UserSearchParams): Promise<CursorPage<User>> {
  return apiGet<CursorPage<User>>('/users/search', { params })
}
