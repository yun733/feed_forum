<template>
  <div class="article-editor">
    <el-form :model="form" label-position="top">
      <el-form-item label="标题">
        <el-input v-model="form.title" placeholder="输入文章标题..." size="large" />
      </el-form-item>
      <el-form-item label="摘要">
        <el-input
          v-model="form.preview"
          placeholder="一句话概括文章内容..."
          maxlength="200"
          show-word-limit
        />
      </el-form-item>
      <el-form-item label="分类">
        <el-select v-model="form.category" placeholder="选择分类" clearable>
          <el-option label="技术" value="技术" />
          <el-option label="生活" value="生活" />
          <el-option label="财经" value="财经" />
          <el-option label="教育" value="教育" />
          <el-option label="其他" value="其他" />
        </el-select>
      </el-form-item>
      <el-form-item label="内容">
        <el-input
          v-model="form.content"
          placeholder="支持 Markdown 语法..."
          type="textarea"
          :rows="15"
        />
      </el-form-item>
      <el-form-item>
        <el-button type="primary" @click="submitArticle" :loading="submitting">
          {{ isEdit ? '保存修改' : '发布文章' }}
        </el-button>
        <el-button @click="$router.back()">取消</el-button>
        <el-button text @click="form.status = form.status === 'draft' ? 'published' : 'draft'">
          {{ form.status === 'draft' ? '切换为发布' : '切换为草稿' }}
        </el-button>
      </el-form-item>
    </el-form>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted } from 'vue';
import { useRouter } from 'vue-router';
import { ElMessage } from 'element-plus';
import axios from '../axios';

const props = defineProps<{ articleId?: string }>();
const router = useRouter();
const isEdit = !!props.articleId;
const submitting = ref(false);

const form = reactive({
  title: '',
  preview: '',
  content: '',
  category: '',
  status: 'published',
});

onMounted(async () => {
  if (isEdit && props.articleId) {
    try {
      const res = await axios.get(`/articles/${props.articleId}`);
      const a = res.data;
      form.title = a.Title;
      form.preview = a.Preview;
      form.content = a.Content;
      form.category = a.Category;
      form.status = a.Status;
    } catch {
      ElMessage.error('加载文章失败');
    }
  }
});

const submitArticle = async () => {
  if (!form.title.trim() || !form.content.trim() || !form.preview.trim()) {
    ElMessage.warning('请填写标题、摘要和内容');
    return;
  }
  submitting.value = true;
  try {
    if (isEdit) {
      await axios.put(`/articles/${props.articleId}`, form);
      ElMessage.success('修改成功');
    } else {
      await axios.post('/articles', form);
      ElMessage.success('发布成功');
    }
    router.push({ name: 'Home' });
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '操作失败');
  } finally {
    submitting.value = false;
  }
};
</script>

<style scoped>
.article-editor {
  max-width: 800px;
  margin: 0 auto;
}
</style>
