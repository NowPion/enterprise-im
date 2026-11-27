<script setup lang="ts">
import { onMounted, ref } from 'vue'
import AppNavRail from '@/components/common/AppNavRail.vue'
import ProfileSettingsDialog from '@/components/common/ProfileSettingsDialog.vue'
import { useChatRealtime } from '@/composables/chat/useChatRealtime'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import { useConversationStore } from '@/stores/conversation'

const authStore = useAuthStore()
const contactStore = useContactStore()
const conversationStore = useConversationStore()
const { ensureRealtimeConnected, reconcileOnAuthenticatedOpen } = useChatRealtime()
const profileDialogOpen = ref(false)

onMounted(() => {
  if (authStore.isAuthenticated) {
    authStore.connectWS()
    ensureRealtimeConnected()
    contactStore.bindWSClient()
    if (conversationStore.conversations.length === 0) {
      void reconcileOnAuthenticatedOpen()
    }
    void Promise.all([
      contactStore.fetchFriends(true),
      contactStore.fetchRequests('received', 'pending', true),
    ]).catch(() => {
      // Errors surfaced when user opens contacts view
    })
  }
})
</script>

<template>
  <div class="app-layout">
    <AppNavRail @open-profile="profileDialogOpen = true" />
    <main class="app-layout__workspace">
      <slot>
        <router-view />
      </slot>
    </main>
    <ProfileSettingsDialog v-model="profileDialogOpen" />
  </div>
</template>

<style scoped>
.app-layout {
  display: flex;
  width: 100vw;
  height: 100vh;
  overflow: hidden;
  background-color: var(--im-bg-grouped);
}

.app-layout__workspace {
  flex: 1;
  min-width: 0;
  min-height: 0;
  height: 100%;
  display: flex;
  background-color: var(--im-bg-surface);
}

@media (max-width: 767px) {
  .app-layout {
    flex-direction: column-reverse;
  }

  .app-layout__workspace {
    height: calc(100vh - 58px);
  }
}
</style>
