<template>
  <div class="comment-list">
    <h3>评论 ({{ total }})</h3>
    <div class="comment-input">
      <el-input
        v-model="newComment"
        placeholder="发表评论..."
        type="textarea"
        :rows="3"
      />
      <el-button
        type="primary"
        @click="submitComment"
        :disabled="!newComment.trim()"
        style="margin-top: 10px"
      >
        发表评论
      </el-button>
    </div>

    <div v-if="comments.length === 0" class="no-comments">
      暂无评论，来说两句吧
    </div>

    <CommentItem
      v-for="comment in comments"
      :key="comment.id"
      :comment="comment"
      :depth="0"
      :article-id="articleId"
      @replied="fetchComments"
    />
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue';
import axios from '../axios';
import type { Comment } from '../types';
import CommentItem from './CommentItem.vue';

const props = defineProps<{ articleId: number }>();

const comments = ref<Comment[]>([]);
const total = ref(0);
const newComment = ref('');

const fetchComments = async () => {
  try {
    const res = await axios.get(`/articles/${props.articleId}/comments`);
    comments.value = res.data.comments || [];
    total.value = res.data.total || 0;
  } catch (e) {
    console.error('fetch comments failed', e);
  }
};

const submitComment = async () => {
  if (!newComment.value.trim()) return;
  try {
    await axios.post(`/articles/${props.articleId}/comments`, {
      content: newComment.value,
    });
    newComment.value = '';
    fetchComments();
  } catch (e) {
    console.error('submit comment failed', e);
  }
};

onMounted(fetchComments);
</script>

<style scoped>
.comment-list {
  margin-top: 40px;
}
.comment-input {
  margin-bottom: 24px;
}
.no-comments {
  text-align: center;
  color: #999;
  padding: 40px 0;
}
</style>
