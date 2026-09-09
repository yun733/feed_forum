# FEED_FORUM 项目面试问答 · 设计树全解

> 基于 **mattpocock/skills 的 grill-me** 方法论（220k+ star）生成：把项目的每个决策画成**设计树**，一轮轮"面试"自己。每个问题都给了**推荐答案**（锚定到真实代码 `file:line`），可直接背诵或用自己的话转述。
>
> 使用方式：先通读 R1–R7，再按最后的**答辩话术锦囊**自查。背答案不如背**决策的理由 + 权衡**。

---

## 项目一句话总览（开场 30 秒必背）

> "这是一个**类知乎的社区 Feed 论坛**（FEED_FORUM）：用户发文章、评论（楼中楼）、点赞、互相关注，系统会异步推送关注流、生成通知、计算热榜。技术栈是 **Go + Gin + GORM/MySQL + Redis + RabbitMQ**，前端 **Vue3 + TypeScript + Element Plus + Pinia + Vite**。这个项目最大的亮点是**后端在 Feed 流和可靠性上做的工程化设计**：推拉结合的 Feed、游标分页、三层去重、大V分流、熔断降级、MQ Outbox 可靠投递、缓存四大问题防护。"

---

## 设计树总览（面试官会追问的决策地图）

```
项目 (FEED_FORUM)
├── R1 技术选型与整体架构
│   ├── 为什么 Go + Gin？为什么 MySQL + Redis + RabbitMQ？
│   ├── 分层设计：controllers / services / utils / models，依赖注入怎么做
│   ├── 启动编排 & 优雅关闭
├── R2 Feed 流（最大亮点，必被深挖）
│   ├── 推模式 vs 拉模式 vs 推拉结合（为什么）
│   ├── 大V/普通用户分流（fan-out 放大问题）
│   ├── 游标分页 vs offset 深翻页
│   ├── 三层去重（布隆→Set→时间戳）
│   ├── 熔断降级（大V fanout 失败怎么办）
│   ├── 缓存击穿防护（时间线重建 SETNX 互斥锁）
│   ├── 活跃用户判定（L1 本地 + L2 Redis）
│   └── 热榜 Wilson 打分
├── R3 消息队列与可靠投递
│   ├── 为什么用 MQ / topic 交换机 / 3 队列设计
│   ├── Publisher Confirms + 消息持久化
│   ├── Outbox 模式（MQ 挂了怎么不丢消息）
│   ├── DLX 死信（⚠️ 当前无消费端，必须自曝）
│   └── 幂等与重复消费
├── R4 缓存体系与点赞/通知
│   ├── 缓存穿透/击穿/雪崩 防护（CacheProtector）
│   ├── Lua 原子点赞（为什么必须原子）
│   ├── 缓存与 MySQL 一致性
│   ├── 通知系统：异步生产、自通知跳过、前端如何拿
│   └── DAU 位图
├── R5 数据库设计与性能
│   ├── 表设计 & 关系（8 张表）
│   ├── 索引设计（配合 Feed 查询）
│   ├── N+1 问题（⚠️ GetFollowers 有 N+1，必须自曝）
│   ├── 全文搜索（MySQL FULLTEXT）
│   ├── 压测：bench_active / wrk
│   └── 连接池
├── R6 前端
│   ├── Vue3 技术栈与状态管理
│   ├── 登录态、路由守卫（⚠️ 没有）
│   ├── 通知获取方式（⚠️ 无轮询/推送）
│   └── 前后端联调 / 游标已打通；仍无路由守卫与通知推送（⚠️）
└── R7 短板自曝与改进方向（必问）
```

---

## Round 1 — 开场与技术选型

### ❓ Q1 - 请用 2 分钟介绍这个项目

**➡️ 推荐答案（45 秒版，可展开）：**

> "我做的是一个**类知乎的社区 Feed 论坛**，核心功能：用户注册登录、发文章（支持草稿/发布、分类、敏感词过滤）、楼中楼评论、点赞、互相关注；系统级能力：关注流的**异步推送**、点赞/评论/关注三类**通知**、**热榜**、搜索。后端 Go + Gin + GORM/MySQL，Redis 做缓存和 Feed 时间线，RabbitMQ 做异步解耦，前端 Vue3 + Element Plus。我重点打磨的是**后端工程化**：Feed 流用了推拉结合、大V分流、熔断降级，消息可靠投递用了 Outbox 模式，缓存做了防穿透/击穿/雪崩三件套，点赞用 Lua 脚本保证原子性。还写了一个**本地滑动窗口+Redis 两级缓存**来追踪活跃用户，并且用基准程序验证了收益。"

**💣 追问陷阱：** 说"推送"时，面试官会立刻问"为什么不用纯推 / 纯拉"——直接引到 Q2。

---

### ❓ Q2 - 为什么技术栈选 Go + Gin + MySQL + Redis + RabbitMQ？

**➡️ 推荐答案：**

> **Go：** 社区 Feed 是 IO 密集 + 高并发场景，Go 的 goroutine 模型适合大量异步任务（发文章推 feed、异步发通知、消费 MQ），部署是单二进制，启动快、资源占用小。
> **Gin：** 主流、中间件生态好、路由性能高；GORM 配 MySQL 开发效率高。
> **MySQL：** 业务数据（文章/评论/关系/通知）都是强一致、事务性的，存 MySQL；**Redis：** 热点数据（feed 时间线、点赞数、粉丝数、活跃标记）要求低延迟，走内存。
> **RabbitMQ：** 点赞落库、通知、feed 推送这些**不需要同步返回、但要求不丢**的操作，用 MQ 异步解耦 + 削峰。选 RabbitMQ 而不是 Kafka 是因为：消息量级还没到 Kafka 的规模，RabbitMQ 自带**交换机/路由键**和 **Publisher Confirms**，对业务级消息（一条通知、一次点赞）更轻量合适。

**💣 追问陷阱：** "为什么不用 Kafka？" → "Kafka 吞吐更高但运维重、消息模型偏日志流，这里每一条都是业务消息且需要确认投递，RabbitMQ 更贴合；如果日活上千万、feed 推送日志化，可以演进到 Kafka。"

---

### ❓ Q3 - 后端是怎么分层和做依赖注入的？

**➡️ 推荐答案：**

