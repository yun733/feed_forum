import { defineStore } from 'pinia';
import { ref, computed } from 'vue';
import axios from '../axios';
import type { User } from '../types';

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem('token'));
  const currentUser = ref<User | null>(null);

  const isAuthenticated = computed(() => !!token.value);

  const login = async (username: string, password: string) => {
    const response = await axios.post('/auth/login', { username, password });
    token.value = response.data.token;
    currentUser.value = response.data.user;
    localStorage.setItem('token', token.value || '');
  };

  const register = async (username: string, password: string) => {
    const response = await axios.post('/auth/register', { username, password });
    token.value = response.data.token;
    currentUser.value = response.data.user;
    localStorage.setItem('token', token.value || '');
  };

  const logout = () => {
    token.value = null;
    currentUser.value = null;
    localStorage.removeItem('token');
  };

  return {
    token,
    currentUser,
    isAuthenticated,
    login,
    register,
    logout,
  };
});
