<template>
  <div class="notification-page">
    <h3>通知</h3>
    <div v-if="loading">加载中...</div>
    <div v-else-if="notifications.length === 0" class="empty">暂无通知</div>
    <div v-else>
      <div
        v-for="n in notifications"
        :key="n.ID"
        class="notif-item"
        :class="{ unread: !n.IsRead }"
        @click="markRead(n)"
      >
        <el-avatar :size="32" :src="n.FromUser?.avatar">
          {{ n.FromUser?.username?.[0]?.toUpperCase() }}
        </el-avatar>
        <div class="notif-body">
          <span class="notif-user">{{ n.FromUser?.username }}</span>
          <span>{{ n.Content }}</span>
          <span class="notif-time">{{ formatTime(n.CreatedAt) }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import { useRouter } from 'vue-router';
import { useNotificationStore } from '../store/notification';
import axios from '../axios';
import type { Notification } from '../types';

const router = useRouter();
const notificationStore = useNotificationStore();
const notifications = ref<Notification[]>([]);
const loading = ref(true);

const fetchNotifications = async () => {
  try {
    const res = await axios.get('/notifications');
    notifications.value = res.data.items || [];
  } catch (e) {
    console.error('fetch notifications failed', e);
  } finally {
    loading.value = false;
  }
};

const markRead = async (n: Notification) => {
  if (!n.IsRead) {
    await axios.put(`/notifications/${n.ID}/read`);
    n.IsRead = true;
    notificationStore.fetchUnreadCount();
  }
  if (n.Type === 'like' || n.Type === 'comment') {
    router.push({ name: 'ArticleDetail', params: { id: n.TargetID } });
  } else if (n.Type === 'follow') {
    router.push({ name: 'UserProfileView', params: { id: n.FromUserID } });
  }
};

const formatTime = (t: string) => {
  const d = new Date(t);
  const now = new Date();
  const diff = now.getTime() - d.getTime();
  const hours = Math.floor(diff / 3600000);
  if (hours < 1) return '刚刚';
  if (hours < 24) return `${hours}小时前`;
  return `${Math.floor(hours / 24)}天前`;
};

onMounted(fetchNotifications);
</script>

<style scoped>
.notification-page {
  max-width: 650px;
  margin: 0 auto;
  padding: 20px 0;
}
.notif-item {
  display: flex;
  gap: 12px;
  padding: 14px 10px;
  border-bottom: 1px solid #f0f0f0;
  cursor: pointer;
}
.notif-item.unread {
  background: #f0f7ff;
}
.notif-body {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  font-size: 14px;
}
.notif-user {
  font-weight: 500;
  color: #333;
}
.notif-time {
  color: #999;
  font-size: 12px;
  width: 100%;
}
.empty {
  text-align: center;
  color: #999;
  padding: 60px 0;
}
</style>