> 目录是 `controllers`（HTTP 层，参数校验+组装响应）/ `services`（业务逻辑，Feed 拉取、大V fanout、Outbox）/ `utils`（无状态工具：缓存防护、布隆、活跃窗口）/ `models`（GORM 模型）/ `mq`（RabbitMQ 封装）/ `middlewares`。
> **依赖注入：** 用全局包 `global/global.go` 持有 DB、Redis、MQ 连接。有个有意思的地方：Auth 中间件要写"用户活跃标记"，而活跃窗口实例在 Feed 服务里，两层会产生**循环依赖**，我用一个函数指针 `global.MarkActiveFunc` 在 `InitFeedServices()` 里注入，中间件只调用函数指针（`global/global.go:9-17`、`feed_controller.go:35-44`）。"

**💣 追问陷阱：** "为什么不用 ioc/container 框架？" → "项目规模小，全局包 + 启动时按序初始化足够，可读性更好；规模大了会演进到显式传入依赖。"

---

### ❓ Q4 - 启动流程和优雅关闭怎么做的？

**➡️ 推荐答案：**

> 启动顺序在 `main.go:18-44`：加载 viper 配置 → 初始化 DB（连接池 + 建表 + 建索引）→ Redis → RabbitMQ（声明交换机/队列）→ 初始化 Feed 服务（活跃窗口 + fanout + 去重）→ 启动 Outbox 扫描器 → 起 3 个 MQ 消费者 → 起 HTTP。**优雅关闭**：监听 `os.Interrupt`，先停 Outbox 扫描，再 `srv.Shutdown(5s)` 等存量请求处理完，最后关 MQ 连接（`main.go:63-86`）。这样能保证关闭时**不会丢正在处理的消息**。"

---

## Round 2 — Feed 流核心设计（最大亮点）

> 开场白背法："Feed 流我做了四件事：**推拉结合**、**游标分页**、**三层去重**、**大V分流 + 熔断降级**。这样能扛住"大V发文章一次性扇出几十万粉丝"的场景。"

### ❓ Q5 - Feed 流为什么用"推拉结合"？纯推或纯拉有什么问题？

**➡️ 推荐答案：**

> **纯推（Push / 写扩散）：** 大V发一篇文章要给所有粉丝各写一条时间线，粉丝多时 fan-out 放大，写放大严重；且僵尸号/活跃度低的用户也会被写入，浪费。
> **纯拉（Pull / 读扩散）：** 每次刷 feed 都要把关注的人的文章聚合、排序、去重，读放大，且冷启动时"读时空转"。
> **我用的推拉结合：** 发文章后**异步推送到关注者的 Redis ZSet 时间线**（`feed:timeline:{uid}`，score=时间戳），读时直接按时间线读（快）；但为了控制写放大，**只推给"活跃用户"**（用 L1 本地 + L2 Redis 判断），非活跃用户不推，等他们上线时走 **Pull 兜底**：从关注的人里**按 (status, created_at) 时间范围扫描** + 冷数据回查 MySQL 补足一页（`feed_controller.go:475-546`、`feed_pull_service.go:152-243`）。

**💣 追问陷阱：** "怎么判断活跃用户？" → 引到 Q10。"非活跃用户上线会不会漏文章？" → "上线首次读走 Pull 兜底 + 冷数据回查，不丢。"

---

### ❓ Q6 - 大V发文章怎么处理 fan-out 放大？

**➡️ 推荐答案：**

> 分两条路（`services/bigv_fanout.go:44-86`）：
> 1. **普通用户：** 粉丝全量遍历写入；
> 2. **大V（粉丝 ≥ 10000，阈值可配）：** 走 `batchFanout`，**粉丝分批（默认 1000/批）用 Redis Pipeline** 批量 `ZAdd`，每批同时 `ZRemRangeByRank` 裁剪时间线（每人的 timeline 只保留最近 500 条）+ 设置 30 天过期，避免无限增长。
> 另外 fanout 只在**作者活跃**时触发；`ConsumeFeedPush` 消费时先 `isUserActive(authorID)` 过滤，作者不活跃就不扇出（`feed_controller.go:437-451`）。配合 Q7 的熔断，能兜住极端情况。"

---

### ❓ Q7 - fanout 失败（Redis 抖动）怎么办？讲一下熔断降级。

**➡️ 推荐答案：**

> 我写了一个 **FanoutCircuitBreaker**（`services/circuit_breaker.go:13-65`），状态机 **Closed → Open → HalfOpen**：连续失败 3 次熔断（Open），冷却 30 秒进 HalfOpen 放一个探测请求，连续成功 2 次恢复 Closed。熔断时不再硬推，而是把文章标记 `feed:degraded:{id}` 24 小时，**降级为 pull-only**——用户读 feed 时从冷数据回查里把它捞出来（`bigv_fanout.go:163-185`）。同时 fanout 每批有 500ms 超时检测，超时就熔断。这样**宁可延迟可见，也不阻塞发文章主流程**。"

**💣 追问陷阱：** "熔断后的文章 24 小时怎么被看到？" → "`rebuildFeed` 里 `BigVSvc.GetColdArticles` 会把 7–30 天的冷数据按时间回查补进时间线（`bigv_fanout.go:209-228`）。"

---

### ❓ Q8 - Feed 分页为什么用游标？offset 分页有什么问题？

**➡️ 推荐答案：**

> Feed 时间线是**写多、数据会动态变化**的流式数据。offset 深翻页有两个问题：一是 `OFFSET 100000` 要扫描并丢弃前 10 万行，越翻越慢；二是期间有新文章插入时，**offset 语义漂移**，会重复或漏条。我用的**游标分页**：把 `(score, articleID)` 编码成 base64 游标（`FeedCursor`，`feed_controller.go:140-173`），下一页用 `ZREVRANGEBYSCORE` 取 `score < cursor.Score` 的记录（`feed_controller.go:228-240`），天然去重且 O(log n)。大小限制 1~50，默认 10。"

**💣 追问陷阱：** "为什么游标里要带 articleID？" → "同一 score（同一秒）可能出现多条，用 articleID 做二级排序去重，避免漏。"

---

### ❓ Q9 - Feed 为什么要三层去重？具体怎么实现的？

**➡️ 推荐答案：**

