# FEED_FORUM 后端压测报告

> 工具：k6 v2.2.0 ｜ 首轮压测日期：2026-09-21 ｜ 被测版本：`main` @ `90bccf10`
> 回归压测日期：**2026-10-14**（推拉分层改造后，见 §3.6）
> 脚本与原始数据：`loadtest/`（可复现，见文末附录）

---

## 0. 摘要（面试开场可以直接背这 60 秒）

**首轮压测最值钱的三个结论：**

1. **热榜接口 `GET /api/feed/hot` 是唯一一个会"随数据量爆炸"的接口。**
   文章数 479 → 20479（42.7 倍）时，QPS 从 **9354 掉到 15**，P99 从 5.5ms 涨到 **13.6 秒**，进程内存峰值 923MB。
   根因是 `feed_controller.go:64`：`Find()` 把 30 天内**全部**文章连 `longtext` 正文一起拉进内存，再用**双层冒泡排序**（O(n²)）打分。它跟并发无关，只跟数据量有关——**加机器解决不了**。

2. **活跃度判断的写路径（`MarkActive`）本身很快，真正危险的是它每次都会派生一个 goroutine 去写 MySQL。**
   正常情况下 `user:active:throttle:<id>` 这个 5 分钟节流把所有写操作挡住了，实测慢 SQL = 0；
   一旦节流失效（多实例时钟漂移 / Redis 抖动 / 缓存被清），强制打开写路径后立刻出现 **125 条慢 `UPDATE users SET last_active_at`**，最慢 **2.6 秒**，P99 从 55ms 恶化到 **313ms**，失败率 6.96%。
   **节流是这套设计的承重墙，而不是优化项。**

3. **每个"1 条 SQL"实际是 3 次 MySQL 往返。** 用 MySQL general log 抓实证：
   `Prepare SELECT count(*) ... WHERE user_id = ?` → `Execute ... user_id = 13` → `Close stmt`。
   DSN 没开 `interpolateParams`，GORM 也没开 `PrepareStmt` 缓存，导致每次参数化查询都要 **PREPARE → EXECUTE → CLOSE 三次往返且执行计划不复用**。
   挂上这个系数：`GET /api/users/:id` 代码里 4 条 SQL，实测 **12 次服务端交互/请求**，是压测里数据库压力最大的接口。

**回归压测（§3.6）新增的第四条结论：**

4. **"大V推不动"这个问题的正解不是"推得更快"，而是"别推"。**
   10 万粉丝发一篇，旧行为是 `fanout timeout after 57000/100000, degrading` —— **43% 的粉丝永远收不到推送**，靠冷查询兜底；新架构把同一个账号判成 C 类后，写侧只花 **1 次 ZADD**，粉丝读时归并。端到端验证过：读者的时间线是满的（60 条），大V的新文章照样出现在第一屏。
   而**改造过程中被压测抓出三个实现 bug**（worker 池无限弹跳、keyset 分页被优化器选成 filesort、超时重试从头再来导致大V永远推不完）——这三个都是"读代码看不出来、跑一遍就现原形"的类型，见 §3.6.3。

**一句话总览（首轮，改造前的状态）：**

| 接口 | 20 VU QPS | 拐点 | 峰值 P99 | 主要瓶颈 |
|---|---:|---|---:|---|
| `GET /api/feed` | 2236 | ~50 VU 就饱和 | 212ms | 缓存击穿锁的 100ms 自旋 |
| `GET /api/notifications/unread-count` | 11449 | 未见明显拐点 | 55ms | 无缓存，1 次 COUNT = 3 次交互 |
| `GET /api/articles/:id` | 2787 | ~20 VU | 215ms | `view_count` 自增热点行锁（实测单条 SQL 20 秒） |
| `GET /api/users/:id` | 5125 | ~20 VU | 187ms | 无缓存，4 条 SQL = 12 次交互 |
| `GET /api/feed/hot` | 9511 | 数据量相关 | 100ms | **O(n²) 全量内存排序** |
| `POST /api/auth/login` | **151** | 硬件饱和 | 2313ms | bcrypt + 同一用户行 UPDATE |
| `GET /api/articles` | 7126 | ~50 VU | 66ms | 缓存命中，表现最好 |

> `GET /api/feed` 一行的 **2236 QPS / P99 212ms** 是首轮的旧数字。经过"击穿锁优化"（见 `压测报告_Feed击穿锁优化.md`）和本次推拉分层改造，同一接口现在是 **4310 QPS / P95 6.77ms / P99 9.10ms @20VU**，完整对比见 §3.6.2。

---

## 1. 被测系统与环境

### 1.1 系统规模

| 项 | 值 |
|---|---|
| 语言/框架 | Go 1.25 + Gin 1.12 + GORM 1.31 |
| 存储 | MySQL 8.0.45 / Redis 5.0.14 / RabbitMQ 3（Docker） |
| 端口 | `:9090` |

### 1.2 压测数据（由 `loadtest/scripts/prepare_data.sh` 构造，幂等可重放）

| 数据 | 数量 |
|---|---|
| 用户 | 58（其中 50 个 `testuser_1..50` 作为压测账号） |
| 已发布文章 | 479 |
| 关注关系 | 1726（平均每人关注 33.9 人） |
| 点赞 / 评论 / 通知 | 647 / 160 / 70 |
| Redis `user:active:*` | 35 个（**70% 活跃 / 30% 不活跃**的真实分布） |
| Redis `feed:timeline:*` | 30 人 × 60 篇（推路径预热） |

> 为什么必须先造数据：`GetFeed` 的推路径靠 Redis 时间线，**时间线为空会走完全不同的代码路径**（拉取 + 互斥锁）。不区分这两条路径压出来的数字没有意义。

### 1.3 压测机 = 被测机（**重要局限**）

单机 32 逻辑核 / 32GB，**k6 与服务端跑在同一台机器上**。后果：压测机自身要吃掉一部分 CPU 和网络栈，且同机测量噪声实测达到 **±30%**（同一个接口同样的并发，重复跑能差 20-30%）。

**这决定了报告的可信度分级：**

- ✅ **可信**：差异在 5 倍以上的结论（热榜 623 倍、login 与其他接口 50 倍）
- ⚠️ **仅供参考**：20-50% 量级的差异（例如活跃度写路径打开前后的对比）
- ❌ **不可信**：单次 30s 跑出的"最优并发点"

**给面试官的诚实说法**："本机压测只能定位瓶颈方向，不能给出容量数字。要出容量结论必须压测机与被测机分离，并用固定 CPU 配额（cgroup）限制压测进程。"

---

## 2. 压测方法

### 2.1 关键决策：活跃度判断没有独立接口，怎么压？

面试官问"你的活跃度判断接口 QPS 多少"时，**第一件事是纠正前提**：

> "它不是独立 HTTP 接口，而是嵌在两条链路里的：
> **写路径**在 Auth 中间件 `refreshLastActive`（每个受保护请求都会走到），
> **读路径**在 `FeedPullSvc.PullFromInactiveUsers` → `ActiveWindow.FilterInactive`（只有 Feed 走拉路径时才执行）。
> 所以我没法拿一个 URL 去压它，只能分读、写两条链路分别隔离出来压。"

隔离方法（**没有改动一行业务代码**）：

| 路径 | 隔离手段 | 效果 |
|---|---|---|
| 写路径 | 后台每 200ms 清一次 `user:active:throttle:*` | 让 5 分钟节流永不命中，强制每个请求都执行 `SETNX + MarkActive + 异步 UPDATE users` |
| 读路径 | 清空 `feed:timeline:*`，逼 Feed 走拉路径 | 让 `FilterInactive` 真正被调用（推路径拿够 10 条就直接返回了，根本不碰活跃度判断） |
| 读路径最坏 | 清空 timeline + 清空所有 `user:active:*` | `FilterInactive` 全员未命中 → 全量走 DB 拉取 |
| 稳态对照 | 35 活跃 + 时间线齐全 + 节流正常 | 真实生产形状 |

### 2.2 三种观测手段（缺一不可）

| 手段 | 看什么 | 为什么不能省 |
|---|---|---|
| **k6 客户端指标** | QPS / P50 / P95 / P99 / 失败率 | 只能反映客户端视角 |
| **服务端 2 秒采样**（`scripts/watch.sh`） | 进程 RSS、线程数、MySQL 连接数、Redis 连接数、行锁等待 | AFTER 采样会抹掉峰值——连接池早就缩回 `MaxIdleConns` 了 |
| **服务端日志增量分析** | 真实状态码分布、GORM 慢 SQL 条数/类型/最大值、日志字节增长 | 状态码只有服务端知道；慢 SQL 是定位"哪条语句"的唯一手段 |

### 2.3 负载模型

- **单接口阶梯爬坡**：每个接口按 20 → 50 → 100 → 200 → 400 VU，每档独立跑 30s，档间冷却 5s
  为什么每档单独跑：`ramping-vus` 把多档混在一条时间线里，P99 会被前面的档位污染，看不出拐点。
- **混合流量**：按前端真实调用频率配比（feed 40% / 文章详情 20% / 用户主页 10% / 未读数 10% / 其余 20%），100 与 200 VU 各跑 60s。
- **每轮压测前复位状态**：活跃标记是被测代码自己会写的副作用，不复位两轮数字不可比（`scripts/reset_state.sh`）。
- **每个 VU 用不同的用户 token**：所有 VU 共用一个 token 的话，节流会让写路径一辈子只触发一次。50 个用户的 JWT 由 `scripts/get_tokens.sh` 生成。

---

## 3. 压测结果

### 3.1 单接口阶梯爬坡

#### `GET /api/feed`（活跃度判断读路径主要载体）

| 并发 VU | QPS | avg | P50 | P95 | P99 | 失败率 |
|---:|---:|---:|---:|---:|---:|---:|
| 20 | 2235.6 | 8.78ms | 2.75ms | **100.89ms** | 102.32ms | 0% |
| 50 | 2667.2 | 18.53ms | 6.31ms | 104.72ms | 106.47ms | 0% |
| 100 | 2720.8 | 36.49ms | 14.71ms | 112.76ms | 115.55ms | 0% |
| 200 | 2620.5 | 75.91ms | 41.33ms | 136.75ms | 144.80ms | 0% |
| 400 | 2729.0 | 145.29ms | 172.01ms | 206.89ms | 212.61ms | **5.98%** |

