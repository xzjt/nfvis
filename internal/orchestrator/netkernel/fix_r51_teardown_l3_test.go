package netkernel

// round51 P2：数据面整体 teardown 把 `type=l3` 的交换机当成 L2 bridge 删，顺手抹掉 VRF 设备
// 并跳过它全部清理。
//
// 缺陷形态（静态评审定位）：teardown 第 5 段遍历 cfg.VirtualSwitches（**含 type=l3 条目**）调
// DeleteBridgeDomain，而该方法的第一动作是 `ip link del LinkName(name)`——L3 交换机的内核设备名
// 正是交换机名本身（ApplyVRF 的 LinkName(vrf.Name)，一台 VRF 设备）。VRF 设备被删掉后，紧随
// 其后的 DeleteVRF 再查成员（按 master）与设备在场与否一律为空 ⇒ 成员口地址不清、`<口>.<vlan>`
// 子接口不删、域兜底路由（决策 #446 的两条 unreachable default）永久留在表里，而整个 teardown
// 报成功（无 DATAPLANE_TEARDOWN_FAILED）。
//
// ⚠️ 假件必须**模拟「删掉 master 设备会解 enslave」**：真内核如此，不模拟这一步，后续 DeleteVRF
// 仍能从假内核里读到成员并照常清理——缺陷会被假件藏起来（本仓常见失效模式）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fixR51Kernel 在内核夹具 dhcpFakeHost（设备表 + 建/删/up/master 语义）之上补齐本用例必需的
// 读命令与一条写语义：
//   - `ip -d -j link show dev <名>`：设备类型（判「同名设备是 VRF」的闸门要用）；
//   - `ip -j link show master <名>`：按 master 取成员（DeleteVRF 的成员清理来源）；
//   - `ip -j addr show dev <名>`：设备地址（同上）；
//   - `ip link add name <n> type {vrf,vlan}`：按真内核语义把设备登记进设备表（含类型）——让
//     「设备类型」这条判据（DeleteBridgeDomain 的 VRF 闸门、isVlanSubifDevice）真正可验；
//   - `ip addr replace|del <cidr> dev <n>`：按真内核语义增删地址（让「地址是否被清掉」可验）；
//   - `ip link del <设备>`：真内核会解 enslave 其成员，假件照做（否则缺陷不可见）。
type fixR51Kernel struct{ *dhcpFakeHost }

func (k fixR51Kernel) record(line string) {
	k.mu.Lock()
	k.cmds = append(k.cmds, line)
	k.mu.Unlock()
}