> Feed 是推拉结合，同一篇文章可能**既被 push 进时间线、又被 pull 兜底扫到**，会造成重复。我做三层去重（`utils/dedup.go:38-66`、`feed_controller.go:183-186`）：
> 1. **布隆过滤器**（`utils/bloom_filter.go:10-59`，5 个 FNV-1a 哈希，1M bits，Redis SetBit/GetBit）：O(1) 快速"否决"——布隆说没读过就一定没读过，内存省；
> 2. **Redis Set 精确确认**：布隆说"可能读过"时，去 `feed:read:{uid}` Set 里精确查，**消除布隆的假阳性**；
> 3. **时间戳兜底**：极端情况下用时间戳比对。
> 返回前 `go MarkBatchAsRead` **异步**把整页标记已读（`feed_controller.go:274-285`），不阻塞响应。

---

### ❓ Q10 - 活跃用户判定为什么用"本地 + Redis 两级缓存"？

**➡️ 推荐答案：**

> 活跃标记是**读多写多**的路径：Auth 中间件每个请求都要写"该用户活跃"，fanout/pull 每个都要读"是否活跃"。如果都打 Redis，高频下会放大网络 IO。
> 我做了 `ActiveWindow`（`utils/active_window.go:20-183`）：
> - **写路径：** Auth 中间件 `refreshLastActive` 先 `SETNX` 5 分钟节流，再通过 `global.MarkActiveFunc` 写**本地 dirty map**（0 网络 IO），由 1 秒定时任务 **Pipeline 批量 flush** 到 Redis（`user:active:{id}`，TTL 7 天），上限 5 万条，满了淘汰。
> - **读路径：** `isUserActive` 先查本地 clean map（命中 0 IO），miss 再查 Redis；Redis 故障时**降级返回 true**（宁可多推）。
> 我还写了 `bench_active/main.go` 验证：纯 Redis 写 5 万次 vs 本地 map 500 万次，本地走 0 网络 IO；单次读 78μs vs 50ns。

---

### ❓ Q11 - 热榜是怎么算的？为什么用 Wilson 打分？

**➡️ 推荐答案：**

> `GetHotFeed`（`feed_controller.go:51-133`）取 30 天内 published 文章，`calculateHotScore`（`feed_controller.go:110-133`）用 **Wilson Score**（z=1.96，n=赞+评论，N=浏览量）加权 + **时间衰减** `1/((hoursAgo+2)/gravity)`（gravity=1.8）+ 互动分（赞×1 + 评论×2）。Wilson 的优点是**能处理样本量差异**——1 赞 0 踩的样本置信度低，不会无脑排前；时间衰减保证新内容能顶上。"

**💣 追问陷阱：** "热榜计算在内存里对全量文章做，量大怎么办？" → 自曝 + 改进：目前是单机内存排序，量大后会演进成定时任务预计算 + 布隆/分段，或引入在线打分。

---

## Round 3 — 消息队列与可靠投递

> 开场白背法："MQ 这块我做的核心是**可靠投递**：Publisher Confirms + 消息持久化 + Outbox 兜底 + 幂等消费。"

### ❓ Q12 - MQ 的交换机/队列/路由键怎么设计的？

**➡️ 推荐答案：**

> 用 **topic 交换机 `zhihu.topic`**（durable，`mq/rabbitmq.go:10-19`），3 个队列：
> - `feed.push.queue` ← 路由键 `feed.push.article`：文章发布后推送 Feed；
> - `notification.queue` ← `notification.*`（通配，订阅 like/comment/follow 三类通知）；
> - `like.sync.queue` ← `like.sync`：点赞异步落库。
> 队列和消息都 **durable + DeliveryMode Persistent**，重启不丢。开启 **Publisher Confirms**（`rabbitmq.go:49-51`），发布时**阻塞等确认**，没收到 ack 返回错误走 Outbox 兜底。"

---

### ❓ Q13 - 为什么用 Outbox 模式？MQ 挂了怎么保证消息不丢？

**➡️ 推荐答案：**

> 这是我最看重的一层。`PublishWithOutbox`（`mq/rabbitmq.go:99-117`）是**统一发布入口**：正常走 RabbitMQ；**发布失败（MQ 挂/confirm 超时）时自动落 MySQL `outbox_messages` 表**（pending 状态）。`OutboxService`（`services/outbox_service.go:33-142`）每 5 秒扫一次 pending 且到重试时间的消息，**指数退避**（5s×2ⁿ），最多 10 次，超限置 failed 留痕。这样"业务操作成功"与"消息投递"解耦，**业务不因 MQ 故障回滚**，消息最多延迟、不会丢。"

**💣 追问陷阱：** "Outbox 写表和业务写库怎么保证原子？" → 自曝：目前是**代码顺序先业务后 outbox**，没有用同一事务。改进方向：把 outbox 写入和业务操作放**同一个 DB 事务**（本地消息表），这是标准做法，值得主动讲出来。

---

### ❓ Q14 - 消费端怎么做幂等和防重复？

**➡️ 推荐答案：**

> 点赞同步消费用 `FirstOrCreate`（存在即不建，`like_controller.go:146-160`），天然幂等；取消点赞用删除 + `GREATEST(count-1,0)` 防负（`like_controller.go:162-165`）。通知消费是 Create，重复消费可能产生重复通知——我通过"跳过自通知" + 消费端 ack 语义缓解，但严格幂等可加业务唯一键/消费去重表，这也是我列在改进清单里的一条。"

**💣 追问陷阱：** "RabbitMQ 默认 auto-ack 还是手动 ack？为什么？" → "手动 Ack，处理失败 Nack/不 ack 让消息重投，配合 Outbox 的 `retry_count` 区分是重发还是重投。"

---

### ❓ Q15 - DLX 死信队列怎么用的？

**➡️ 推荐答案（必须自曝的坑）：**

> 三个队列声明时都带 **DLX 参数**（`x-dead-letter-exchange: zhihu.topic.dlx` + 对应死信路由键，`mq/rabbitmq.go:59-64`），被拒绝/过期/超出长度限制的消息会进死信。这里我**主动交代一个现状**：目前只声明了 DLX 交换机，**还没有绑定死信消费者**，也就是说死信消息目前会滞留，没有做二次处理。这也是我下一步要补的：绑定死信队列 + 消费者，做**重试入原队列或人工告警**。