**读法（这张表是本次压测信息量最大的一张）：**

- **QPS 从 50 VU 起就死了**：50 → 400 VU（8 倍并发）QPS 只从 2667 涨到 2729（+2.3%），而 avg 延迟涨了 **7.8 倍**（18.5ms → 145ms）。**加并发完全不换吞吐，只换延迟** —— 这是典型的资源饱和。
- **P95 = 100.89ms 这个数字太干净了，一定是代码里的常数。** 翻代码：`feed_controller.go:497` 有一句 `time.Sleep(100 * time.Millisecond)`。P50（2.75ms）和 P95（100.89ms）之间的断崖就是这个 sleep 造成的。

#### `GET /api/notifications/unread-count`（活跃度判断写路径最轻载体）

| 并发 VU | QPS | avg | P99 | 失败率 |
|---:|---:|---:|---:|---:|
| 20 | 11448.8 | 1.56ms | 4.53ms | 0% |
| 50 | 10147.3 | 4.70ms | 38.62ms | 0% |
| 100 | 8972.3 | 10.89ms | 92.09ms | 0% |
| 200 | 13908.9 | 14.03ms | 47.52ms | 0% |
| 400 | 13182.6 | 27.97ms | 55.17ms | **4.16%** |

单接口 QPS 最高（1.1-1.4 万），但注意它**没有任何缓存** —— 每次请求 1 条 `SELECT count(*)`。QPS 曲线非单调（11449 → 8972 → 13909）是压测机噪声，不是真实拐点。

#### `GET /api/articles/:id`（文章详情）

| 并发 VU | QPS | P50 | P99 | **max** | 失败率 |
|---:|---:|---:|---:|---:|---:|
| 20 | 2786.8 | 3.86ms | 12.29ms | 3698ms | **3.62%** |
| 50 | 2957.5 | 14.68ms | 58.16ms | 1665ms | 1.93% |
| 100 | 2909.2 | 33.16ms | 97.97ms | 8264ms | 2.22% |
| 200 | 2864.9 | 65.08ms | 118.81ms | 8206ms | 0.37% |
| 400 | 3507.0 | 108.33ms | 214.83ms | 6311ms | **17.07%** |

**这个接口最反直觉：它明明有缓存，却是失败最多的接口之一。** P50 只有 3.86ms（缓存命中），但 max 能到 8.2 秒。原因见 §4.2 —— 缓存只挡住了 SELECT，没挡住 `view_count` 的 UPDATE。

#### `GET /api/users/:id`（用户主页，完全无缓存）

| 并发 VU | QPS | P99 | 失败率 |
|---:|---:|---:|---:|
| 20 | 5124.5 | 8.57ms | 0% |
| 50 | 4394.8 | 73.62ms | **9.35%** |
| 100 | 3587.9 | 187.81ms | **18.85%** |
| 200 | 5417.6 | 99.29ms | 2.43% |
| 400 | 6135.7 | 186.52ms | 9.94% |

**非单调的失败率（0% → 9% → 19% → 2% → 10%）不是随便的噪声，是连接池争抢的典型特征**：失败恰好出现在连接池刚被打满、GORM 拿不到连接的档位。服务端采样证实 —— login@200VU 时 **MySQL 峰值连接 115**（`MaxOpenConns=114`，正好打满）。这个接口是压测中 **MySQL 压力最大的**（§4.4）。

#### `GET /api/feed/hot`（热榜）

| 并发 VU | QPS | P99 |
|---:|---:|---:|
| 20 | 9511.3 | 12.35ms |
| 50 | 9416.9 | 40.89ms |
| 100 | 8716.2 | 93.21ms |
| 200 | 13434.5 | 56.54ms |
| 400 | 11321.6 | 100.27ms |

**在这个数据量下热榜看起来是最健康的接口之一（9.5k QPS，P99 12ms）。这是一个陷阱** —— 它的瓶颈不是并发的，是数据量的。见 §3.4。

#### `POST /api/auth/login`（登录，唯一公开接口）

| 并发 VU | QPS | avg | P99 |
|---:|---:|---:|---:|
| 20 | **150.9** | 131.98ms | 307.65ms |
| 50 | **153.3** | 323.81ms | 546.51ms |
| 100 | **150.4** | 657.48ms | 915.63ms |
| 200 | **155.9** | 1253.56ms | 2313.31ms |
| 400 | 418.1（**67.21% 失败**） | 893.54ms | 5865.96ms |

**最干脆的一个天花板**：20 → 200 VU（10 倍并发），QPS 死死钉在 150-156，一帧都不动；延迟精确线性增长（132ms → 1254ms，×9.5）。这是纯粹的 CPU 饱和 —— `utils.HashPassword` 用 bcrypt，是故意设计成慢的。

400 VU 那一档的 418 QPS 是假的：67% 请求都失败了，剩下的成功请求数是 13749×0.33，真实成功 QPS 只有约 138。

#### `GET /api/articles`（文章列表，带缓存 + 分布式锁）

| 并发 VU | QPS | P99 | 失败率 |
|---:|---:|---:|---:|
| 20 | 7125.9 | 6.47ms | 0% |
| 50 | 7799.1 | 12.30ms | 0% |
| 100 | 7697.3 | 23.40ms | 0% |
| 200 | 7309.0 | 34.24ms | 0% |
| 400 | 7086.0 | 65.78ms | 2.42% |

**全项目表现最好的接口**，QPS 稳定在 7000-7800，P99 随并发线性温和增长。服务端证实它在 7000 QPS 下 **MySQL 查询几乎为 0**（缓存全命中）。

它证明了架构里"缓存 + 分布式锁"这套东西**方向是对的、也是能打的** —— 问题出在别处没照着它抄。

---

### 3.2 活跃度判断专项：读路径

把 Feed 强制打到拉路径上（清空 Redis 时间线），对比三种活跃度分布：

| 场景 | 20 VU | 100 VU | 400 VU | P99@400 |
|---|---:|---:|---:|---:|
| 推路径命中（对照组，有 timeline） | 2235.6 | 2720.8 | 2729.0 | 212.61ms |
| 拉路径 · 35 活跃 / 15 不活跃（70% 命中） | 2135.0 | 2716.4 | 2739.5 | 226.76ms |
| 拉路径 · **全员不活跃**（0% 命中，最坏） | 2199.8 | 2716.7 | 2876.3 | 212.30ms |

**三者差异完全落在测量噪声里（<5%）。**

再看服务端：

```
feed:rd@vus400       LOG|codes=[200=78085]|slowSQL=0     PEAK|rss=66MB|threads=46|mysql_conn=12|redis_cli=321
feed:rdcold@vus400   LOG|codes=[200=84799]|slowSQL=0     PEAK|rss=67MB|threads=46|mysql_conn=12|redis_cli=321
```

**结论（这条对面试很关键）：**

1. **`ActiveWindow` 的 L1/L2 两级缓存设计是正确的** —— 活跃度从"70% 命中"退化到"0% 命中"（最坏情况），接口吞吐和延迟**没有任何可观测变化**。两级缓存在这里确实起到了防抖作用。
2. **但读路径不是 Feed 的瓶颈**：慢 SQL = 0，MySQL 连接恒定 12，线程数只有 46。Feed 的 2700 QPS 天花板**跟数据库、跟活跃度判断都无关**，是 §4.3 那个锁的问题。
3. 所以"活跃度判断读路径很快"这个结论**不是靠两级缓存的收益证明的，而是因为它根本不是瓶颈** —— 这一点要说清楚，不要说成"因为两级缓存所以快"，那是两回事。

---

### 3.3 活跃度判断专项：写路径

后台每 200ms 清空 `user:active:throttle:*`，强制每个请求都执行完整写路径（`SETNX` → `MarkActive` → 异步 `UPDATE users`）：

| 场景 | 20 VU | 100 VU | 400 VU | P99@400 | 失败率@400 |
|---|---:|---:|---:|---:|---:|
| 节流**正常**（生产形状） | 11448.8 | 8972.3 | 13182.6 | 55.17ms | 4.16% |
| 节流**失效**（强制走写路径） | 10404.5 | 12063.5 | 7590.7 | **313.03ms** | **6.96%** |

服务端慢 SQL 增量（这是硬证据）：

```
节流正常 @100VU   LOG|...|slowSQL=0
节流正常 @400VU   LOG|...|slowSQL=0
节流失效 @100VU   LOG|...|slowSQL=70 (users=21)|slowSQL_max=295.039ms|logGrowth=40MB
节流失效 @400VU   LOG|...|slowSQL=125(users=7) |slowSQL_max=2639.740ms|logGrowth=23MB
```

**结论：**

1. **QPS 几乎不变（1.1-1.2 万），但 P99 从 55ms 恶化 5.7 倍到 313ms，失败率翻倍，并新增 125 条慢 SQL，最慢 2.64 秒。**
   为什么 QPS 不掉？因为 `MarkActive` 本身只是写本地 map（0 网络 IO），Go 侧几乎免费；**真正的代价全部被转移到 MySQL 的 `UPDATE users SET last_active_at` 上**，而那条语句在请求返回之后才异步执行，所以它不拖慢当前请求，只在宏观上堆积行锁。
2. **`refreshLastActive` 的 `SETNX` 5 分钟节流不是优化项，是这套架构的承重墙。** 它把写路径的实际执行频率压低了 5 个数量级（每个用户 5 分钟内只写一次）。一旦它失效，这个接口就退化成"每个请求一条 UPDATE"，跟直接的同步写没有区别，而且还有 `go func()` 无限制派生 goroutine 的额外风险。
3. 反向也说明：**`ActiveWindow` 的收益主要体现在写路径的"能省"，而不是"更快"** —— 有节流的时候根本轮不到它出场。

> ⚠️ 可信度声明：写路径开/关的 QPS 差异（10404 vs 11449 等）落在本机 ±30% 噪声内，**不能作为结论**。可信的是：慢 SQL 从 0 变成 125 条、慢 SQL 最大值从 0 变成 2639ms、P99 从 55ms 变成 313ms 这三项数量级级别的变化，以及服务端日志里 `UPDATE users` 这条语句从"完全不存在"变成"持续出现"。

---

### 3.4 热榜瓶颈放大实验（最漂亮的一组数据）

