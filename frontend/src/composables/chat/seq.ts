export function parseDecimalSeq(seq: string | number | null | undefined): bigint {
  if (typeof seq === 'number') {
    if (Number.isSafeInteger(seq) && seq >= 0) {
      return BigInt(seq)
    }
    return 0n
  }
  if (typeof seq !== 'string') {
    return 0n
  }
  const trimmed = seq.trim()
  if (!/^\d+$/.test(trimmed)) {
    return 0n
  }
  return BigInt(trimmed)
}

export function compareSeq(
  a: string | number | null | undefined,
  b: string | number | null | undefined,
): -1 | 0 | 1 {
  const va = parseDecimalSeq(a)
  const vb = parseDecimalSeq(b)
  if (va < vb) return -1
  if (va > vb) return 1
  return 0
}

export function maxSeq(
  a: string | null | undefined,
  b: string | null | undefined,
): string {
  const va = parseDecimalSeq(a)
  const vb = parseDecimalSeq(b)
  return (va >= vb ? va : vb).toString()
}

export function minSeq(
  a: string | null | undefined,
  b: string | null | undefined,
): string {
  const va = parseDecimalSeq(a)
  const vb = parseDecimalSeq(b)
  return (va <= vb ? va : vb).toString()
}

export function isPositiveSeq(seq: string | null | undefined): boolean {
  return parseDecimalSeq(seq) > 0n
}

export function generateClientMsgId(): string {
  if (typeof globalThis.crypto?.randomUUID === 'function') {
    return globalThis.crypto.randomUUID()
  }
  if (typeof globalThis.crypto?.getRandomValues === 'function') {
    const bytes = new Uint8Array(16)
    globalThis.crypto.getRandomValues(bytes)
    bytes[6] = (bytes[6] & 0x0f) | 0x40
    bytes[8] = (bytes[8] & 0x3f) | 0x80
    const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0'))
    return `${hex.slice(0, 4).join('')}-${hex.slice(4, 6).join('')}-${hex.slice(6, 8).join('')}-${hex.slice(8, 10).join('')}-${hex.slice(10, 16).join('')}`
  }
  const rnd = Math.random().toString(16).slice(2).padEnd(12, '0').slice(0, 12)
  return `${Date.now().toString(16)}-4000-8000-${rnd}`
}

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

export function formatConversationTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ''
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const startOfYesterday = startOfToday - 24 * 60 * 60 * 1000
  const ts = date.getTime()
  if (ts >= startOfToday) {
    return `${pad2(date.getHours())}:${pad2(date.getMinutes())}`
  }
  if (ts >= startOfYesterday) {
    return '昨天'
  }
  if (date.getFullYear() === now.getFullYear()) {
    return `${pad2(date.getMonth() + 1)}/${pad2(date.getDate())}`
  }
  return `${date.getFullYear()}/${pad2(date.getMonth() + 1)}/${pad2(date.getDate())}`
}

export function formatMessageTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ''
  return `${pad2(date.getHours())}:${pad2(date.getMinutes())}`
}

export function formatDividerTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ''
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const startOfYesterday = startOfToday - 24 * 60 * 60 * 1000
  const timePart = `${pad2(date.getHours())}:${pad2(date.getMinutes())}`
  if (date.getTime() >= startOfToday) {
    return timePart
  }
  if (date.getTime() >= startOfYesterday) {
    return `昨天 ${timePart}`
  }
  if (date.getFullYear() === now.getFullYear()) {
    return `${date.getMonth() + 1}月${date.getDate()}日 ${timePart}`
  }
  return `${date.getFullYear()}年${date.getMonth() + 1}月${date.getDate()}日 ${timePart}`
}

export function formatFileSize(bytes: number | undefined): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let val = bytes
  let idx = 0
  while (val >= 1024 && idx < units.length - 1) {
    val /= 1024
    idx += 1
  }
  return idx === 0 ? `${val} ${units[idx]}` : `${val.toFixed(1)} ${units[idx]}`
}

export function formatDurationMs(ms: number | undefined): string {
  if (!ms || ms <= 0) return '1"'
  const totalSec = Math.max(1, Math.round(ms / 1000))
  const min = Math.floor(totalSec / 60)
  const sec = totalSec % 60
  if (min > 0) {
    return `${min}'${pad2(sec)}"`
  }
  return `${sec}"`
}
