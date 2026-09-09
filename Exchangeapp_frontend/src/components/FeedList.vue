<template>
  <div class="feed-list">
    <el-tabs v-model="activeTab" @tab-change="handleTabChange">
      <el-tab-pane label="关注" name="following" />
      <el-tab-pane label="最新" name="latest" />
    </el-tabs>

    <div v-if="loading" class="loading">加载中...</div>

    <template v-else>
      <div v-if="articles.length === 0" class="empty">
        <template v-if="activeTab === 'following'">
          关注的人还没有新内容，去看看<a @click="$router.push({ name: 'HotFeed' })" style="color: #1890ff; cursor: pointer">热榜</a>吧
        </template>
        <template v-else>
          还没有文章，<a @click="$router.push({ name: 'ArticlePublish' })" style="color: #1890ff; cursor: pointer">写一篇</a>吧
        </template>
      </div>

      <template v-else>
        <ArticleCard v-for="article in articles" :key="article.ID" :article="article" />

        <!-- 关注流：游标翻页（加载更多） -->
        <div v-if="activeTab === 'following' && feedHasMore" class="load-more">
          <el-button :loading="feedLoadingMore" @click="loadMoreFeed">加载更多</el-button>
        </div>
        <div v-else-if="activeTab === 'following'" class="feed-end">已经到底了</div>

        <!-- 最新：offset 页码 -->
        <el-pagination
          v-if="activeTab === 'latest' && latestTotal > pageSize"
          v-model:current-page="latestPage"
          :page-size="pageSize"
          :total="latestTotal"
          layout="prev, next"
          @current-change="fetchLatest"
          style="justify-content: center; margin-top: 20px"
        />
      </template>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, computed } from 'vue';
import axios from '../axios';
import type { Article } from '../types';
import ArticleCard from './ArticleCard.vue';

const activeTab = ref<'following' | 'latest'>('latest');
const pageSize = ref(10);

// ===== 最新列表：offset 分页（GET /articles?page&size）=====
const latestArticles = ref<Article[]>([]);
const latestTotal = ref(0);
const latestPage = ref(1);
const loadingLatest = ref(false);

// ===== 关注 Feed：游标分页（GET /feed?size&cursor）=====
const feedArticles = ref<Article[]>([]);
const feedCursor = ref<string | null>(null); // 后端返回的 next_cursor
const feedHasMore = ref(false);
const feedLoading = ref(false);
const feedLoadingMore = ref(false);

const articles = computed<Article[]>(() =>
  activeTab.value === 'following' ? feedArticles.value : latestArticles.value
);
const loading = computed(() =>
  activeTab.value === 'following' ? feedLoading.value : loadingLatest.value
);

const fetchLatest = async () => {
  loadingLatest.value = true;
  try {
    const res = await axios.get('/articles', {
      params: { page: latestPage.value, size: pageSize.value },
    });
    latestArticles.value = res.data.items || [];
    latestTotal.value = res.data.total || 0;
  } catch (e) {
    console.error('fetch articles failed', e);
    latestArticles.value = [];
    latestTotal.value = 0;
  } finally {
    loadingLatest.value = false;
  }
};

// 拉取关注 Feed：reset=true 拉第一页（不带 cursor），否则带上一次返回的 next_cursor 翻页
const fetchFollowing = async (reset: boolean) => {
  if (reset) {
    feedLoading.value = true;
  } else {
    feedLoadingMore.value = true;
  }
  try {
    const params: Record<string, unknown> = { size: pageSize.value };
    if (!reset && feedCursor.value) {
      params.cursor = feedCursor.value;
    }
    const res = await axios.get('/feed', { params });
    const items: Article[] = res.data.items || [];

    if (reset) {
      feedArticles.value = items;
    } else {
      // 后端整页已标记已读，理论上不会重复；仍按 ID 去重兜底
      const seen = new Set(feedArticles.value.map((a) => a.ID));
      feedArticles.value = feedArticles.value.concat(
        items.filter((a) => !seen.has(a.ID))
      );
    }

    feedHasMore.value = !!res.data.has_more;
    feedCursor.value = res.data.next_cursor || null;
  } catch (e) {
    console.error('fetch following feed failed', e);
    if (reset) {
      feedArticles.value = [];
      feedHasMore.value = false;
    }
  } finally {
    feedLoading.value = false;
    feedLoadingMore.value = false;
  }
};

const loadMoreFeed = () => {
  if (feedLoadingMore.value || !feedHasMore.value) return;
  fetchFollowing(false);
};

const handleTabChange = () => {
  // 切到关注：重新拉顶部未读（后端会整页标已读）；切到最新：刷新最新列表
  if (activeTab.value === 'following') {
    feedCursor.value = null;
    fetchFollowing(true);
  } else {
    latestPage.value = 1;
    fetchLatest();
  }
};

onMounted(() => {
  fetchLatest();
});
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
.load-more {
  text-align: center;
  margin: 16px 0;
}
.feed-end {
  text-align: center;
  color: #bbb;
  font-size: 13px;
  padding: 12px 0;
}
</style>
