import { createRouter, createWebHistory, RouteRecordRaw } from 'vue-router';
import HomeView from '../views/HomeView.vue';
import CurrencyExchangeView from '../views/CurrencyExchangeView.vue';
import ArticleDetailView from '../views/ArticleDetailView.vue';
import ArticlePublishView from '../views/ArticlePublishView.vue';
import UserProfileView from '../views/UserProfileView.vue';
import SearchResultView from '../views/SearchResultView.vue';
import NotificationView from '../views/NotificationView.vue';
import HotFeedView from '../views/HotFeedView.vue';
import Login from '../components/Login.vue';
import Register from '../components/Register.vue';

const routes: RouteRecordRaw[] = [
  { path: '/', name: 'Home', component: HomeView },
  { path: '/hot', name: 'HotFeed', component: HotFeedView },
  { path: '/exchange', name: 'CurrencyExchange', component: CurrencyExchangeView },
  { path: '/articles/:id', name: 'ArticleDetail', component: ArticleDetailView },
  { path: '/publish', name: 'ArticlePublish', component: ArticlePublishView },
  { path: '/articles/:id/edit', name: 'ArticleEdit', component: ArticlePublishView },
  { path: '/users/:id', name: 'UserProfileView', component: UserProfileView },
  { path: '/search', name: 'Search', component: SearchResultView },
  { path: '/notifications', name: 'Notifications', component: NotificationView },
  { path: '/profile', name: 'UserProfile', component: UserProfileView },
  { path: '/login', name: 'Login', component: Login },
  { path: '/register', name: 'Register', component: Register },
];

const router = createRouter({
  history: createWebHistory(),
  routes,
});

export default router;
