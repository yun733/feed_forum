package services

import (
	"exchangeapp/models"
	"log"
	"sync"
	"time"
)

// FanoutWorkerPool B 类作者（异步延迟推）的执行池
//
// 两个目的：
//
//  1. **不阻塞消费主流程**。B 类是 5,000 ~ 50 万粉丝，写成本可控但不便宜；
//     直接在 MQ 消费者的 goroutine 里跑，一条消息就能把整个队列堵住（P7）。
//
//  2. **保证同作者保序（§5.2）**。多消费者并发处理时，同一个作者的文章必须保序，
//     否则用户 inbox 里 500 条内容的相对顺序会乱掉。
//     解法：按 authorID 取模分区到固定 worker —— 同作者永远落到同一个 channel，
//     FIFO 天然保序；不同作者之间并行。
//
// 队列满了不丢弃：带超时地阻塞投递，超时则返回 false 让调用方丢进重试队列。
// 「丢进重试」比「静默丢弃」和「无限阻塞消费者」都好——前者会丢文章，后者会堵队列。
type FanoutWorkerPool struct {
	queues   []chan models.Article
	handler  func(models.Article)
	wg       sync.WaitGroup
	stopCh   chan struct{}
	stopOnce sync.Once
}

// submitTimeout 队列满时的最长等待时间
const submitTimeout = 200 * time.Millisecond

// NewFanoutWorkerPool 创建 worker 池并启动
// handler 在 worker goroutine 里被调用，必须自己处理错误（不能 panic 出去）
func NewFanoutWorkerPool(workers, queueSize int, handler func(models.Article)) *FanoutWorkerPool {
	if workers <= 0 {
		workers = 4
	}
	if queueSize <= 0 {
		queueSize = 256
	}

	p := &FanoutWorkerPool{
		queues:  make([]chan models.Article, workers),
		handler: handler,
		stopCh:  make(chan struct{}),
	}
	for i := range p.queues {
		p.queues[i] = make(chan models.Article, queueSize)
		p.wg.Add(1)
		go p.worker(i)
	}
	log.Printf("[FanoutPool] Started %d workers, queue size %d each", workers, queueSize)
	return p
}

func (p *FanoutWorkerPool) worker(idx int) {
	defer p.wg.Done()
	for article := range p.queues[idx] {
		func() {
			// handler 里的 panic 不能让 worker 死掉，否则这个作者的消息永远没人处理
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[FanoutPool] worker %d recovered from panic on article %d: %v", idx, article.ID, r)
				}
			}()
			p.handler(article)
		}()
	}
}

// Submit 把文章投递给负责该作者的 worker
// 返回 false 表示队列拥塞，调用方应该走重试队列而不是丢弃
func (p *FanoutWorkerPool) Submit(article models.Article) bool {
	idx := int(article.AuthorID) % len(p.queues)

	select {
	case p.queues[idx] <- article:
		return true
	default:
	}

	timer := time.NewTimer(submitTimeout)
	defer timer.Stop()
	select {
	case p.queues[idx] <- article:
		return true
	case <-timer.C:
		log.Printf("[FanoutPool] queue %d congested, article %d deferred to retry queue", idx, article.ID)
		return false
	case <-p.stopCh:
		return false
	}
}

// Stop 停止所有 worker，等待正在处理的文章完成
func (p *FanoutWorkerPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		for _, q := range p.queues {
			close(q)
		}
		p.wg.Wait()
	})
}

// Pending 当前排队中的文章数（供观测背压）
func (p *FanoutWorkerPool) Pending() int {
	n := 0
	for _, q := range p.queues {
		n += len(q)
	}
	return n
}
