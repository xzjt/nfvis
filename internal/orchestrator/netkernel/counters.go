package netkernel

// 接口计数的内核侧读数（决策 #504）。
//
// 与 VPP 数据面的**关键差别**：VPP 的 stats segment 只有 `/interfaces/<if>/drops`
// 一个方向无关的总数，内核的 sysfs 有 `statistics/rx_dropped` 与 `statistics/tx_dropped`
// **两个方向的真值**。故两边各按自己诚实的那一口径呈现：
//
//   - VPP：`drops`（总数，不摊方向）；
//   - 内核：`rx_drops` + `tx_drops`（按方向），**不发** `drops`
//     （把两个方向相加冒充「总数」会让同一事实有两种口径，见决策 #504 的如实边界）。
//
// 读法：逐项读 sysfs，读不到即返回 ok=false（上层按「取不到就不给」处理，不编造 0）。
// `statistics/` 下的计数是内核维护的累计值，读取**不重置**（这点与 `clear` 不同：内核
// 数据面下 `clear interfaces statistics` 如实拒绝，见 netkernel 的清零实现）。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/state"
)

// InterfaceCounters 单口的内核计数（`state.InterfaceCounters` 的内核口径：按方向给丢弃）。
//
// 任何一个计数读不到都返回 ok=false：半个读数比没有更危险（会让人以为口上没流量）。
// 内核里不存在该 netdev（口被 DPDK 接管 / 名字写错）同样 ok=false。
//
// ctx 不参与取数（sysfs 是本地文件读，量级为微秒），保留它是为了满足 `state.Counters`
// 的签名——与 `state.Runtime.InterfaceCounters` 同形，装配处不必写适配层。
func (p *Provider) InterfaceCounters(ctx context.Context, ifname string) (state.InterfaceCounters, bool) {
	if p == nil {
		return state.InterfaceCounters{}, false
	}
	return p.rt().interfaceCounters(ifname)
}

// interfaceCounters 逐项读 sysfs（`statistics/<counter>`）。
func (r *Runtime) interfaceCounters(ifname string) (state.InterfaceCounters, bool) {
	if ifname == "" {
		return state.InterfaceCounters{}, false
	}
	dir := filepath.Join(sysfsNetRoot, ifname, "statistics")
	read := func(file string) (uint64, bool) {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	var c state.InterfaceCounters
	for _, f := range []struct {
		name string
		dst  *uint64
	}{
		{"rx_packets", &c.RxPackets}, {"tx_packets", &c.TxPackets},
		{"rx_bytes", &c.RxBytes}, {"tx_bytes", &c.TxBytes},
		{"rx_errors", &c.RxErrors}, {"tx_errors", &c.TxErrors},
		{"rx_dropped", &c.RxDrops}, {"tx_dropped", &c.TxDrops},
	} {
		v, ok := read(f.name)
		if !ok {
			return state.InterfaceCounters{}, false
		}
		*f.dst = v
	}
	// 内核口径：按方向给丢弃，出口**不发** `drops`
	// （state.InterfaceCounters.MarshalJSON 按该标记决定发哪些键）。
	c.DirectionalDrops = true
	return c, true
}
