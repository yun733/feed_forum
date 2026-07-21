<template>
  <el-card class="hot-ranking">
    <h3>🔥 热榜</h3>
    <div v-if="loading" class="loading">加载中...</div>
    <div v-else-if="articles.length === 0" class="empty">暂无热榜</div>
    <div v-else>
      <div
        v-for="(article, idx) in articles"
        :key="article.ID"
        class="hot-item"
        @click="$router.push({ name: 'ArticleDetail', params: { id: article.ID } })"
      >
        <span class="rank" :class="'rank-' + (idx + 1)">{{ idx + 1 }}</span>
        <div class="hot-info">
          <div class="hot-title">{{ article.Title }}</div>
          <div class="hot-meta">👍 {{ article.LikeCount }} 💬 {{ article.CommentCount }}</div>
        </div>
      </div>
    </div>
  </el-card>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import axios from '../axios';
import type { Article } from '../types';

const articles = ref<Article[]>([]);
const loading = ref(true);

onMounted(async () => {
  try {
    const res = await axios.get('/feed/hot?size=10');
    articles.value = res.data.items || [];
  } catch (e) {
    console.error('fetch hot feed failed', e);
  } finally {
    loading.value = false;
  }
});
</script>

<style scoped>
.hot-ranking h3 {
  margin-top: 0;
}
.hot-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px solid #f0f0f0;
  cursor: pointer;
}
.hot-item:hover {
  background: #fafafa;
}
.rank {
  font-weight: bold;
  font-size: 16px;
  width: 24px;
  text-align: center;
  flex-shrink: 0;
}
.rank-1 { color: #f56c6c; }
.rank-2 { color: #e6a23c; }
.rank-3 { color: #67c23a; }
.hot-title {
  font-size: 14px;
  line-height: 1.4;
  overflow: hidden;
  text-overflow: ellipsis;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
}
.hot-meta {
  font-size: 12px;
  color: #999;
  margin-top: 4px;
}
.loading, .empty {
  text-align: center;
  color: #999;
  padding: 20px 0;
}
</style>