**💣 为什么主动自曝：** 面试官看到 DLX 基本必问"死信谁来消费"。主动承认 + 给出补救方案，比被戳穿好得多。

---

## Round 4 — 缓存体系与点赞/通知

### ❓ Q16 - 缓存穿透/击穿/雪崩分别怎么防？

**➡️ 推荐答案：**

> 我封装了 `CacheProtector`（`utils/cache_protect.go`）统一处理：
> - **击穿**（热点 key 失效瞬间大量请求打到 DB）：`GetOrSetWithLock` 用 Redis `SETNX` 互斥锁 + **double-check** + 未抢到锁的自旋等缓存（`cache_protect.go:22-102`），超时降级直接查 DB。
> - **穿透**（查不存在的 key 疯狂打 DB）：`GetOrSetWithNullGuard` 把空结果缓存成 `__CACHE_NULL__`（30 秒 TTL）（`cache_protect.go:104-141`）。
> - **雪崩**（大量 key 同时过期）：`addJitter` 给 TTL 加 **±15% 随机抖动**（`cache_protect.go:143-155`）。
> 还有一个 `GetOrSetLogicalExpire` 做**热点 key 逻辑过期 + 异步刷新**（`cache_protect.go:157-218`），读线程拿到旧值立即返回，后台线程刷新，避免缓存线程被热点打爆。文章列表/详情都应用了这套（`article_controller.go:78-184`）。"

**💣 追问陷阱：** "逻辑过期期间数据旧怎么办？" → "对一致性要求高的（如点赞数）不用逻辑过期，用互斥锁那套；逻辑过期只用于可容忍短时陈旧的热点数据。"

---

### ❓ Q17 - 点赞为什么用 Lua 脚本？为什么点赞走 MQ 异步？

**➡️ 推荐答案：**

> 点赞是**高并发 + 需要读改写原子**的操作。我用一段 **Lua 脚本**（`like_controller.go:23-37`）一次 Redis 往返完成"判断已赞/未赞 → 增减计数 → 记录用户状态"，返回 `(action, count)`。Lua 在 Redis 里是原子的，避免"先读后写"的竞态（两个人同时点赞，一个读到旧值覆盖另一个）。计数先写 Redis（`article:{id}:likes`），再**异步**发 `like.sync` 消息，由 `SyncLikesConsumer` 落 MySQL（`like_controller.go:49-57`、`130-171`）。这样**点赞的 P99 延迟很低**，MySQL 写入被削峰。读的时候优先用 Redis 计数（`article_controller.go:179-181`）。"

**💣 追问陷阱：** "Redis 和 MySQL 计数不一致怎么办？" → "以 Redis 为准做读；MySQL 是最终落库 + 兜底。若需要强一致，会改成同步写或做对账任务。另外评论点赞目前是**同步**直接更新 MySQL（`like_controller.go:85-127`），和文章点赞异步不一致——主动提出来可以展示对一致性的思考。"

---

### ❓ Q18 - 通知系统怎么设计的？前端怎么拿通知？

**➡️ 推荐答案：**

> **生产端：** 点赞/评论/关注三类事件都**异步**发 MQ（`like_controller.go:173-188`、`comment_controller.go:57-70`、`follow_controller.go:75-85`），`ConsumeNotifications` 消费时**跳过"自己给自己"的通知**（`notification_controller.go:58-93`）再落库。查询分页 + `Preload("FromUser")`；标记已读时 `WHERE id=? AND user_id=?` **防止越权改别人通知**（`notification_controller.go:36-45`）。
> **前端：** 目前**没有轮询也没有 WebSocket**，Pinia store 只在页面挂载时拉一次未读数（`store/notification.ts:8-15`）。这里是我的短板，我打算演进成**短轮询（如 30s）或 SSE/WebSocket 推送**，让未读角标实时更新。

---

### ❓ Q19 - DAU 位图是什么？

**➡️ 推荐答案：**

> 浏览文章时异步 `SETBIT dau:{date} userId 1`（TTL 48h）（`article_controller.go:170-176`），用位图做当日活跃统计，1 个用户只占 1 bit，统计活跃数用 `BITCOUNT` 一条命令。这是**空间省、统计快**的活跃指标方案。"

---

## Round 5 — 数据库设计与性能

### ❓ Q20 - 表怎么设计的？索引怎么配合查询？

**➡️ 推荐答案：**

> 8 张表（`config/db.go:29-40`、`models/*.go`）：User / Article / Comment（`parent_id` 自引用支持楼中楼）/ Follow（`follower_id+following_id` 复合唯一）/ Like（`user_id+target_type+target_id` 复合唯一）/ Notification / ExchangeRate / **OutboxMessage**（专门给 MQ 兜底）。
> 索引是**照着查询反推的**（`config/db.go:42-58`）：Feed 拉取是 `WHERE status='published' ORDER BY created_at DESC` → 建 `idx_articles_feed_scan (status, created_at DESC)`；作者主页 → `idx_articles_author_time (author_id, status, created_at DESC)`；Outbox 扫描是 `WHERE status='pending' AND next_retry_at<=now` → `idx_outbox_status_next_retry`；关注关系双向查 → 两个 follow 索引；活跃统计 → `idx_users_active`；搜索 → 全文索引 `ft_article_search(title, content)`。"

**💣 追问陷阱：** "为什么不用 `IN(2000 ids)` 批量查关注的人的文章？" → "MySQL `IN` 列表过长会放弃索引优化、产生深分页和内存排序。我改成了**时间范围扫描 + 内存过滤**（`feed_pull_service.go:67-150`）：按 `(status, created_at)` 每次取一批，内存里判断是不是关注的人，循环凑满一页，最多 10 轮。"

---

### ❓ Q21 - 有没有 N+1 问题？怎么处理的？

**➡️ 推荐答案（必须自曝 + 给出方案）：**

> 列表查询大部分用 `Preload("Author")`（如 `feed_controller.go:326`、`article_controller.go:104`）和"批量 `IN` 查文章后**按 ZSet 顺序内存重排**"（`feed_controller.go:299-344`）解决了 N+1。
> **但有一个我现在承认的 N+1**：`GetFollowers`/`GetFollowing` 是 offset 分页后**逐行查 User**（`follow_controller.go:102-110`、`133-141`）。修复方案：改成一次 `WHERE id IN (...)` 批量查 + 内存重排，和 feed 那套一样。

