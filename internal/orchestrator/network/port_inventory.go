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
	"strconv"
	"strings"
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

// KernelIfFacts 内核侧物理网卡事实（决策 #302，首装接口可见性）：未被 VPP 接管的口在
// 内核侧仍有 netdev，驱动/MAC/速率/状态逐项读 sysfs——读视图据此如实展示，
// **不编造数据面（VPP）侧事实**。清单口径与 KernelIfnames 相同（仅含带 device 的物理口）。
type KernelIfFacts struct {
	Name      string // 网卡名（ens160…）
	AdminUp   bool   // 管理态（flags 的 IFF_UP 位）
	LinkUp    bool   // operstate == up
	LinkKnown bool   // operstate 可判（up/down）；unknown/读取失败 → false（上层不给 link）
	SpeedMbps uint32 // speed 文件（本就以 Mbps 计）；取不到（含口未连、驱动不支持）→ 0
	MAC       string
	Driver    string // device/driver 链接目标名；无 → 空（上层不给）
	MTU       int
}

// KernelIfFacts 内核侧物理口事实清单（顺序与 KernelIfnames 一致：按名排序去重）。
func (n *L2Network) KernelIfFacts() ([]KernelIfFacts, error) {
	names, err := n.KernelIfnames()
	if err != nil {
		return nil, err
	}
	out := make([]KernelIfFacts, 0, len(names))
	for _, name := range names {
		out = append(out, kernelIfFacts(sysfsNetRoot, name))
	}
	return out, nil
}

// kernelIfFacts 单口的内核事实：逐项读 sysfs，读不到/解析不了的字段保持零值
// （上层「取不到就不给」，不编造）。口未连时内核 speed 文件报 Invalid argument 或 -1，
// 解析失败即零值，正合口径。
func kernelIfFacts(root, name string) KernelIfFacts {
	f := KernelIfFacts{Name: name}
	if v, err := os.ReadFile(filepath.Join(root, name, "operstate")); err == nil {
		switch s := strings.TrimSpace(string(v)); s {
		case "up":
			f.LinkUp, f.LinkKnown = true, true
		case "down":
			f.LinkKnown = true
		}
	}
	if v, err := os.ReadFile(filepath.Join(root, name, "flags")); err == nil {
		if bits, err := strconv.ParseUint(strings.TrimSpace(string(v)), 0, 64); err == nil {
			f.AdminUp = bits&0x1 != 0 // IFF_UP
		}
	}
	if v, err := os.ReadFile(filepath.Join(root, name, "speed")); err == nil {
		if sp, err := strconv.ParseUint(strings.TrimSpace(string(v)), 10, 32); err == nil {
			f.SpeedMbps = uint32(sp)
		}
	}
	if v, err := os.ReadFile(filepath.Join(root, name, "address")); err == nil {
		f.MAC = strings.TrimSpace(string(v))
	}
	if link, err := os.Readlink(filepath.Join(root, name, "device", "driver")); err == nil {
		f.Driver = filepath.Base(link)
	}
	if v, err := os.ReadFile(filepath.Join(root, name, "mtu")); err == nil {
		if mtu, err := strconv.Atoi(strings.TrimSpace(string(v))); err == nil && mtu > 0 {
			f.MTU = mtu
		}
	}
	return f
}

// BridgeDomains VPP 中全部 bridge-domain 的运行态（决策 #84）。
func (n *L2Network) BridgeDomains() ([]BDRuntime, error) {
	if n == nil || n.l2 == nil {
		return nil, fmt.Errorf("VPP 未接入")
	}
	c, err := n.l2.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.BridgeDomains()
}

// InterfaceStates VPP 接口运行态（接口名 → 状态/速率/驱动）：`show interfaces physical`
// 的链接状态/速率/驱动自此取值（此前该列取自配置，与实测可能不一致）。
func (n *L2Network) InterfaceStates() (map[string]SwIfInfo, error) {
	if n == nil || n.l2 == nil {
		return nil, fmt.Errorf("VPP 未接入")
	}
	c, err := n.l2.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	m, err := c.SwInterfaceNames()
	if err != nil {
		return nil, err
	}
	out := make(map[string]SwIfInfo, len(m))
	for _, info := range m {
		if info.Name == "" {
			continue
		}
		out[info.Name] = info
	}
	return out, nil
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