// Run 实现 Runner；未覆盖的命令原样交给夹具（夹具自己记录命令）。
func (k fixR51Kernel) Run(ctx context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	switch {
	case strings.HasPrefix(line, "ip link add ") && (strings.Contains(line, " type vrf") ||
		strings.Contains(line, " type vlan")):
		kind := "vlan"
		if strings.Contains(line, " type vrf") {
			kind = "vrf"
		}
		dev := ""
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "name" {
				dev = fields[i+1]
			}
		}
		k.record(line)
		if dev != "" {
			k.mu.Lock()
			if _, ok := k.links[dev]; !ok {
				k.next++
				k.links[dev] = &dhcpFakeLink{index: k.next, kind: kind, mac: "02:00:00:00:00:02"}
			}
			k.mu.Unlock()
		}
		return "", nil
	case strings.HasPrefix(line, "ip addr replace "), strings.HasPrefix(line, "ip addr del "):
		// `ip addr <replace|del> <cidr> dev <名>`
		k.record(line)
		fields := strings.Fields(line)
		if len(fields) >= 6 {
			verb, cidr, dev := fields[2], fields[3], fields[5]
			k.mu.Lock()
			if l := k.links[dev]; l != nil {
				if verb == "del" {
					out := l.addrs[:0]
					for _, a := range l.addrs {
						if a != cidr {
							out = append(out, a)
						}
					}
					l.addrs = out
				} else if !fixR51HasAddr(l.addrs, cidr) {
					l.addrs = append(l.addrs, cidr)
				}
			}
			k.mu.Unlock()
		}
		return "", nil
	case strings.HasPrefix(line, "ip -d -j link show dev "):
		dev := strings.TrimPrefix(line, "ip -d -j link show dev ")
		k.record(line)
		l := k.link(dev)
		if l == nil {
			// 真 ip 的设备不存在行为（linkDetail 据此判 ok=false）。
			return fmt.Sprintf("Device %q does not exist.", dev), fmt.Errorf("exit status 1")
		}
		type info struct {
			InfoKind  string `json:"info_kind"`
			SlaveKind string `json:"info_slave_kind,omitempty"`
		}
		type row struct {
			Ifname   string `json:"ifname"`
			Master   string `json:"master,omitempty"`
			LinkInfo *info  `json:"linkinfo,omitempty"`
		}
		r := row{Ifname: dev, Master: l.master}
		if l.kind != "" {
			r.LinkInfo = &info{InfoKind: l.kind}
			if l.master != "" {
				if m := k.link(l.master); m != nil && m.kind != "" {
					r.LinkInfo.SlaveKind = m.kind
				}
			}
		}
		b, _ := json.Marshal([]row{r})
		return string(b), nil
	case strings.HasPrefix(line, "ip -j link show master "):
		master := strings.TrimPrefix(line, "ip -j link show master ")
		k.record(line)
		type row struct {
			Ifname string `json:"ifname"`
		}
		rows := []row{}
		k.mu.Lock()
		for n, l := range k.links {
			if l.master == master {
				rows = append(rows, row{Ifname: n})
			}
		}
		k.mu.Unlock()
		b, _ := json.Marshal(rows)
		return string(b), nil
	case strings.HasPrefix(line, "ip -j addr show dev "):
		dev := strings.TrimPrefix(line, "ip -j addr show dev ")
		k.record(line)
		l := k.link(dev)
		if l == nil {
			return fmt.Sprintf("Device %q does not exist.", dev), fmt.Errorf("exit status 1")
		}
		type addrInfo struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		}
		type row struct {
			AddrInfo []addrInfo `json:"addr_info"`
		}
		r := row{AddrInfo: []addrInfo{}}
		for _, cidr := range l.addrs {
			if i := strings.IndexByte(cidr, '/'); i > 0 {
				var pl int
				_, _ = fmt.Sscanf(cidr[i+1:], "%d", &pl)
				r.AddrInfo = append(r.AddrInfo, addrInfo{Local: cidr[:i], PrefixLen: pl})
			}
		}
		b, _ := json.Marshal([]row{r})
		return string(b), nil
	case strings.HasPrefix(line, "ip link del "):
		out, err := k.dhcpFakeHost.Run(ctx, name, args...)
		if err == nil {
			// 真内核：删掉 master 设备会把它名下的成员解 enslave（缺陷可见性的前提）。
			fields := strings.Fields(line)
			dev := fields[3]
			if len(fields) > 4 && fields[3] == "dev" {
				dev = fields[4]
			}
			k.mu.Lock()
			for _, l := range k.links {
				if l.master == dev {
					l.master = ""
				}
			}
			k.mu.Unlock()
		}
		return out, err
	}
	return k.dhcpFakeHost.Run(ctx, name, args...)
}

// fixR51HasAddr 地址列表里是否已有该 CIDR（假内核的 addr replace 幂等）。
func fixR51HasAddr(addrs []string, cidr string) bool {
	for _, a := range addrs {
		if a == cidr {
			return true
		}
	}
	return false
}

// fixR51TeardownHost 预置一台「已收敛的现场」：L2 交换机 vs-lan（bridge + 一个成员口）与
// L3 交换机 vs-l3（VRF 设备 + 一条带地址与 vlan 的 l3-interface + 静态路由）。
func fixR51TeardownHost() (*dhcpFakeHost, model.Config) {
	h := newDHCPFakeHost()
	h.addLink("vs-lan", "bridge")
	h.addLink("ens192", "")
	h.link("ens192").master = "vs-lan"
	h.addLink("vs-l3", "vrf")
	// 子接口按真内核语义登记为 vlan 设备（DeleteVRF 的「要不要连设备一起删」判据是内核类型，
	// 见 isVlanSubifDevice）。
	h.addLink("ens224.100", "vlan", "10.0.0.1/24")
	h.link("ens224.100").master = "vs-l3"
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-lan", Type: "l2"},
			{Name: "vs-l3", Type: "l3"},
		},
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{{
			Interface: "ens224", Vlan: 100, Addresses: []string{"10.0.0.1/24"},
		}}}},
	}
	return h, cfg
}