**假设**：`GetHotFeed` 把 30 天内全部已发布文章加载进内存，用双层冒泡排序（O(n²)）打分。那么耗时应该随文章数**平方**增长。

**验证**：同一台机器、同一份代码、同一个接口，只改文章总数。

| 已发布文章 | VU | **QPS** | avg | P99 | 进程内存峰值 |
|---:|---:|---:|---:|---:|---:|
| 479 | 20 | **9354.4** | 1.93ms | 5.51ms | — |
| 479 | 100 | 7632.3 | 12.77ms | 89.35ms | — |
| **20479** | 20 | **15.0** | 1321.15ms | 1702.27ms | — |
| **20479** | 100 | **13.4** | 6788.84ms | **13765ms** | **923MB** |

**文章数 ×42.7，QPS ÷623。**

理论预测：n² 缩放应该带来 42.7² ≈ **1823 倍** 的耗时增长；实测 **623 倍**。方向和量级完全吻合（差额由固定开销和测量噪声解释）。

**为什么这个发现比"QPS 低"重要得多：**

- 它不是并发问题，**加机器、加缓存穿透保护、加连接池都救不了**。
- 它在小数据量下**完全看不出来**（479 篇时 P99 只有 5.5ms，看起来是全项目最健康的接口）。
- 它是**线性的技术债累积**：每次发文都在给这个接口加成本，到某个点会突然从"很快"掉到"不可用"。

**面试话术**："我压测时发现热榜在 479 篇文章下 QPS 9354、P99 5.5ms，看起来最健康。但我知道它的实现是全量加载 + 冒泡排序，所以我做了个放大实验：灌到 2 万篇，QPS 掉到 15，P99 13.6 秒，内存 923MB。**压测的意义不是测出今天的数字，是测出它什么时候会崩。**"

---

### 3.5 混合流量（真实形状）

| VU | QPS | avg | P95 | P99 | 失败率 |
|---:|---:|---:|---:|---:|---:|
| 100 | 3981.4 | 24.83ms | 119.21ms | 125.31ms | 0.05% |
| 200 | **3723.9** | 53.28ms | 150.92ms | 376.94ms | 0.61% |

**并发翻倍，QPS 反而下降 6.5%，P99 涨 3 倍。** 系统在 100 VU 附近已经饱和。

服务端：100 VU 时 MySQL 查询约 3.5 万次/秒（含 Prepare/Close 系数，逻辑查询约 1.2 万/秒），Redis 命令 2.4 万次/秒。

混合场景 QPS（约 3900）远低于单压 `unread`（约 1.2 万）—— 因为混合里 40% 是 Feed，而 Feed 的天花板就是 2700。**一个慢接口会把整个系统的吞吐拖下来**，这是单接口压测绝对看不出来的。

---

### 3.6 回归压测：推拉分层改造 before/after（2026-10-14）

> 被测改动：把「推还是拉」从「超时后的被动降级」改成「写之前按粉丝量做的确定性路由」
> （A <5,000 同步推 / B 5,000~50 万 异步推 / C ≥50 万 只写作者 outbox，读时归并）。
> 设计与实施对照见 `设计文档_Feed推拉分层.md`。原始数据在 `loadtest/results/*_after*.json`。

#### 3.6.1 靶心实验：10 万粉丝发一篇文章

数据集：`follows` 里给一个作者灌 **100,000** 条粉丝记录（`follower_id` 用 1000000+ 的合成 ID，
无外键约束所以可以直接插）。三种配置各发一篇文章，观察 fanout 行为。

| # | 代码 | 路由配置 | fanout 结果 | 写侧命令数 | 实际推送到的粉丝 |
|---|---|---|---|---:|---|
| **A** | 旧（`90bccf10`） | `big_v_threshold=10000` | `fanout timeout after 57000/100000, degrading` | 171,000 | **57%** |
| **B** | 新 | tier B（`tier_a_max=5000`，100k < 50 万） | 3 轮让路后 **`done`** | 300,000 | **100%** |
| **C** | 新 | tier C（`tier_c_min=50000`，同一个号） | `→ outbox only (1 ZADD, no fanout)` | **4** | 0（读时归并） |

> **注意 B 的命令数（30 万）比 A（17.1 万）还多** —— 因为 A 只推了 57% 就放弃了，B 把 10 万粉丝
> **全都推到了**。所以 B 的改进不是"更省"，是"**真的推完了**"。真正省掉三个数量级的是 C。

**A（旧代码）实测日志：**

```
SLOW SQL >= 200ms
[407.565ms] [rows:100000] SELECT `follower_id` FROM `follows` WHERE following_id = 65 ...
[BigVFanout] Batch fanout failed for big-V article 37867: fanout timeout after 57000/100000 followers, degrading
[FeedConsumer] Article 37867 degraded to pull-only mode
```

`Pluck` 407ms（与设计文档记录的 408ms 吻合，两个独立数据集互相印证），1 秒推了 57,000 条，
**剩下 43,000 个粉丝一条都没推到**，只能靠读路径的冷数据回查兜底。

**B（新代码，tier B）实测日志：**

```
[FeedConsumer] article=999903 tier=B strategy=ASYNC_PUSH followers=100000 threshold=5000 queued for async fanout
[BigVFanout] ... stopped after 1.005s (pushed=48000): fanout yielded after 48000 followers at id=1047999 (progress saved, will resume)
[BigVFanout] ... stopped after 1.011s (pushed=49000): fanout yielded after 49000 followers at id=1096999 (progress saved, will resume)
[BigVFanout] ... done in 39.3387ms: pushed=3000 skipped_inactive=0
```

Redis 核对：**100,000 / 100,000 个 timeline 全部写入。**

**C（新代码，tier C）实测日志：**

```
[BigVFanout] article=999904 tier=C strategy=PULL_ONLY followers=100000 threshold=50000 → outbox only (1 ZADD, no fanout)
```

Redis 核对：`author:outbox:65 = [999904]`、`feed:authors:c = {65}`、
合成粉丝的 timeline 写入数 = **0**。

**一张表看写侧的量级差（外推到 1000 万粉）：**

| | 旧（推所有） | 新 tier B（异步推） | 新 tier C（走拉） |
|---|---:|---:|---:|
| 写侧命令数 @1000万 | **3,000 万条** | 3,000 万条 | **4 条** |
| 单次拉取内存 | 1000 万行 ID 一次进内存（80MB+） | 恒定一个 batch（1000 行） | 0 |
| 能否跑完 | ❌ 超时降级，推一半 | ✅ 分片续跑，全部推完 | ✅ 毫秒级返回 |
| 读侧成本 | 1 次 `ZREVRANGE` | 1 次 `ZREVRANGE` | +K 条 `ZREVRANGEBYSCORE`（搭在同一次 RTT 上） |

> ⚠️ **别把 tier B 说成"更快"**：tier B 的命令数和推完耗时都和旧代码同一量级
> （实测吞吐都是约 48,000~57,000 粉丝/秒）。它真正的改进是**能跑完**（100% vs 57%）
> 和**内存有界**（一个 batch vs 全部 ID）。真正省掉三个数量级的是 tier C。

#### 3.6.2 端到端验证：读者时间线是满的，还能看到大V的文章吗？

这是整个改造**最容易翻车**的地方。方案最初的写法是把 outbox 归并放进读路径的
`Phase 2/3`（只在"时间线装不满一页"时才跑），但那个位置**永远不会被执行**：
大V发文章时，读者的时间线通常是满的（还关注着别的小作者），一页直接取满就返回了
—— 大V的新文章会**永久不可见**。所以归并必须在主路径上。

实测验证（这是设计正确性的直接证据）：

```
# 让 testuser_1(13) 关注大V(65)，清掉他的已读集合
# 他的 timeline 有 60 条夹具文章（一页 size=5 装得满满当当）
$ redis-cli zrevrange feed:timeline:13 0 2     →  565 564 563

$ curl -H "Authorization: $TOKEN" 'localhost:9090/api/feed?size=5'
returned article IDs: [999904, 565, 564, 563, 562]
   来自 timeline：565 564 563 562
   来自 outbox  ：999904          ← 大V的文章，按时间插到了首位
```

**读者的时间线是满的，大V的文章照样出现在第一屏** —— 归并发生在主路径上，而不是兜底路径。

#### 3.6.3 改这一块时被压测抓出来的三个 bug

> 这三个都值得单独讲：**它们全都是"代码看起来对、跑起来才现原形"的类型**。
> 单元测试只能覆盖其中第一个（而且是我事后补的）。

**① B 类 worker 池无限弹跳 —— 文章不推、不报错、日志空白、CPU 烧起来**

worker 池的 handler 原本调的是 `execute()`，而 `execute()` 会**重新跑一遍路由** —— 判定还是
B 类，于是把文章**又投回同一个池**。worker 取出来再投回去，无限循环。

现象：发布后日志只有一行 `queued for async fanout`，之后再无任何输出；`feed:timeline:*`
一条没写；MySQL 没有活跃查询；进程 CPU 持续累积。**没有任何错误信息**。

修法：把"要不要异步"的决策限制在入口一次，worker 里只做执行（`executeNow`）。
回归测试在 `services/fanout_pool_integration_test.go` —— 它断言的不是返回值，而是
**`feed:timeline:<fid>` 里真的多了东西**（把 bug 改回去，测试 5 秒超时失败，0/10 写入）。

**② keyset 分页被优化器选成 filesort —— "修好的"比"修坏的"还慢**

为了修 `Pluck` 一次拉 10 万行的问题，分页流式拉取改成了 keyset pagination
（`WHERE following_id = ? AND follower_id > ? ORDER BY follower_id LIMIT 1000`）。
看起来是标准做法，实测**单批 1000 行要 507ms** —— 比它要替换掉的"一次拉 10 万行 407ms"还慢。

EXPLAIN 抓出根因：GORM 的软删除会补上 `deleted_at IS NULL`，优化器因此选了
`idx_follows_deleted_at` 然后对 4.8 万行做 filesort：

```
key=idx_follows_deleted_at  rows=48530  filtered=25%  Using index condition; Using where; Using filesort
```

更阴的是**同一个索引两副面孔**：首屏查询（不带 `follower_id > ?`）它选得对，
一加上游标条件就切到坏计划上：

