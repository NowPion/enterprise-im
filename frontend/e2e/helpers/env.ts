export interface TestUserCredentials {
  username: string
  nickname: string
  password: string
}

export function getVitePort(): string {
  return process.env.IM_VITE_PORT || '5173'
}

export function getBaseUrl(): string {
  return process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${getVitePort()}`
}

export function getBackendOrigin(): string {
  return process.env.IM_BACKEND_ORIGIN || 'http://127.0.0.1:8080'
}

export function getAdminCredentials(): { username: string; password: string } {
  return {
    username: process.env.IM_ADMIN_USERNAME || 'admin',
    password: process.env.IM_ADMIN_PASSWORD || 'Admin@123456',
  }
}

export function uniqueSuffix(): string {
  return `${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 7)}`
}

export function makeTestUser(prefix = 'e2e'): TestUserCredentials {
  const suffix = uniqueSuffix()
  return {
    username: `${prefix}_${suffix}`,
    nickname: `${prefix.toUpperCase()}_${suffix}`,
    password: 'PassWord@123',
  }
}
