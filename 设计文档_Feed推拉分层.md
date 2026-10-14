# Feed 推拉分层设计（大V场景优化）

> 前置阅读：`压测报告_FEED_FORUM.md`（瓶颈定位）、`压测报告_Feed击穿锁优化.md`（读路径优化）
> 本文所有实测数字都来自本机真实代码路径，采集方式见附录 A
> 状态：**已实施（四步全部落地）** —— 逐条对照见附录 C
> ⚠️ 实施后本文描述的是**现状**，不再是「计划」。文中的「改造前」一律指改造前的代码。

---

## 0. 一句话

**把「推还是拉」从「超时后的被动降级」改成「写之前按粉丝量和活跃度做的确定性路由」。**

现在的行为是「推给所有粉丝，超 1 秒就中止并降级」——决策信号是运行时耗时，同一篇文章两次发布结果可能不同。改成按 `followerCount` 和 `isActive` 这两个纯函数路由，行为可预测、可测试、可解释。

---

## 1. 现状：设计意图 vs 实际行为

### 1.1 设计意图（来自项目内注释与面试文档）

| 层 | 意图 |
|---|---|
| 小V | 全量推送到粉丝 Timeline |
| 大V | 走 `BigVFanoutService` 分批次推送，防单次写入阻塞 Redis |
| 推失败 | 熔断 + 降级为 pull-only（`feed:degraded:<id>`，24h） |

### 1.2 实际行为（读代码 + 实测验证）

**① 大V通道和普通通道是同一个函数。**

```go
// bigv_fanout.go:46-86  FanoutArticle
if followerCount >= int64(threshold) {
    count, err := s.batchFanout(article)   // 「大V通道」
    ...
}
count, err := s.normalFanout(article)      // 「普通用户」
// bigv_fanout.go:158-160
func (s *BigVFanoutService) normalFanout(article models.Article) (int, error) {
	return s.batchFanout(article)          // ← 同一个实现
}
```

两个分支最终都是 `batchFanout`（`bigv_fanout.go:89`）。「分批次」也是它内部统一的（`fanout_batch_size: 1000`）。**「大V走特殊通道」目前只贡献了一行日志。**

所以真实行为等价于：**推给所有粉丝，超 1 秒中止并降级。**

**② 实测验证（10 万粉丝，真实 MQ → 消费者 → fanout 链路）**

```
SLOW SQL >= 200ms
[408.456ms] [rows:100034] SELECT `follower_id` FROM `follows` WHERE following_id = 13 ...

[BigVFanout] Batch fanout failed for big-V article 37866: fanout timeout after 49000/100034 followers, degrading
[FeedConsumer] Article 37866 degraded to pull-only mode
```

| 指标 | 实测值 |
|---|---|
| `Pluck` 拉 10 万行粉丝 ID | **408ms**（已触发慢 SQL 告警） |
| 1 秒内实际推送 | **49,000 / 100,034 = 49%** |
| 实测 fanout 吞吐 | **49,000 粉丝/秒** |
| 剩余 51,034 个粉丝 | 未推送，标记 `feed:degraded` |

**独立交叉验证**：用 `redis-benchmark` 按真实 pipeline 形状（深度 3000 = 1000 粉丝 × 3 命令）测得 ZADD 111,500 / ZREMRANGEBYRANK 213,000 / EXPIRE 188,000 ops/s，折算 **52,766 粉丝/秒**——与真实链路的 49,000 相差 7%，两个方法互相印证。

---

## 2. 问题清单

| # | 问题 | 位置 | 严重度 | 性质 |
|---|---|---|---|---|
| P1 | **降级是概率性的**：1 秒够就推完、不够就推一半，取决于当时 Redis 多忙。用户 A 看到、用户 B 没看到，不可复现、无法解释 | `bigv_fanout.go:149` | 🔴 | 可用性 / 一致性 |
| P2 | **大V失败会打瘫全局**：一个大V连续失败 3 次 → 熔断器 OPEN 30 秒 → 这 30 秒内**所有文章**（含小作者的）全部降级 | `circuit_breaker.go:59,62` + `bigv_fanout.go:66-84` | 🔴 | 可用性 |
| P3 | **`Pluck` 的错误被吞掉**：查询失败 → 空切片 → `return 0, nil`，看起来推送成功实际一条没推，且不触发熔断 | `bigv_fanout.go:91-97` | 🔴 | 正确性 |
| P4 | **单次拉全部粉丝 ID 进内存**：1000 万行 = 80MB+ 切片，实测 10 万行 408ms → 1000 万行约 41 秒，且不可中断 | `bigv_fanout.go:91-93` | 🟠 | 内存 / 延迟 |
| P5 | **降级标记是死代码**：`feed:degraded:<id>` 写了但全仓库无人读，`IsDegraded` / `RetryDegradedArticle` 零调用 | `bigv_fanout.go:163-185` | 🟠 | 正确性 |
| P6 | **熔断器的延迟判据永不触发**：`RecordSuccess(fanoutCtx, 0)` 把耗时硬编码成 0，`latency > timeout` 恒为 false | `bigv_fanout.go:72,84` + `circuit_breaker.go:110` | 🟡 | 正确性 |
| P7 | **单消费者串行 + prefetch 无上限**：一条 4 分钟的 fanout 阻塞整个队列；`ch.Consume` 未设 `Qos`，消息持续灌进内存 | `feed_controller.go:370,383` | 🟠 | 吞吐 / 内存 |
| P8 | **阈值无推导依据**：`big_v_threshold: 10000` 是拍脑袋值，不与任何容量预算挂钩 | `config/config.yml:20` | 🟡 | 可维护性 |

**P1 和 P2 是这篇文章最该记住的两条**——它们不是性能问题，是**可用性**问题。QPS 低可以扩容，行为不可预测没法扩容。