**💣 为什么主动自曝：** 面试官用 `Preload` 或 `IN` 或"你查过慢查询吗"必然顺藤摸到。主动承认 + 方案具体，是加分项。

---

### ❓ Q22 - 搜索怎么做的？为什么不用 ES？

**➡️ 推荐答案：**

> 用 MySQL **FULLTEXT** `MATCH(title, content) AGAINST('+q*' IN BOOLEAN MODE)`（`article_controller.go:280-288`），靠全文索引 `ft_article_search`。没有上 ES 是因为数据量还没到，FULLTEXT 零额外组件、够用。**演进路径**：数据量大/需要分词/拼音纠错时迁到 ES 或 Meilisearch，索引由 MQ 异步同步。"

---

### ❓ Q23 - 压测做了什么？结果如何？

**➡️ 推荐答案：**

> 两条线：
> 1. **wrk** + `wrk_debug.lua`：统计状态码分布、打印前 5 个 4xx/5xx 响应体，用来在压测时快速定位错误；
> 2. **bench_active**（`bench_active/main.go`）：单独写了基准程序验证"纯 Redis vs 本地滑动窗口"两级缓存的收益——写路径本地 map 走 0 网络 IO（500 万次 vs Redis 5 万次），单次读 50ns vs 78μs，批量读用 70% L1 命中率时 Pipeline 只补 miss 部分。**结论：Auth 中间件这种热点写路径收益最大，读路径更优雅。**
> 另外配了 seed 工具造 50 用户/500 文章/35 活跃用户来压（`cmd/seed/main.go`）。"

**💣 追问陷阱：** "为什么只有基准没有完整压测报告？" → 诚实："bench 是单点验证，全链路 QPS/TPS 压测我做了但没有留档，这也是我改进项——补一份可复现的压测脚本和报告。"

---

### ❓ Q24 - 连接池怎么配的？

**➡️ 推荐答案：**

> MySQL `MaxOpenConns 114 / MaxIdleConns 11 / ConnMaxLifetime 1h`（`config/config.yml:8-9`、`config/db.go:25-27`）。MaxIdle 设置成约 1/10 峰值连接，避免闲置连接占内存；ConnMaxLifetime 1h 是**防止 MySQL 侧 8 小时超时把连接断掉**，强制周期换新连接。"

---

## Round 6 — 前端

### ❓ Q25 - 前端架构和状态管理？

**➡️ 推荐答案：**

> **Vue3 + TypeScript + Element Plus + Pinia + Vite 5**（`package.json`）。页面：首页 feed（关注/最新两个 tab）、文章详情（点赞/关注/楼中楼评论）、发布/编辑、热榜、搜索、通知中心、个人中心、登录注册（`router/index.ts:13-26`）。Pinia 管 **auth**（token 存 localStorage、`isAuthenticated` computed）和 **notification**（未读数）两个 store；feed 数据在组件本地 ref。axios 单例 `baseURL:'/api'`，**请求拦截器从 localStorage 注入 Authorization**（`axios.ts:7-14`）。dev 环境 Vite 代理 `/api` → `localhost:9090`（`vite.config.ts:12-18`）。"

---

### ❓ Q26 - 登录态和权限怎么控制？

**➡️ 推荐答案（自曝短板）：**

> JWT 由后端签发（HS256，72h，`utils/utils.go:28-44`），前端存 localStorage，请求拦截器自动带 `Authorization`。**但目前没有路由守卫**，登录态是组件级判断（NavBar 用 `v-if="authStore.isAuthenticated"` 控制按钮显隐，`NavBar.vue:12-52`）。未登录点点赞会被前端拦截，但更稳妥的做法是**加 `router.beforeEach` 路由守卫 + axios 响应拦截器 401 统一跳登录**，这是我列的改进项。"

---

### ❓ Q27 - 前后端联调有什么坑/不一致？

**➡️ 推荐答案（自曝）：**

> 一个是代理端口：后端 `config.yml` 端口 9090，前端 `vite.config.ts` 代理 target 从 8080 改到 9090 对齐（这个改动就是最近修的）。
> 分页这块**已打通**：后端 Feed 游标分页 + 前端 `FeedList.vue` 关注流现在按 `next_cursor` 加载更多（不再传 `page/size`）。真正剩余的坑是**前端没有路由守卫**、**通知无轮询/推送**（仅页面挂载时拉一次未读数），见 R7。

---

## Round 7 — 短板自曝与改进方向（必问）

> 面试官几乎必问："这个项目有什么缺点？如果重做你会怎么改？"——**主动自曝 5 条 + 方案**，比被逼问好看得多。

| # | 短板（事实） | 改进方案 |
|---|---|---|
| 1 | **DLX 死信无消费者**，死信消息滞留（`mq/rabbitmq.go:59-64`） | 补死信队列 + 消费者：重试入原队列（限制次数）、最终转人工/告警 |
| 2 | **Outbox 写入和业务写库不在同一事务** | 本地消息表 + 业务操作放同一 DB 事务，保证原子性 |
| 3 | **GetFollowers/GetFollowing 存在 N+1**（`follow_controller.go:102-110`） | 批量 `IN` 查询 + 内存重排 |
| 4 | **前端无路由守卫 / 通知无轮询推送**（未读数仅挂载时拉一次） | 加 `beforeEach` 守卫、SSE/短轮询通知 |
| 5 | **无部署编排 / 无压测报告 / 单次提交** | 补 Dockerfile + docker-compose + nginx、留档压测报告、补 README 和设计文档 |
| 6 | **全局限流（令牌桶）已实现但注释未启用**（`router.go:24`） | 按环境开启，或下沉到网关层 |
| 7 | **本地无死信/L1 缓存扩容方向**：ActiveWindow 单机 5 万上限 | 多实例时按用户 ID 一致性哈希分片，L1 只缓存本实例热点 |

---

## 答辩话术锦囊

**1. 讲技术永远带"为什么 / 权衡 / 兜底"三件套**
> 错误示范："用了 Redis。" 正确示范："Feed 时间线用 Redis ZSet，因为要按时间排序+分页+裁剪；代价是内存，所以每人的时间线只保留 500 条、30 天过期，冷数据回查 MySQL 兜底。"

