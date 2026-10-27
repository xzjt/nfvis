package main

// 决策 #516：提交后宿主收敛按修订代际串行。
//
// 由来（round50 只读评审）：每次成功提交都新起一个 goroutine 应用 TLS/syslog/主机防火墙/
// 系统基线/日志保留/监听重绑，既无 revision 代际也无队列；多个 goroutine 可并发、可乱序
// 完成，旧配置可能最后写宿主（`FirewallApplier` 自身无互斥）——committed 已是新配置、宿主
// 却是旧配置，读视图按 committed 对照显示「未收敛」，极端情形旧的 `default-policy drop`
// 会切断管理面。
//
// 口径：单 worker 串行执行；同一时刻只有一个应用在跑；尚未启动的中间修订**合并**丢弃，
// 保证**最新修订最后生效**。失败仍只告警、不回灌提交（apply 回调内部自行处理）。
// 启动路径的一次性应用保持同步语义，不经此队列。

import "sync"

// hostApplyQueue 是一个单 worker、携带修订号、可合并待办项的串行队列。
//
// 线程模型：enqueue 与 worker 的状态迁移（pending/hasPending/running/closed）都在 mu 下进行；
// worker 只在「没有待办或已关闭」时退出，退出前把 running 复位——与 enqueue 的新增同在一把
// 锁下，因而**无丢唤醒**（要么 enqueue 看到 running=false 自己拉起 worker，要么 worker 在
// 加锁后看到 hasPending=true 继续消费）。apply 回调在 mu 之外执行：它可能耗时、还要回读引擎
// （提交完成之后取锁安全），绝不能在锁内跑。
type hostApplyQueue struct {
	mu         sync.Mutex
	pending    int // 最新待执行修订（仅当 hasPending 为真时有效）
	hasPending bool
	running    bool
	closed     bool
	apply      func(revision int)
	wg         sync.WaitGroup
}

// newHostApplyQueue 构造队列；apply 为实际的宿主收敛动作（携带 revision，内部自行记日志/告警）。
func newHostApplyQueue(apply func(revision int)) *hostApplyQueue {
	return &hostApplyQueue{apply: apply}
}

// enqueue 提交一个新修订。若已有应用在跑，则只**覆盖**待办（合并中间修订，保留最新）；否则
// 拉起 worker。关闭后不再接收——最新配置会在下次启动的同步路径上重新应用。
func (q *hostApplyQueue) enqueue(revision int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.pending = revision
	q.hasPending = true
	if !q.running {
		q.running = true
		q.wg.Add(1)
		go q.worker()
	}
}

// worker 串行消费待办，直到没有待办或队列关闭。
func (q *hostApplyQueue) worker() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if q.closed || !q.hasPending {
			q.running = false
			q.mu.Unlock()
			return
		}
		rev := q.pending
		q.hasPending = false
		q.mu.Unlock()

		q.apply(rev)
	}
}

// close 收尾：不再接收新修订，等待当前正在执行的应用结束（若有）。丢弃尚未启动的待办——
// 它们只是中间修订，不足以代表最新配置；最新配置会在下次启动的同步路径上重新应用，故无需
// 在退出这一刻补跑。
func (q *hostApplyQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wg.Wait()
}