```
-- 首屏：key=idx_follows_fanout  Extra=Using index        ✅
-- 带游标：key=idx_follows_deleted_at  Extra=Using filesort ❌
```

修法两层：把索引扩成 `(following_id, follower_id, deleted_at)` 让条件在索引内完成；
再对分页查询加 `FORCE INDEX` —— 因为即使有了合适的索引，优化器仍然会在带游标时选错
（它以为 `deleted_at IS NULL` 很挑数据，实际这张表几乎不软删）。

**③ 保护超时让大V永远推不完 —— 每次重试都从头再来**

1000 万粉的作者单次跑不完，必然撞上保护超时。原来的实现把超时当**失败**处理，
重试时**从头开始** —— 于是每一轮都在同一处超时，重试次数耗尽后彻底放弃。
实测就是这个形状：`FAILED after 1.08s (pushed=0)`，连推都没推到就超时了（因为慢查询本身吃掉了预算）。

修法：超时改成**让路点**而不是失败点 —— 把「推到哪个 `follower_id` 了」写进
`feed:fanout:progress:<articleID>`，下一轮从断点续跑；续跑**不消耗重试次数**
（它每次至少推进一个 batch，不会死循环）。§3.6.1 的 B 组就是这个机制跑出来的：
48,000 + 49,000 + 3,000 = 100,000。

#### 3.6.4 Feed 接口回归（warm，时间线齐全）

改动确实动了读路径（把 `SMEMBERS` 已读集合 + timeline + K 个 outbox 合并进同一次
pipeline，并在进 MySQL 前就把候选裁到一页），所以要回归。**基线用"击穿锁优化"之后的
v2**（`压测报告_Feed击穿锁优化.md`），不是首轮的 v1 —— 否则会把别人的功劳算到自己头上。

| 并发 VU | v2 QPS | 改造后 QPS | Δ | v2 P95 | 改造后 P95 | 失败率 |
|---:|---:|---:|---:|---:|---:|---:|
| 20 | 3911.0 | **4310.1** | +10.2% | 7.10ms | **6.77ms** | 0% |
| 50 | — | 4582.9 | — | — | 16.47ms | 0% |
| 100 | 3813.5 | **4094.6** | +7.4% | 28.97ms | 32.87ms | 0% |
| 200 | — | 3812.2 | — | — | 61.43ms | 0% |
| 400 | 3514.7 | **4078.0** | +16.0% | 123.45ms | **117.48ms** | 6.98% |

**读法：**

- QPS 在 20/100/400 三档分别 +10.2% / +7.4% / +16.0%。**这是小幅改善，不是数量级改善** ——
  feed 的天花板仍然是"每请求 3~4 次 Redis 往返"，本次改动没有动这个结构。
- 改造后 **@20 VU 的 P95 只有 6.77ms、P99 9.10ms，全程 0 失败** —— 对比首轮 v1 的
  P95 100.89ms，彻底不是一个时代了（但那次跃迁主要是击穿锁优化的功劳，见下方声明）。
- 400 VU 仍有约 7% 失败率，和 v2 的 1.54% 相比偏高。本机噪声 ±30%，
  单次 400 VU 的失败率在这台机器上历史波动范围是 1.5%~17%，**这一档不作为结论**。

> ⚠️ **归属声明（重要）**：`GET /api/feed` 从首轮 v1 的 2236 QPS / P95 100.89ms
> 到现在 4310 QPS / P95 6.77ms，跨越 14 倍的 P95 改善**主要来自"击穿锁优化"**
> （自旋 100ms 固定 sleep → 5ms 指数退避），那是一次独立的改动，不是我这次做的。
> 我这次能认领的是上表里相对 v2 的那 **+7%~+16%**。

#### 3.6.5 活跃度写路径回归（节流失效的病理态）

本次改动给 `refreshLastActive` 加了一次 `EXISTS`（用来判断"用户是不是刚从 7 天窗口外回来"）。
生产形状下这个判断**被 5 分钟节流摊薄了**，几乎零成本；但既然有病理态压测，就一并量一下：

| 场景（节流失效，强制走写路径） | 首轮 20VU | 改造后 20VU | 首轮 400VU | 改造后 400VU |
|---|---:|---:|---:|---:|
| QPS | 10404.5 | 9799.4 | 7590.7 | 9214.4 |
| P99@400 | — | — | 313.03ms | 125.05ms |
| 慢 SQL 条数 | 70 (@100VU) | 53 (@100VU) | 125 | 246 |

**结论：落在本机噪声内，不作为结论。** 两边 QPS 都是 1 万上下 ±30%（这份报告 §3.3 自己
就声明过写路径的开/关差异不可作结论），慢 SQL 的绝对值波动更大（125 → 246），
但语句还是同一条 `UPDATE users SET last_active_at` —— **主导因素是"节流失效"这件事本身，
不是多出来的那一次 EXISTS**。真正的结论只有一条：这次改动没有让写路径变差一个量级。

#### 3.6.6 改造中发现但**没有**修的问题（诚实清单）

| 问题 | 现状 | 为什么没修 |
|---|---|---|
| **游标分页的同秒边界** | `maxScore = cursor.Score - 1`（score 是秒级），同一秒内未展示完的文章翻页时会被跳过。游标里的 `ArticleID` 编码了但边界计算没用它 | 改它要连前端游标契约一起动，本次范围外。这是**读代码读出来的**，不是被问出来的 |
| **分层阈值没有实测支撑** | `tier_a_max=5000` / `tier_c_min=50万` 是按内存预算反推的（32GB ÷ 44.6KB ≈ 70 万可物化 inbox），**不是压测扫出来的拐点** | 要找真实拐点需要构造多档粉丝量的账号做扫描实验，本次只测了 10 万这一档 |
| **C 类走拉后读侧没压** | 只做了功能验证（§3.6.2），没压"一个用户关注 K 个大V时归并的读侧开销" | 夹具里只有 1 个大V，K=1 时的归并开销测不出量级 |
| **`materialize_active_only=true` 后重建频率上升** | 非活跃读者回来时会强制重建 timeline，重建有 30s 缓存 + SETNX 锁兜底。**其中"重复重建"那部分已修**（见 §3.6.7），但改造后**没有重跑 §3.2 的拉路径专项**，曲线未实测 | 压测时间关系，这是明确的欠账 |
| **重建没有全局并发闸** | `feed:mutex:timeline:<uid>` 是按用户的，N 个不同用户同时回来 = N 把锁，互不阻塞；真正兜底的只有 MySQL 连接池（`MaxOpenConns=114`，且全站共享） | 需要引入信号量，超出并发数的请求返回陈旧缓存 |
| **重建结果不物化、导致每 30 秒重扫一次** | ✅ **已修** | 见 §3.6.7 |

> 面试里这张表比分数字好用：**能说清楚"我测了什么"和"我没测什么"**，
> 比报一堆自己都没底的数字可信得多。

#### 3.6.7 修一个读路径的逻辑缺口：重建结果从没被物化过

**这不是压测抓出来的，是顺着读路径走一遍才发现的** —— 但它比前三个 bug 更隐蔽，
因为它不报错、不慢、功能"看起来是好的"。

**问题**：`rebuildFeed` 的结果只写进 `feed:pull:cache:<uid>`（30s），
**从来没写回 `feed:timeline:<uid>`**。而那份缓存只有一个读取入口：

```go
need := size - len(articles)      // 读者回来时页面是满的 → 0
if returning { need = size }      // 是 feedReturning 把它顶成 size 的
if need > 0 { fillFeedWithMutex(...) }   // ← 缓存只在这里面被读
```

于是「补内容」只对**当前这个请求**有效：

| 请求 | `feedReturning` | 结果 |
|---|---|---|
| 回来后第 1 个 | `true` → `need = size` | ✅ 重建，补上漏掉的内容 |
| 第 2 个起 | `false`（活跃标记已写上）→ `need = 0` | ❌ 不进 `fillFeedWithMutex` → 缓存永远读不到 → **退回旧内容** |

补一次、刷新一下就没了。设计文档写的是「首次回来时**按需重建 inbox**」，
代码做的是「重建一份**读结果**」—— 差了一个「物化」。

**修复**：新增 `BigVFanoutService.MaterializeInbox(userID, articles)`，
在抢到锁、重建完成之后，把结果按**和 fanout 完全相同的约定**写回 timeline
（score = `CreatedAt.Unix()`、member = articleID、同样裁剪到 500、同样 30 天 TTL）。
补上的内容在下一次请求的 Phase 1 里就自然出现 —— 不用重建、不碰缓存。
`feedReturning` 因此从「每次回来都要触发的补丁」变成真正的**一次性自愈**。

只在首屏（`cursor == nil`）物化：带游标的重建拉的是更旧的内容，
写进去会立刻被 `ZRemRangeByRank` 当最旧的裁掉。

**端到端实测**（数据与日志均为真实输出）：

```
场景：用户 13 的 timeline 有 60 条旧内容；删掉 user:active:13 模拟「离开 7 天」；
     库里插入一篇他关注的作者在「他离开期间」发的文章 999905（不经 MQ，所以不在任何 timeline 里）

请求 1  → [999905, 565, 564, 563, 562]   日志: materialized 1 articles into feed:timeline:13
         timeline: 60 → 61；ZSCORE 999905 = 1791969529

请求 2  → 清掉已读集合后: [999905, 565, 564, 563, 562]   日志: 无 materialized
         ← 内容是从 timeline 读到的，没有再扫库

负向对照（手动 ZREM 999905、保持 user:active:13 存在 → feedReturning=false）
        → [565, 564, 563, 562, 561]      日志: 无 materialized
        ← 内容没了，而且再也找不回来 —— 这正是修复前的行为
```

**负向对照是这组验证的关键**：它复现的正是「`need=0` → 不进 `fillFeedWithMutex` →
缓存读不到 → 内容永久丢失」这条链。修复后同样的操作能稳定拿到内容，且不触发第二次扫库。

回归测试：`services/materialize_inbox_test.go`。

> 这个缺口值得单独讲，因为它和前三个 bug 的性质不同：
> 前三个是「跑起来就炸/就慢」，这个是**功能静默降级** —— 用户看到的是旧内容，
> 没有任何错误、没有任何慢查询、监控上一切正常。**只有把读路径一行行走完才能发现。**

---

## 4. 瓶颈分析（按严重程度排序）

