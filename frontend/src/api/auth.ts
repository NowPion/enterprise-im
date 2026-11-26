import { apiPost, apiPut } from './request'
import type {
  ChangePasswordPayload,
  ChangePasswordResponse,
  LoginPayload,
  LoginResponse,
  LogoutPayload,
  LogoutResponse,
  RefreshPayload,
  RefreshResponse,
  RegisterPayload,
  RegisterResponse,
} from '@/types/auth'

export function register(payload: RegisterPayload): Promise<RegisterResponse> {
  return apiPost<RegisterResponse>('/auth/register', payload, { skipAuth: true })
}

export function login(payload: LoginPayload): Promise<LoginResponse> {
  return apiPost<LoginResponse>('/auth/login', payload, { skipAuth: true })
}

export function refresh(payload: RefreshPayload): Promise<RefreshResponse> {
  return apiPost<RefreshResponse>('/auth/refresh', payload, { skipAuth: true })
}

export function logout(payload: LogoutPayload = { scope: 'current' }): Promise<LogoutResponse> {
  return apiPost<LogoutResponse>('/auth/logout', payload)
}

export function changePassword(payload: ChangePasswordPayload): Promise<ChangePasswordResponse> {
  return apiPut<ChangePasswordResponse>('/me/password', payload)
}
