package api

// 决策 #444（收口 R7-2）：成员端口读视图的**运行态列合并**——CLI 与 REST 同一份实现。
//
// 由来：内核数据面下 Web 交换机详情页的容器派生端口（nfvisct…）运行态列显示「—」，
// 而同刻 CLI `show virtual-switches <n> ports` 同一行显示 up/up。根因是读路径差——
// 按展示名合并接口状态（InterfaceStates）与计数（state.InterfaceCounters）此前只写在
// CLI 的渲染里，REST `/virtual-switches/{name}/ports` 只发配置派生行，Web 只能改从
// `statistics.ports` 合并，而该列表按决策 #441 有意隐藏产品自持的容器宿主端 veth ⇒
// 派生条目拿不到状态。三面同源不能靠两处各写一遍（必然漂移），故把「按展示名取该端口的
// 接口状态与计数」抽成这一份实现：CLI 文本/结构化渲染与 REST `/ports` 逐行补字段都调它。
//
// 取不到即对应字段为 nil（调用方不写该键 / 文本渲染 "-"），不编造。

import "github.com/xzjt/nfvis/internal/state"

// switchPortRuntime 单个端口的运行态列（指针即「有没有该事实」：nil = 取不到）。
type switchPortRuntime struct {
	AdminUp   *bool
	LinkUp    *bool
	RxPackets *uint64
	TxPackets *uint64
}

// resolveSwitchPortRuntime 按展示名合并接口状态与计数，判定与 CLI 原行为**逐字一致**：
//   - states[label] 命中 ⇒ AdminUp/LinkUp 有值（取副本地址，避免别名到共享项）；
//   - counters 非 nil 且 counters(label) 命中 ⇒ RxPackets/TxPackets 有值
//     （counters 为 nil 视为计数源未装配 ⇒ 取不到）。
//
// 两类事实**独立**判定：状态取不到不牵连计数，反之亦然（与 CLI 改前两个 if 的行为相同）。
func resolveSwitchPortRuntime(label string, states map[string]InterfaceState,
	counters func(string) (state.InterfaceCounters, bool)) switchPortRuntime {
	var rt switchPortRuntime
	if st, ok := states[label]; ok {
		admin, link := st.AdminUp, st.LinkUp
		rt.AdminUp, rt.LinkUp = &admin, &link
	}
	if counters != nil {
		if c, ok := counters(label); ok {
			rx, tx := c.RxPackets, c.TxPackets
			rt.RxPackets, rt.TxPackets = &rx, &tx
		}
	}
	return rt
}
