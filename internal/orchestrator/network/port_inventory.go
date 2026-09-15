package network

// 运行态端口清单（决策 #83）。
//
// 背景：`<ifname>` 的动态候选此前取自 committed 配置里的 `interfaces[].name`，
// 于是**既漏真又含假**——漏掉未声明的 DPDK 口（已接管的口在内核中已无 netdev，
// 只存在于 VPP），又会列出根本不存在的名字（set 阶段不校验、commit 才失败）。
// 候选与展示都必须来自真实端口，且两侧语义不同：
//
//   - VPP 侧（`sw_interface_dump`）：数据面端口，即「已被 DPDK 接管」的那批
//     （`set interfaces <n>`、`show interfaces physical`、抓包、monitor、clear 统计…）；
//   - 内核侧（`/sys/class/net/<n>/device` 存在的物理口）：尚未接管的网卡
//     （管理口、`bind-dpdk`、SR-IOV PF——这三处的名字在内核侧才成立）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// sysfsNetRoot 内核网卡目录（测试可改）。
var sysfsNetRoot = "/sys/class/net"

// vppLoopbackName VPP 内置 loopback：不是物理口，不作为端口候选。
const vppLoopbackName = "local0"

// VPPIfnames VPP 中的接口名（数据面端口；已排序去重），不含 VPP 内置 loopback。
// VPP 未接入（编排器未装配或连接失败）时返回错误，由调用方退化为「仅关键字」。
func (n *L2Network) VPPIfnames() ([]string, error) {
	if n == nil || n.l2 == nil {
		return nil, fmt.Errorf("VPP 未接入")
	}
	c, err := n.l2.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	names, err := c.SwInterfaceNames()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, info := range names {
		if info.Name == "" || info.Name == vppLoopbackName {
			continue
		}
		out = append(out, info.Name)
	}
	return dedupeSorted(out), nil
}

// KernelIfnames 内核网卡名（物理口）：仅取含 `device` 链接的条目——lo/docker0/virbr0
// 这类虚拟接口没有 `device`，据此自然排除。已由 DPDK 接管的口在内核中已无 netdev，
// 故不会出现在这里。
func (n *L2Network) KernelIfnames() ([]string, error) {
	entries, err := os.ReadDir(sysfsNetRoot)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if _, err := os.Stat(filepath.Join(sysfsNetRoot, name, "device")); err != nil {
			continue // 无 device = 虚拟接口（lo/docker0/virbr0/bond…），不是物理口
		}
		out = append(out, name)
	}
	return dedupeSorted(out), nil
}

// dedupeSorted 去重并排序（候选顺序稳定，便于比对与测试）。
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