### 4.1 【P0】`GetHotFeed`：全量加载 + O(n²) 内存排序

**代码**：`controllers/feed_controller.go:63-86`

```go
query.Where("created_at > ?", since).Preload("Author").Find(&articles)   // 无 LIMIT，含 longtext 正文
// ...
for i := 0; i < len(scoredList); i++ {
    for j := i + 1; j < len(scoredList); j++ {   // 双层循环 = O(n²)
        if scoredList[j].score > scoredList[i].score { ... }
    }
}
```

**证据**：文章 479 → 20479 时 QPS 9354 → 15（§3.4），内存峰值 923MB。

**根因**（四件事叠在一起）：

1. `Find()` 没有 `Limit`，30 天内全部文章进内存
2. `models.Article.Content` 是 `longtext`，全量正文被读进内存又被 JSON 序列化
3. 冒泡排序 O(n²)
4. 排序完再用 offset 切片分页 —— 前 99% 的排序工作对当次请求是白做的

**修复**（按性价比排序）：

| 方案 | 改动量 | 效果 |
|---|---|---|
| ① 排序换 `sort.Slice` | 1 行 | O(n²) → O(n log n)，立刻兑现，但没解决全量加载 |
| ② 查询层加 `Limit`（取 Top-N 候选，如 500 条） | 1 行 | 把内存和排序规模都封顶 |
| ③ 用 SQL 只 `Select` 打分需要的列（`id, like_count, comment_count, view_count, created_at`），不取 `Content` | 2 行 | 内存降一个数量级 |
| ④ 分页下推到 SQL / 定时预计算 Top-N 落到 Redis ZSet | 半天 | 彻底解决，QPS 回到缓存级 |

**面试怎么答**："我知道这是 O(n²)，当时选择先跑通是因为数据量小。压测让我看到了它的临界点 —— 我的改进顺序是：先换成 `sort.Slice` 并加 `Limit` 止血（两行代码），再把打分列单独 `Select` 掉 `longtext`，最后演进到定时任务预计算 Top-N 写进 Redis ZSet，接口变成纯缓存读。"

---

### 4.2 【P0】`view_count` 自增：热点行锁

**代码**：`controllers/article_controller.go:170-176`

```go
go func() {
    global.Db.Model(&models.Article{}).Where("id = ?", id).
        UpdateColumn("view_count", gorm.Expr("view_count + 1"))   // 每次请求都写，缓存命中也写
    ...
}()
```

**证据 1 —— 单条 SQL 实测 20 秒**（服务端日志原文）：

```
2026/09/21 20:22:48 controllers/article_controller.go:172 SLOW SQL >= 200ms
[20251.164ms] [rows:1] UPDATE `articles` SET `view_count`=view_count + 1 WHERE id = '683' AND `articles`.`deleted_at` IS NULL
```

**证据 2 —— 累计慢 SQL 分类**（整个压测过程的日志统计；总数精确，分类为近似值）：

| 慢 SQL 语句 | 条数 |
|---|---:|
| `UPDATE articles SET view_count = view_count + 1 ...` | 约 **254,700** |
| `UPDATE users SET last_active_at = ...` | 约 **29,100** |
| 其它（`SELECT count(...)` 等） | 约 9,300 |
| **合计** | **293,110**（最慢 28,008ms） |

**根因**：InnoDB 的**行锁是加在索引上的**。所有请求都更新同一篇文章的同一行，等于把该行变成一个串行化热点。缓存只挡住了 `SELECT`，**没挡住 `UPDATE`** —— 这是"有缓存就不会慢"这个直觉的反例。

**为什么前面看到 P50 只有 3.86ms**：`go func()` 把 UPDATE 甩到后台了，不用等它。所以延迟指标看起来很好，但**代价是后台无限制堆积 goroutine**（article-detail@400VU 时进程 **285 个线程、807MB RSS**），以及 MySQL 行锁队列越排越长，最终反过来拖垮所有接口。

**修复**：

| 方案 | 说明 |
|---|---|
| ① 写合并 / 本地聚合 | 同一篇文章的浏览计数在本地累加，定时（如 1s）批量刷一次。N 次写 → 1 次写，热点行锁直接消失 |
| ② `UPDATE ... SET view_count = view_count + N` | 配合 ①，一次写加 N |
| ③ 浏览计数改 Redis 计数器 | 彻底不走 MySQL，定时落库 |
| ④ 至少先加有界队列 | 现在的 `go func()` 完全无背压（`middlewares/auth_middleware.go:62`、`like_controller.go:49`、`comment_controller.go:52`、`follow_controller.go:58` 同样问题） |

**顺带一个 bug**：这个 goroutine 里用了 `ctx`（gin.Context）去访问 Redis（`article_controller.go:174-175`），但响应已经返回、context 已取消 —— 这两次 Redis 调用大概率是静默失败的。

---

### 4.3 【P1】Feed 缓存击穿锁：100ms 自旋把吞吐钉死 —— ✅ 已修复

> **状态：已修复**（2026-09 的"击穿锁优化"）。自旋改成 5ms 起步指数退避到 50ms，
> 另加 owner token、重建后不立即释放锁、超时返回陈旧缓存。完整 before/after 见
> `压测报告_Feed击穿锁优化.md`，feed 接口的回归数字见本文 §3.6.4。
> 下面保留的是**问题当时的原始记录**，作为"怎么从 P95 这个数字反推出代码里的常数"的案例。

**代码**：`controllers/feed_controller.go:475-510`（当时；现在这段在 `fillFeedWithMutex`）

```go
locked, err := global.RedisDB.SetNX(feedCtx, mutexKey, "1", 3*time.Second).Result()
if locked {
    result := rebuildFeed(...)                       // 重建
    global.RedisDB.Set(feedCtx, cacheKey, data, 30*time.Second)
    global.RedisDB.Del(feedCtx, mutexKey)            // ← 立刻释放锁
    return result
}
for i := 0; i < 30; i++ {
    time.Sleep(100 * time.Millisecond)               // ← P95 = 100.89ms 的来源
    cached, err := global.RedisDB.Get(feedCtx, cacheKey).Result()
    if err == nil && cached != "" { ... return }
}
return nil                                           // ← 超时直接返回空，静默丢数据
```

**证据**：P50 = 2.75ms 与 P95 = 100.89ms 之间的断崖（§3.1）；QPS 从 50 VU 起完全不再增长。

**这其实是三个独立缺陷叠在一起：**

1. **锁在重建完成后立刻 `Del`，等于没有锁。** 下一个请求只要在临界区外到达，就会重新抢到锁、重新扫库。防击穿退化成"每次并发冲突就多扫一次库"。
2. **自旋等待是 100ms 粒度的 `time.Sleep`，不是通知机制。** 代价是真实的 CPU 时间（400 VU × 每秒 10 次唤醒）和固定 100ms 延迟。
3. **超时后 `return nil` 静默返回空 Feed** —— 用户看到白屏，而监控上看不到任何错误。

**另外 `SetNX` 的 value 恒为 `"1"`、`Del` 无条件执行** —— 锁没有 owner 标识，A 的锁 TTL 过期后 B 拿到锁，A 的 `defer` 会把 **B 的锁删掉**，第三个请求又能抢到。这个模式在 `utils/cache_protect.go:28-35` 里有一模一样的实现。

**修复**：

1. 锁 value 用 UUID，释放时校验 owner（Lua 脚本原子判断+删除）
2. **重建完成后不释放锁，让它自然过期**（或把重建结果写进缓存后再释放），避免重建风暴
3. 自旋改 `time.Sleep(5ms)` 或 Redis 发布订阅通知
4. 超时兜底返回**上一次的缓存**（哪怕过期），而不是 `nil`
5. 更好的方案：`utils/cache_protect.go:167-218` 里已经写了**逻辑过期 + 异步刷新**（`GetOrSetLogicalExpire`），这是正解 —— 但**全项目没有任何地方调用它，是死代码**。

---

### 4.4 【P1】每条参数化 SQL 实际是 3 次 MySQL 往返

**证据**（MySQL general log 抓的一次真实 `GET /api/notifications/unread-count`）：

```
Prepare    SELECT count(*) FROM `notifications` WHERE (user_id = ? AND is_read = ?) AND `notifications`.`deleted_at` IS NULL
Execute    SELECT count(*) FROM `notifications` WHERE (user_id = 13 AND is_read = 0) AND `notifications`.`deleted_at` IS NULL
Close stmt
```

**一次逻辑查询 = PREPARE + EXECUTE + CLOSE 三次服务端交互。**

**根因**：`config/config.yml:7` 的 DSN 没有 `interpolateParams=true`，`config/db.go` 里也没开 GORM 的 `PrepareStmt: true`。go-sql-driver 在 `interpolateParams=false` 时对每个带参数的查询都走"临时预处理"路径 —— 预处理、执行、立刻关闭，**执行计划完全不复用**。

**实测系数（已知请求数的标定实验）：**

| 接口 | 代码里的逻辑 SQL | 实测服务端交互/请求 | 倍数 |
|---|---:|---:|---:|
| `unread-count` | 1 | **3** | 3× |
| `users/:id` | 4 | **12** | 3× |
| `login` | 2 | **8** | 4× |
| `articles`（缓存命中） | 0 | **~0** | — |

**后果**：`GET /api/users/:id` 在 400 VU 时产生了约 **5.4 万次 MySQL 服务端交互/秒**（约 1.8 万逻辑查询/秒），是全部接口里数据库压力最大的 —— 而它只是个"查一下用户主页"。

**修复**：一行 DSN 参数或一个 GORM 配置项：

```go
// 方案 A：DSN 加参数（客户端插值，省掉 PREPARE/CLOSE）
dsn: "...&interpolateParams=true"

// 方案 B：GORM 缓存预处理语句（执行计划复用，推荐）
gorm.Open(mysql.Open(dsn), &gorm.Config{ PrepareStmt: true })
```

**注意**：`PrepareStmt: true` 会占用更多连接侧资源，需要跟连接池配置一起调。**这个改动必须在压测环境验证后再上生产。**

**面试话术**："我在压测时发现代码里写着 4 条 SQL 的接口，实测每请求产生 12 次 MySQL 交互。我开 general log 抓了一次请求，看到是 `Prepare → Execute → Close stmt`。根因是 DSN 没开 `interpolateParams`、GORM 也没开 `PrepareStmt`，所以每次查询都要三次往返且不复用执行计划。这是个一行配置的修复，但收益是**把所有带参数查询的 MySQL 往返砍掉 2/3**。"

