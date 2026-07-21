<template>
  <div class="article-detail" v-if="article">
    <h1>{{ article.Title }}</h1>
    <div class="meta">
      <span @click="$router.push({ name: 'UserProfileView', params: { id: article.AuthorID } })" style="cursor: pointer">
        <el-avatar :size="24" :src="article.Author?.avatar" />
        {{ article.Author?.username }}
      </span>
      <span>{{ formatTime(article.CreatedAt) }}</span>
      <span>👁 {{ article.ViewCount }}</span>
      <el-tag v-if="article.Category" size="small">{{ article.Category }}</el-tag>
    </div>

    <div class="content" v-html="renderedContent"></div>

    <div class="actions">
      <el-button
        :type="liked ? 'primary' : 'default'"
        @click="toggleLike"
        :loading="likeLoading"
      >
        {{ liked ? '已赞' : '👍 点赞' }} ({{ likeCount }})
      </el-button>
      <el-button
        :type="isFollowing ? 'default' : 'primary'"
        @click="toggleFollow"
        v-if="!isAuthor"
      >
        {{ isFollowing ? '已关注' : '关注作者' }}
      </el-button>
      <el-button v-if="isAuthor" @click="$router.push({ name: 'ArticleEdit', params: { id: articleId } })">
        编辑
      </el-button>
    </div>

    <CommentList :article-id="articleId" />
  </div>
  <div v-else-if="loading" class="center">加载中...</div>
  <div v-else class="center">文章不存在</div>
</template>

<script setup lang="ts">
import { ref, onMounted, computed } from 'vue';
import { useRoute } from 'vue-router';
import axios from '../axios';
import { useAuthStore } from '../store/auth';
import type { Article } from '../types';
import CommentList from '../components/CommentList.vue';

const route = useRoute();
const authStore = useAuthStore();
const articleId = Number(route.params.id);
const article = ref<Article | null>(null);
const loading = ref(true);
const liked = ref(false);
const likeCount = ref(0);
const likeLoading = ref(false);
const isFollowing = ref(false);

const isAuthor = computed(() => authStore.currentUser?.id === article.value?.AuthorID);

const renderedContent = computed(() => {
  if (!article.value) return '';
  return article.value.Content.replace(/\n/g, '<br>');
});

const fetchArticle = async () => {
  try {
    const res = await axios.get(`/articles/${articleId}`);
    article.value = res.data;
    likeCount.value = res.data.LikeCount || 0;
  } catch {
    article.value = null;
  } finally {
    loading.value = false;
  }
};

const fetchLikeStatus = async () => {
  if (!authStore.isAuthenticated) return;
  try {
    const res = await axios.get(`/articles/${articleId}/like/status`);
    liked.value = res.data.liked;
  } catch { /* ignore */ }
};

const toggleLike = async () => {
  if (!authStore.isAuthenticated) return;
  likeLoading.value = true;
  try {
    const res = await axios.post(`/articles/${articleId}/like`);
    liked.value = res.data.liked;
    likeCount.value = res.data.like_count;
  } catch (e) {
    console.error('like failed', e);
  } finally {
    likeLoading.value = false;
  }
};

const fetchFollowStatus = async () => {
  if (!authStore.isAuthenticated || !article.value || isAuthor.value) return;
  try {
    const res = await axios.get(`/users/${article.value.AuthorID}/follow/status`);
    isFollowing.value = res.data.is_following;
  } catch { /* ignore */ }
};

const toggleFollow = async () => {
  if (!article.value) return;
  try {
    const res = await axios.post(`/users/${article.value.AuthorID}/follow`);
    isFollowing.value = res.data.is_following;
  } catch (e) {
    console.error('follow failed', e);
  }
};

const formatTime = (t: string) => {
  const d = new Date(t);
  return d.toLocaleDateString() + ' ' + d.toLocaleTimeString();
};

onMounted(() => {
  fetchArticle().then(() => {
    fetchLikeStatus();
    fetchFollowStatus();
  });
});
</script>

<style scoped>
.article-detail {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px 0;
}
.meta {
  display: flex;
  align-items: center;
  gap: 16px;
  color: #666;
  font-size: 14px;
  margin-bottom: 24px;
}
.content {
  font-size: 16px;
  line-height: 1.8;
  margin-bottom: 32px;
}
.actions {
  display: flex;
  gap: 12px;
  margin-bottom: 40px;
}
.center {
  text-align: center;
  padding: 60px 0;
  color: #999;
}
</style>
