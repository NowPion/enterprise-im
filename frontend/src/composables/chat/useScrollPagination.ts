import { nextTick, ref, type Ref } from 'vue'

export interface UseScrollPaginationOptions {
  hasMoreOlder: () => boolean
  isLoadingOlder: () => boolean
  onLoadOlder: () => Promise<void>
  onReachBottom?: () => void
  nearBottomThresholdPx?: number
  topLoadThresholdPx?: number
}

export function useScrollPagination(
  containerRef: Ref<HTMLElement | null>,
  options: UseScrollPaginationOptions,
) {
  const isNearBottom = ref(true)
  const unreadBelowCount = ref(0)

  const nearThreshold = options.nearBottomThresholdPx ?? 120
  const topThreshold = options.topLoadThresholdPx ?? 64

  function measureNearBottom(el: HTMLElement): boolean {
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight
    return distance <= nearThreshold
  }

  async function scrollToBottom(smooth = false): Promise<void> {
    await nextTick()
    const el = containerRef.value
    if (!el) return
    el.scrollTo({
      top: el.scrollHeight,
      behavior: smooth ? 'smooth' : 'auto',
    })
    isNearBottom.value = true
    unreadBelowCount.value = 0
  }

  async function preserveScrollOnPrepend(run: () => Promise<void>): Promise<void> {
    const el = containerRef.value
    if (!el) {
      await run()
      return
    }
    const prevScrollHeight = el.scrollHeight
    const prevScrollTop = el.scrollTop
    await run()
    await nextTick()
    const delta = el.scrollHeight - prevScrollHeight
    el.scrollTop = Math.max(0, prevScrollTop + delta)
  }

  async function handleScroll(): Promise<void> {
    const el = containerRef.value
    if (!el) return

    const near = measureNearBottom(el)
    isNearBottom.value = near
    if (near) {
      if (unreadBelowCount.value > 0) {
        unreadBelowCount.value = 0
      }
      options.onReachBottom?.()
    }

    if (el.scrollTop <= topThreshold && options.hasMoreOlder() && !options.isLoadingOlder()) {
      await preserveScrollOnPrepend(() => options.onLoadOlder())
    }
  }

  function notifyIncomingBottomOrHold(isOwnMessage: boolean): void {
    if (isOwnMessage || isNearBottom.value) {
      void scrollToBottom(false)
      return
    }
    unreadBelowCount.value += 1
  }

  function resetScrollState(): void {
    isNearBottom.value = true
    unreadBelowCount.value = 0
  }

  return {
    isNearBottom,
    unreadBelowCount,
    scrollToBottom,
    preserveScrollOnPrepend,
    handleScroll,
    notifyIncomingBottomOrHold,
    resetScrollState,
  }
}
