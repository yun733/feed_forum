#!/usr/bin/env node
// ============================================================
// 把 results/*.json（k6 handleSummary 落盘的原始指标）解析成
// Markdown 表格，直接贴进压测报告。
//
// 用法: node scripts/build_tables.js [结果目录]
// ============================================================
const fs = require('fs');
const path = require('path');

const dir = process.argv[2] || path.join(__dirname, '..', 'results');
const files = fs.readdirSync(dir).filter((f) => f.endsWith('.json') && f !== 'smoke.json');

const rows = [];
for (const f of files) {
  let d;
  try {
    d = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'));
  } catch (e) {
    console.error(`跳过 ${f}: ${e.message}`);
    continue;
  }
  const m = d.metrics || {};
  const dur = (m.http_req_duration || {}).values || {};
  const reqs = ((m.http_reqs || {}).values || {}).count || 0;
  const rps = ((m.http_reqs || {}).values || {}).rate || 0;
  const fail = ((m.http_req_failed || {}).values || {}).rate || 0;

  // 文件名: <base>_vus<N>.json  →  base 里可能带 tag，如 feed_cold
  const base = f.replace(/\.json$/, '');
  const vuMatch = base.match(/_vus(\d+)$/);
  const vus = vuMatch ? Number(vuMatch[1]) : 0;
  const name = vuMatch ? base.slice(0, -vuMatch[0].length) : base;

  // 状态码分布（只在加过 Counter 的轮次里有）
  const sc = {};
  for (const [k, v] of Object.entries(m)) {
    const mm = k.match(/^http_status_codes\{status:(\d+)(?:,error:(.*))?\}$/);
    if (mm) sc[mm[1] + (mm[2] ? ` (${mm[2]})` : '')] = v.values.count;
  }

  rows.push({
    name, vus, reqs,
    rps: +rps.toFixed(1),
    avg: +(dur.avg || 0).toFixed(2),
    p50: +(dur.med || 0).toFixed(2),
    p95: +(dur['p(95)'] || 0).toFixed(2),
    p99: +(dur['p(99)'] || 0).toFixed(2),
    max: +(dur.max || 0).toFixed(2),
    fail: +(fail * 100).toFixed(2),
    sc,
  });
}

// 按接口分组，组内按并发升序
const groups = {};
for (const r of rows) (groups[r.name] = groups[r.name] || []).push(r);

const fmt = (n) => n.toFixed(2);
const out = [];

for (const name of Object.keys(groups).sort()) {
  const g = groups[name].sort((a, b) => a.vus - b.vus);
  out.push(`\n### ${name}\n`);
  out.push('| 并发 VU | 总请求 | QPS | avg | P50 | P95 | P99 | max | 失败率 |');
  out.push('|---:|---:|---:|---:|---:|---:|---:|---:|---:|');
  for (const r of g) {
    out.push(
      `| ${r.vus} | ${r.reqs} | ${fmt(r.rps)} | ${fmt(r.avg)}ms | ${fmt(r.p50)}ms | ${fmt(r.p95)}ms | ${fmt(r.p99)}ms | ${fmt(r.max)}ms | ${r.fail}% |`
    );
  }
  // 拐点提示：QPS 增速相对上一档
  for (let i = 1; i < g.length; i++) {
    const gr = g[i - 1].rps > 0 ? ((g[i].rps - g[i - 1].rps) / g[i - 1].rps) * 100 : 0;
    const p99r = g[i - 1].p99 > 0 ? ((g[i].p99 - g[i - 1].p99) / g[i - 1].p99) * 100 : 0;
    out.push(
      `\n> VU ${g[i - 1].vus} → ${g[i].vus}: QPS ${gr >= 0 ? '+' : ''}${gr.toFixed(1)}%，P99 ${p99r >= 0 ? '+' : ''}${p99r.toFixed(1)}%`
    );
  }
  out.push('');
}

console.log(out.join('\n'));
