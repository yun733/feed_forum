<template>
  <div class="search-page">
    <h3>搜索结果：{{ query }}</h3>
    <div v-if="loading">搜索中...</div>
    <div v-else-if="articles.length === 0" class="empty">未找到相关文章</div>
    <ArticleCard v-for="a in articles" :key="a.ID" :article="a" />
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, watch } from 'vue';
import { useRoute } from 'vue-router';
import axios from '../axios';
import type { Article } from '../types';
import ArticleCard from '../components/ArticleCard.vue';

const route = useRoute();
const query = ref((route.query.q as string) || '');
const articles = ref<Article[]>([]);
const loading = ref(true);

const doSearch = async (q: string) => {
  loading.value = true;
  try {
    const res = await axios.get('/articles/search', { params: { q } });
    articles.value = res.data.items || [];
  } catch {
    articles.value = [];
  } finally {
    loading.value = false;
  }
};

onMounted(() => doSearch(query.value));

watch(
  () => route.query.q,
  (q) => {
    query.value = (q as string) || '';
    doSearch(query.value);
  }
);
</script>

<style scoped>
.search-page {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px 0;
}
.empty {
  text-align: center;
  color: #999;
  padding: 60px 0;
}
</style>
