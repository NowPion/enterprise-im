<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ArrowLeft, ChatDotRound, Plus, Refresh, UserFilled } from '@element-plus/icons-vue'
import { extractErrorMessage } from '@/api/group'
import AuthorizedAvatar from '@/components/chat/AuthorizedAvatar.vue'
import GroupAuditPanel from '@/components/groups/GroupAuditPanel.vue'
import GroupCreateDialog from '@/components/groups/GroupCreateDialog.vue'
import GroupInviteDialog from '@/components/groups/GroupInviteDialog.vue'
import GroupMemberList from '@/components/groups/GroupMemberList.vue'
import GroupProfilePanel from '@/components/groups/GroupProfilePanel.vue'
import { useGroupPermissions } from '@/composables/groups/useGroupPermissions'
import { getWSClient } from '@/stores/auth'
import { useGroupStore } from '@/stores/group'
import type { AddGroupMembersResult, CreateGroupResult, GroupStatus } from '@/types/group'

const route = useRoute()
const router = useRouter()
const groupStore = useGroupStore()

const activeTab = ref<'members' | 'profile' | 'audit'>('members')
const createVisible = ref(false)
const inviteVisible = ref(false)
const quickChatBanner = ref<{ conversationId: string; title: string } | null>(null)

const activeGroup = computed(() => groupStore.activeGroup)
const activeRole = computed(() => groupStore.activeRole)
const { canViewAudit, canInviteMembers } = useGroupPermissions(activeGroup, activeRole)

const routeGroupId = computed(() => {
  const raw = route.params.groupId
  return typeof raw === 'string' && raw.trim() ? raw.trim() : ''
})

const existingMemberIds = computed(() => groupStore.members.map((m) => m.user_id))

function roleLabel(role: string): string {
  if (role === 'owner') return '群主'
  if (role === 'admin') return '管理员'
  return '成员'
}

async function loadGroupList(status: GroupStatus | '' = groupStore.statusFilter) {
  try {
    await groupStore.fetchGroups({ reset: true, status })
  } catch (err) {
    ElMessage.error(extractErrorMessage(err, '加载群列表失败'))
  }
}

async function openGroup(groupId: string) {
  activeTab.value = 'members'
  if (routeGroupId.value !== groupId) {
    await router.push(`/groups/${encodeURIComponent(groupId)}`)
  } else {
    try {
      await groupStore.selectGroup(groupId)
    } catch (err) {
      ElMessage.error(extractErrorMessage(err, '加载群详情失败'))
    }
  }
}

function backToList() {
  void router.push('/groups')
}

function goToChat(conversationId: string) {
  void router.push(`/chat/${encodeURIComponent(conversationId)}`)
}

function handleCreated(result: CreateGroupResult) {
  quickChatBanner.value = {
    conversationId: result.conversation.id,
    title: `群聊「${result.group.name}」已创建`,
  }
  void router.push(`/groups/${encodeURIComponent(result.group.id)}`)
}

function handleInvited(result: AddGroupMembersResult) {
  quickChatBanner.value = {
    conversationId: result.conversation_id,
    title: `已邀请 ${result.members.length} 位新成员加入群聊`,
  }
}

function handleLeftGroup() {
  quickChatBanner.value = null
  void router.push('/groups')
}

watch(
  routeGroupId,
  async (gid) => {
    if (!gid) {
      groupStore.activeGroupId = null
      groupStore.activeGroupDetail = null
      return
    }
    try {
      await groupStore.selectGroup(gid)
      if (!canViewAudit.value && activeTab.value === 'audit') {
        activeTab.value = 'members'
      }
    } catch (err) {
      ElMessage.error(extractErrorMessage(err, '无法打开该群详情'))
    }
  },
  { immediate: true },
)

let unbindGroupWs: (() => void) | null = null

onMounted(() => {
  unbindGroupWs?.()
  unbindGroupWs = groupStore.bindWsClient(getWSClient())
  void loadGroupList()
})

onUnmounted(() => {
  unbindGroupWs?.()
  unbindGroupWs = null
})
</script>

