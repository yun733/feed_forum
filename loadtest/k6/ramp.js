// ============================================================
// 单接口定压脚本：固定并发跑固定时长，用于"阶梯加压找拐点"
//
// 用法：
//   TARGET=feed VUS=50 DURATION=30s \
//   k6 run -e TARGET=feed -e VUS=50 -e DURATION=30s ...
//
// 每次只压一个接口，保证指标干净（不混流量）。
// 每个 (接口, 并发) 组合单独跑一轮 → 单独出一份 JSON。
// ============================================================
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { TARGETS, buildRequest, pickUser, makeSummary } from './lib.js';

// 按状态码计数：失败率只能说明"有错"，状态码分布才能说明"错在哪"
// 0 = 请求没拿到响应（连接被拒/超时），和 4xx/5xx 是完全不同的问题
const statusCounter = new Counter('http_status_codes');

const targetName = __ENV.TARGET || 'feed';
const target = TARGETS[targetName];
if (!target) throw new Error(`未知 TARGET: ${targetName}`);

const VUS = Number(__ENV.VUS || 50);
const DURATION = __ENV.DURATION || '30s';
const OUT = __ENV.OUT || `../results/${targetName}_vus${VUS}.json`;

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      gracefulStop: '5s',
    },
  },
  // 不设阈值卡失败：压测目的是观测，不是 CI 卡口
  thresholds: {
    http_req_failed: ['rate<1'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  discardResponseBodies: true, // 只关心延迟/吞吐，不解析 body，避免压测机 CPU 成为瓶颈
};

export default function () {
  const user = pickUser();
  const req = buildRequest(target, user);
  const res = http.request(req.method, req.url, req.body, req.params);
  statusCounter.add(1, { status: String(res.status) });
  if (res.status === 0 && res.error) {
    // status=0 = 压根没拿到响应：连接被拒 / 读超时 / 服务端挂连接
    statusCounter.add(1, { status: '0', error: String(res.error).slice(0, 60) });
  }
  check(res, { 'status is 2xx': (r) => r.status >= 200 && r.status < 300 });
}

export const handleSummary = makeSummary(OUT);
