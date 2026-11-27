<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChatDotSquare, Lock, User } from '@element-plus/icons-vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import { BIZ_CODE, isApiError } from '@/types/common'

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()

const formRef = ref<FormInstance>()
const submitting = ref(false)
const errorMessage = ref('')

const form = reactive({
  username: typeof route.query.username === 'string' ? route.query.username : '',
  password: '',
})

const rules: FormRules = {
  username: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 1, max: 64, message: '用户名长度为 1 到 64 个字符', trigger: 'blur' },
  ],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

function mapLoginError(err: unknown): string {
  if (!isApiError(err)) return '登录失败，请检查网络连接'
  if (err.code === BIZ_CODE.INVALID_CREDENTIALS) return '用户名或密码错误'
  if (err.code === BIZ_CODE.ACCOUNT_DISABLED) return '该账号已被禁用，请联系管理员'
  if (err.code === BIZ_CODE.SESSION_LIMIT_REACHED) return '当前账号活跃设备数已达上限'
  return err.message || '登录失败'
}

async function handleSubmit() {
  errorMessage.value = ''
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    await authStore.login(form.username.trim(), form.password)
    ElMessage.success('登录成功')
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/chat'
    const safeRedirect = redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/chat'
    await router.replace(safeRedirect)
  } catch (err) {
    errorMessage.value = mapLoginError(err)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="auth-page">
    <div class="auth-card">
      <div class="auth-card__brand">
        <div class="auth-card__logo">
          <el-icon :size="26"><ChatDotSquare /></el-icon>
        </div>
        <div>
          <h1 class="auth-card__title">企业即时通讯</h1>
          <p class="auth-card__subtitle">请登录您的企业账号</p>
        </div>
      </div>

      <el-alert
        v-if="errorMessage"
        type="error"
        :title="errorMessage"
        :closable="false"
        show-icon
        class="auth-card__alert"
      />

      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        label-position="top"
        @submit.prevent="handleSubmit"
      >
        <el-form-item label="用户名" prop="username">
          <el-input
            v-model="form.username"
            placeholder="请输入用户名"
            :prefix-icon="User"
            autocomplete="username"
            size="large"
          />
        </el-form-item>

        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            placeholder="请输入密码"
            :prefix-icon="Lock"
            autocomplete="current-password"
            size="large"
          />
        </el-form-item>

        <el-button
          type="primary"
          native-type="submit"
          size="large"
          :loading="submitting"
          class="auth-card__submit"
        >
          登录
        </el-button>
      </el-form>

      <div class="auth-card__footer">
        <span>还没有账号？</span>
        <router-link to="/register" class="auth-card__link">立即注册</router-link>
      </div>
    </div>
  </div>
</template>

<style scoped>
.auth-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  background-color: var(--im-bg-grouped);
}

.auth-card {
  width: 100%;
  max-width: 400px;
  background-color: var(--im-bg-surface);
  border: 0.5px solid var(--im-border-light);
  border-radius: var(--im-radius-xl);
  padding: 32px 28px 26px;
  box-shadow: var(--im-shadow-md);
}

.auth-card__brand {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  gap: 12px;
  margin-bottom: 24px;
}

.auth-card__logo {
  width: 56px;
  height: 56px;
  border-radius: 14px;
  background-color: var(--im-accent-primary);
  color: var(--im-text-inverse);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  box-shadow: 0 4px 12px rgba(0, 122, 255, 0.25);
}

.auth-card__title {
  margin: 0;
  font-size: 24px;
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--im-text-primary);
}

.auth-card__subtitle {
  margin: 4px 0 0;
  font-size: 14px;
  color: var(--im-text-secondary);
}

.auth-card__alert {
  margin-bottom: 16px;
}

.auth-card__submit {
  width: 100%;
  margin-top: 8px;
  height: 44px;
  font-size: 16px;
  font-weight: 600;
  border-radius: 12px;
}

.auth-card__footer {
  margin-top: 22px;
  padding-top: 16px;
  border-top: 0.5px solid var(--im-border-light);
  display: flex;
  justify-content: center;
  gap: 6px;
  font-size: 14px;
  color: var(--im-text-secondary);
}

.auth-card__link {
  color: var(--im-accent-primary);
  font-weight: 600;
}

.auth-card__link:hover {
  text-decoration: underline;
}
</style>
