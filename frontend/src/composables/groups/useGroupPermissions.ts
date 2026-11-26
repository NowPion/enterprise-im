import { computed, type Ref } from 'vue'
import { useAuthStore } from '@/stores/auth'
import type { GroupInfo, GroupMember, GroupRole } from '@/types/group'

export function useGroupPermissions(
  group: Ref<GroupInfo | null | undefined>,
  myRole: Ref<GroupRole | null | undefined>,
) {
  const authStore = useAuthStore()

  const currentUserId = computed(() => {
    const user = (authStore as { user?: { id?: string } | null }).user
    return user?.id ? String(user.id) : ''
  })

  const isDisbanded = computed(() => group.value?.status === 'disbanded')
  const isActive = computed(() => group.value?.status === 'active')
  const isOwner = computed(() => myRole.value === 'owner')
  const isAdmin = computed(() => myRole.value === 'admin')
  const isOwnerOrAdmin = computed(() => isOwner.value || isAdmin.value)

  const canEditGroupInfo = computed(() => isActive.value && isOwnerOrAdmin.value)
  const canInviteMembers = computed(() => isActive.value && isOwnerOrAdmin.value)
  const canViewAudit = computed(() => isOwnerOrAdmin.value)
  const canDisbandGroup = computed(() => isActive.value && isOwner.value)
  const canTransferOwnership = computed(() => isActive.value && isOwner.value)
  const canManageRoles = computed(() => isActive.value && isOwner.value)
  const canLeaveGroup = computed(() => isActive.value && !isOwner.value && Boolean(myRole.value))
  const canEditMyNickname = computed(() => isActive.value && Boolean(myRole.value))

  function canRemoveMember(target: GroupMember): boolean {
    if (!isActive.value) return false
    if (target.user_id === currentUserId.value) return false
    if (target.role === 'owner') return false
    if (isOwner.value) return true
    if (isAdmin.value) return target.role === 'member'
    return false
  }

  function canChangeMemberRole(target: GroupMember): boolean {
    if (!canManageRoles.value) return false
    return target.role !== 'owner' && target.user_id !== currentUserId.value
  }

  return {
    currentUserId,
    isDisbanded,
    isActive,
    isOwner,
    isAdmin,
    isOwnerOrAdmin,
    canEditGroupInfo,
    canInviteMembers,
    canViewAudit,
    canDisbandGroup,
    canTransferOwnership,
    canManageRoles,
    canLeaveGroup,
    canEditMyNickname,
    canRemoveMember,
    canChangeMemberRole,
  }
}
