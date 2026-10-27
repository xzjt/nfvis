package main

// 决策 #513（round50）：凭据变更在**全部提交路径**收尾。
//
// 由来：主动吊销只挂在 REST 的用户 handler 三处（resources_w5.go）：改 class / 重置口令 /
// 删用户 / 本人改口令。而 CLI `delete/set system login user …`（+ commit）、load override|merge、
// restore、zeroize 等**整文档提交**不经过那里。`aaa.VerifyToken` 只保护后续 HTTP 请求，
// 已升级的串口/VNC/容器 shell 是长连接（不再发 HTTP），`ActiveTokenID` 又只查内存表 ⇒ 被删
// 用户的容器 shell 仍可执行命令、`show system api tokens` 仍把它列为活动会话。
//
// 口径：装配层维护「上一次已应用提交的登录配置」快照，每次成功提交后按前后差异算出受影响
// 用户名（删除 / class 变 / 口令哈希变；zeroize 清空账号 ⇒ 全体），逐一调用服务端的统一收尾
// （`api.Server.RevokeUserSessions` → aaa 吊销 token + 丢弃 CLI 会话 + 关闭终端）。仅恢复型
// 回滚（用户重新出现）不吊销、不误伤。
//
// 两条硬约束（本仓踩过坑）：
//   - **不得在引擎写锁内回读 committed**：`OnCommitted` 在 `Engine.Commit` 持写锁时回调，
//     而 `Engine.Committed()` 要同一把锁 ⇒ 直接回读会自死锁（R42-F1）。故这里把「读 committed
//     + 差分 + 收尾」放在**独立 worker goroutine** 里异步执行：`enqueue`（在锁内）只做一次
//     非阻塞唤醒。
//   - **顺序正确 + 快照更新加锁**：worker 串行（同一时刻只有一次在跑），每次都读**当前**
//     committed 并与快照差分；多次连续提交只会让 worker 再跑一次（合并中间修订），差分用的是
//     最新配置与上一次已处理快照——不会把中间状态算成两次。

