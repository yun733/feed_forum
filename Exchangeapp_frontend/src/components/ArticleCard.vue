<template>
  <el-card class="article-card" @click="$router.push({ name: 'ArticleDetail', params: { id: article.ID } })">
    <div class="article-header">
      <div class="author-info" @click.stop="$router.push({ name: 'UserProfileView', params: { id: article.AuthorID } })">
        <el-avatar :size="32" :src="article.Author?.avatar">
          {{ article.Author?.username?.[0]?.toUpperCase() }}
        </el-avatar>
        <span class="author-name">{{ article.Author?.username || '匿名' }}</span>
      </div>
      <span class="article-time">{{ formatTime(article.CreatedAt) }}</span>
    </div>

    <h3 class="article-title">{{ article.Title }}</h3>
    <p class="article-preview">{{ article.Preview }}</p>

    <div class="article-footer">
      <span>👍 {{ article.LikeCount || 0 }}</span>
      <span>💬 {{ article.CommentCount || 0 }}</span>
      <span>👁 {{ article.ViewCount || 0 }}</span>
      <el-tag v-if="article.Category" size="small">{{ article.Category }}</el-tag>
    </div>
  </el-card>
</template>

<script setup lang="ts">
import type { Article } from '../types';

defineProps<{ article: Article }>();

const formatTime = (t: string) => {
  const d = new Date(t);
  const now = new Date();
  const diff = now.getTime() - d.getTime();
  const hours = Math.floor(diff / 3600000);
  if (hours < 1) return '刚刚';
  if (hours < 24) return `${hours}小时前`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}天前`;
  return d.toLocaleDateString();
};
</script>

<style scoped>
.article-card {
  margin-bottom: 16px;
  cursor: pointer;
  transition: box-shadow 0.2s;
}
.article-card:hover {
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
}
.article-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 10px;
}
.author-info {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
}
.author-name {
  font-size: 14px;
  color: #555;
}
.article-time {
  font-size: 12px;
  color: #999;
}
.article-title {
  margin: 0 0 8px;
  font-size: 18px;
}
.article-preview {
  color: #666;
  line-height: 1.5;
  margin-bottom: 10px;
}
.article-footer {
  display: flex;
  gap: 16px;
  font-size: 13px;
  color: #999;
}
</style>
