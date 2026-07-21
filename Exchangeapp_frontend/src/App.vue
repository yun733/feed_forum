<template>
  <el-container>
    <NavBar />
    <el-main>
      <router-view />
    </el-main>
  </el-container>
</template>

<script setup lang="ts">
import NavBar from './components/NavBar.vue';
</script>

<style>
body {
  margin: 0;
  background: #f5f5f5;
}
.el-main {
  max-width: 1200px;
  margin: 0 auto;
  padding: 20px;
}
</style>