import (
	"log/slog"
	"sort"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// loginFingerprint 一个用户「登录语义」的指纹：class + 口令哈希。
// 二者任一变化都意味着该用户既有凭据的语义已变（降权/升权、口令轮换）⇒ 应吊销。
type loginFingerprint struct {
	class string
	hash  string
}

// loginFingerprints 从 committed 配置提取「用户名 → 指纹」。无登录配置时返回空表。
func loginFingerprints(cfg model.Config) map[string]loginFingerprint {
	out := map[string]loginFingerprint{}
	if cfg.System == nil || cfg.System.Login == nil {
		return out
	}
	for _, u := range cfg.System.Login.Users {
		out[u.Name] = loginFingerprint{class: u.Class, hash: u.PasswordHash}
	}
	return out
}

// affectedLoginUsers 返回「凭据已变」的用户名（稳定升序）：相对上一次快照，**被删除**、
// **class 变**、**口令哈希变**者。**新增**（包含恢复型回滚里「用户重新出现」）不在内——
// 那些用户没有需要作废的既有会话，吊销他们只会误伤。zeroize 清空账号时，快照里的全体都被
// 判为「删除」，自然覆盖。
func affectedLoginUsers(prev, next map[string]loginFingerprint) []string {
	var out []string
	for name, p := range prev {
		n, ok := next[name]
		if !ok || n != p {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// loginIdentityFinalizer 提交后的身份收尾器（决策 #513）。
//
// 线程模型：`enqueue` 可被多个提交（引擎写锁内）调用，只做非阻塞唤醒；单 worker 串行执行
// `run`。`run` 与装配层参数（committed 读取 / 统一收尾）都不在引擎写锁内——`run` 由 worker
// goroutine 调用，`f.mu` 只在「是否需要拉起 worker」与「dif 快照推进」这类极短临界区上持有，
// 且**持有 f.mu 时绝不调用 committed/revoke**，故不可能与「引擎写锁 → enqueue → f.mu」构成锁环。
type loginIdentityFinalizer struct {
	// 快照：上一次已处理的 committed 登录配置。单独一把锁（与调度锁 f.mu 分离）。
	snapMu   sync.Mutex
	snapshot map[string]loginFingerprint

	committed func() (model.Config, error) // 读当前 committed（装配层注入 engine.Committed）
	revoke    func(user string) []string   // 统一收尾（装配层注入 api.Server.RevokeUserSessions）
	log       *slog.Logger

	// 调度状态：enqueue/worker 迁移都在 mu 下进行，无丢唤醒（要么 enqueue 看到 running=false
	// 自己拉起 worker，要么 worker 加锁后看到 pending=true 继续消费）。
	mu      sync.Mutex
	pending bool
	running bool
	closed  bool
	wg      sync.WaitGroup
}

// newLoginIdentityFinalizer 构造收尾器并**先建立启动快照**（否则首个成功提交会把全部既有用户
// 都当成「新增/变更」之外的歧义状态；建立快照后，只有真正被删/改的用户才会被收尾）。
func newLoginIdentityFinalizer(committed func() (model.Config, error), revoke func(string) []string, log *slog.Logger) *loginIdentityFinalizer {
	if log == nil {
		log = slog.Default()
	}
	f := &loginIdentityFinalizer{committed: committed, revoke: revoke, log: log}
	f.snapshot = f.readFingerprints()
	return f // worker 由 enqueue 按需拉起（与 hostApplyQueue 同法）
}

// readFingerprints 读一次 committed 的指纹表；读不到时返回空表（保守：首个提交按「全部新增」
// 处理——不误吊销任何既有会话）。
func (f *loginIdentityFinalizer) readFingerprints() map[string]loginFingerprint {
	cfg, err := f.committed()
	if err != nil {
		f.log.Warn("身份收尾：读取 committed 配置失败，快照按空处理", "err", err)
		return map[string]loginFingerprint{}
	}
	return loginFingerprints(cfg)
}

// enqueue 提示 worker 复核一次（可在引擎写锁内调用：非阻塞，多次合并为一次）。
func (f *loginIdentityFinalizer) enqueue() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.pending = true
	if !f.running {
		f.running = true
		f.wg.Add(1)
		go f.worker()
	}
	f.mu.Unlock()
}

// worker 串行消费待办，直到没有待办或已关闭。
func (f *loginIdentityFinalizer) worker() {
	defer f.wg.Done()
	for {
		f.mu.Lock()
		if f.closed || !f.pending {
			f.running = false
			f.mu.Unlock()
			return
		}
		f.pending = false
		f.mu.Unlock()

		f.run()
	}
}

// run 做一次「读 committed → 与快照差分 → 收尾 → 推进快照」。**不在任何锁内**调用
// committed/revoke（见类型注释的锁纪律）。
func (f *loginIdentityFinalizer) run() {
	cfg, err := f.committed()
	if err != nil {
		f.log.Warn("身份收尾：读取 committed 配置失败，本次跳过", "err", err)
		return
	}
	next := loginFingerprints(cfg)

	f.snapMu.Lock()
	affected := affectedLoginUsers(f.snapshot, next)
	f.snapshot = next
	f.snapMu.Unlock()

	if len(affected) == 0 || f.revoke == nil {
		return
	}
	for _, user := range affected {
		ids := f.revoke(user)
		f.log.Info("凭据变更：已收尾该用户的会话", "user", user, "tokens", len(ids))
	}
}

// drain 等待在途的一次收尾跑完（供测试确定性同步；生产不使用）。
func (f *loginIdentityFinalizer) drain() { f.wg.Wait() }

// close 收尾：不再接收新提交，等在跑的收尾结束（供进程优雅退出）。
func (f *loginIdentityFinalizer) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.wg.Wait()
}
