<template>
  <div class="comment-item" :style="{ marginLeft: depth > 0 ? '24px' : '0' }">
    <div class="comment-header">
      <el-avatar :size="24" :src="comment.user?.avatar">
        {{ comment.user?.username?.[0]?.toUpperCase() }}
      </el-avatar>
      <span class="comment-user">{{ comment.user?.username }}</span>
      <span class="comment-time">{{ formatTime(comment.created_at) }}</span>
    </div>
    <div class="comment-content">{{ comment.content }}</div>
    <div class="comment-actions">
      <el-button text size="small" @click="handleLike">👍 {{ comment.like_count }}</el-button>
      <el-button text size="small" @click="showReply = !showReply">回复</el-button>
    </div>

    <div v-if="showReply" class="reply-box">
      <el-input
        v-model="replyContent"
        placeholder="写下你的回复..."
        type="textarea"
        :rows="2"
      />
      <el-button size="small" type="primary" @click="submitReply" style="margin-top: 8px">
        提交回复
      </el-button>
    </div>

    <CommentItem
      v-for="reply in comment.replies"
      :key="reply.id"
      :comment="reply"
      :depth="depth + 1"
      :article-id="articleId"
      @replied="$emit('replied')"
    />
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue';
import axios from '../axios';
import type { Comment } from '../types';

const props = defineProps<{
  comment: Comment;
  depth: number;
  articleId: number;
}>();

const emit = defineEmits(['replied']);

const showReply = ref(false);
const replyContent = ref('');

const handleLike = async () => {
  try {
    await axios.post(`/comments/${props.comment.id}/like`);
    emit('replied');
  } catch (e) {
    console.error('like failed', e);
  }
};

const submitReply = async () => {
  if (!replyContent.value.trim()) return;
  try {
    await axios.post(`/articles/${props.articleId}/comments`, {
      content: replyContent.value,
      parent_id: props.comment.id,
    });
    replyContent.value = '';
    showReply.value = false;
    emit('replied');
  } catch (e) {
    console.error('reply failed', e);
  }
};

const formatTime = (t: string) => {
  const d = new Date(t);
  const now = new Date();
  const diff = now.getTime() - d.getTime();
  const hours = Math.floor(diff / 3600000);
  if (hours < 1) return '刚刚';
  if (hours < 24) return `${hours}小时前`;
  return `${Math.floor(hours / 24)}天前`;
};
</script>

<style scoped>
.comment-item {
  padding: 10px 0;
  border-bottom: 1px solid #f0f0f0;
}
.comment-header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.comment-user {
  font-size: 13px;
  font-weight: 500;
  color: #333;
}
.comment-time {
  font-size: 12px;
  color: #999;
}
.comment-content {
  font-size: 14px;
  line-height: 1.6;
  margin-bottom: 6px;
  color: #333;
}
.comment-actions {
  display: flex;
  gap: 8px;
}
.reply-box {
  margin-top: 10px;
}
</style>
