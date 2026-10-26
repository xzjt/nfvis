package network

// 接口计数的**契约映射**（决策 #504）：govpp 的 stats 结构 → `state.InterfaceCounters`。
//
// 为什么单列一个文件：stats_linux.go 整个文件带 `//go:build linux`（statsclient 依赖 Linux
// 专属 syscall），映射本身却只是字段搬运——抽出来才能在日常开发平台（Windows）上用假 stats
// 值断言「丢弃到底是接上了还是恒 0」。本文件不碰任何底座调用。

import (
	"go.fd.io/govpp/api"

	"github.com/xzjt/nfvis/internal/state"
)

// interfaceCountersFrom 把一次 stats 取数的结果映射成契约计数。
//
// 口径（决策 #504）：VPP 的 `/interfaces/<if>/drops` **不区分方向**，故只填方向无关的
// `Drops`；`RxDrops`/`TxDrops` 留给内核网络数据面（它有两个方向的真值），VPP 出口**不发**
// 这两个键——把总数摊给 rx 或 tx 都属编造方向。
func interfaceCountersFrom(c api.InterfaceCounters) state.InterfaceCounters {
	return state.InterfaceCounters{
		RxPackets: c.Rx.Packets, TxPackets: c.Tx.Packets,
		RxBytes: c.Rx.Bytes, TxBytes: c.Tx.Bytes,
		RxErrors: c.RxErrors, TxErrors: c.TxErrors,
		Drops: c.Drops,
	}
}
