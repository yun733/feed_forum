<template>
  <div class="feed-list">
    <el-tabs v-model="activeTab" @tab-change="handleTabChange">
      <el-tab-pane label="关注" name="following" />
      <el-tab-pane label="最新" name="latest" />
    </el-tabs>

    <div v-if="loading" class="loading">加载中...</div>
    <div v-else-if="articles.length === 0" class="empty">
      <template v-if="activeTab === 'following'">
        还没有关注任何人，去看看<a @click="$router.push({ name: 'HotFeed' })" style="color: #1890ff; cursor: pointer">热榜</a>吧
      </template>
      <template v-else>
        还没有文章，<a @click="$router.push({ name: 'ArticlePublish' })" style="color: #1890ff; cursor: pointer">写一篇</a>吧
      </template>
    </div>

    <ArticleCard v-for="article in articles" :key="article.ID" :article="article" />

    <el-pagination
      v-if="total > pageSize"
      v-model:current-page="currentPage"
      :page-size="pageSize"
      :total="total"
      layout="prev, next"
      @current-change="fetchArticles"
      style="justify-content: center; margin-top: 20px"
    />
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import axios from '../axios';
import type { Article } from '../types';
import ArticleCard from './ArticleCard.vue';

const activeTab = ref('latest');
const articles = ref<Article[]>([]);
const loading = ref(true);
const currentPage = ref(1);
const pageSize = ref(10);
const total = ref(0);

const fetchArticles = async () => {
  loading.value = true;
  try {
    const endpoint = activeTab.value === 'following' ? '/feed' : '/articles';
    const res = await axios.get(endpoint, {
      params: { page: currentPage.value, size: pageSize.value },
    });
    articles.value = res.data.items || [];
    total.value = res.data.total || 0;
  } catch (e) {
    console.error('fetch articles failed', e);
  } finally {
    loading.value = false;
  }
};

const handleTabChange = () => {
  currentPage.value = 1;
  fetchArticles();
};

onMounted(fetchArticles);
</script>

<style scoped>
.feed-list {
  min-height: 400px;
}
.loading, .empty {
  text-align: center;
  color: #999;
  padding: 60px 0;
}
</style>