---

## 3. 设计目标与约束

### 目标

| 目标 | 衡量方式 |
|---|---|
| G1 发文延迟可控 | 1000 万粉账号发文，fanout 阶段从「4 分钟」变成「毫秒级」 |
| G2 行为确定性 | 同一篇文章、同一个粉丝量，路由结果恒定，不随时机波动 |
| G3 故障隔离 | 大V的失败不影响小作者的推送 |
| G4 内存可预算 | 内存占用有明确公式，可反推阈值 |
| G5 用户无感知 | 迁移过程中不出现文章「消失」或「重复」 |

### 约束

- 保留推拉结合架构（纯拉在读多写少 + 中长尾作者多的场景下更差，见 §6.1）
- 读路径复用已有的 `FeedDedup` 三层去重与 `rebuildFeed` 的 Phase 结构
- 单机 Redis，无 Redis Cluster（设计要能在单机成立，集群只是放大器）

---

## 4. 目标设计

### 4.1 核心：三个正交的路由维度

现在的路由只有一个维度（超时），且是被动触发。改成**两个主动维度**：

```
维度 1：作者粉丝量  →  决定「写侧做多少」
维度 2：读者活跃度  →  决定「要不要物化 inbox」
```

组合出四个象限：

|  | 活跃读者 | 非活跃读者 |
|---|---|---|
| **A/B 类作者**（中小V） | 推送（物化既有 timeline） | **不推**，回来时按需重建 |
| **C 类作者**（大V） | 不推，读时归并 outbox | 不推，读时归并 outbox |

**这个表就是整套设计。** 下面逐块展开。

#### 4.1.1 术语先统一（重要，不然后面会读混）

| 术语 | Redis key | 归属 | 装什么 |
|---|---|---|---|
| **timeline** = **inbox** | `feed:timeline:<uid>` | **每个用户一个** | A/B 类作者推给他的文章 |
| **outbox** | `author:outbox:<authorID>` | **每个 C 类作者一个，全局共享** | 该大V自己发的文章 |

> ⚠️ **`timeline` 和 `inbox` 是同一个东西**——前者是现有代码里的命名（`feed:timeline:<uid>`），后者是行业通用叫法。本文档统一用 `timeline`，只在讲行业对照时说 inbox。
>
> **一个读者实际只有两个数据源**：自己的 1 个 timeline + 他关注的那几个 C 类作者的 outbox（K 个，且这 K 个是和所有人共享的，不是他自己独占）。
>
> **两者的内容不相交**：一篇文章要么是 A/B 类作者发的（推 → 进 N 个粉丝的 timeline），要么是 C 类作者发的（不推 → 只进作者自己的 outbox）。**同一篇文章不会同时在两边。**

### 4.2 维度一：按粉丝量路由

| 层级 | 粉丝量 | 策略 | 理由 |
|---|---|---|---|
| **A** | < T1（建议 5,000） | 同步推 | 写成本 ≤ 1.5 万条命令，秒级完成；读侧 `ZREVRANGE` O(1) |
| **B** | T1 ~ T2（建议 5,000 ~ 50 万） | **异步延迟**推 | 写成本仍可控，但不阻塞发文主流程 |
| **C** | ≥ T2（建议 50 万） | **不推**，写作者 outbox | 推的边际收益已归零 |

**决策点从 `batchFanout` 内部提到 `FanoutArticle` 甚至是发文 handler 里**：

```go
// 现在的结构（简化）
func FanoutArticle(article) {
    followerCount := getFollowerCount(article.AuthorID)
    if followerCount >= threshold {
        batchFanout(article)        // 大V：推（会超时降级）
    }
    normalFanout(article)           // 普通：推（同一个函数）
}

// 目标结构
func Route(article) RouteDecision {
    followerCount := getFollowerCount(article.AuthorID)
    switch {
    case followerCount >= T2:
        return PullOnly          // 只写作者 outbox，不碰粉丝 inbox
    case followerCount >= T1:
        return AsyncPush         // 进延迟队列，不阻塞发文
    default:
        return SyncPush          // 直接推
    }
}
```

**超时保留，但语义变了**：不再充当「降级触发器」，只作为纯粹的保护机制（防止一条消息永久阻塞消费者）。因为路由已经在写之前按粉丝量决定完了，超时命中就说明是**异常**（Redis 抖动），应该告警而不是"静默换个策略"。

### 4.3 维度二：只为活跃读者物化 inbox

这是**内存维度**，和维度一完全正交。

现在的行为：fanout 会给**所有**粉丝写 inbox，无论这人今天有没有打开 App。内存成本 = `被推送到的用户总数 × 单时间线字节数`。

改成：**非活跃用户不推**，他们的 Feed 读走纯拉（读时归并作者的 outbox），首次回来时按需重建 inbox。

判断"活跃"正好是 `ActiveWindow.IsActive()` 能回答的问题（`utils/active_window.go`）——**你项目里已经有这个模块了，它现在是给 Feed 按需过滤用的，这里可以复用成 fanout 的分流器。**

> ⚠️ 但要注意：`ActiveWindow` 的 L1 本地缓存目前**没有 TTL 校验**（只检查 key 是否存在，从不检查时间戳），用户访问过一次就被永久判为活跃。要做 fanout 分流器必须先修这个，否则「非活跃用户」永远是空集，这个维度整条失效。修法见 `压测报告_Feed击穿锁优化.md` §3.2。

### 4.4 C 类走拉：数据结构与读路径

**数据结构——关键是 outbox 全局共享：**

```
author:outbox:<authorID>    ZSet，score = 发文时间戳，member = articleID
                            TTL 与 timeline_ttl_days 一致
```

