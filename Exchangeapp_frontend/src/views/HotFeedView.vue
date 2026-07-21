<template>
  <div class="hot-page">
    <h2>🔥 热门文章</h2>
    <div v-if="loading">加载中...</div>
    <div v-else-if="articles.length === 0" class="empty">暂无热榜</div>
    <ArticleCard v-for="(a, idx) in articles" :key="a.ID" :article="a">
      <template #header>
        <span class="rank-badge" :class="'rank-' + (idx + 1)">TOP {{ idx + 1 }}</span>
      </template>
    </ArticleCard>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import axios from '../axios';
import type { Article } from '../types';
import ArticleCard from '../components/ArticleCard.vue';

const articles = ref<Article[]>([]);
const loading = ref(true);

onMounted(async () => {
  try {
    const res = await axios.get('/feed/hot?size=30');
    articles.value = res.data.items || [];
  } catch (e) {
    console.error('fetch hot feed failed', e);
  } finally {
    loading.value = false;
  }
});
</script>

<style scoped>
.hot-page {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px 0;
}
.rank-badge {
  font-weight: bold;
  font-size: 12px;
  padding: 2px 6px;
  border-radius: 4px;
}
.rank-1 { background: #f56c6c; color: #fff; }
.rank-2 { background: #e6a23c; color: #fff; }
.rank-3 { background: #67c23a; color: #fff; }
.empty {
  text-align: center;
  color: #999;
  padding: 60px 0;
}
</style>