---

### 4.5 【P1】完全没有缓存的接口

| 接口 | 代码位置 | 每请求逻辑 SQL | 实测交互 | 400VU 时 |
|---|---|---:|---:|---|
| `GET /api/users/:id` | `user_controller.go:21-30` | 4（First + 3×Count） | 12 | 失败率 9.94% |
| `GET /api/notifications` | `notification_controller.go:23-28` | 3 | 9 | — |
| `GET /api/notifications/unread-count` | `notification_controller.go:50-52` | 1 | 3 | 失败率 4.16% |
| `GET /api/articles/search` | `article_controller.go:280-288` | 3 | 9 | — |
| `GET /api/users/:id/followers` | `follow_controller.go:96-110` | **2 + N**（N 无上限） | ~60 | — |

**三个具体的坑：**

1. **`GetUserProfile` 的 3 个 `Count` 每次实时算**（文章数/粉丝数/关注数）。而 `services/bigv_fanout.go:188-205` 里**已经有** `user:%d:follower_count` 的 Redis 缓存（5 分钟 TTL），粉丝数在 `follow_controller.go:71-72` 还会被主动失效 —— **同一份数据两套逻辑，profile 接口没用上这个缓存。**
2. **`unread-count` 没有 `(user_id, is_read)` 复合索引**，`models/notification.go:7` 只有单列 `user_id` 索引。
3. **`GetFollowers`/`GetFollowing` 是标准 N+1**：`follow_controller.go:104` 在 for 循环里逐行 `First(&user, f.FollowerID)`。更糟的是 **`size` 参数没有上限**（`follow_controller.go:89-90`），`?size=10000` 会触发 1 万次串行查询。对比 `GetFeed` 是有上限的（`feed_controller.go:190-192` 强制 `size > 50 → 10`）。

**修复**：`GetUserProfile` 复用已有的 follower_count 缓存 + 上 Redis 缓存（TTL 60s）；`unread-count` 加复合索引 + Redis 缓存；`GetFollowers` 改 `WHERE id IN (...)` 批量查 + 给 `size` 加上限。

---

### 4.6 【P1】连接池配置与实际情况错配

**配置**：`config/config.yml:8-9` → MySQL `MaxOpenConns: 114` / `MaxIdleConns: 11`；`config/redis.go:12-16` → Redis **完全没配 PoolSize**。

**实测峰值：**

| 资源 | 峰值 | 上限 | 说明 |
|---|---:|---:|---|
| MySQL 连接（login@200VU） | **115** | 114 | 正好打满，失败率随之飙升 |
| MySQL 连接（article-detail@400VU） | 54 | 114 | |
| MySQL 连接（跑完稳定态） | **12** | 11 | 缩回 `MaxIdleConns` |
| Redis 连接 | **320-321** | go-redis 默认 `10 × GOMAXPROCS` = **320** | 也正好打满 |
| 进程线程数 | 22（空闲）→ **285** | — | |
| 进程 RSS | 28MB（空闲）→ **923MB** | — | |

**三个发现：**

1. **配置的意图和实际倒挂了。** config 里 MySQL 给 114、Redis 没配（默认 10×32=320）。但**代码里几乎每个接口都要走 Redis**（Auth 中间件的 `SETNX` 是所有受保护请求的必经之路），Redis 才是真正的热点。结果是 Redis 连接数（320）是 MySQL（12-115）的几十倍 —— **跟 config 想表达的容量配比正好相反**。
2. **`MaxIdleConns=11` 只有 `MaxOpenConns=114` 的 1/10**，突发流量下连接要从 11 一路新建到 114，之后又批量关闭。这是失败率非单调（0% → 19% → 2%）的直接原因。
3. **没有设置 `SetConnMaxIdleTime`**，只有 `SetConnMaxLifetime(1h)`。

**修复**：给 Redis 显式配 `PoolSize`（按实际并发定，不是默认值）；把 `MaxIdleConns` 提到 `MaxOpenConns` 的 50-100%（连接是廉价的，反复建连才贵）；补 `SetConnMaxIdleTime`。

---

### 4.7 【P2】登录：CPU 饱和 + 同一行 UPDATE

**证据**：QPS 钉死 150-156（20→200 VU 完全不动），P99 从 307ms 线性涨到 2313ms。

**两个叠加因素：**

1. **bcrypt 是故意慢的**（`utils.HashPassword`），纯 CPU 密集。150 QPS × ~100ms ≈ 15 个核打满 —— 32 核机器上的上限就是这个数量级。
2. **`auth_controller.go:63` 的 `Update("last_active_at")` 每次都更新同一行**（压测用的都是 `testuser_1`）。服务端采样显示 login 压测期间 **`Innodb_row_lock_waits` 从 32,297 涨到 41,682，平均等待 254ms**。

**修复**：登录不写 `last_active_at`（交给 Auth 中间件按节流去写，本来就是它的职责）；bcrypt cost 从默认 10 降到 8 需要谨慎评估（安全性 vs 性能）；登录接口加 IP 维度限流（`TokenBucketRateLimiter` **已实现但是全项目没有任何地方启用**，`router/router.go:24` 只有一行注释）。

---

### 4.8 【P2】其它确认存在的问题

| 问题 | 位置 | 说明 |
|---|---|---|
| **全局限流中间件没启用** | `router/router.go:24` | `TokenBucketRateLimiter` 已实现但零调用。压测期间**没有任何背压手段**，400 VU 时直接 5-17% 失败率 |
| **无界 goroutine** | `auth_middleware.go:62`、`article_controller.go:170`、`like_controller.go:49`、`comment_controller.go:52`、`follow_controller.go:58`、`feed_controller.go:275` | 每请求一条，无 worker pool、无信号量。400 VU 时线程数 22 → 285 |
| **MQ Publisher 泄漏 confirm channel** | `mq/rabbitmq.go:81` | 每次 `Publish` 都 `NotifyPublish` 注册一个监听者且从不注销；`<-confirms` 无视 ctx，MQ 卡住则 goroutine 永久挂起 |
| **熔断器基于延迟的判据永不触发** | `bigv_fanout.go:72,84` | `RecordSuccess(ctx, 0)` 把 latency 硬编码为 0，而 `circuit_breaker.go:110` 唯一的延迟判据是 `latency > timeout` |
| **`KEYS articles:list:*` 全键扫描** | `article_controller.go:57,236,259` | `KEYS` 是 O(全库 key 数) 且阻塞 Redis 单线程。写一篇文章就能卡住整个 Redis |
| **写文章后缓存全量失效** | 同上 | 每次发文把所有分页/分类缓存清空，随后是大范围冷启动 |
| **`GetComments` 无 LIMIT + 递归建树** | `comment_controller.go:76-79,100-119` | 爆款文章单请求拉全部评论；递归闭包无深度限制 |
| **死代码：最优雅的缓存方案没人用** | `utils/cache_protect.go:167-218` | `GetOrSetLogicalExpire`（逻辑过期 + 异步刷新）全项目零调用 |
| **本地缓存淘汰是 Go map 随机顺序** | `utils/local_cache.go:59` | 不是 LRU，热点 key 可能被随机淘汰；淘汰时持写锁删 5000 个键 |
| **布隆过滤器全局单例** | `utils/dedup.go:32`、`bloom_filter.go:56` | 所有用户共用 1M bits 的位数组，无扩容机制，用户量上去后假阳性率飙升 |
| **日志无节制** | `router/router.go:11` `gin.Default()` | 12k QPS 下日志增长 **40MB / 30 秒**（约 1.3MB/s）。本机 A/B 实验证明它**不是**性能瓶颈（见下），但磁盘和日志采集是运维隐患 |

**关于日志的 A/B 实验（一个诚实的 negative result）：**
我怀疑 gin 的 Logger 中间件在关键路径上，做了对照实验：服务端 stdout 分别重定向到文件 vs `/dev/null`，同一个接口同样 100 VU。
结果 A（写文件）= 8684 QPS，B（丢弃）= 7074 / 8025 QPS —— **A 反而比 B 快，差异完全落在本机 ±30% 噪声内。结论：日志 I/O 在本机不构成瓶颈。** 我把它保留在报告里，因为它同样重要：**验证过的假设，即使不成立，也比没验证的猜测有价值。**

---

## 5. 问题小结

| # | 问题 | 严重度 | 触发条件 | 影响 |
|---|---|---|---|---|
| 1 | 热榜 O(n²) 全量排序 | 🔴 P0 | 文章数 > 1 万 | 接口从 9354 QPS 崩到 15 QPS |
| 2 | `view_count` 热点行锁 | 🔴 P0 | 热点文章 + 高并发 | 单条 SQL 20 秒，慢 SQL 25 万条 |
| 3 | Feed 击穿锁 + 100ms 自旋 | 🟠 P1 | 时间线不满 10 条 | 吞吐 2700 封顶，P95 钉死 100ms |
| 4 | 每查询 3 次 MySQL 往返 | 🟠 P1 | 所有参数化查询 | MySQL 负载放大 3 倍 |
| 5 | 活跃度写路径无背压 | 🟠 P1 | 节流失效 | P99 55ms → 313ms |
| 6 | 多个接口零缓存 | 🟠 P1 | 正常流量 | `users/:id` 失败率 19% |
| 7 | 连接池配置倒挂 | 🟠 P1 | 突发流量 | 失败率非单调跳变 |
| 8 | 登录 CPU 饱和 | 🟡 P2 | 正常 | QPS 天花板 150 |
| 9 | 限流未启用 / 无界 goroutine / MQ channel 泄漏 | 🟡 P2 | 高并发 | 无背压，资源不可控 |

**贯穿所有问题的三个系统性根因：**

1. **缓存只覆盖了 SELECT，没覆盖写。** `view_count`、`last_active_at` 都在缓存命中时照写 MySQL —— 一旦是热点行，缓存等于没做。
2. **异步化只做了"甩出去"，没做"控住"。** 7 处 `go func()` 全都没有 worker pool、没有有界队列、没有失败降级。
3. **配置项写了但没被验证过。** `MaxOpenConns: 114` / 限流中间件 / `GetOrSetLogicalExpire` —— 项目里有不少"看起来做了"的东西，压测才发现它们要么没启用、要么和实际负载对不上。