**一份，不是每人一份。** 一个 1000 万粉的账号，Redis 里只有这一个 key。

**读路径——好消息是插槽已经存在。** `rebuildFeed`（`feed_controller.go:691`）现在的结构：

```go
// Phase 2: Pull 非活跃用户
pulled, err := FeedPullSvc.PullFromInactiveUsers(userID, need, excludeIDs)   // :696

// Phase 3: 冷数据回查
cold, err := BigVSvc.GetColdArticles(userID, followingIDs, need-len(result), coldBefore)  // :712
//   ↑ GetColdArticles 做的是 author_id IN (followingIDs) 的 MySQL 查询
```

**Phase 3 的语义就是「按关注列表回查近期文章」——正好是 C 类 outbox 归并要做的事。** 把 MySQL 换成 Redis：

```
读路径（新增 Phase 2.5）：
1. bigVs = 我关注的 C 类作者列表        （本地缓存，变更频率极低）
2. pipeline: ZREVRANGEBYSCORE author:outbox:<cid> × K    （1 次 RTT）
3. 与 Phase 1/2 的结果做时间序归并
4. 过 FeedDedup 三层去重（已有，直接复用）
```

成本对比：

| | 现在（`GetColdArticles`） | 目标（outbox 归并） |
|---|---|---|
| 存储 | MySQL，`author_id IN (...)` + 时间范围 + `Preload("Author")` | Redis ZSet |
| 每请求 DB 查询 | 2（含 Preload） | 0 |
| 往返 | 1 次 MySQL | 1 次 Redis pipeline（K 个 key） |
| 实测代价 | 见 `压测报告_FEED_FORUM.md` §4.4：每逻辑查询 = Prepare+Execute+Close 三次往返 | 1 次 RTT |

#### 4.4.1 「多查一个 outbox，是不是白多一次往返？」

**不是，因为读路径的几次 Redis 调用可以合并成一次 pipeline。** 一次 Feed 读需要从 Redis 拿三样东西：

```
① feed:read:<uid>            SMEMBERS      —— 已读集合（用于过滤）
② feed:timeline:<uid>        ZREVRANGEBYSCORE —— 我自己的 timeline
③ author:outbox:<cid> × K    ZREVRANGEBYSCORE —— 我关注的 K 个大V的 outbox
```

这三者**互相不依赖**（③ 只需要"我关注了哪些大V"，那个列表走本地缓存），所以可以一次性发出去：

```
pipe := redis.Pipeline()
pipe.SMembers(feed:read:<uid>)
pipe.ZRevRangeByScore(feed:timeline:<uid>, ...)
for _, cid := range bigVIds {
    pipe.ZRevRangeByScore(author:outbox:<cid>, ...)
}
pipe.Exec()      // ← 全部 1 次 RTT
```

| | 纯推（现状） | 推拉结合（目标） |
|---|---|---|
| Redis 往返 | 2（SMEMBERS + ZREVRANGE） | **2**（1 次 pipeline 打包 2+K 条命令） |
| 命令数 | 2 | 2 + K |
| **增量往返** | — | **0** |

**代价从「多一次网络往返」变成了「多 K 条命令」**——而命令是搭在已有的那一次往返里走的，边际成本基本为零。这也正好对上压测里的实测：feed 接口的瓶颈是**往返次数**（每请求 4 次串行 Redis 往返把 QPS 钉在 ~3,900），不是命令数。所以合并 pipeline 是**同时**解决了这个新需求带来的成本。

> ✅ 已实施：`GetReadArticleIDs` 的 SMEMBERS 现在和 timeline / outbox 的读取合并在同一次 pipeline 里。
> 做法见 `feed_controller.go` 的 `collectFeedCandidates()` —— 它直接读裸命令的返回值
> （`utils.ParseReadMembers`），而不是走 `FeedDedup.GetReadArticleIDs()` 那层封装。

### 4.5 第四类：热点作者（延迟推 + 拉兜底）

C 类走拉会引入一个新的读侧压力：**一个大V发文章后，他的 500 万粉丝在 10 分钟内陆续来读，每人都要读一次他的 outbox = 500 万次 `ZRANGEBYSCORE`。**

读比写便宜得多，但这仍是一个尖峰。三道防线（按代价排序）：

1. **本地缓存**：大V的 outbox 头部（最近 N 条）加 `LocalCache`（`utils/local_cache.go` 已有），命中后 0 网络 IO。这和你 `ActiveWindow` 的 L1/L2 两级缓存是同一个模式。
2. **延迟推**：大V发文后 60 秒内**不推**，等在线用户的读尖峰过去，再推给「当前活跃」的那部分人；剩下的人永久走拉。这就是 §4.1 那张表的实际运作方式。
3. **合并视图缓存**：对「某用户关注的 C 类作者集合」的归并结果做短期缓存（30s）。可以复用 `fillFeedWithMutex` 那套（已经在 `压测报告_Feed击穿锁优化.md` 里重写过，owner token / 不立即释放 / 陈旧缓存兜底都现成）。

### 4.6 阈值反推（不要拍脑袋）

阈值应该由**内存预算**反推，而不是凭感觉：

```
内存预算公式：
  Redis 可用内存 M
  单个物化 inbox 字节数 S        （实测 44,558 字节，500 条上限）
  → 可物化的 inbox 总数上限 N = M / S

维度一（粉丝量）的阈值 T2：
  C 类作者不产生 inbox 写入，所以 T2 越小 → 写侧越省，但读侧归并越重
  T2 应该选在「推的边际内存成本 = 拉的边际读成本」的交叉点
  实践取值：几百到几万量级，取决于日活与内存比

维度二（活跃度）决定实际用掉多少：
  只有活跃用户物化 inbox → 实际占用 ≈ DAU × S
  所以 N = M / S 是对「总用户数」的约束，DAU × S 才是实际用量
```

