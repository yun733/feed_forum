import { defineStore } from 'pinia';
import { ref } from 'vue';
import axios from '../axios';

export const useNotificationStore = defineStore('notification', () => {
  const unreadCount = ref(0);

  const fetchUnreadCount = async () => {
    try {
      const res = await axios.get('/notifications/unread-count');
      unreadCount.value = res.data.unread_count;
    } catch {
      // ignore
    }
  };

  return { unreadCount, fetchUnreadCount };
});
