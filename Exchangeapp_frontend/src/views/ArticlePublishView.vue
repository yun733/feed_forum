<template>
  <div class="publish-page">
    <h2>写文章</h2>
    <ArticleEditor />
  </div>
</template>

<script setup lang="ts">
import ArticleEditor from '../components/ArticleEditor.vue';
</script>

<style scoped>
.publish-page {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px 0;
}
</style>