**本机代入实测数字**：单机 Redis 可用内存按 32GB 算，`32GB / 44,558 字节 ≈ 70 万`。

> 也就是说：**这台机器最多只能为约 70 万个用户物化 inbox。** 超过就必须靠「只给活跃用户物化」把实际用量压回 `DAU × S` 以内。
>
> 这个推导过程本身就是面试答案——把「我设了 10000」换成「我按 Redis 内存预算反推出阈值在几千这个量级」，完全不是一个水平的回答。

---

## 5. 关键工程问题

### 5.1 迁移兼容：不能出现「文章消失」或「重复」

一个作者从 B 类升到 C 类（或阈值调整导致分类变化）时：

| 风险 | 解法 |
|---|---|
| **重复** | **不要回灌历史**——outbox 从切换点开始写，之前的文章只留在粉丝的 timeline 里，之后的只进 outbox。按时间切开，天然零重复。`FeedDedup` 三层去重仍然保留作为兜底，但不应该靠它来修设计缺陷 |
| **消失**：切换瞬间发的文章两边都没有 | 写侧顺序：**先建空 outbox → 再切路由**，中间不留窗口（只建 key，不灌历史） |
| **顺序错乱**：outbox 的时间戳与 timeline 的 score 语义不一致 | 统一用「发文时间 `CreatedAt.Unix()`」作 score，现在 `batchFanout:103` 就是这么算的，保持一致即可 |

> **为什么明确「不回灌历史」**：回灌会让同一篇文章同时存在于粉丝的 timeline（历史推送）和作者的 outbox（回灌），归并时读两遍——这正是「重复」风险的来源。而不回灌**没有任何损失**：timeline 本来就有 `timeline_max_items: 500` 的裁剪，用户能看到的旧文章本来就有限；切换后新文章照常可见。
>
> **不制造重复，比事后去重更好。** 去重应该留着兜底（网络重试、MQ 重复投递），而不是用来掩盖一个本可以避免的设计选择。

### 5.2 顺序性

多消费者并发处理消息时，**同一个作者的文章必须保序**，否则 inbox 里 500 条的相对顺序会乱。

解法二选一：
- 按 `authorID` hash 分区到固定消费者（同作者永远落到同一个 goroutine）
- 单消费者 + 按作者维度并行（同作者串行、跨作者并行）

### 5.3 缓存层级统一

改造后会出现四种缓存，需要统一在同一套 L1/L2 模式里，避免每个模块各写一套：

| 缓存对象 | 位置 | 失效时机 |
|---|---|---|
| 用户自己的 inbox | `feed:timeline:<uid>` | 推入时追加 |
| 大V的 outbox | `author:outbox:<uid>` | 发文时追加，TTL 过期 |
| 归并结果 | `feed:merge:<uid>` | 30s TTL（可复用 `fillFeedWithMutex`） |
| 关注的大V列表 | `LocalCache` | 关注变更时失效（`InvalidateFollowingCache` 已有） |

---

## 6. 与「纯推」「纯拉」的定量对比

### 6.1 为什么不能纯拉

用户的工作负载是**读多写少**，且绝大多数作者是中小作者。

纯拉意味着**每次 Feed 读**都要对「用户关注的全部作者」做 K 路归并。关注 500 人就是 500 个 outbox 的合并——这个成本摊在每次读上，而读的次数是写的成百上千倍。

**推模式下 Feed 读是 1 次 `ZREVRANGE`（O(1)）**，这正是 `GetFeed` 推路径做的事。所以中长尾必须推。

### 6.2 1000 万粉账号发一篇的对比

| | 现状（推所有，无超时假设） | C 类走拉 |
|---|---|---|
| 写侧命令数 | **3000 万条**（1000 万 × 3：ZADD + ZREMRANGEBYRANK + EXPIRE） | **1 条 ZADD** |
| 写侧耗时 | **~204 秒**（实测 49,000 粉丝/秒外推） | <1ms |
| 发文总延迟 | ~245 秒（Pluck ~41s + Redis ~204s） | 毫秒级 |
| 读侧成本 | 1 次 `ZREVRANGE` | K 路归并（可 pipeline 成 1 次 RTT） |
| 稳态 inbox 内存 | **1000 万 × 44.6KB = 414 GB**（理论上限） | 取决于维度二 |

写侧差了约 **3000 万倍**。读侧拉略贵一点。这笔账在任何规模下都是拉赢。

### 6.3 但内存的账要算准确

**必须诚实说清楚**：C 类走拉**不会**直接把 414GB 变成 0。因为那些用户还会因为关注的中小作者而被创建 inbox。

414GB 是「**坚持给全部 1000 万粉丝物化 inbox**」的成本。C 类走拉真正省掉的是：

1. **写侧**：3000 万条命令 → 1 条
2. **发文延迟**：4 分钟 → 毫秒级
3. **inbox 容量的机会成本**：500 条上限不再被大V的内容挤占，留给真正需要推的中小作者内容

**要把内存真正降到 `DAU × S`，靠的是维度二**（只为活跃用户物化），而不是维度一。

> 这个区分很重要——如果说成「大V走拉省了 414GB」，面试官一追问「那用户关注的中小作者还在推啊」就露馅了。

### 6.4 什么时候推仍然是对的

| 作者粉丝量 | 推 | 拉 |
|---|---|---|
| 100 | 300 条命令，写侧秒级 | 100 个粉丝每次读都多一次归并，**净亏** |
| 1 万 | 3 万条命令，写侧可接受 | 边际开始划算 |
| 100 万 | 300 万条命令，写侧已很难看 | 明显更优 |
| 1000 万 | 3000 万命令 + 414GB | 唯一可行 |

