<template>
  <div class="user-page" v-if="userId">
    <UserProfile :user-id="userId" />
    <el-divider />
    <div class="user-articles">
      <h3>TA 的文章</h3>
      <div v-if="loading">加载中...</div>
      <div v-else-if="articles.length === 0" class="empty">还没有发布文章</div>
      <ArticleCard v-for="a in articles" :key="a.ID" :article="a" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import { useRoute } from 'vue-router';
import axios from '../axios';
import type { Article } from '../types';
import UserProfile from '../components/UserProfile.vue';
import ArticleCard from '../components/ArticleCard.vue';

const route = useRoute();
const userId = ref<number>(Number(route.params.id));
const articles = ref<Article[]>([]);
const loading = ref(true);

onMounted(async () => {
  try {
    const res = await axios.get(`/users/${userId.value}/articles`);
    articles.value = res.data.items || [];
  } catch (e) {
    console.error('fetch user articles failed', e);
  } finally {
    loading.value = false;
  }
});
</script>

<style scoped>
.user-page {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px 0;
}
.empty {
  text-align: center;
  color: #999;
  padding: 40px 0;
}
</style>