// TestTeardownDataplaneCleansL3SwitchViaVrfPath 带 `type=l3` 交换机的 teardown：
// L3 交换机必须走 VRF 路径清理（成员口地址、vlan 子接口、域兜底路由），而**不是**在 bridge
// 路径上被裸删——否则那些清理全部落空、teardown 却报成功。
func TestTeardownDataplaneCleansL3SwitchViaVrfPath(t *testing.T) {
	h, cfg := fixR51TeardownHost()
	// LinkName("ens224") + ".100"（内核实况里 l3-interface 的子接口设备名）。
	subif, err := l3DeviceNameChecked(cfg.Vrfs[0].L3Interfaces[0])
	if err != nil {
		t.Fatalf("用例前提：子接口设备名应可派生: %v", err)
	}
	p := New(fixR51Kernel{h})
	if err := p.TeardownDataplane(context.Background(), cfg); err != nil {
		t.Fatalf("teardown 应成功（各删除原语幂等）: %v", err)
	}
	for _, want := range []string{
		"ip addr del 10.0.0.1/24 dev " + subif,                                   // 成员口地址
		"ip link del " + subif,                                                   // vlan 子接口设备
		"ip route del vrf vs-l3 unreachable default metric " + vrfFallbackMetric, // 域兜底（v4）
		"ip -6 route del vrf vs-l3 unreachable default metric " + vrfFallbackMetric,
		"ip link del vs-lan", // L2 交换机照常删（过滤不能把 L2 一起跳过）
	} {
		if !h.hasCmd(want) {
			t.Fatalf("teardown 应下发 %q；实际：\n%s", want, strings.Join(h.lastCmds(), "\n"))
		}
	}
	// 判据的核心：删 VRF 设备必须在成员清理**之后**（先解引用、后删被引用），
	// 即不存在「先把 VRF 设备删掉、后续清理查不到对象」的那条路径。
	cmds := h.lastCmds()
	addrAt, delAt := -1, -1
	for i, c := range cmds {
		if c == "ip addr del 10.0.0.1/24 dev "+subif {
			addrAt = i
		}
		if c == "ip link del vs-l3" {
			delAt = i
		}
	}
	if addrAt < 0 || delAt < 0 || addrAt > delAt {
		t.Fatalf("成员口地址清理必须发生在 VRF 设备被删之前（addr=%d del-vrf=%d）：\n%s",
			addrAt, delAt, strings.Join(cmds, "\n"))
	}
}

// TestDeleteBridgeDomainRefusesVRFDevice 闸门在被删对象本身：任何调用方对「内核里的同名设备是
// 一台 VRF」调 DeleteBridgeDomain 都必须被拒（不得下发 `ip link del <VRF 设备>`）；对真正的
// bridge（或设备不在场）照旧删。
func TestDeleteBridgeDomainRefusesVRFDevice(t *testing.T) {
	h, _ := fixR51TeardownHost()
	p := New(fixR51Kernel{h})
	ctx := context.Background()
	err := p.DeleteBridgeDomain(ctx, "vs-l3")
	if err == nil {
		t.Fatal("对 VRF 设备调 DeleteBridgeDomain 应被拒绝（否则会凭空删掉 L3 交换机的 VRF）")
	}
	if !strings.Contains(err.Error(), "L3 交换机") {
		t.Fatalf("拒绝文案应说清「这是 L3 交换机的 VRF 设备」并给出出路：%v", err)
	}
	if h.hasCmd("ip link del vs-l3") {
		t.Fatalf("拒绝路径不得下发任何删除命令：\n%s", strings.Join(h.lastCmds(), "\n"))
	}
	if !h.has("vs-l3") {
		t.Fatal("拒绝路径不得改动设备表")
	}
	// 真 bridge：照旧删除（闸门不误伤 L2）。
	if err := p.DeleteBridgeDomain(ctx, "vs-lan"); err != nil {
		t.Fatalf("L2 交换机的 bridge 应照常删除: %v", err)
	}
	if h.has("vs-lan") {
		t.Fatal("L2 bridge 应被删除")
	}
	// 设备不在场（从未收敛/已删过）：幂等放行，不误报。
	if err := p.DeleteBridgeDomain(ctx, "vs-gone"); err != nil {
		t.Fatalf("设备不在场时应幂等放行: %v", err)
	}
}
