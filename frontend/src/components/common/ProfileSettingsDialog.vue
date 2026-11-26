<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { Lock, Upload, User as UserIcon } from '@element-plus/icons-vue'
import AppAvatar from '@/components/common/AppAvatar.vue'
import { useAuthStore } from '@/stores/auth'
import { useContactStore } from '@/stores/contact'
import { isApiError } from '@/types/common'

const modelValue = defineModel<boolean>({ default: false })

const router = useRouter()
const authStore = useAuthStore()
const contactStore = useContactStore()

const activeTab = ref<'profile' | 'security'>('profile')
const profileSaving = ref(false)
const avatarUploading = ref(false)
const passwordSaving = ref(false)
const fileInputRef = ref<HTMLInputElement | null>(null)
const profileFormRef = ref<FormInstance>()
const passwordFormRef = ref<FormInstance>()

const profileForm = reactive({
  nickname: '',
  signature: '',
})

const passwordForm = reactive({
  oldPassword: '',
  newPassword: '',
  confirmPassword: '',
})

watch(modelValue, (open) => {
  if (open && authStore.user) {
    activeTab.value = 'profile'
    profileForm.nickname = authStore.user.nickname
    profileForm.signature = authStore.user.signature
    passwordForm.oldPassword = ''
    passwordForm.newPassword = ''
    passwordForm.confirmPassword = ''
  }
})

const profileRules: FormRules = {
  nickname: [
    { required: true, message: '请输入昵称', trigger: 'blur' },
    { min: 1, max: 64, message: '昵称长度为 1 到 64 个字符', trigger: 'blur' },
  ],
  signature: [
    { max: 255, message: '个性签名最多 255 个字符', trigger: 'blur' },
  ],
}

function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