---

## 6. 改进优先级建议

> ✅ = 已完成（含 2026-10-14 的推拉分层改造）

### P0 —— 止血（当天可完成，收益最大）

| 动作 | 改动 | 预期 | 状态 |
|---|---|---|---|
| `GetHotFeed` 换 `sort.Slice` + 加 `Limit(500)` + 只 `Select` 打分列 | ~5 行 | 2 万篇文章下 QPS 从 15 回到千级 | ⬜ 未做 |
| `view_count` 改本地聚合 + 定时批量刷 | ~50 行 | 慢 SQL 25 万 → 0，行锁排队消失 | ⬜ 未做 |
| DSN 开 `interpolateParams=true` 或 GORM 开 `PrepareStmt` | 1 行 | MySQL 往返砍 2/3 | ⬜ 未做 |

### P1 —— 结构性修复（1-2 天）

| 动作 | 预期 | 状态 |
|---|---|---|
| 击穿锁加 owner token + 不立即释放 + 自旋改 5ms + 超时返回旧缓存 | Feed QPS 天花板从 2700 抬到 Redis 的极限 | ✅ 见 `压测报告_Feed击穿锁优化.md` |
| **推拉分层：按粉丝量路由 + 大V只写 outbox + 读时归并** | 见 §3.6：10 万粉写侧 17 万条命令 → 4 条 | ✅ 见 `设计文档_Feed推拉分层.md` |
| **fanout 分页流式拉取（替代一次性 `Pluck` 10 万行）** | 内存恒定一个 batch，可中断可续跑 | ✅ §3.6.3 |
| **熔断器按作者隔离 + 失败进重试队列** | 一个大V不再打瘫全部小作者 | ✅ 设计文档附录 C |
| 启用 `TokenBucketRateLimiter`（并改 key 维度，现在只用 `ClientIP`） | 有背压，高并发下失败率可控 | ⬜ 未做 |
| `GetUserProfile` / `unread-count` 上缓存 + 加 `(user_id, is_read)` 复合索引 | 数据库负载降一个数量级 | ⬜ 未做 |
| `GetFollowers` 批量查 + `size` 加上限 | 消除 N+1 与无上限查询 | ⬜ 未做 |
| **MQ 消费者加 `Qos` prefetch + fanout 改有界 worker 池** | 消息不再无上限灌进内存；线程数可控 | ✅ 消费者已加；`go func()` 面还没扫完 |
| Redis 显式配 `PoolSize`，`MaxIdleConns` 提到 50-100% | 消除连接抖动 | ⬜ 未做 |

### P2 —— 演进（长期）

| 动作 | 预期 | 状态 |
|---|---|---|
| 热榜改定时预计算 Top-N → Redis ZSet | 热榜变成纯缓存读 | ⬜ 未做 |
| `GetOrSetLogicalExpire` 逻辑过期替换现有的击穿锁 | 用已经写好的正解 | ⬜ 未做 |
| 本地缓存改 LRU，按字节而非条目限容 | 避免热点 key 被随机淘汰 | ⬜ 未做 |
| 布隆过滤器按用户分片 | 假阳性率可控 | ⬜ 未做 |
| `gin.Default()` → `gin.New()` + 采样日志 | 日志从 1.3MB/s 降到可控 | ⬜ 未做 |
| 浏览/点赞计数走 Redis，定时落库 | 彻底移出 MySQL 热点 | ⬜ 未做 |
| 游标分页的**同秒边界**（`maxScore = cursor.Score - 1` 会跳过同秒未展示的文章） | 改排他边界 + 复合排序键，或 score 换毫秒 | ⬜ 未做，见 §3.6.6 |

### 改进后必须做的一件事：**回归压测**

每一个 P0/P1 改动都要用**同一套脚本**重跑一遍存档，形成 before/after 对比曲线。压测的价值不在于跑一次，在于它成了**可重复的回归门槛**。

> 这次推拉分层改造正是这条原则的实例 —— 三个实现 bug（§3.6.3）**没有一个是读代码看出来的**，
> 全是"跑一遍 + 断言副作用真的发生了"抓出来的。其中 worker 池无限弹跳那个最典型：
> 代码每一行都对，错的是一句话的语义，而且失败时**不报错、不打日志**。

### 6.1 已知但未修的：游标同秒边界

`GetFeed` 的游标上界是 `maxScore = cursor.Score - 1`，而 score 是 `CreatedAt.Unix()`（秒级）。
同一秒内如果有多篇文章，翻页时**上一页没展示完的那些会被永久跳过**。
游标里带了 `ArticleID` 但边界计算没用它 —— 这是个已知的粗糙点，改它要连前后端游标契约一起动，
所以本次没碰。详见 §3.6.6。

---

## 7. 面试追问预案

**Q：你这个 QPS 数字怎么来的？可信吗？**
> 本机压测，压测机和服务端同机，实测噪声 ±30%。所以我在报告里对结论做了分级：差异 5 倍以上的（热榜 623 倍、登录 50 倍）可信；20-50% 的差异我只当作方向性参考；单次 30 秒跑出来的"最优并发点"我明确标注不可信。要出容量数字必须压测机分离 + cgroup 限制压测进程。

**Q：为什么活跃度判断没有独立压测报告？**
> 因为它不是独立接口。写路径在 Auth 中间件里，读路径在 Feed 拉取里。我压测时是这么隔离的：写路径后台持续清 `throttle` key 强制它每次执行，读路径清空 Redis 时间线逼 Feed 走拉取。**这个隔离过程本身就让我发现了"节流失效"这个最危险的失效模式。**

**Q：两级缓存到底带来了多少收益？**
> 我要诚实说：在这次的压测里，**读路径的两级缓存没有可测量的收益**——活跃度从 70% 命中退化到 0% 命中，接口吞吐和延迟没有任何可观测变化。原因是读路径根本不是 Feed 的瓶颈，瓶颈是那个 100ms 自旋的锁。**两级缓存的真实价值在写路径**：它让 `MarkActive` 变成 0 网络 IO 的本地 map 写。但真正把写频率压下来的其实是那个 5 分钟节流，不是本地缓存。如果重做，我会先说清楚这个优先级。

**Q：压测发现的最大问题是什么？**
> 热榜。它是最能说明"压测要测什么"的例子：479 篇文章时它 QPS 9354、P99 5.5ms，是全项目最健康的接口。但我知道它是全量加载 + 冒泡排序，所以我做了放大实验——灌到 2 万篇，QPS 掉到 15，P99 13.6 秒，内存 923MB。**压测的意义不是测出今天的数字，是测出它什么时候会崩。**

**Q：那 293,110 条慢 SQL 你怎么定位的？**
> k6 只能看客户端视角。我在服务端加了两个观测：2 秒一次的进程/连接/行锁采样（BEFORE/AFTER 采样会被连接池回缩抹掉峰值，必须持续采），以及对 Gin 日志做增量分析统计 GORM 的 `SLOW SQL` 告警。慢 SQL 分类一出来就非常清楚：25 万条是 `UPDATE articles SET view_count`，2.9 万条是 `UPDATE users SET last_active_at`——一条是文章详情，一条正好是活跃度写入，两条都是**缓存命中时仍然照写 MySQL 的热点行**。

**Q：如果让你重做这个项目，你会改什么？**
> 三件事：第一，**缓存要连写一起考虑**，不能只挡 SELECT；第二，**所有异步化都要有界**，7 处 `go func()` 没有一处有 worker pool 或队列；第三，**配置项必须被验证**——我项目里的 `MaxOpenConns: 114`、限流中间件、逻辑过期缓存，都是"看起来做了"，压测才发现要么没启用要么和实际负载对不上。

**Q：大V推不动这个问题，你是怎么解决的？**
> 先量出来再改。10 万粉丝发一篇，实测 `Pluck` 拉粉丝 ID 用 **408ms**，Redis 写入吞吐 **49,000 粉丝/秒**，1 秒只推了 49%就超时降级 —— 剩下 43% 的粉丝永远收不到推送。
>
> 但结论不是"推得更快"。我算了两笔账：写侧推 1000 万粉要 **3000 万条命令**，内存侧每个用户时间线实测 **44.6KB**、1000 万全物化是 **414GB**。这两笔账在任何规模下都是**别推**更划算。
>
> 所以改成按粉丝量分层：中小V同步推（写成本几千条命令，读侧 O(1)），中大V异步推，大V**只写一份作者 outbox**（1 次 ZADD），粉丝读时归并。改造后同一个 10 万粉账号：tier C 是 `1 ZADD + 毫秒级返回`，端到端验证读者的时间线满载也照样能看到这篇文章。
>
> **但我要说清楚跟"内存省了 414GB"的区别** —— 大V走拉**不会**直接省下那 414GB，因为那些用户还会因为关注中小作者而被创建 inbox。它真正省的是**写侧**（3000 万条 → 1 条）和**发文延迟**（4 分钟 → 毫秒级）。要把内存真正降到 O(日活)，靠的是另一个正交的维度：只给活跃读者物化 inbox。

**Q：改造过程中最有价值的发现是什么？**（★ 这题最能拉开差距）
> **三个实现 bug 全是跑出来才发现的，而且都是"读代码看不出来"的类型：**
>
> ① **worker 池无限弹跳**。B 类文章投进异步池，worker 取出来执行时又走了一遍路由，判定还是 B 类，于是**又投回同一个池**。现象是：不推、不报错、日志一片空白、CPU 一直烧。代码每一行都对，错的是一句话的语义。
>
> ② **我为了修"一次拉 10 万行"写的 keyset 分页，单批 1000 行要 507ms —— 比它替换掉的 407ms 还慢。** EXPLAIN 抓出来是 GORM 的软删除条件让优化器选了 `deleted_at` 索引做 filesort。更阴的是同一个索引两副面孔：首屏查询选得对，一加游标条件就选错。
>
> ③ **保护超时让大V永远推不完**：超时当失败处理、重试从头开始，于是每轮都卡在同一处。改成"让路"（进度落 Redis、下一轮从断点续跑、不消耗重试次数）才跑通。
>
> 这三个我事后都补了回归测试。**尤其是第一个，测试断言的不是返回值，而是"`feed:timeline` 里真的多了东西"** —— 把 bug 改回去，测试立刻 5 秒超时失败。