**2. 数字要具体**
> 阈值都背下来：粉丝 ≥10000 走大V分流、fanout 每批 1000、timeline 保留 500 条/30 天、熔断阈值连续失败 3 次/冷却 30s、Outbox 5s 扫描/10 次重试/指数退避、缓存 TTL 抖动 ±15%、活跃窗口 5 万上限/1s flush、MySQL 池 114/11/1h、JWT 72h、热榜 gravity 1.8。

**3. 五个"我主动交代"的坑，先说为敬**
> DLX 无消费者 → 补死信重试；Outbox 无事务 → 本地消息表同事务；N+1 → 批量 IN；前端无守卫/无推送 → 加守卫 + SSE；全局限流注释 → 按环境开启。

**4. 被问"这不是课程设计吗"的破局句**
> "核心不是 CRUD，是**在高并发下的三件事**：Feed 流的写放大与读放大权衡（推拉结合 + 大V分流 + 熔断）、消息可靠投递（Outbox + confirms + 幂等）、缓存四大问题防护。这三块都是真实系统会踩的坑。"

**5. 反问环节（结尾）**
> - "这个岗位最看重候选人的什么能力？"
> - "团队现在的 Feed/社区业务遇到的最大技术挑战是什么？"
> - "如果我有幸入职，前三个月最希望我解决的 tech debt 是哪个？"

---

## 附录 · 全站前后端全链路流程（第 0–6 节）

> 这一节回答"这个全站项目的前后端全链路流程"：把**浏览器点一下 → HTTP 请求 → 后端中间件/控制器 → 异步 MQ → 数据落到 MySQL/Redis** 的每一条链路按顺序串起来。可作为 R1–R7 之外的"横向贯穿复习"，也可直接用于"请讲一条完整的全链路"这类开场题。

---

### 0. 一句话总览

> 一个 **Go(Gin) 后端 + Vue3 前端** 的"仿知乎/微博"社交应用。前端负责渲染与交互，所有数据走 `/api` REST 接口；后端用 **MySQL 存事实、Redis 扛热点（时间线/计数/缓存/去重/活跃标记）、RabbitMQ 做异步解耦（Feed 写扩散、点赞落库、通知）**；Feed 流是核心，采用 **推拉结合 + 大V 分治 + 游标分页 + 三层去重 + 熔断降级** 的高性能方案。

```
[Vue3 SPA :5173]  axios(baseURL=/api) ── vite proxy ──▶ http://localhost:9090
                                                            │
   Browser ─ Vue Router(路由) ─ Pinia(store) ─ axios拦截器(带JWT)
                                                            ▼
                                             ┌────────────────────────────┐
                                             │   Gin Engine(9090)          │
                                             │  CORS + [TokenBucket限流*]   │
                                             │    分组路由 (router)          │
                                             │      └─ AuthMiddleware        │
                                             │           ▼                   │
                                             │      Controllers              │
                                             │      Services / Utils         │
                                             └────┬────────┬────────┬───────┘
                                     MySQL(事实) Redis(缓存) RabbitMQ(异步)
                                    8+表含outbox │ 时间线/计数/布隆   │ topic交换机"zhihu.topic"
                                 活跃标记/锁/空值 │ 活跃标记/锁/空值  │ 3队列(Feed/Notify/LikeSync)
                                                └──────────────┴──▶ 3个后台消费者goroutine
```

**技术栈**：前端 `Exchangeapp_frontend/` = Vue3 + TS + Vite + Element Plus + Pinia + Vue Router + axios；后端 `EXCHANGEAPP/` = Go + Gin + GORM(MySQL) + go-redis + amqp091 + JWT + bcrypt + viper；中间件 = MySQL8、Redis、RabbitMQ。

---

### 1. 基础设施（先答这层）

#### 端口 / 代理
| 项 | 值 |
|---|---|
| 后端监听 | `:9090`（`EXCHANGEAPP/config/config.yml`）|
| 前端 dev | `:5173`，`vite.config.ts` 把 `/api/*` 透传后端 9090，CORS 只放行 `localhost:5173` |
| MySQL | 库 `exchangeapp`；GORM AutoMigrate 全表 + 手工建索引（`config/db.go:29-58`）|
| Redis | `localhost:6379 db0` |
| RabbitMQ | `amqp://guest:guest@localhost:5672/` |

#### 数据表（`models/*`）
`users` / `articles` / `comments`（`parent_id` 自引用楼中楼）/ `follows` / `likes` / `notifications` / `exchange_rates` / `outbox_messages`。
索引配合查询：articles 的 FULLTEXT `ft_article_search(title,content)`、`(status,created_at)`、`(author_id,status,created_at)`；users `last_active_at`；follows 双向；outbox `(status,next_retry_at)`。

#### 路由总表（`router/router.go:12-83`）
- 公开：`POST /api/auth/login|register`、`GET /api/exchangeRates`；
- 其余**全部挂 `AuthMiddleware`**（浏览/操作都要求登录态）：文章 CRUD/搜索、点赞、评论、关注、`/feed`、用户、通知。
- 预留 Redis **令牌桶限流**中间件（`middlewares/ratelimit_middleware.go`），路由中注释未启用。

---

### 2. 后端进程启动时序（`main.go:18-44`）

```
main()
├─ config.InitConfig()         读 config.yml
├─ config.InitDB()             GORM 连 MySQL + AutoMigrate 建表 + 建索引(含FULLTEXT)
├─ config.InitRedis()          ping Redis
├─ mq.InitRabbitMQ()           topic交换机"zhihu.topic" + 3队列 + Publisher Confirms
├─ controllers.InitFeedServices()    Feed 三件套全局服务
│    ├─ ActiveWindow  本地滑动窗口(1s批量flush Redis)
│    ├─ global.MarkActiveFunc = ActiveWin.MarkActive    ← 注入，破 Auth↔Feed 循环依赖
│    ├─ FeedPullSvc / BigVSvc(带熔断器) / FeedDedupSvc(布隆+Set)
├─ NewOutboxService(5s).Start()      定时扫 outbox_messages 重试
├─ mq.OutboxFallbackFunc=outbox.Enqueue  MQ发消息失败的本地落库兜底
├─ 起3个消费者: ConsumeFeedPush / ConsumeNotifications / SyncLikesConsumer
├─ router.SetupRouter() + http.Server :9090
└─ 优雅关闭：信号→停Outbox→srv.Shutdown(5s)→关MQ连接   (main.go:63-86)
```