<template>
  <div class="groups-page">
    <aside class="group-sidebar" :class="{ 'mobile-hidden': Boolean(routeGroupId) }">
      <div class="sidebar-header">
        <h2 class="sidebar-title">群聊管理</h2>
        <div class="sidebar-header-actions">
          <el-button
            size="small"
            :icon="Refresh"
            circle
            title="刷新列表"
            :loading="groupStore.groupsLoading"
            @click="loadGroupList()"
          />
          <el-button type="primary" size="small" :icon="Plus" @click="createVisible = true">
            新建群聊
          </el-button>
        </div>
      </div>

      <div class="status-filter">
        <el-radio-group
          :model-value="groupStore.statusFilter"
          size="small"
          @update:model-value="loadGroupList($event as GroupStatus | '')"
        >
          <el-radio-button value="">全部</el-radio-button>
          <el-radio-button value="active">正常</el-radio-button>
          <el-radio-button value="disbanded">已解散</el-radio-button>
        </el-radio-group>
      </div>

      <div v-loading="groupStore.groupsLoading" class="group-list">
        <div
          v-for="item in groupStore.groups"
          :key="item.id"
          class="group-list-item"
          :class="{ 'is-active': item.id === routeGroupId }"
          @click="openGroup(item.id)"
        >
          <AuthorizedAvatar :src="item.avatar_url" :name="item.name" :size="40" />
          <div class="item-content">
            <div class="item-top">
              <span class="item-name">{{ item.name }}</span>
              <span class="item-role">{{ roleLabel(item.role) }}</span>
            </div>
            <div class="item-bottom">
              <span>{{ item.member_count }} 人</span>
              <span v-if="item.status === 'disbanded'" class="disbanded-tag">已解散</span>
            </div>
          </div>
        </div>

        <div v-if="!groupStore.groupsLoading && groupStore.groups.length === 0" class="empty-list">
          暂无加入的群聊，可点击右上角“新建群聊”
        </div>

        <div v-if="groupStore.groupsHasMore" class="load-more-wrap">
          <el-button
            size="small"
            link
            :loading="groupStore.groupsLoading"
            @click="groupStore.fetchGroups({ reset: false })"
          >
            加载更多群聊
          </el-button>
        </div>
      </div>
    </aside>

    <main class="group-workspace" :class="{ 'mobile-hidden': !routeGroupId }">
      <div v-if="quickChatBanner" class="quick-chat-banner">
        <span>{{ quickChatBanner.title }}</span>
        <div class="banner-actions">
          <el-button
            type="primary"
            size="small"
            :icon="ChatDotRound"
            @click="goToChat(quickChatBanner.conversationId)"
          >
            进入聊天
          </el-button>
          <el-button size="small" link @click="quickChatBanner = null">关闭</el-button>
        </div>
      </div>

      <div v-if="!routeGroupId" class="workspace-placeholder">
        <el-icon :size="40" class="placeholder-icon"><UserFilled /></el-icon>
        <p class="placeholder-title">选择左侧群聊查看或管理成员与群设置</p>
        <el-button type="primary" :icon="Plus" @click="createVisible = true">创建新群聊</el-button>
      </div>

      <div v-else v-loading="groupStore.detailLoading" class="workspace-detail">
        <template v-if="groupStore.activeGroupDetail">
          <header class="detail-header">
            <div class="detail-title-group">
              <el-button
                class="mobile-back-btn"
                size="small"
                :icon="ArrowLeft"
                @click="backToList"
              />
              <div>
                <div class="detail-name-row">
                  <h1 class="detail-title">{{ groupStore.activeGroupDetail.group.name }}</h1>
                  <span
                    class="status-pill"
                    :class="
                      groupStore.activeGroupDetail.group.status === 'disbanded'
                        ? 'pill-disbanded'
                        : 'pill-active'
                    "
                  >
                    {{ groupStore.activeGroupDetail.group.status === 'disbanded' ? '已解散' : '活跃' }}
                  </span>
                </div>
                <div class="detail-sub">
                  群 ID：{{ groupStore.activeGroupDetail.group.id }} · 成员
                  {{ groupStore.activeGroupDetail.member_count }} /
                  {{ groupStore.activeGroupDetail.group.max_members }} · 我的角色：{{
                    roleLabel(groupStore.activeGroupDetail.role)
                  }}
                </div>
              </div>
            </div>

            <div class="detail-header-actions">
              <el-button
                v-if="canInviteMembers"
                :icon="Plus"
                @click="inviteVisible = true"
              >
                邀请成员
              </el-button>
              <el-button
                type="primary"
                :icon="ChatDotRound"
                @click="goToChat(groupStore.activeGroupDetail.conversation.id)"
              >
                进入聊天
              </el-button>
            </div>
          </header>

          <el-tabs v-model="activeTab" class="workspace-tabs">
            <el-tab-pane label="成员管理" name="members">
              <GroupMemberList
                :group="groupStore.activeGroupDetail.group"
                :my-role="groupStore.activeGroupDetail.role"
                :members="groupStore.members"
                :loading="groupStore.membersLoading"
                :has-more="groupStore.membersHasMore"
                @open-invite="inviteVisible = true"
              />
            </el-tab-pane>

            <el-tab-pane label="群资料与设置" name="profile">
              <GroupProfilePanel
                :group="groupStore.activeGroupDetail.group"
                :my-role="groupStore.activeGroupDetail.role"
                :members="groupStore.members"
                :member-count="groupStore.activeGroupDetail.member_count"
                @enter-chat="goToChat"
                @left-group="handleLeftGroup"
              />
            </el-tab-pane>

            <el-tab-pane v-if="canViewAudit" label="管理审计" name="audit">
              <GroupAuditPanel
                :group-id="groupStore.activeGroupDetail.group.id"
                :logs="groupStore.auditLogs"
                :loading="groupStore.auditLoading"
                :has-more="groupStore.auditHasMore"
              />
            </el-tab-pane>
          </el-tabs>
        </template>
      </div>
    </main>

    <GroupCreateDialog v-model="createVisible" @created="handleCreated" />
    <GroupInviteDialog
      v-if="groupStore.activeGroupDetail"
      v-model="inviteVisible"
      :group-id="groupStore.activeGroupDetail.group.id"
      :existing-member-ids="existingMemberIds"
      @invited="handleInvited"
    />
  </div>
