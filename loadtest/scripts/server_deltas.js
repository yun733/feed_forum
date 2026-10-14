#!/usr/bin/env node
// ============================================================
// 把 serverside_*.txt 里的 BEFORE/AFTER 采样换算成"压测期间的服务端速率"
//
// 单看累计值说明不了问题（Queries 天然一直涨），必须除以时长变成
// "每秒多少次"。这里同时给出峰值（压测期间 2s 一次的采样）。
// ============================================================
const fs = require('fs');
const path = require('path');

const dir = path.join(__dirname, '..', 'results');
const files = process.argv.slice(2).length
  ? process.argv.slice(2)
  : fs.readdirSync(dir).filter((f) => /^serverside.*\.txt$/.test(f));

const num = (s) => {
  if (s === undefined) return null;
  const m = String(s).match(/-?\d+(\.\d+)?/);
  return m ? Number(m[0]) : null;
};

for (const file of files) {
  const full = path.join(dir, file);
  if (!fs.existsSync(full)) continue;
  const lines = fs.readFileSync(full, 'utf8').split('\n');

  // label -> { before, after, peak }
  const stages = new Map();
  for (const line of lines) {
    if (!line.trim()) continue;
    const tag = line.split('|')[0];
    const m = tag.match(/^(.*):(BEFORE|AFTER)$/);
    if (m) {
      const s = stages.get(m[1]) || {};
      s[m[2]] = line;
      stages.set(m[1], s);
    }
    const p = line.match(/^PEAK\((.*)\)\|(.*)$/);
    if (p) {
      const s = stages.get(p[1]) || {};
      s.PEAK = p[2];
      stages.set(p[1], s);
    }
  }

  const rows = [];
  for (const [label, s] of stages) {
    if (!s.BEFORE || !s.AFTER) continue;
    const pick = (line, re) => num((line.match(re) || [])[1]);
    const bq = pick(s.BEFORE, /mysql\([^)]*q=(\d+)/);
    const aq = pick(s.AFTER, /mysql\([^)]*q=(\d+)/);
    const br = pick(s.BEFORE, /redis\(cli=\d+,cmds=(\d+)/);
    const ar = pick(s.AFTER, /redis\(cli=\d+,cmds=(\d+)/);
    // 采样点之间覆盖的时长不好精确知道，用 30s（本仓 DURATION 默认值）估算
    const secs = Number(process.env.SECS || 30) + 7; // + 建立/收尾开销的粗略修正
    rows.push({
      label,
      mysqlQPS: bq !== null && aq !== null ? Math.round((aq - bq) / secs) : null,
      redisOPS: br !== null && ar !== null ? Math.round((ar - br) / secs) : null,
      peak: s.PEAK || '',
    });
  }

  console.log(`\n### ${file}`);
  console.log('| 阶段 | MySQL 查询/秒 | Redis 命令/秒 | 压测期间峰值 |');
  console.log('|---|---:|---:|---|');
  for (const r of rows) {
    console.log(`| ${r.label} | ${r.mysqlQPS ?? '-'} | ${r.redisOPS ?? '-'} | ${r.peak} |`);
  }
}
