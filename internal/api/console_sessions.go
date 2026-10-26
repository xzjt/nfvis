package api

// 进程内活动终端会话登记表（决策 #507；round49 第 2 条）。
//
// 由来：一次性 ticket 只在 **WS 握手期**校验凭据；握手成功后是一条长连接（串口 console /
// VNC 图形控制台 / 容器交互式终端），而这条链路拿不到 Bearer Token（WS 握手带不了），
// 于是登出、吊销、删用户都不会终止它——被吊销的凭据在终端上继续可用（round49 报告的第 2 条）。
//
// 两条索引（按 token 稳定 ID 与按用户名）：
//   - 按 ID：登出（本会话）、逐 token 吊销、以及「用户凭据变更」吊销出的每个 ID；
//   - 按用户：兜底关闭**没有稳定 ID** 的登记（CLI 路径签发的 ticket 无 Bearer 上下文可比，
//     其会话按用户关闭；见 aaa.Service.ActiveTokenID 对空 ID 的保守口径）。
//
// 关闭动作实现为**取消流**：登记时传入底层流的关闭函数，关闭即让 io.Copy 桥接两端随即出错
// 返回、handler 收尾。这是 roud49 场景下最小可用的中断手段——比新造一套 ctx 广播更贴近
// 既有实现（三处桥接都是 `io.Copy(stream, ws)` / `io.Copy(ws, stream)` 两侧 goroutine，
// 流的 Close 会把两侧都推醒）。会话进程本身（容器内的 shell、guest 内的会话）**不保证**随之
// 结束：容器侧如实登记过（决策 #358，Docker 无 exec 中止接口），本轮不改该边界。

import (
	"io"
	"sync"
)

// streamCloser 把桥接用的流适配为登记表的关闭函数（重复关闭的返回值忽略——流 A 被登记表关过
// 之后，handler 的 defer stream.Close() 还会再关一次，这是既有收尾路径，不该报错）。
func streamCloser(c io.Closer) func() { return func() { _ = c.Close() } }

// consoleSessionTable 活动终端会话登记表。
type consoleSessionTable struct {
	mu     sync.Mutex
	nextID uint64
	m      map[uint64]consoleSession
}

// consoleSession 一条活动终端会话。
type consoleSession struct {
	tokenID string
	user    string
	close   func()
}

func newConsoleSessionTable() *consoleSessionTable {
	return &consoleSessionTable{m: map[uint64]consoleSession{}}
}

// register 登记一条活动终端会话，返回**注销函数**（幂等，可安全 defer）。
// tokenID 为空时只按用户名可关（无稳定 ID 的旧式路径）。
func (t *consoleSessionTable) register(tokenID, user string, closeFn func()) func() {
	if t == nil || closeFn == nil {
		return func() {}
	}
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	t.m[id] = consoleSession{tokenID: tokenID, user: user, close: closeFn}
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		delete(t.m, id)
		t.mu.Unlock()
	}
}

// closeByToken 关闭该 token 稳定 ID 的全部活动会话，返回关闭条数。
// 空 ID 不关闭任何会话——调用方拿不到稳定 ID 时不能凭空串推断「该关谁」（不误伤）。
func (t *consoleSessionTable) closeByToken(tokenID string) int {
	if t == nil || tokenID == "" {
		return 0
	}
	return t.closeMatching(func(s consoleSession) bool { return s.tokenID == tokenID })
}

// closeByUser 关闭该用户的全部活动会话（含无稳定 ID 的登记），返回关闭条数。
func (t *consoleSessionTable) closeByUser(user string) int {
	if t == nil || user == "" {
		return 0
	}
	return t.closeMatching(func(s consoleSession) bool { return s.user == user })
}

// closeMatching 取走匹配的会话（**先从表里摘掉再关闭**：关闭会引发桥接收尾、注销函数随
// defer 再进一次表锁——摘掉后再关，路径最干净）并逐个关闭，返回条数。
func (t *consoleSessionTable) closeMatching(match func(consoleSession) bool) int {
	t.mu.Lock()
	var fns []func()
	for id, s := range t.m {
		if match(s) {
			fns = append(fns, s.close)
			delete(t.m, id)
		}
	}
	t.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return len(fns)
}

// count 当前登记条数（仅测试与判读用）。
func (t *consoleSessionTable) count() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// ---------- 凭据变更的统一收尾 ----------

// revokeUserSessions 用户凭据已变（改 class / 重置口令 / 删除用户 / 本人改口令）后的**主动收尾**
// （决策 #506）。分三层，少任何一层都有残留：
//
//	aaa       —— token 是凭据本体：吊销即「下一个请求要求重新登录」（含按当前配置重算身份的
//	             第二道防线之外的第一道，改动**立即**生效，不靠 TTL）；
//	cliExec   —— CLI 会话态（模式/层级）按 token 稳定 ID 键控，凭据没了它只增不减（#317 既有口径）；
//	终端登记表 —— 串口/VNC/容器终端拿不到 Bearer（WS 握手带不了），只能被主动关闭（#507）。
//
// 返回被吊销的 token 稳定 ID 列表（调用方可据此写审计/日志）。
// 无稳定 ID 的终端登记（CLI 路径签发的 ticket）按用户名兜底关闭。
func (s *Server) revokeUserSessions(user string) []string {
	if user == "" || s.aaa == nil {
		return nil
	}
	ids := s.aaa.RevokeUserTokens(user)
	for _, id := range ids {
		if s.cliExec != nil {
			s.cliExec.DropSession(id)
		}
		s.consoleTix.closeSessionsByToken(id)
	}
	s.consoleTix.closeSessionsByUser(user)
	return ids
}