---

### 3. 一次受保护请求的后端通用生命周期

```
Gin路由 → CORS → AuthMiddleware → Controller → (Service / Redis / DB) → JSON响应
```

**AuthMiddleware**（`middlewares/auth_middleware.go:14-36`）：
1. 取 `Authorization` 头 → `utils.ParseJWT`（HS256，自动剥 `"Bearer "` 前缀）→ `userID/username` 注入 gin Context；
2. `refreshLastActive`（`auth_middleware.go:43-67`）活跃节流：`SETNX user:active:throttle:{id}`（5 分钟）去重 → `MarkActiveFunc` 只写**本地 dirty map**（0 网络 IO，1s 批量 flush 到 Redis `user:active:{id}`，TTL 7 天）→ goroutine 异步 UPDATE MySQL `last_active_at`（持久化兜底）。

> 面试点：活跃状态 = **进程内本地窗口(L1) + Redis(L2) + MySQL 异步持久化**三层，写路径尽量 0 网络 IO（`utils/active_window.go`）。

---

### 4. 黄金全链路走查（一条主线贯穿全部核心组件）

> 用户 A **发文章 → 粉丝 B 拉 Feed → B 点赞 → B 收到通知**，一镜到底。

#### 4.1 发文章（前端 → Controller → MQ）
1. 前端 `ArticleEditor.vue submitArticle` → `POST /api/articles`，axios 拦截器自动带 `Authorization`（`src/axios.ts:7-14`）；
2. 后端 `CreateArticle`（`controllers/article_controller.go:32-67`）：取 userID → **DFA 敏感词过滤** `ad/spam`（`utils/sensitive_words.go`）→ 默认 `published` → `Db.Create` 落库 + `Preload("Author")` → 模糊删列表缓存 `articles:list:*` → 发布态则 `go pushArticleToFeed(article)`；
3. `pushArticleToFeed` → `mq.PublishWithOutbox(routingKey="feed.push.article", body=整篇JSON)`：走 topic 交换机 + **Publisher Confirms**，失败自动落 MySQL `outbox_messages` 兜底（`mq/rabbitmq.go:80-117`）；
4. 响应前端 200。

#### 4.2 后端消费 + 写扩散（Fanout）
`ConsumeFeedPush`（`controllers/feed_controller.go:368-418`）手动 Ack 消费：
1. 反序列化 article；
2. `isUserActive(作者ID)`：ActiveWindow L1 → Redis L2；**作者不活跃直接 Ack 跳过**（该文交给"拉"路径兜底）。作者刚发文章必然刚被标活跃，正常推进；
3. `BigVSvc.FanoutArticle(article)`（`services/bigv_fanout.go:44-86`）：
   - 先查熔断器 `Allow()`：熔断开启 → 标 `feed:degraded:{id}`（24h）降级 pull-only；
   - 查粉丝数（Redis 缓存 5min → MySQL count）vs `big_v_threshold=10000` 自动分流；
   - 普通/大V 都走 `batchFanout`：粉丝按 1000/批 **Pipeline** 对每人 `ZADD feed:timeline:{fid}`（score=文章时间戳）+ `ZRemRangeByRank` 只留 500 条 + `EXPIRE` 30 天；
   - 单批超时（500ms）→ `RecordFailure`，连失 3 次熔断（CLOSED→OPEN，30s 冷却→HALF_OPEN 放一个探测→连成 2 次恢复，`services/circuit_breaker.go`）；
4. 完成 `msg.Ack`。

#### 4.3 粉丝 B 拉 Feed（读路径 = 推拉结合 + 游标）
前端 `FeedList.vue` **关注 Tab** 现已走游标：首页不带 cursor 拉第一页，点"加载更多"带上一次返回的 `next_cursor` 翻页（不再传 `page/size`）；**最新 Tab** 保持 offset 页码打 `/articles`。后端 `GetFeed`（`controllers/feed_controller.go:187-293`）：
1. 取用户已读 ID 集合（`feed:read:{uid}` Redis Set → map）；
2. **Phase 1 · Push 热路径**：读 `feed:timeline:{uid}` ZSet，首屏 `ZREVRANGE 0..size-1`，带 cursor 则 `ZREVRANGEBYSCORE` 取 `score < cursor.Score`（游标 = base64(`score:articleID`)，`FeedCursor`，`feed_controller.go:140-173`）；候选 ID → 批量 `SELECT id IN (...) status='published'` 回 MySQL，按 ZSet 顺序重排（`fetchArticlesByIDs`，`feed_controller.go:299-344`）、`Preload("Author")`、**剔除已读**；
3. **Phase 2/3 · Pull 兜底**：不足 size 时 `fillFeedWithMutex`（`feed_controller.go:475-510`）用 `SETNX feed:mutex:timeline:{uid}` **互斥锁防击穿**（3s），抢到锁者执行 `rebuildFeed`：
   - **Phase2** `FeedPullSvc.PullFromInactiveUsers`（`feed_pull_service.go:152-243`）：关注列表（本地缓存 `following:{uid}`）→ `FilterInactive` 找**7 天不活跃的被关注作者** → 时间倒序分片扫 `(status,created_at)`，内存过滤凑满；
   - **Phase3** `BigVSvc.GetColdArticles`（`bigv_fanout.go:209-228`）：MySQL 补最近 30 天 `author_id IN (关注)` 的"冷数据"（大V降级/漏推的洞）；
   - 结果写 `feed:pull:cache:{uid}`（30s）后放锁；没抢到锁的自旋 100ms×30 次读缓存，超时降级空返回；
4. 组装 `items + next_cursor + has_more`；
5. **异步整页标已读** `FeedDedup.MarkBatchAsRead`（`feed_controller.go:274-285`）：布隆 `feed:bloom:read` + Set `feed:read:{uid}`（TTL 7 天），key=`{articleID}:{createdAt秒}`。

> **三层去重**（`utils/dedup.go:38-66`）：①布隆说"没读过"→ 新文章直接放行；②布隆说"可能读过" → Set 精确确认（消假阳性）；③Set 里有 → 真读过，过滤。布隆快、Set 准、时间戳 key 唯一兜底。