**所以结论不是「推翻推拉结合」，而是「把阈值从拍脑袋变成容量推导，并且让决策在写之前发生」。**

---

## 7. 实施计划

分四步，**每一步都能独立验证和回滚**。

### 第一步：止血（当天，零架构改动，约几十行）

| 动作 | 位置 | 收益 |
|---|---|---|
| `Pluck` 加错误检查 | `bigv_fanout.go:91-97` | 修掉「查询失败伪装成推送成功」 |
| `Pluck` 改分页流式拉取（按 follower_id 游标分批） | 同上 | 1000 万行不再一次性进内存，可中断 |
| 消费者加 `Qos(prefetch)` | `feed_controller.go:383` | 消息不再无上限灌进内存 |
| 熔断器**按作者维度隔离** | `bigv_fanout.go:66-84` | 大V失败不再熔断全局 |
| `RecordSuccess` 传真实耗时 | `bigv_fanout.go:72,84` | 让 `circuit_breaker.go:110` 的延迟判据真正生效 |
| 降级标记补齐（读+重试）或删掉 | `bigv_fanout.go:163-185` | 消除死代码 / 补齐兜底 |

**这一步完成后，「一个大V连发 3 篇打瘫全局 30 秒」这个可用性 bug 就没了。** 在面试里的分量比 QPS 数字更重。

### 第二步：路由确定化（1-2 天）

- `big_v_threshold` 改成按内存预算反推的值（§4.6）
- 路由判断从「超时」改成「粉丝量」，决策点提到 `FanoutArticle` 之前
- 超时降级为纯保护机制 + 告警（不再静默换策略）
- 顺便修 `ActiveWindow` 的 L1 无 TTL bug（维度二的前置条件）

**这一步不改数据结构，只改决策逻辑**，但可预测性立刻上来。

### 第三步：C 类走拉（3-5 天，架构改动）

- 新增 `author:outbox:<uid>` 结构（发文时写入）
- 读路径新增 Phase 2.5：outbox pipelein 归并
- `GetColdArticles` 的 MySQL 查询逐步被 outbox 取代
- 迁移兼容测试（§5.1 的三种风险）

### 第四步：维度二 + 热点优化

- 非活跃用户不物化 inbox，首次回来按需重建（复用 `ActiveWindow`，需先修 TTL）
- 热点大V的本地缓存 / 延迟推（§4.5）

### 每一步都要回归压测

用 `loadtest/` 那套脚本（见 `压测报告_FEED_FORUM.md` 附录 A）重跑存档，形成 before/after 曲线。

**第三步做完后，`loadtest/scripts/hot_scale_test.sh` 的思路可以复用来验证**：造 10 万粉丝发一篇文章，观察
- 改造前：`fanout timeout after 49000/100034 followers, degrading`
- 改造后：fanout 阶段毫秒级返回，读路径按需归并

这是可以直接测出来对比的，**有实测数字的优化才叫优化，没有的只是重构**。

---

## 8. 风险与明确不做的事

| 风险 | 应对 |
|---|---|
| 读路径复杂度上升 | 归并逻辑集中在 `rebuildFeed` 的新 Phase 里，复用已有的去重和缓存模式，不散落 |
| C 类作者数量增长导致 K 路归并变慢 | K 有天然上限（一个用户关注的大V数通常 < 100）；给 K 设硬上限并在超限时按粉丝量截断 |
| 迁移期数据不一致 | 写侧原子切换 + 读侧去重兜底，两者叠加 |
| 单机 Redis 内存仍是硬上限 | 本设计不解决单机容量问题，只把内存占用从 `O(总用户)` 降到 `O(DAU)`；真要扩容需要 Redis Cluster 或分片 |

**明确不做**：
- 不引入 Redis Cluster（当前规模不需要，设计保持单机成立）
- 不上完整的流式/CDC 同步（发文频率远未到需要）
- 不改前端（Feed 接口的响应结构不变）

---

## 9. 面试问答预案

**Q：你的大V推送是怎么设计的？**

> 推拉结合。中小作者走推——写侧成本 O(粉丝数) 在几千这个量级可接受，读侧是 1 次 `ZREVRANGE` 的 O(1)；大V走拉——只写一份作者 outbox，粉丝读时归并，写侧从 O(粉丝数) 降到 O(1)。
>
> 但我实现上有个缺口：`big_v_threshold` 那个分支和普通通道是同一个 `batchFanout`，等于「推所有粉丝 + 超时降级」，真正的大V拉模式没落地。

**Q：你怎么知道大V推不动？有数据吗？**

> 有，我实测跑过。造 10 万粉丝发一篇文章：`Pluck` 拉粉丝 ID 用了 **408ms**，然后 fanout 在 **1 秒时中止**，只推了 **49,000/100,034**，剩下一半降级 pull-only。实测吞吐 49,000 粉丝/秒。
>
> 我用 `redis-benchmark` 按真实 pipeline 形状独立测了一遍，算出来 52,766 粉丝/秒，两个方法差 7%，互相印证。红线外推到 1000 万：纯 Redis 约 **204 秒**，加上 Pluck 约 **4 分钟**。
>
> 但真正的墙不是速度是内存——实测每个用户时间线（500 条上限）占 **44.6KB**，**1000 万粉丝如果坚持全物化就是 414GB**。

**Q：那你说大V走拉能省多少内存？**（★ 追问陷阱）

> 这里要区分清楚：**大V走拉不会直接省下那 414GB**。因为那些用户还会因为关注的中小作者而创建 inbox。
>
> 大V走拉真正省的是**写侧**：3000 万条命令变成 1 条，发文延迟从 4 分钟变成毫秒级。
>
> 要把内存真正降到 O(日活)，靠的是**另一个正交的维度**：只给活跃用户物化 inbox，非活跃用户回来时按需重建。判断活跃正好是我项目里 `ActiveWindow` 能回答的问题。
>
> 这两个维度是分开的：**粉丝量决定写侧做多少，活跃度决定要不要物化**。

