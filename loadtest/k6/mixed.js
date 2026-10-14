// ============================================================
// 混合流量场景：按真实前端行为配比打流量
//
// 为什么需要：单接口定压只能测"这个接口的极限"，测不出
// "所有接口共用一个进程/一个 MySQL 连接池/一个 Redis 连接池时，
// 谁把资源吃掉了"。混合场景才是生产形状。
//
// 配比依据（前端实际调用频率）：
//   feed 首页刷新最频繁（40%）> 文章详情（20%）
//   > 用户主页（10%）> 未读数轮询（10%）> 其余（20%）
// ============================================================
import http from 'k6/http';
import { check } from 'k6';
import { TARGETS, buildRequest, pickUser, makeSummary } from './lib.js';

const VUS = Number(__ENV.VUS || 100);
const DURATION = __ENV.DURATION || '60s';
const OUT = __ENV.OUT || `../results/mixed_vus${VUS}.json`;

export const options = {
  scenarios: {
    mixed: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      gracefulStop: '5s',
    },
  },
  thresholds: { http_req_failed: ['rate<1'] },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  discardResponseBodies: true,
};

// 权重表：累加到 100
const WEIGHTS = [
  ['feed', 40],
  ['article-detail', 20],
  ['user-profile', 10],
  ['unread', 10],
  ['articles-list', 7],
  ['hot', 5],
  ['notifications', 4],
  ['comments', 2],
  ['followers', 2],
];

const TOTAL = WEIGHTS.reduce((s, [, w]) => s + w, 0);

function pickTargetName() {
  let r = Math.random() * TOTAL;
  for (const [name, w] of WEIGHTS) {
    if (r < w) return name;
    r -= w;
  }
  return WEIGHTS[0][0];
}

export default function () {
  const name = pickTargetName();
  const target = TARGETS[name];
  const user = pickUser();
  const req = buildRequest(target, user);
  const res = http.request(req.method, req.url, req.body, {
    ...req.params,
    tags: { endpoint: name }, // 打 tag → k6 会按接口分别统计延迟
  });
  check(res, { 'status is 2xx': (r) => r.status >= 200 && r.status < 300 });
}

export const handleSummary = makeSummary(OUT);
