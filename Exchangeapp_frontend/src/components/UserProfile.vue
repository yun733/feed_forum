<template>
  <div class="user-profile" v-if="user">
    <div class="profile-header">
      <el-avatar :size="64" :src="user.avatar">
        {{ user.username?.[0]?.toUpperCase() }}
      </el-avatar>
      <h2>{{ user.username }}</h2>
      <p v-if="user.bio">{{ user.bio }}</p>
      <div class="stats">
        <span>{{ user.article_count || 0 }} 文章</span>
        <span>{{ user.follower_count || 0 }} 关注者</span>
        <span>{{ user.following_count || 0 }} 关注中</span>
      </div>
      <el-button
        v-if="!isSelf"
        :type="isFollowing ? 'default' : 'primary'"
        @click="toggleFollow"
        :loading="followLoading"
      >
        {{ isFollowing ? '已关注' : '关注' }}
      </el-button>
      <el-button v-if="isSelf" @click="$router.push({ name: 'UserProfile' })">
        编辑资料
      </el-button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, computed } from 'vue';
import axios from '../axios';
import { useAuthStore } from '../store/auth';
import type { User } from '../types';

const props = defineProps<{ userId: number }>();
const authStore = useAuthStore();
const user = ref<User | null>(null);
const isFollowing = ref(false);
const followLoading = ref(false);

const isSelf = computed(() => authStore.currentUser?.id === props.userId);

const fetchUser = async () => {
  try {
    const res = await axios.get(`/users/${props.userId}`);
    user.value = res.data;
  } catch (e) {
    console.error('fetch user failed', e);
  }
};

const fetchFollowStatus = async () => {
  if (isSelf.value || !authStore.isAuthenticated) return;
  try {
    const res = await axios.get(`/users/${props.userId}/follow/status`);
    isFollowing.value = res.data.is_following;
  } catch { /* ignore */ }
};

const toggleFollow = async () => {
  followLoading.value = true;
  try {
    const res = await axios.post(`/users/${props.userId}/follow`);
    isFollowing.value = res.data.is_following;
    fetchUser(); // refresh count
  } catch (e) {
    console.error('follow failed', e);
  } finally {
    followLoading.value = false;
  }
};

onMounted(() => {
  fetchUser();
  fetchFollowStatus();
});
</script>

<style scoped>
.user-profile {
  text-align: center;
  padding: 40px 0;
}
.profile-header {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}
.stats {
  display: flex;
  gap: 24px;
  color: #666;
  margin: 8px 0;
}
</style>
