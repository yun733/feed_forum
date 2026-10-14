// ============================================================
// 压测公共库：目标接口注册表 + 请求构造 + 结果落盘
// ============================================================
import { SharedArray } from 'k6/data';

export const BASE = __ENV.BASE_URL || 'http://localhost:9090';

// 51 个 testuser 的 JWT（由 scripts/get_tokens.sh 生成）
export const USERS = new SharedArray('users', function () {
  return JSON.parse(open('../data/tokens.json'));
});

// 真实的已发布文章 ID（prepare_data.sh 从 MySQL 导出）
// 必须用真实 ID：articles 表 id 稀疏（502 行散在 1..1002），
// 随机取会大量 404，而 404 被空值缓存兜住 → 压出来的是假数据
export const ARTICLE_IDS = new SharedArray('articles', function () {
  return JSON.parse(open('../data/articles.json'));
});

// 用户在 LoadTest 中的 id 池 = token 表里的 userId
export const USER_IDS = USERS.map((u) => u.userId);

export function pickUser() {
  return USERS[Math.floor(Math.random() * USERS.length)];
}
export function pickArticleID() {
  return ARTICLE_IDS[Math.floor(Math.random() * ARTICLE_IDS.length)];
}
export function pickUserID() {
  return USER_IDS[Math.floor(Math.random() * USER_IDS.length)];
}

// ------------------------------------------------------------
// 接口注册表
//   auth: 是否需要 Authorization 头（true = 会走 Auth 中间件 → 触发活跃度写入）
//   build(user): 返回 { path, body? }
// ------------------------------------------------------------
export const TARGETS = {
  // ── 核心：活跃度判断 ───────────────────────────────────
  // 读路径：GetFeed → FeedPullSvc.PullFromInactiveUsers → ActiveWindow.FilterInactive
  // 写路径：Auth 中间件 refreshLastActive → SETNX 节流 → MarkActive 本地窗口
  'feed': {
    auth: true,
    label: 'GET /api/feed（活跃度判断·读路径 + 推拉结合）',
    build: (u) => ({ path: '/api/feed?size=10' }),
  },
  'feed-deep': {
    auth: true,
    label: 'GET /api/feed 深翻页（带游标）',
    build: () => ({ path: '/api/feed?size=10&cursor=MTc1NzAwMDAwMDo1MDA=' }),
  },

  // 写路径最轻载体：只做 1 次 COUNT，把活跃度写入的成本单独暴露出来
  'unread': {
    auth: true,
    label: 'GET /api/notifications/unread-count（活跃度判断·写路径载体）',
    build: () => ({ path: '/api/notifications/unread-count' }),
  },

  // ── 对照接口 ───────────────────────────────────────────
  'hot': {
    auth: true,
    label: 'GET /api/feed/hot（热榜：全量扫描 + 内存排序）',
    build: () => ({ path: '/api/feed/hot?page=1&size=20' }),
  },
  'article-detail': {
    auth: true,
    label: 'GET /api/articles/:id（缓存命中仍写 MySQL view_count）',
    build: () => ({ path: `/api/articles/${pickArticleID()}` }),
  },
  'articles-list': {
    auth: true,
    label: 'GET /api/articles（缓存 + 分布式锁）',
    build: () => ({ path: '/api/articles?page=1&size=10' }),
  },
  'user-profile': {
    auth: true,
    label: 'GET /api/users/:id（无缓存，4 次 MySQL）',
    build: () => ({ path: `/api/users/${pickUserID()}` }),
  },
  'user-articles': {
    auth: true,
    label: 'GET /api/users/:id/articles',
    build: () => ({ path: `/api/users/${pickUserID()}/articles?page=1&size=10` }),
  },
  'followers': {
    auth: true,
    label: 'GET /api/users/:id/followers（N+1）',
    build: () => ({ path: `/api/users/${pickUserID()}/followers?page=1&size=20` }),
  },
  'search': {
    auth: true,
    label: 'GET /api/articles/search（全文索引，无缓存）',
    build: () => ({ path: '/api/articles/search?q=Go&page=1&size=10' }),
  },
  'notifications': {
    auth: true,
    label: 'GET /api/notifications',
    build: () => ({ path: '/api/notifications?page=1&size=20' }),
  },
  'comments': {
    auth: true,
    label: 'GET /api/articles/:id/comments（无 LIMIT）',
    build: () => ({ path: `/api/articles/${pickArticleID()}/comments` }),
  },
  'login': {
    auth: false,
    method: 'POST',
    label: 'POST /api/auth/login（bcrypt 校验，公开接口）',
    build: () => ({
      path: '/api/auth/login',
      body: JSON.stringify({ username: 'testuser_1', password: '123456' }),
    }),
  },
};

export function buildRequest(target, user) {
  const { path, body } = target.build(user);
  const headers = { 'Content-Type': 'application/json' };
  if (target.auth) headers['Authorization'] = user.token;
  return {
    method: target.method || 'GET',
    url: BASE + path,
    body: body || null,
    params: { headers, tags: {} },
  };
}

// ------------------------------------------------------------
// handleSummary：把 k6 的原始指标落盘成 JSON，报告用这个数据
// 同时打印一行人类可读的摘要，方便边跑边看
// ------------------------------------------------------------
export function makeSummary(outFile) {
  return function (data) {
    const m = data.metrics;
    const dur = m.http_req_duration ? m.http_req_duration.values : {};
    const reqs = m.http_reqs ? m.http_reqs.values.count : 0;
    const rps = m.http_reqs ? m.http_reqs.values.rate : 0;
    const failed = m.http_req_failed ? m.http_req_failed.values.rate : 0;

    const line = [
      `reqs=${reqs}`,
      `rps=${rps.toFixed(1)}`,
      `avg=${(dur.avg || 0).toFixed(2)}ms`,
      `p50=${(dur.med || 0).toFixed(2)}ms`,
      `p95=${(dur['p(95)'] || 0).toFixed(2)}ms`,
      `p99=${(dur['p(99)'] || 0).toFixed(2)}ms`,
      `max=${(dur.max || 0).toFixed(2)}ms`,
      `failed=${(failed * 100).toFixed(2)}%`,
      `vus_max=${m.vus_max ? m.vus_max.values.max : 0}`,
      `dropped=${m.dropped_iterations ? m.dropped_iterations.values.count : 0}`,
    ].join('  ');

    const summary = {};
    summary[outFile] = JSON.stringify(data, null, 2);
    summary['stdout'] = line + '\n';
    return summary;
  };
}