**Q：阈值 10000 是怎么定的？**

> 原来是拍脑袋的，这是我准备改的。正确做法是按内存预算反推：单机 Redis 可用内存 ÷ 单时间线字节数 = 可物化 inbox 上限。用我实测的 44.6KB 代入，32GB 内存只能物化约 **70 万个** 用户的时间线。所以阈值应该落在几千这个量级，并且要跟日活挂钩——因为只有活跃用户才真的占内存。

**Q：多消费者并发怎么保证顺序？**

> 按 `authorID` 做 hash 分区，保证同一个作者的消息永远落到同一个消费者，同作者串行、跨作者并行。否则一个用户 inbox 里 500 条内容的相对顺序会乱掉。

**Q：降级之后文章还能被看到吗？**

> 能，但要说清楚是怎么做到的——**是巧合不是设计**。文章被标记 `feed:degraded:<id>`（24h），但全仓库没有任何地方读这个标记，`RetryDegradedArticle` 也是零调用。用户最终能看到，是因为拉取路径的 `GetColdArticles` 按 `author_id IN (followingIDs)` 回查，**恰好没有过滤这个标记**。
>
> 所以「降级机制」目前是半成品，这也是我第一步要修的：要么补齐（读标记 + 重试），要么删掉别留死代码。

**Q：这套设计最大的风险是什么？**

> 读路径复杂度。推模式下 Feed 读就是 1 次 `ZREVRANGE`，加了归并之后要做 K 路合并。我的控制手段是：把归并逻辑集中在 `rebuildFeed` 的同一个 Phase 里，复用已有的三层去重和 `fillFeedWithMutex` 那套缓存（那块我在压测后重写过，有 owner token、陈旧缓存兜底），不让它散落到各处。

---

## 附录 A：实测数据来源

| 数字 | 采集方式 | 备注 |
|---|---|---|
| `Pluck` 10 万行 = 408ms | 真实链路，GORM 慢 SQL 告警 | `rows:100034` |
| fanout 1 秒推 49,000 | 真实链路日志 `fanout timeout after 49000/100034` | 10 万粉丝数据集 |
| 49,000 粉丝/秒 | 上条推算 | |
| 52,766 粉丝/秒 | `redis-benchmark --dbnum 15 -P 3000 -c 1 -r 1000000` | ZADD 111,500 / ZREMRANGEBYRANK 213,000 / EXPIRE 188,000 ops/s，按 1/(1/A+1/B+1/C) 折算 |
| 44,558 字节/时间线 | Redis `used_memory` 增量 / 500 个用户 × 500 条 | 在生产稳态形状下测量 |
| 414 GB | 44,558 × 1000 万 | 理论上限（全部粉丝都物化 inbox） |
| ~204 秒 / ~245 秒 | 1000 万 ÷ 49,000；再加 `Pluck` 外推 41 秒 | 线性外推 |

**测量环境**：Windows 11 / 32 逻辑核 / 32GB，Redis 5.0.14（Windows 原生版，单机单实例），压测机与服务端同机。
**可信度**：本机测量噪声 ±30%，所以只有量级结论可信（3000 万倍、414GB、4 分钟这个量级）；7% 这种精度差异在两个独立方法互相印证下才有意义。

## 附录 B：相关代码位置

| 功能 | 位置 |
|---|---|
| fanout 主入口 | `services/bigv_fanout.go:46-86` `FanoutArticle` |
| fanout 实际实现 | `services/bigv_fanout.go:89-155` `batchFanout` |
| 粉丝 ID 一次性拉取（吞错误） | `services/bigv_fanout.go:91-97` |
| 超时中止点 | `services/bigv_fanout.go:149` |
| 降级标记（死代码） | `services/bigv_fanout.go:163-185` |
| 冷数据回查（要被 outbox 取代的位置） | `services/bigv_fanout.go:209-228` `GetColdArticles` |
| 熔断器 | `services/circuit_breaker.go:59,62,110` |
| MQ 消费者（单 goroutine、无 Qos） | `controllers/feed_controller.go:370-420` |
| Feed 读路径（Phase 2/3） | `controllers/feed_controller.go:691-724` `rebuildFeed` |
| 缓存击穿防护（已优化） | `controllers/feed_controller.go:614-677` `fillFeedWithMutex` |
| 拉取服务 | `services/feed_pull_service.go:154` `PullFromInactiveUsers` |
| 三层去重 | `utils/dedup.go` |
| 活跃度窗口（维度二的前置） | `utils/active_window.go` |
| 本地缓存 | `utils/local_cache.go` |
| 配置 | `config/config.yml:19-25` |

## 附录 C：实施对照表

### 第一步：止血（全部落地）

| P | 问题 | 修复 |
|---|---|---|
| P3 | `Pluck` 吞错误 | `streamFollowers()` 里错误一路上抛，不再「查询失败 → 空切片 → return 0, nil」 |
| P4 | 单次拉全部粉丝进内存 | `streamFollowers()` 改成 keyset pagination（`follower_id > ?` + `LIMIT n`），内存恒定为一个 batch、可中断；新增索引 `idx_follows_following_follower (following_id, follower_id)` 让它是纯索引范围扫描而不是 filesort |
| P7 | 消费者无背压 | `ch.Qos(consumer_prefetch, 0, false)`，积压留在 broker 队列（可见可告警）而不是变成进程 RSS |
| P2 | 大V失败熔断全局 | `AuthorCircuitRegistry` 按 `authorID` 隔离，每个作者一个独立熔断器；容量满时只淘汰 CLOSED 条目 |
| P6 | 延迟判据永不触发 | `RecordSuccess(fanoutCtx, elapsed)` 传真实耗时 |
| P5 | `feed:degraded` 死代码 | 删除 `IsDegraded`/`RetryDegradedArticle`/`markAsPullOnly`，换成可索引的 `feed:fanout:retry` ZSet + `FanoutRetryWorker`（指数退避、超限告警） |

