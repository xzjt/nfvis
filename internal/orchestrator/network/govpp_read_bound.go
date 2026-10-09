package network

// 读数路径有界化（决策 #422）：本文件是 `*_govpp.go` 各读数调用点的**唯一**有界包装实现。
//
// 背景（真机证据）：VPP 崩溃窗口内执行的一条 `show interfaces <if> detail`，其
// `sw_interface_dump` 多请求在**无超时**等待下永久阻塞在 govpp 的 receiveReplyInternal
// （SIGQUIT 全栈可见：core.(*Channel).receiveReplyInternal ← govppL2Client.SwInterfaceNames
// ← cliExecutor.vppIfaceNamesSafe）；该 goroutine 持着 CLI 执行器的全局互斥，于是**所有**
// CLI 命令（含 `show version`）排队挂起——HTTP 层仍回 200，管理面整体不可用，只能重启 nfvisd。
//
// 口径：
//   - 读数路径上的每一次 ReceiveReply 都装上应答时限（缺省 3s，见 vppReadBudget），
//     到期返回明确错误（errVPPReadTimeout），绝不无限期等待、绝不长期占用调用方（含 CLI 全局锁）；
//   - 连接中断/应答转发失败时 govpp 会把带错误码的应答投进通道，包装原样透传（立即返回，
//     不误判成超时）；
//   - **写路径（提交/下发）不改行为**：时限只在**单次 ReceiveReply 期间**生效，返回前即撤销回
//     govpp 缺省（0 = 不限时）；写方法走的 SendRequest+ReceiveReply 不经过本包装，错误语义与
//     超时语义逐字不变。README 式的口径：本包装只服务「读数」，不改变编排下发。
//
// 为什么用 govpp 自带的应答时限，而不是外挂 goroutine + timer：govpp 的 `Channel.Close()`
// 只关闭请求队列（`close(ch.reqChan)`），**不会**打断已阻塞在应答通道上的 ReceiveReply
// （v0.13.0 core/channel.go 的 receiveReplyInternal 只 select 应答通道与自带 timer），
// 外挂 goroutine 在超时后无处可退——每次超时泄漏一个 goroutine，且它可能偷走该通道后续
// 复用时的应答。channel 的应答时限由 govpp 在 receiveReplyInternal 内以 timer 实现，
// 是唯一能真正结束等待的机制，故本包装以它为底座（api.Channel 已暴露 SetReplyTimeout，
// 不引入新依赖）。

import (
	"errors"
	"fmt"
	"time"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/core"
)

// defaultVPPReadBudget 读数路径的单次应答预算（决策 #422）。
//
// 取值口径：正常机器上一次 dump 的**单条**应答都在毫秒级；3s 足够吸收负载尖峰与
// 大表（VPP 逐条回包，预算按「下一次应答」计，整表不累计），而管理面感知的卡顿上限
// 就是它——比「先卡死再等人重启 nfvisd」低三个数量级。
const defaultVPPReadBudget = 3 * time.Second

// vppReadBudget 读数路径的统一预算（测试可改成小值；<=0 = 关闭有界，退回原有「不限时」语义）。
var vppReadBudget = defaultVPPReadBudget

// errVPPReadTimeout 读数超时（在预算内没等到应答）。错误文案自带自查路径，操作者据此
// 先看数据面连接与状态，而不是把它当成「这个接口/这条命令坏了」。
var errVPPReadTimeout = errors.New("数据面读数超时")

// recvReplyBound 等一次**单请求**应答（有界）。
func recvReplyBound(ch api.Channel, req api.RequestCtx, reply api.Message) error {
	return recvBound(ch, func() error { return req.ReceiveReply(reply) })
}

// recvMultiBound 等一次**多请求**（dump）应答（有界）：stop=true 表示该 dump 已读完。
func recvMultiBound(ch api.Channel, req api.MultiRequestCtx, reply api.Message) (bool, error) {
	var stop bool
	err := recvBound(ch, func() error {
		var e error
		stop, e = req.ReceiveReply(reply)
		return e
	})
	return stop, err
}

// recvBound 在读数预算内执行一次 ReceiveReply（两个包装的唯一实现）。
//
// 逐次装/撤时限（而不是构造期一次性设定）有三个好处：① 写路径完全不受影响——调用写方法时
// 通道的应答时限恒为 govpp 缺省（本包装每次返回前已撤销）；② 嵌套/复用安全——dump 循环里
// 每次 ReceiveReply 自己装上时限，方法之间互相调用（如 BridgeDomains 先调 SwInterfaceNames）
// 不会把外层的时限「还原」掉；③ 连接中断立刻可见——govpp 对断连/转发失败会投递带错误的
// 应答，ReceiveReply 立即返回，本包装原样透传，不会被超时掩盖。
//
// 前提：同一条 channel 不被多个 goroutine 并发使用（govpp 本就不允许共享 channel，
// 本包的各 Provider 每次操作各自经工厂取新 channel、用完即 Close）。
func recvBound(ch api.Channel, recv func() error) error {
	budget := vppReadBudget
	if budget <= 0 {
		return recv()
	}
	ch.SetReplyTimeout(budget)
	defer ch.SetReplyTimeout(0)
	err := recv()
	if err != nil && errors.Is(err, core.ErrReplyTimeout) {
		return fmt.Errorf("%w（等待数据面应答超过 %s，已放弃本次读数；自查：show vpp 确认数据面连接与状态）: %w",
			errVPPReadTimeout, budget, err)
	}
	return err
}
