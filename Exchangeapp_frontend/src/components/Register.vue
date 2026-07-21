<template>
  <div class="auth-form">
    <h2>注册</h2>
    <el-form @submit.prevent="handleRegister">
      <el-form-item label="用户名">
        <el-input v-model="username" placeholder="请输入用户名" />
      </el-form-item>
      <el-form-item label="密码">
        <el-input v-model="password" type="password" placeholder="请输入密码" />
      </el-form-item>
      <el-form-item>
        <el-button type="primary" @click="handleRegister" :loading="loading">
          注册
        </el-button>
        <el-button text @click="$router.push({ name: 'Login' })">
          已有账号？去登录
        </el-button>
      </el-form-item>
    </el-form>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue';
import { useRouter } from 'vue-router';
import { ElMessage } from 'element-plus';
import { useAuthStore } from '../store/auth';

const router = useRouter();
const authStore = useAuthStore();
const username = ref('');
const password = ref('');
const loading = ref(false);

const handleRegister = async () => {
  if (!username.value || !password.value) {
    ElMessage.warning('请填写用户名和密码');
    return;
  }
  loading.value = true;
  try {
    await authStore.register(username.value, password.value);
    ElMessage.success('注册成功');
    router.push({ name: 'Home' });
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '注册失败');
  } finally {
    loading.value = false;
  }
};
</script>

<style scoped>
.auth-form {
  max-width: 400px;
  margin: 60px auto;
  padding: 40px;
  background: #fff;
  border-radius: 8px;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.08);
}
</style>