> P5 为什么用 ZSet 而不是 `KEYS feed:degraded:*`：`KEYS` 是 O(N) 全库扫描，生产环境等同于故障。

### 第二步：路由确定化（全部落地）

- `services/feed_router.go`：`Classify` / `Decide` 是**纯函数**，输入只有 `followerCount`，不碰 IO、不碰时间 → 可以直接单测（`feed_router_test.go` 覆盖边界值 + 确定性 + 非法配置回落）。
- 阈值改为按内存预算反推：`InboxesWithinBudget(预算 MB, 每 timeline 字节数)`，配置里给出推导注释，启动时打印推导结果。
- 超时降级为**纯保护机制**：命中即 `ALERT` 日志，不再静默换策略。
- `ActiveWindow` 的 L1 无 TTL bug 已修（`isFresh`）：这是维度二的前置条件，不修的话「非活跃读者」永远是空集。

### 第三步：C 类走拉（全部落地）

- `author:outbox:<authorID>` ZSet + `feed:authors:c` Set，见 `services/author_outbox.go`。
- 写侧：`writeOutboxOnly()` —— **先 `Append` 再 `RegisterTierC`**，不留「两边都没有」的窗口。
- 读侧：`GetFeed` 的 Phase 1 一次 pipeline 取回
  ① `SMEMBERS feed:read:<uid>` ② `feed:timeline:<uid>` ③ `author:outbox:<cid> × K`，
  按 score 归并（同分用 articleID 兜底保证顺序稳定）。
- **归并必须在主路径上**（这是原文没写清楚、实施时才暴露的关键点）：Phase 2/3 只在「装不满一页」时才跑，
  而大V发文时读者的 timeline 通常是满的 —— 放在 Phase 2.5 的话归并永远不发生，大V的新文章会永久不可见。
- 迁移兼容：**不回灌历史**，按时间切开；`authorID` 从 C 类退出时 `UnregisterTierC` 自愈。

### 第四步：维度二 + 热点优化（全部落地）

- 维度二：`activeOnly()` 在 fanout 里过滤粉丝，只给活跃读者物化 inbox。
- 「刚回来」的判定：`MarkActiveAndDetectReturn` —— 写活跃标记**之前** `user:active:<uid>` 不存在 ⟺ 上一个 7 天窗口里他没出现过 ⟺ 期间被跳过。读路径据此强制重建一次，把漏掉的推文补上。
  这个判定是**精确且近乎免费**的：`AuthMiddleware.refreshLastActive` 本来就有 5 分钟节流，多的那次 `EXISTS` 被摊到 5 分钟上。
  （原计划靠「头部时间戳有多旧」去猜，那个启发式有明显假阳性和假阴性，实施时换掉了。）
- 热点作者：`author:outbox` 头部按**单作者**做本地缓存（`headCache`），`Append` 精确失效自己那一个 key。
  粒度不能是「用户关注的大V集合」——那样一个大V发文就会清掉所有人的缓存，而清缓存的那一刻正是读尖峰。

### 测试

| 文件 | 覆盖 |
|---|---|
| `services/feed_router_test.go` | 纯函数：边界值、确定性（同一粉丝量 100 次结果恒定）、非法配置回落、预算反推 |
| `services/author_outbox_integration_test.go` | 打真 Redis（DB 15）：跨作者归并、游标分页、头部缓存精确失效 + 按作者隔离、K 上限、重试队列 attempt 记账 |
| `utils/active_window_test.go` | L1 TTL 回归测试、`FilterInactive` 忽略过期条目、`MarkActiveAndDetectReturn` 三种情形 |
| `controllers/feed_merge_test.go` | 归并去重/排序/截断、游标编解码往返 |

实施过程中被测试抓出来的两个真问题（不是测试写错）：
1. `RegisterTierC` 没有失效本地缓存 → 空集合被缓存 60 秒，新登记的大V文章对所有人不可见。
2. 重试队列的 self-enqueue 会和 worker 的「按 member 删除」撞车 → worker 取出 `id:1` 后若内部又插一条 `id:1`，紧接的 `Complete` 会把刚插入的删掉，文章静默丢失。已改成「三条路径各自只有一个入队点」。

### 回归压测（2026-10-14，已完成）

端到端压测已跑，结果写在 `压测报告_FEED_FORUM.md` **§3.6**。核心结论：

| 验证项 | 结果 |
|---|---|
| 10 万粉单篇 fanout（旧代码基线） | `fanout timeout after 57000/100000, degrading`，**43% 粉丝永远收不到推送** |
| 同一个号判成 B 类（新） | 3 轮让路后全部推完，**100,000/100,000 写入** |
| 同一个号判成 C 类（新） | `1 ZADD + 毫秒级返回`，写侧命令数从 17 万降到 **4** |
| 读路径端到端 | ✅ **读者的时间线是满的（60 条），大V的文章照样出现在第一屏** —— 直接证明归并必须在主路径而非 Phase 2/3 |
| feed 接口回归 | 相对"击穿锁优化"后的基线 **+7%~+16% QPS**（归因声明见报告 §3.6.4） |

### 实施与回归中抓出的四个问题（全部已修）

前三个是**读代码看不出来、跑一遍才现原形**的类型，详见报告 §3.6.3；第四个是顺着读路径走一遍才发现的逻辑缺口：