const passwordRules: FormRules = {
  oldPassword: [{ required: true, message: '请输入当前密码', trigger: 'blur' }],
  newPassword: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    {
      validator: (_rule, value: string, callback) => {
        const bytes = utf8ByteLength(value || '')
        if (bytes < 8 || bytes > 72) {
          callback(new Error('密码需为 8 到 72 字节'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
  confirmPassword: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_rule, value: string, callback) => {
        if (value !== passwordForm.newPassword) {
          callback(new Error('两次输入的新密码不一致'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}

function triggerAvatarSelect() {
  fileInputRef.value?.click()
}

async function handleAvatarChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return

  avatarUploading.value = true
  try {
    await authStore.uploadAvatar(file)
    ElMessage.success('头像已更新')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '头像上传失败')
  } finally {
    avatarUploading.value = false
  }
}

async function handleSaveProfile() {
  const valid = await profileFormRef.value?.validate().catch(() => false)
  if (!valid) return

  profileSaving.value = true
  try {
    await authStore.updateProfile({
      nickname: profileForm.nickname.trim(),
      signature: profileForm.signature,
    })
    ElMessage.success('个人资料已保存')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '保存失败')
  } finally {
    profileSaving.value = false
  }
}

async function handleChangePassword() {
  const valid = await passwordFormRef.value?.validate().catch(() => false)
  if (!valid) return

  passwordSaving.value = true
  try {
    await authStore.changePassword({
      old_password: passwordForm.oldPassword,
      new_password: passwordForm.newPassword,
    })
    contactStore.reset()
    modelValue.value = false
    ElMessage.success('密码修改成功，请使用新密码重新登录')
    await router.replace('/login')
  } catch (err) {
    ElMessage.error(isApiError(err) ? err.message : '修改密码失败')
  } finally {
    passwordSaving.value = false
  }
}
</script>

<template>
  <el-dialog
    v-model="modelValue"
    title="账号设置"
    width="480px"
    :close-on-click-modal="false"
    destroy-on-close
  >
    <el-tabs v-model="activeTab">
      <el-tab-pane name="profile">
        <template #label>
          <span class="tab-label">
            <el-icon><UserIcon /></el-icon>
            个人资料
          </span>
        </template>

        <div class="profile-head">
          <AppAvatar
            :src="authStore.user?.avatar_url"
            :name="authStore.user?.nickname || authStore.user?.username"
            :size="64"
            :show-online="true"
            :online="authStore.wsState === 'connected'"
            rounded="md"
          />
          <div class="profile-head__meta">
            <div class="profile-head__name-row">
              <span class="profile-head__username">@{{ authStore.user?.username }}</span>
              <el-tag size="small" type="info" effect="plain">
                {{ authStore.isAdmin ? '平台管理员' : '成员' }}
              </el-tag>
            </div>
            <p class="profile-head__sub">
              实时通道：{{ authStore.wsState === 'connected' ? '已连接' : '未连接' }}
            </p>
            <input
              ref="fileInputRef"
              type="file"
              accept="image/jpeg,image/png,image/webp,image/gif"
              class="hidden-file-input"
              @change="handleAvatarChange"
            />
            <el-button
              size="small"
              :icon="Upload"
              :loading="avatarUploading"
              @click="triggerAvatarSelect"
            >
              更换头像
            </el-button>
          </div>
        </div>

        <el-form
          ref="profileFormRef"
          :model="profileForm"
          :rules="profileRules"
          label-position="top"
        >
          <el-form-item label="昵称" prop="nickname">
            <el-input v-model="profileForm.nickname" maxlength="64" show-word-limit />
          </el-form-item>
          <el-form-item label="个性签名" prop="signature">
            <el-input
              v-model="profileForm.signature"
              type="textarea"
              :rows="3"
              maxlength="255"
              show-word-limit
              placeholder="输入个性签名，留空保存可清除"
            />
          </el-form-item>
          <div class="form-actions">
            <el-button type="primary" :loading="profileSaving" @click="handleSaveProfile">
              保存资料
            </el-button>
          </div>
        </el-form>
      </el-tab-pane>

      <el-tab-pane name="security">
        <template #label>
          <span class="tab-label">
            <el-icon><Lock /></el-icon>
            修改密码
          </span>
        </template>

        <el-alert
          type="info"
          :closable="false"
          show-icon
          title="修改密码后将自动撤销所有设备的活跃会话，需要重新登录。"
          class="security-alert"
        />

        <el-form
          ref="passwordFormRef"
          :model="passwordForm"
          :rules="passwordRules"
          label-position="top"
        >
          <el-form-item label="当前密码" prop="oldPassword">
            <el-input
              v-model="passwordForm.oldPassword"
              type="password"
              show-password
              autocomplete="current-password"
            />
          </el-form-item>
          <el-form-item label="新密码" prop="newPassword">
            <el-input
              v-model="passwordForm.newPassword"
              type="password"
              show-password
              autocomplete="new-password"
              placeholder="8–72 字节"
            />
          </el-form-item>
          <el-form-item label="确认新密码" prop="confirmPassword">
            <el-input
              v-model="passwordForm.confirmPassword"
              type="password"
              show-password
              autocomplete="new-password"
            />
          </el-form-item>
          <div class="form-actions">
            <el-button type="primary" :loading="passwordSaving" @click="handleChangePassword">
              确认修改密码
            </el-button>
          </div>
        </el-form>
      </el-tab-pane>
    </el-tabs>
  </el-dialog>
</template>

<style scoped>
.tab-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.profile-head {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 12px 0 20px;
  margin-bottom: 16px;
  border-bottom: 1px solid var(--im-border-light);
}

.profile-head__meta {
  display: flex;
  flex-direction: column;
  gap: 6px;
  align-items: flex-start;
}

.profile-head__name-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.profile-head__username {
  font-weight: 600;
  color: var(--im-text-primary);
}

.profile-head__sub {
  margin: 0;
  font-size: 12px;
  color: var(--im-text-muted);
}

.hidden-file-input {
  display: none;
}

.security-alert {
  margin-bottom: 16px;
}

.form-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}
</style>
