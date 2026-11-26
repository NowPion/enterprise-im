<script setup lang="ts">
import { onMounted, ref } from 'vue'
import AppNavRail from '@/components/common/AppNavRail.vue'
import ProfileSettingsDialog from '@/components/common/ProfileSettingsDialog.vue'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'

const authStore = useAuthStore()
const contactStore = useContactStore()
const profileDialogOpen = ref(false)

onMounted(() => {
  if (authStore.isAuthenticated) {
    authStore.connectWS()
    contactStore.bindRealtimeListeners()
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
  background-color: var(--im-bg-page);
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
    height: calc(100vh - 56px);
  }
}
</style>