1. **B 类 worker 池无限弹跳** —— worker 的 handler 调 `execute()`，而 `execute()` 会重新跑一遍路由，判定还是 B 类，于是把文章**又投回同一个池**。现象：不推、不报错、日志空白、CPU 烧起来。修法：把"要不要异步"限制在入口一次，worker 里只执行（`executeNow`）。
2. **keyset 分页被优化器选成 filesort** —— 为了修「一次拉 10 万行」而写的分页，实测单批 1000 行要 507ms，**比它替换掉的 407ms 还慢**。根因是 GORM 的软删除条件 `deleted_at IS NULL` 让优化器选了 `idx_follows_deleted_at` 做 filesort；更阴的是**同一个索引两副面孔**：首屏查询选得对，一加游标条件就选错。修法：索引扩成 `(following_id, follower_id, deleted_at)` + 分页查询加 `FORCE INDEX`。
3. **保护超时让大V永远推不完** —— 超时原本当失败处理、重试从头开始，于是每轮都卡在同一处（实测 `FAILED after 1.08s (pushed=0)`）。修法：超时改成**让路点**，进度写 `feed:fanout:progress:<articleID>`，下一轮从断点续跑，且**不消耗重试次数**（每次至少推进一个 batch，不会死循环）。
4. **「首次回来按需重建 inbox」名不副实 —— 只重建了一份读结果，没有重建 inbox**（见下方 §5.4）。

### 5.4 「首次回来重建」的实现缺口与修复

**问题**：`rebuildFeed` 的结果只写进 `feed:pull:cache:<uid>`（30s）和 `feed:pull:stale:<uid>`，
**从来没有写回 `feed:timeline:<uid>`**。而那份缓存只有一个读取入口：

```go
need := size - len(articles)      // 读者回来时页面是满的 → 0
if returning { need = size }      // 是 feedReturning 把它顶成 size 的
if need > 0 { fillFeedWithMutex(...) }   // ← 缓存只在这里面被读
```

于是实际行为是：

| 请求 | `feedReturning` | 结果 |
|---|---|---|
| 回来后第 1 个 | `true` → `need = size` | ✅ 触发重建，补上漏掉的内容 |
| 第 2 个起 | `false`（`user:active:<uid>` 已被写上）→ `need = 0` | ❌ **不进 `fillFeedWithMutex`，那份缓存永远不会被读到 → 退回旧内容** |

补一次、刷新一下就没了。设计文档 §4.3 写的「首次回来时**按需重建 inbox**」，
代码做的是「重建一份**读结果**」—— 这两件事差了一个「物化」。

**修复**：新增 `BigVFanoutService.MaterializeInbox(userID, articles)`，
在 `fillFeedWithMutex` 抢到锁、重建完成之后，把结果按**和 fanout 完全相同的约定**
（score = `CreatedAt.Unix()`、member = articleID、同样裁剪到 `timeline_max_items`、
同样 30 天 TTL）写回 `feed:timeline:<uid>`。

这样补上的内容在下一次请求的 **Phase 1** 里就自然出现 —— 不需要重建、不需要碰缓存。
`feedReturning` 从「每次回来都要触发的补丁」变成真正的**一次性自愈**。

**只在首屏（`cursor == nil`）物化**：带游标的重建拉的是更旧的内容，
写进去会立刻被 `ZRemRangeByRank` 当最旧的裁掉，白写一趟。

**端到端验证**（2026-10-14，实测）：

```
场景：用户 13 的 timeline 有 60 条旧内容；删掉 user:active:13 模拟「离开 7 天」；
     库里插入一篇他关注的作者在「他离开期间」发的文章 999905（不经 MQ，所以不在任何 timeline 里）

请求 1  → [999905, 565, 564, 563, 562]     日志：materialized 1 articles into feed:timeline:13
         timeline: 60 → 61 条，ZSCORE 999905 = 1791969529（写进去了）

请求 2  → 页面被标记已读，返回下一批；清掉已读集合后再请求：
         [999905, 565, 564, 563, 562]      日志：无 materialized ← 内容是从 timeline 读到的，没有再扫库

负向对照：手动 ZREM 999905 之后
         [565, 564, 563, 562, 561]         日志：无 materialized
         ← 内容没了，而且再也找不回来。这正是修复前的行为
```

负向对照是关键：它复现的正是「`need=0` → 不进 `fillFeedWithMutex` → 缓存读不到 →
内容永久丢失」这条链。修复后同样的操作能稳定拿到内容，且不再触发第二次扫库。

回归测试：`services/materialize_inbox_test.go`（裁剪保新、score 约定与 fanout 可归并、TTL、空输入不建 key）。

### 尚未验证的部分

- **K 路归并的读侧开销没压**：夹具里只有 1 个大V（K=1），测不出 K 增长时的归并代价。
- **分层阈值没有实测支撑**：`tier_a_max=5000` / `tier_c_min=50万` 是按内存预算反推的，不是压测扫出来的拐点。
- **`materialize_active_only` 打开后没重跑 §3.2 的拉路径专项**：刚回来的读者会强制重建 timeline，重建有 30s 缓存 + SETNX 锁兜底，但改造后的 QPS/延迟曲线未实测。
- **重建没有全局并发闸**：`feed:mutex:timeline:<uid>` 是按用户的，N 个不同用户同时回来 = N 把锁，互不阻塞。真正的兜底只有 MySQL 连接池（`MaxOpenConns=114`）。修法是把 `FanoutWorkerPool` 那套信号量复用过来，超出并发数的请求直接返回陈旧缓存。
- **游标分页的同秒边界**（`maxScore = cursor.Score - 1` 会跳过同秒未展示的文章）：已知未修，改它要连前后端契约一起动。
