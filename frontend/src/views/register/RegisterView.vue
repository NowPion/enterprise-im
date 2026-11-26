<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ChatDotSquare, Lock, Postcard, User } from '@element-plus/icons-vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import { BIZ_CODE, isApiError } from '@/types/common'

const router = useRouter()
const authStore = useAuthStore()

const formRef = ref<FormInstance>()
const submitting = ref(false)
const errorMessage = ref('')

const form = reactive({
  username: '',
  nickname: '',
  password: '',
  confirmPassword: '',
})

function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

const rules: FormRules = {
  username: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 1, max: 64, message: '用户名长度为 1 到 64 个字符', trigger: 'blur' },
  ],
  nickname: [
    { required: true, message: '请输入显示昵称', trigger: 'blur' },
    { min: 1, max: 64, message: '昵称长度为 1 到 64 个字符', trigger: 'blur' },
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
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
    { required: true, message: '请再次输入密码', trigger: 'blur' },
    {
      validator: (_rule, value: string, callback) => {
        if (value !== form.password) {
          callback(new Error('两次输入的密码不一致'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}

async function handleSubmit() {
  errorMessage.value = ''
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    const username = form.username.trim()
    await authStore.register({
      username,
      nickname: form.nickname.trim(),
      password: form.password,
    })
    ElMessage.success('注册成功，请登录')
    await router.replace({ path: '/login', query: { username } })
  } catch (err) {
    if (isApiError(err) && err.code === BIZ_CODE.USERNAME_EXISTS) {
      errorMessage.value = '该用户名已被注册，请更换后重试'
    } else if (isApiError(err)) {
      errorMessage.value = err.message || '注册失败'
    } else {
      errorMessage.value = '注册失败，请检查网络连接'
    }
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
          <el-icon :size="22"><ChatDotSquare /></el-icon>
        </div>
        <div>
          <h1 class="auth-card__title">创建账号</h1>
          <p class="auth-card__subtitle">注册企业即时通讯客户端账号</p>
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
            placeholder="1–64 个字符，用于登录"
            :prefix-icon="User"
            autocomplete="username"
            maxlength="64"
            size="large"
          />
        </el-form-item>

        <el-form-item label="昵称" prop="nickname">
          <el-input
            v-model="form.nickname"
            placeholder="1–64 个字符，用于对外展示"
            :prefix-icon="Postcard"
            maxlength="64"
            size="large"
          />
        </el-form-item>

        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            placeholder="8–72 字节"
            :prefix-icon="Lock"
            autocomplete="new-password"
            size="large"
          />
        </el-form-item>

        <el-form-item label="确认密码" prop="confirmPassword">
          <el-input
            v-model="form.confirmPassword"
            type="password"
            show-password
            placeholder="请再次输入密码"
            :prefix-icon="Lock"
            autocomplete="new-password"
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
          注册
        </el-button>
      </el-form>

      <div class="auth-card__footer">
        <span>已有账号？</span>
        <router-link to="/login" class="auth-card__link">返回登录</router-link>
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
  background-color: var(--im-bg-page);
}

.auth-card {
  width: 100%;
  max-width: 400px;
  background-color: var(--im-bg-surface);
  border: 1px solid var(--im-border-base);
  border-radius: var(--im-radius-lg);
  padding: 32px;
  box-shadow: var(--im-shadow-md);
}

.auth-card__brand {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 24px;
}

.auth-card__logo {
  width: 44px;
  height: 44px;
  border-radius: 10px;
  background-color: var(--im-bg-rail);
  color: var(--im-text-inverse);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.auth-card__title {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  color: var(--im-text-primary);
}

.auth-card__subtitle {
  margin: 2px 0 0;
  font-size: 13px;
  color: var(--im-text-muted);
}

.auth-card__alert {
  margin-bottom: 16px;
}

.auth-card__submit {
  width: 100%;
  margin-top: 8px;
}

.auth-card__footer {
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid var(--im-border-light);
  display: flex;
  justify-content: center;
  gap: 6px;
  font-size: 13px;
  color: var(--im-text-secondary);
}

.auth-card__link {
  color: var(--im-accent-primary);
  font-weight: 500;
}

.auth-card__link:hover {
  text-decoration: underline;
}
</style>
