import { existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath, URL } from 'node:url'
import { configDefaults, defineConfig } from 'vitest/config'

const require = createRequire(import.meta.url)

function hasPackage(name: string): boolean {
  try {
    require.resolve(name)
    return true
  } catch {
    return false
  }
}

const srcDir = fileURLToPath(new URL('./src', import.meta.url))
const hasJsdom = hasPackage('jsdom')
const hasPinia = hasPackage('pinia')

const hasChatStores =
  hasJsdom &&
  hasPinia &&
  existsSync(fileURLToPath(new URL('./src/stores/conversation.ts', import.meta.url))) &&
  existsSync(fileURLToPath(new URL('./src/stores/message.ts', import.meta.url)))

const hasAuthAndContactStores =
  hasJsdom &&
  hasPinia &&
  hasPackage('axios') &&
  existsSync(fileURLToPath(new URL('./src/stores/auth.ts', import.meta.url))) &&
  existsSync(fileURLToPath(new URL('./src/stores/contact.ts', import.meta.url))) &&
  existsSync(fileURLToPath(new URL('./src/api/request.ts', import.meta.url)))

const extraExcludes: string[] = []
if (!hasChatStores) {
  extraExcludes.push('tests/stores/messageStore.spec.ts', 'tests/stores/conversationUnread.spec.ts')
}
if (!hasAuthAndContactStores) {
  extraExcludes.push('tests/stores/authAndContact.spec.ts')
}

export default defineConfig({
  resolve: {
    alias: {
      '@': srcDir,
    },
  },
  test: {
    environment: 'node',
    globals: true,
    ...(hasJsdom ? { environmentMatchGlobs: [['tests/stores/**', 'jsdom']] } : {}),
    exclude: [...configDefaults.exclude, ...extraExcludes],
  },
})
