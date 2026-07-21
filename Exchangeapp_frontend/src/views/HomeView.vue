<template>
  <div class="home-layout">
    <div class="main-content">
      <FeedList />
    </div>
    <aside class="sidebar">
      <HotRanking />
    </aside>
  </div>
</template>

<script setup lang="ts">
import FeedList from '../components/FeedList.vue';
import HotRanking from '../components/HotRanking.vue';
</script>

<style scoped>
.home-layout {
  display: flex;
  gap: 24px;
  max-width: 1100px;
  margin: 0 auto;
  padding: 20px 0;
}
.main-content {
  flex: 1;
  min-width: 0;
}
.sidebar {
  width: 320px;
  flex-shrink: 0;
}
@media (max-width: 768px) {
  .home-layout {
    flex-direction: column;
  }
  .sidebar {
    width: 100%;
  }
}
</style>