#### 4.4 B 点赞
1. 前端详情页 `toggleLike` → `POST /api/articles/{id}/like`；
2. `LikeArticle`（`controllers/like_controller.go:16-63`）用 **Redis Lua** 原子 toggle（`article:{id}:likes` + `article:{id}:liked:{uid}`，EXPIRE 30 天），毫秒返回 `{liked, like_count}`；
3. goroutine 发 MQ `like.sync`（`PublishWithOutbox` 兜底）；
4. `SyncLikesConsumer`（`like_controller.go:130-171`）消费 → MySQL `FirstOrCreate` Like 记录 + `like_count±1`（GREATEST 防负）→ 发 `notification.like`；
5. `ConsumeNotifications` 消费 → 组装 `Notification` 落库（跳过自通知）。

#### 4.5 B 收到通知
- 前端 `NavBar` 挂载时 `fetchUnreadCount` → `GET /api/notifications/unread-count`（**无 WS/SSE，仅挂载拉一次**，见 R6 短板）；
- `NotificationView` → `GET /api/notifications` 分页 + `Preload("FromUser")`；点击未读 → `PUT /notifications/{id}/read`（`WHERE id AND user_id` 防越权）→ 按 type 跳转。

---

### 5. 其余业务链路端到端要点

- **注册/登录/鉴权**：前端 `Login/Register.vue` → Pinia `authStore` → `POST /api/auth/login|register`；后端 bcrypt 存/校验 → 签发 JWT（HS256、`{user_id,username}`、72h、自带 `Bearer ` 前缀，`utils/utils.go:28-44`）→ 前端存 localStorage，拦截器自动带头。
- **文章列表/详情 = 缓存三件套演示**：列表 `GetArticles`（`article_controller.go:78-132`）用 `CacheProtector.GetOrSetWithLock`（key `articles:list:{page}:{size}:{category}`，`SETNX cache:lock:` 互斥防击穿 + TTL±15% 抖动防雪崩）；详情 `GetArticleByID`（`article_controller.go:135-184`）用 `GetOrSetWithNullGuard`（不存在也缓存空值 `__CACHE_NULL__` 30s 防穿透），命中后异步 `view_count+1` + `SETBIT dau:{date}` 记 DAU，点赞数改从 Redis 计数读。
- **热榜**：`GetHotFeed`（`feed_controller.go:51-133`）= 近 30 天 published → **Wilson Score(z=1.96)** + **时间衰减** `gravity/(hoursAgo+2)` + 互动分 → 内存排序分页（详见 Q11）。
- **评论**：`CreateComment`（`comment_controller.go:14-55`）落库 + `comment_count+1` + goroutine 发 `notification.comment`；读时一次查全量**内存组楼中楼树**；评论点赞 `LikeComment`（`like_controller.go:85-127`）走 Redis Lua + **同步**更新 MySQL（与文章点赞异步不同）。
- **关注/取关**：`FollowUser`（`follow_controller.go:17-85`）查存则删、否则建，双向检测置 `is_mutual`；变更后**失效缓存**：本地 `following:{uid}` + Redis `user:{id}:follower_count`；goroutine 发 `notification.follow`。
- **搜索**：`SearchArticles`（`article_controller.go:266-296`）MySQL FULLTEXT `MATCH(title,content) AGAINST('+q*' IN BOOLEAN MODE)`。
- **汇率(demo)**：唯一公开 GET `exchangeRates` 全量查表，前端本地换算；写接口带鉴权。

---

### 6. 复用清单（面试加分速记）

**Redis Key 汇总**
| 用途 | Key 形态 |
|---|---|
| 关注时间线(ZSet) | `feed:timeline:{uid}` |
| 已读精确去重(Set) | `feed:read:{uid}` |
| 已读布隆 | `feed:bloom:read` |
| 拉路径缓存/互斥锁 | `feed:pull:cache:{uid}` / `feed:mutex:timeline:{uid}` |
| 大V降级标记 | `feed:degraded:{articleID}` |
| 活跃用户/节流 | `user:active:{uid}`(TTL7d) / `user:active:throttle:{uid}` |
| 粉丝数 | `user:{id}:follower_count` |
| 点赞 | `article:{id}:likes` / `article:{id}:liked:{uid}` |
| 列表/详情缓存与锁 | `articles:list:*` / `article:detail:{id}` / `cache:lock:*` |
| DAU 位图 | `dau:{yyyy-MM-dd}` |
| 关注列表本地缓存(进程内) | `following:{uid}` |

**RabbitMQ 拓扑**（topic 交换机 `zhihu.topic`，durable + Publisher Confirms + DLX）
| 队列 | 路由键 | 消费者 | 作用 |
|---|---|---|---|
| `feed.push.queue` | `feed.push.article` | ConsumeFeedPush | 文章写扩散到粉丝时间线 |
| `notification.queue` | `notification.*` | ConsumeNotifications | like/comment/follow → 落库通知 |
| `like.sync.queue` | `like.sync` | SyncLikesConsumer | Redis 点赞计数异步落 MySQL |

**可靠性 / 性能模式**（对应到代码逐一可讲）
1. **Outbox 本地消息表兜底**（MQ 故障不丢消息，5s 扫描 + 指数退避，`services/outbox_service.go`）；
2. **Publisher Confirms + 消息持久化 + DLX 死信**；
3. **Feed 推拉结合 + 大V 分治 + 熔断降级**（超阈值分 1000/批 Pipeline；超时/连失→熔断→pull-only→半开恢复）；
4. **缓存击穿/穿透/雪崩三防**（互斥锁 + 空值缓存 + TTL 抖动，另备逻辑过期）；
5. **三层去重**（布隆快筛 + Set 精判 + 时间戳唯一键）；
6. **游标分页**（ZSet score 滑动，避免深翻页）——前端关注流已按 `next_cursor` 打通；
7. **活跃用户本地滑动窗口批量 flush**（写 0 网络 IO，读两级）；
8. bcrypt 散列、DFA 敏感词 O(n) 过滤、JWT 无状态鉴权、令牌桶限流中间件（预留）。

---

*生成工具：mattpocock/skills `grill-me`（GitHub 220k+ star，面试式压力测试方法论）。所有代码锚点基于仓库当前工作区内容，背诵前建议对照代码快速过一遍。*
