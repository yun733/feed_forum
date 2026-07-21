<template>
  <el-header>
    <el-menu
      :default-active="activeIndex"
      mode="horizontal"
      :ellipsis="true"
      @select="handleSelect"
    >
      <el-menu-item index="home">首页</el-menu-item>
      <el-menu-item index="hot">热榜</el-menu-item>
      <el-menu-item index="exchange">兑换货币</el-menu-item>
      <el-menu-item index="publish" v-if="authStore.isAuthenticated">
        写文章
      </el-menu-item>
      <el-menu-item index="login" v-if="!authStore.isAuthenticated">
        登录
      </el-menu-item>
      <el-menu-item index="register" v-if="!authStore.isAuthenticated">
        注册
      </el-menu-item>

      <div class="nav-right">
        <el-menu-item index="search" class="search-item">
          <el-input
            v-model="searchQuery"
            placeholder="搜索文章..."
            size="small"
            @keyup.enter="doSearch"
          >
            <template #prefix>
              <span @click="doSearch" style="cursor:pointer">🔍</span>
            </template>
          </el-input>
        </el-menu-item>

        <template v-if="authStore.isAuthenticated">
          <el-menu-item index="notifications">
            通知
            <el-badge
              v-if="notificationStore.unreadCount > 0"
              :value="notificationStore.unreadCount"
              style="margin-left: 4px"
            />
          </el-menu-item>
          <el-sub-menu index="user-menu">
            <template #title>
              {{ authStore.currentUser?.username || '用户' }}
            </template>
            <el-menu-item index="profile">个人主页</el-menu-item>
            <el-menu-item index="logout">退出登录</el-menu-item>
          </el-sub-menu>
        </template>
      </div>
    </el-menu>
  </el-header>
</template>

<script setup lang="ts">
import { ref, watch, onMounted } from 'vue';
import { useRouter, useRoute } from 'vue-router';
import { useAuthStore } from '../store/auth';
import { useNotificationStore } from '../store/notification';

const router = useRouter();
const route = useRoute();
const authStore = useAuthStore();
const notificationStore = useNotificationStore();
const activeIndex = ref(route.name?.toString() || 'home');
const searchQuery = ref('');

watch(route, (r) => {
  activeIndex.value = r.name?.toString() || 'home';
});

onMounted(() => {
  if (authStore.isAuthenticated) {
    notificationStore.fetchUnreadCount();
  }
});

const doSearch = () => {
  if (searchQuery.value.trim()) {
    router.push({ name: 'Search', query: { q: searchQuery.value.trim() } });
    searchQuery.value = '';
  }
};

const routeMap: Record<string, string> = {
  home: 'Home',
  hot: 'HotFeed',
  exchange: 'CurrencyExchange',
  publish: 'ArticlePublish',
  login: 'Login',
  register: 'Register',
  notifications: 'Notifications',
  profile: 'UserProfile',
};

const handleSelect = (key: string) => {
  if (key === 'logout') {
    authStore.logout();
    router.push({ name: 'Home' });
  } else if (key === 'search') {
    return;
  } else if (key === 'user-menu') {
    return;
  } else if (key === 'profile') {
    if (authStore.currentUser) {
      router.push({ name: 'UserProfileView', params: { id: authStore.currentUser.id } });
    }
  } else {
    const name = routeMap[key] || key;
    router.push({ name });
  }
};
</script>

<style scoped>
.el-header {
  padding: 0;
  position: sticky;
  top: 0;
  z-index: 100;
  background: #fff;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.08);
}

.nav-right {
  float: right;
  display: flex;
  align-items: center;
}

.search-item {
  width: 200px;
}
</style>