**Q：你说改造后更快了，有多少是你的功劳？**
> 我把归属拆开了，因为不拆就是抢别人的功劳。`GET /api/feed` 从首轮的 2236 QPS / P95 100.89ms 到现在 4310 QPS / P95 6.77ms，**这 14 倍的 P95 改善主要来自"击穿锁优化"那次改动**（把固定 100ms 的自旋 sleep 改成 5ms 指数退避），不是我做的。
>
> 我这次能认领的是相对那次优化之后基线的 **+7%~+16% QPS**（20/100/400 三档），以及 §3.6 那组大V走拉的数据。**能说清楚哪部分是自己的，比报一个大数字可信。**

---

## 附录 A：脚本用法（可复现）

```bash
# 0) 依赖：k6 v2.2.0、MySQL/Redis/RabbitMQ 已启动、后端跑在 :9090

# 1) 造数据（幂等，可重复执行）
bash loadtest/scripts/prepare_data.sh

# 2) 生成 50 个用户的 JWT（活跃度写路径需要多用户才能暴露）
bash loadtest/scripts/get_tokens.sh

# 3) 单接口阶梯爬坡（20/50/100/200/400 VU × 30s）
bash loadtest/scripts/run_ramp.sh feed unread article-detail user-profile hot login articles-list

# 4) 活跃度判断 · 读路径（清空时间线，逼 Feed 走拉取）
MODE=nopush      bash loadtest/scripts/run_ramp.sh feed
MODE=nopush-cold bash loadtest/scripts/run_ramp.sh feed      # 最坏情况

# 5) 活跃度判断 · 写路径（持续清节流，强制每次执行 MarkActive）
MODE=nothrottle CLEAR_THROTTLE=1 bash loadtest/scripts/run_ramp.sh unread

# 6) 混合流量
cd loadtest/k6 && k6 run -e VUS=100 -e DURATION=60s mixed.js

# 7) 热榜瓶颈放大实验（灌 2 万篇 → 压 → 自动清理）
bash loadtest/scripts/hot_scale_test.sh

# 8) 生成报告用表格
node loadtest/scripts/build_tables.js
node loadtest/scripts/server_deltas.js
```

## 附录 B：文件索引

| 文件 | 说明 |
|---|---|
| `loadtest/k6/lib.js` | 接口注册表（13 个目标）、多用户 token 加载、结果落盘 |
| `loadtest/k6/ramp.js` | 单接口定压（带状态码分布统计） |
| `loadtest/k6/mixed.js` | 混合流量，按前端调用频率配比，按 `endpoint` tag 分别统计 |
| `loadtest/scripts/prepare_data.sh` | 造数据：关注/点赞/评论/通知/活跃标记/时间线/导出真实文章 ID |
| `loadtest/scripts/get_tokens.sh` | 批量登录生成 tokens.json |
| `loadtest/scripts/reset_state.sh` | 状态复位（warm / cold / allactive / nothrottle / nopush / nopush-cold） |
| `loadtest/scripts/run_ramp.sh` | 阶梯加压调度器 + 峰值采样 + 日志增量分析 |
| `loadtest/scripts/sample.sh` / `watch.sh` | 服务端指标采样（进程/MySQL/Redis） |
| `loadtest/scripts/hot_scale_test.sh` | 热榜扩容对比实验 |
| `loadtest/scripts/build_tables.js` | k6 JSON → Markdown 表格 |
| `loadtest/scripts/server_deltas.js` | 采样值 → 服务端每秒速率 |
| `loadtest/results/` | 原始 k6 JSON、**汇总 `summary_*.txt`**、服务端采样 `serverside_*.txt`、峰值 `watch_*.txt` |

## 附录 C：压测中踩到的坑（工程细节，面试可讲）

1. **随机文章 ID 会让结果全错。** `articles` 表 502 行但 id 稀疏到 1002，随机取一半落空 → 404 被空值缓存兜住，压出来是假数据。必须从 MySQL 导出真实的已发布 ID 列表（`data/articles.json`）。
2. **`--scan` 在 Redis 5.0 Windows 版不可用**，计数一直返回 0。改用 `EVAL` + `KEYS` 才拿到真实 key 数。
3. **Windows 上 mysql 客户端默认走 gbk**，夹具里的中文直接触发 `Illegal mix of collations`；`mysql` 输出的 CRLF 不剥掉会让 shell 算术炸掉。两处都修了（`--default-character-set=utf8mb4` + `tr -d '\r'`）。
4. **`while read | redis-cli` 会开子 shell**，循环里累加的计数器出不来 → 除零。
5. **k6 的 `Counter` 要从 `k6/metrics` 导入**，不是 `k6`。
6. **BEFORE/AFTER 采样会抹掉峰值** —— 连接池跑完就缩回 `MaxIdleConns`，每次看都是 12。改成压测期间每 2 秒持续采样才看到真实峰值（115）。
7. **`FORCE INDEX` 的名字要和建索引的地方一起改。** 这是 §3.6.3 里那个 filesort 问题的代价 —— 优化器在带游标的分页查询上会稳定选错索引（估算 `deleted_at IS NULL` 的过滤率差了数量级），只能显式指定。改索引名时 `config/db.go` 和 `bigv_fanout.go` 要一起改。
8. **两个 k6 同时在跑会互相污染。** 我用后台任务跑阶梯爬坡、同时在前台补跑剩下两档，两边抢 CPU 和被测服务，出来的数字（vus200 同时出现 1400 rps 和 3778 rps）完全不可用。**压测期间必须保证被测机上只有一个负载源** —— 这也是为什么"可比数字"比"更多数字"重要。
9. **造合成粉丝要连活跃标记一起造。** 新版 fanout 只在读者活跃时才物化 inbox（维度二），只灌 `follows` 不灌 `user:active:*` 的话，10 万粉的推送会**一条都推不出去**（表现为 `pushed=0`，看起来像 bug 其实是设计）。10 万个 key 用 `redis-cli < 命令文件` 灌约 8.5 秒。
10. **测试夹具的清理要用 `Unscoped()`。** 集成测试造完数据后用 `Delete()` 清理，但 GORM 的 `gorm.Model` 是**软删除** —— 它只写 `deleted_at`，行还在。而测试同时硬删了用作外键的 `users` 行，于是每跑一次测试就在 `follows` 里留下一批**指向不存在用户的孤儿记录**（跑了 5 次积了 50 行，导致夹具计数对不上）。压测前的 `follows` 总数对不上基线时，先查这个。

---

## 附录 D：本次新增脚本（推拉分层回归）

大V场景没有现成脚本，是把下面这段流程手工跑出来的（原始数据在 `loadtest/results/*_after*.json`）：

```bash
MYSQL="mysql --default-character-set=utf8mb4 -uroot -p*** exchangeapp"
REDIS="/c/Program Files/Redis/redis-cli.exe"
BASE=/tmp/base                      # git archive HEAD | tar -x -C /tmp/base → 旧代码基线

# 1) 造一个 10 万粉的作者（合成 follower_id，follows 上没有外键）
$MYSQL -e "INSERT IGNORE INTO users (username,password,avatar,bio,created_at,updated_at)
           VALUES ('bigv_100k','x','','',NOW(3),NOW(3));
           SELECT id FROM users WHERE username='bigv_100k';"     # → 65
$MYSQL -e "INSERT IGNORE INTO follows (follower_id,following_id,is_mutual,created_at,updated_at)
           SELECT 1000000+n, 65, 0, NOW(3), NOW(3)
           FROM (...5 张 0..9 数字表笛卡尔积...) t;"              # → 100,000 行，约 20s

# 2) 造活跃标记（维度二开着就必须造，否则推送全被过滤）
seq 1000000 1099999 | awk '{print "SET user:active:" $1 " 1 EX 604800"}' > /tmp/active_cmds.txt
"$REDIS" < /tmp/active_cmds.txt                                   # 约 8.5s

# 3) A 组：旧代码（config: big_v_threshold=10000, fanout_timeout_ms=1000）
(cd /tmp/base/EXCHANGEAPP && /tmp/base/server_old.exe > /tmp/server_old.log 2>&1 &)
curl -X POST localhost:9090/api/articles -H "Authorization: $BIGV_TOKEN" \
     -H 'Content-Type: application/json' \
     --data-binary '{"title":"...","content":"...","preview":"x","status":"published"}'
grep BigVFanout /tmp/server_old.log       # → fanout timeout after 57000/100000, degrading

# 4) B 组：新代码，tier B（tier_a_max=5000 / tier_c_min=500000）
# 5) C 组：新代码，tier C（把 tier_c_min 调到 50000 → 同一个号被路由进 outbox-only）
# 每轮跑前清掉上轮的残留，保证计数干净：
"$REDIS" --raw eval "local n=0
  for _,k in ipairs(redis.call('keys','feed:timeline:*')) do
    local id=tonumber(string.sub(k,15))
    if id and id>=1000000 then redis.call('del',k); n=n+1 end
  end return n" 0
"$REDIS" del "user:65:follower_count" "feed:authors:c" "feed:fanout:retry"   # 清粉丝数缓存和路由状态

# 6) 读路径端到端：让一个真实用户关注大V，验证 outbox 归并确实出现在主路径上
$MYSQL -e "INSERT IGNORE INTO follows (follower_id,following_id,is_mutual,created_at,updated_at)
           VALUES (13, 65, 0, NOW(3), NOW(3));"
"$REDIS" del "feed:read:13" "feed:pull:cache:13" "feed:pull:stale:13"      # 清已读，否则被过滤
curl -H "Authorization: $TOKEN_13" 'localhost:9090/api/feed?size=5'        # → [999904, 565, 564, 563, 562]
```

**清理**（跑完记得删掉合成数据，否则会影响其它压测）：

```sql
DELETE FROM follows WHERE following_id = 65;
DELETE FROM articles WHERE author_id = 65;
DELETE FROM users WHERE username = 'bigv_100k';
```

**回归压测的常规部分**（和首轮完全一样，只是加 `TAG` 区分）：

```bash
TAG=after bash loadtest/scripts/run_ramp.sh feed
MODE=nothrottle CLEAR_THROTTLE=1 TAG=afterwrite bash loadtest/scripts/run_ramp.sh unread
```