</template>

<style scoped>
.groups-page {
  display: flex;
  height: 100%;
  min-height: 0;
  background-color: #f5f6f8;
  color: #111827;
}

.group-sidebar {
  width: 300px;
  flex-shrink: 0;
  background-color: #ffffff;
  border-right: 1px solid #e5e7eb;
  display: flex;
  flex-direction: column;
}

.sidebar-header {
  padding: 16px;
  border-bottom: 1px solid #eceef2;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.sidebar-title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: #111827;
}

.sidebar-header-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.status-filter {
  padding: 10px 16px;
  border-bottom: 1px solid #eceef2;
}

.group-list {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}

.group-list-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px;
  border-radius: 6px;
  cursor: pointer;
  margin-bottom: 4px;
}

.group-list-item:hover {
  background-color: #f5f6f8;
}

.group-list-item.is-active {
  background-color: #eceef2;
}

.item-content {
  flex: 1;
  min-width: 0;
}

.item-top {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 6px;
}

.item-name {
  font-size: 13px;
  font-weight: 500;
  color: #111827;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.item-role {
  font-size: 11px;
  color: #4b5563;
  background-color: #f5f6f8;
  padding: 1px 6px;
  border-radius: 3px;
  flex-shrink: 0;
}

.item-bottom {
  margin-top: 4px;
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  color: #6b7280;
}

.disbanded-tag {
  color: #dc2626;
}

.empty-list {
  padding: 32px 16px;
  text-align: center;
  font-size: 13px;
  color: #6b7280;
}

.load-more-wrap {
  display: flex;
  justify-content: center;
  padding: 8px 0;
}

.group-workspace {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  padding: 20px 24px;
  overflow-y: auto;
}

.quick-chat-banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  margin-bottom: 14px;
  background-color: #ffffff;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  font-size: 13px;
  color: #111827;
}

.banner-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.workspace-placeholder {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  color: #6b7280;
}

.placeholder-icon {
  color: #9ca3af;
}

.placeholder-title {
  margin: 0;
  font-size: 14px;
}

.workspace-detail {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.detail-header {
  background-color: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 16px 20px;
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}

.detail-title-group {
  display: flex;
  align-items: center;
  gap: 10px;
}

.mobile-back-btn {
  display: none;
}

.detail-name-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.detail-title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: #111827;
}

.status-pill {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 4px;
  font-weight: 500;
}

.pill-active {
  background-color: #f0fdf4;
  color: #16a34a;
  border: 1px solid #bbf7d0;
}

.pill-disbanded {
  background-color: #fef2f2;
  color: #dc2626;
  border: 1px solid #fecaca;
}

.detail-sub {
  margin-top: 4px;
  font-size: 12px;
  color: #6b7280;
}

.detail-header-actions {
  display: flex;
  gap: 8px;
}

@media (max-width: 768px) {
  .group-sidebar {
    width: 100%;
  }

  .mobile-hidden {
    display: none;
  }

  .mobile-back-btn {
    display: inline-flex;
  }

  .group-workspace {
    padding: 12px;
  }
}
</style>
