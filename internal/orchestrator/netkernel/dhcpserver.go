package netkernel

// 内核数据面 DHCP 服务器（v3 决策 #438；FR-NET-019 的内核侧落地）。
//
// VPP 侧形态（决策 #359）= 用户态服务器 + 每交换机一条内置 L2 tap（bridge-domain 成员）
// + UDP/67 punt。其中**池/租约/报文解析与构造/handleMessage/池耗尽告警都是传输无关的**，
// VPP 专有的只有两处：「tap 由 tapv2 建并加入 BD」与「UDP/67 的 punt 注册」。故内核侧
// **不复制**服务器核心：network.DHCPServerProvider 整个复用，本文件只提供两个 seam 的
// 内核实现 + 装配（复用路径与 VPP 侧的差别逐字写在下面两段）。
//
//  1. **tap 面**（network.DHCPServerClient 的内核实现，见 kernelDHCPServerClient）：
//     每交换机建**一条内核 TUN/TAP netdev**（`ip tuntap add dev <nfvisdhXXXX> mode tap`，
//     名沿用 network.DHCPServerTapName：`nfvisdh` + 8 位十六进制 = 15 字符 = IFNAMSIZ 上限），
//     `ip link set dev <tap> master <该交换机的内核 bridge>`（内核数据面下 BVI 等价物就是
//     这个 bridge，网关地址落在它上面，见 l2.go 的 applyGateway）、`ip link set dev <tap> up`。
//     **删除按名核对身份**（`nfvisdh` + 8 位十六进制）后再删——内核接口索引会被复用
//     （round143 真机实测同一 tap 三次重建均拿到同一索引），绝不按旧索引删。
//     **并由产品自己打开 /dev/net/tun + TUNSETIFF 长期持有该 fd**（见 dhcpserver_tap.go 的文件头：
//     没有持有者的持久化 tap 是 NO-CARRIER、bridge 不投递；且 tap 的「线」就是持有它的 fd，
//     收发必须直接读写该 fd——写它的 AF_PACKET 是「交给持有者」的反方向）。
//
//  2. **单播续租面**（UDP/67 socket，见 dhcpUnicastManager）：每交换机一个**绑到该交换机
//     BVI 网关地址的 UDP/67 socket**（`SO_BINDTODEVICE` 绑该域 VRF 设备，与既有 DHCP 中继
//     上行同法：BVI 地址在 VRF 里，不绑设备则本地查找落在主表）。socket 收到的载荷带客户端
//     源地址，按「合成 IPv4 头 + 原载荷」交给 provider 的上行解析路径（与 VPP punt 上行
//     **同一入口**：描述符的 SwIfIndex 写该交换机内核 bridge 的 ifindex，provider 经
//     SetSwitchResolver 派发到对应交换机）。内核没有 punt 机制，故 PuntClient 实现为空操作
//     （内核侧的「注册在不在场」由 socket 生命周期表达：socket 在＝注册在）。
//     **为什么绑具体地址而不是 INADDR_ANY**：绑具体地址的 socket 只收目的地址＝该地址的单播
//     （续租/释放），域内广播（DISCOVER 等）天然不进这条路径、只走 tap 洪泛——两条入径互补，
//     与 VPP 侧「广播→tap、单播→punt」同构。
//
// 生命周期与口径（与 VPP 侧同族）：随提交编排在 bridge-domain 之后收敛（Sync；未声明/池被删
// ＝teardown）；**恢复重放必须含它**（nfvisd 重启后进程内的 socket 全失，tap 是内核对象、
// 按名复用）；15s 巡检幂等收敛（补 tap/补 socket、回收到期租约、复核池耗尽告警）；进程退出
// 关闭 socket 与收包协程（tap 对象保留，下次启动按名复用）。起不来（bridge 不存在 / tap 建不出
// / socket 绑不上）**如实报错**并进未收敛项（提交失败并回滚 / 恢复收敛告警），不静默。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	// dhcpTapNamePrefix 产品自持内核 DHCP tap 的名字前缀（与 network.DHCPServerTapName 一致）。
	// 名字总形如 `nfvisdh` + 8 位小写十六进制；**按名核对身份**是删/复用/读视图过滤的唯一判据。
	dhcpTapNamePrefix = "nfvisdh"
	// dhcpUnicastPort DHCP 服务器端口（RFC 2131；与 VPP 侧的 punt 注册端口同值）。
	dhcpUnicastPort = 67
	// dhcpUnicastStopTimeout 停一台交换机单播接收时的等待上界（「起→停 socket/收包协程不残留」
	// 是契约要求：超时如实报错，不谎称已停）。
	dhcpUnicastStopTimeout = 3 * time.Second
)

// isProductDHCPTapName 是否是产品自持的内核 DHCP tap 名（`nfvisdh` + 8 位小写十六进制）。
// 严格到字符集：删/复用都要按它核对身份，放宽会把用户自己的接口认成内置 tap。
func isProductDHCPTapName(name string) bool {
	if len(name) != len(dhcpTapNamePrefix)+8 || !strings.HasPrefix(name, dhcpTapNamePrefix) {
		return false
	}
	for _, c := range name[len(dhcpTapNamePrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ---------- 交换机规格（由声明派生） ----------

// dhcpPlan 一台交换机 DHCP 服务器在内核侧所需的传输面规格：tap 名、bridge 名、BVI 地址与
// 该域 VRF 的内核设备名。规格变化 ⇒ 单播 socket 重建（tap 由复用的 provider 核心自行处理）。
type dhcpPlan struct {
	switchName string
	bridge     string // 该交换机的内核 bridge（LinkName 口径；网关地址落在它上面）
	tapName    string // 内置 tap 的内核侧名（network.DHCPServerTapName）
	bvi        net.IP // 单播接收绑定的 BVI 网关地址（也是服务器标识 option 54）
	vrfDevice  string // 该域 VRF 的内核设备名（socket 的作用域，同 applyGateway 的派生口径）
}

func (p dhcpPlan) equal(o dhcpPlan) bool {
	return p.switchName == o.switchName && p.bridge == o.bridge && p.tapName == o.tapName &&
		p.vrfDevice == o.vrfDevice && p.bvi.Equal(o.bvi)
}

// dhcpPlanOf 由交换机声明派生传输面规格。
//
// 与 network.dhcpServerSpecOf / netkernel.relayTargetOf **逐字同源**：BVI 取
// gateway.addresses 里**第一个 IPv4**（顺序敏感）；VRF 设备名 = 显式 `gateway vrf` 经
// LinkName，否则派生的 `vr-<交换机名>`。配置层（model.Validate）已拦下不满足的形态，这里是
// 纵深防御（恢复重放与直接调用编排的路径仍会走到），失败如实报错、不静默用零值。
func dhcpPlanOf(vs model.VirtualSwitch) (dhcpPlan, error) {
	if vs.Type == "l3" {
		return dhcpPlan{}, fmt.Errorf(
			"交换机 %s 是 type=l3（没有 BVI 网关），DHCP 服务器仅支持已配置网关的 L2 交换机", vs.Name)
	}
	bvi, _, ok := vs.GatewayIPv4()
	if !ok || bvi == nil {
		return dhcpPlan{}, fmt.Errorf(
			"交换机 %s 没有 IPv4 网关地址，无法作为 DHCP 服务器域（先 set virtual-switches %s gateway ip <ip-prefix>）",
			vs.Name, vs.Name)
	}
	vrfDevice := GatewayVRFName(vs.Name)
	if vs.Gateway != nil && vs.Gateway.Vrf != "" {
		vrfDevice = LinkName(vs.Gateway.Vrf)
	}
	return dhcpPlan{
		switchName: vs.Name,
		bridge:     LinkName(vs.Name),
		tapName:    network.DHCPServerTapName(vs.Name),
		bvi:        append(net.IP(nil), bvi.To4()...),
		vrfDevice:  vrfDevice,
	}, nil
}

// ---------- 内核接口清单（tap 的识别/身份核对/索引回读） ----------

// dhcpLinkRow `ip -d -j link show` 的一行（只取 DHCP 内置 tap 需要判的字段）。
type dhcpLinkRow struct {
	Ifname   string `json:"ifname"`
	Ifindex  uint32 `json:"ifindex"`
	Address  string `json:"address"`
	LinkInfo *struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

// kind 设备自身类型（读不到返回空串——调用方按「读不到」保守处理，不猜）。
func (r dhcpLinkRow) kind() string {
	if r.LinkInfo == nil {
		return ""
	}
	return r.LinkInfo.InfoKind
}

// kernelLinkRows 读内核接口清单（**纯读**，故不走 Provider 的命令串行锁——它只读不写，
// 与任何下发交错都不改变结果）。空输出按空清单处理（无错误即视为命令成功；真机上
// `ip -j link show` 至少输出 `[]`）。
func kernelLinkRows(ctx context.Context, run Runner) ([]dhcpLinkRow, error) {
	out, err := run.Run(ctx, "ip", "-d", "-j", "link", "show")
	if err != nil {
		return nil, fmt.Errorf("读取内核接口清单: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var rows []dhcpLinkRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("解析内核接口清单: %w", err)
	}
	return rows, nil
}

// linkRowsByIndex 按内核接口索引取一行（索引不存在返回 ok=false——内核接口索引会被复用，
// **必须**每次现查，不信任任何进程内记下的旧索引）。
func linkRowsByIndex(rows []dhcpLinkRow, index uint32) (dhcpLinkRow, bool) {
	for _, r := range rows {
		if r.Ifindex == index && r.Ifname != "" {
			return r, true
		}
	}
	return dhcpLinkRow{}, false
}

// ---------- seam 1：tap 面（network.DHCPServerClient 的内核实现） ----------

// kernelDHCPServerClient network.DHCPServerClient 的内核实现（见文件头第 1 段）。
//
// **交换机事实（bridge 名 / 服务 VLAN）全部来自调用点传入的声明 sw**（provider 的 Sync 拿的
// 就是本次下发的那份 vs）——**绝不在 apply 路径回读配置引擎**：提交期间发动机锁由本次提交
// 自己持有，`Provider.config()` → `Engine.Committed` 重入即**自死锁**（真机 SIGQUIT 全栈实证：
// 一次 `set … dhcp-server pool …` 提交把整个管理面挂死）。内核事实（索引/身份）每次现读，
// 不缓存。
type kernelDHCPServerClient struct {
	p  *Provider
	sw model.VirtualSwitch // 本次收敛的交换机声明（回收路径只有名字，见 deleteTap）
}

// Close 无资源可收（tap 是内核对象，生命周期由 TapCreate/TapDelete 显式表达）。
func (c *kernelDHCPServerClient) Close() {}

// TapCreate 创建/复用内核 tap：同名设备已存在即复用（幂等），否则 `ip tuntap add … mode tap`
// 并回读索引（内核刚注册的设备条目可能稍后才可见，有界重试；仍读不到如实报错）。
func (c *kernelDHCPServerClient) TapCreate(hostIfName, _ string) (uint32, error) {
	ctx := context.Background()
	// 建 tap **之前**先做纯配置校验：服务 VLAN 派生不出来（纯 trunk 声明了 VLAN 却没有
	// access/native VLAN）时如实失败——该错误与内核状态无关、重试也不会好，在这里挡住就
	// 不会留下一个「建了却永远不服务」的孤儿 tap（真机实证过中间态：tap 建了、后续步骤失败）。
	if c.sw.Name != "" {
		if _, err := tapServiceVlanOf(c.sw); err != nil {
			return 0, err
		}
	}
	rows, err := kernelLinkRows(ctx, c.p.run)
	if err != nil {
		return 0, err
	}
	if row, ok := dhcpTapRowByName(rows, hostIfName); ok {
		if k := row.kind(); k != "" && k != "tun" {
			return 0, fmt.Errorf("内核设备 %s 已存在但不是 TUN/TAP（类型 %q）：不冒认，请先处理该设备", hostIfName, k)
		}
		return row.Ifindex, nil // 同名已存在 ⇒ 复用（恢复重放/重装路径）
	}
	if err := c.p.ipReq(ctx, "tuntap", "add", "dev", hostIfName, "mode", "tap"); err != nil {
		// 竞态（同名设备在探测与创建之间出现）：按已存在处理，回读身份与索引。
		if rows, rerr := kernelLinkRows(ctx, c.p.run); rerr == nil {
			if row, ok := dhcpTapRowByName(rows, hostIfName); ok {
				return row.Ifindex, nil
			}
		}
		return 0, fmt.Errorf("创建内核内置 tap（ip tuntap add dev %s mode tap）: %w", hostIfName, err)
	}
	var lastErr error
	for i := 0; i < 10; i++ {
		rows, err := kernelLinkRows(ctx, c.p.run)
		if err != nil {
			return 0, err
		}
		if row, ok := dhcpTapRowByName(rows, hostIfName); ok {
			return row.Ifindex, nil
		}
		lastErr = fmt.Errorf("内核里还没有它")
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("内核内置 tap %s 已创建但回读不到（%v）", hostIfName, lastErr)
}

// TapDelete 删除内置 tap：**按名核对身份**——先按索引现查名字，名不符（索引被复用给别的接口）
// 或索引已不存在（设备被带外删）都不删（前者防误删用户接口，后者视为已达成）。
func (c *kernelDHCPServerClient) TapDelete(swIfIndex uint32) error {
	ctx := context.Background()
	rows, err := kernelLinkRows(ctx, c.p.run)
	if err != nil {
		return err
	}
	row, ok := linkRowsByIndex(rows, swIfIndex)
	if !ok {
		return nil // 索引已不存在（带外删/内核重启）：已达成
	}
	if !isProductDHCPTapName(row.Ifname) {
		// 内核接口索引会被复用：按旧索引删会误伤用户接口，故名字不符一律不删。
		slog.Warn("内核接口索引已被复用给非内置 DHCP tap 的接口，跳过删除（不按旧索引误删）",
			"ifindex", swIfIndex, "ifname", row.Ifname)
		return nil
	}
	// **删除走 rtnetlink 删设备（`ip link del`），不用 `ip tuntap del`**：后者是 TUNSETIFF 的
	// detach 语义，设备仍被本进程持有时会失败——而本产品按设计**长期持有**该 tap（决策 #438：
	// carrier 来自持有者），于是「DHCP 服务器服务过客户端后删交换机」时它确定性 EBUSY
	// （真机实证：`ioctl(TUNSETIFF): Device or resource busy`，重试不恢复；对照实验：同状态
	// `ip link del dev <tap>` 成功，之后产品删除一次提交通过）。rtnetlink 删设备不受持有影响；
	// 设备消失后持有者的 fd 由 teardown 的 Close 释放（下次启用按名重建/复用）。
	return c.p.ipBest(ctx, "link", "del", "dev", row.Ifname)
}

// TapDump 产品自持的内核 DHCP tap 存量（识别＝名字形如 nfvisdh+8 位十六进制；类型可读且不是
// tun 的不认——不把同名用户设备当内置 tap）。
func (c *kernelDHCPServerClient) TapDump() ([]network.TapInfo, error) {
	ctx := context.Background()
	rows, err := kernelLinkRows(ctx, c.p.run)
	if err != nil {
		return nil, err
	}
	out := make([]network.TapInfo, 0, 2)
	for _, r := range rows {
		if r.Ifname == "" || !isProductDHCPTapName(r.Ifname) {
			continue
		}
		if k := r.kind(); k != "" && k != "tun" {
			continue
		}
		out = append(out, network.TapInfo{SwIfIndex: r.Ifindex, HostIfName: r.Ifname, HostMAC: r.Address})
	}
	return out, nil
}

// SetL2Bridge 把内置 tap enslave 到该交换机内核 bridge（enable=false 时解绑；本产品路径不用，
// 纵深防御）。bridge 名与服务 VLAN 都从**调用点传入的交换机声明**派生（`LinkName(sw.Name)` 与
// `tapServiceVlanOf(sw)`，与内核侧其余族同一派生口径）——**不**读配置引擎（自死锁根源）。
func (c *kernelDHCPServerClient) SetL2Bridge(swIfIndex, bdID uint32, enable bool) error {
	ctx := context.Background()
	rows, err := kernelLinkRows(ctx, c.p.run)
	if err != nil {
		return err
	}
	row, ok := linkRowsByIndex(rows, swIfIndex)
	if !ok {
		return fmt.Errorf("内核里找不到接口索引 %d（内置 tap 是否已被带外删除？）", swIfIndex)
	}
	vs := c.sw
	if vs.Name == "" {
		return fmt.Errorf("内核 DHCP 服务器客户端未带交换机声明（bd %d）：这是装配缺陷，请上报", bdID)
	}
	// 声明与 BD 必须同源（都由 provider 从同一份 vs 派生）：对不上说明接线错了，如实报错不猜。
	if bdID != network.BDID(vs.Name) {
		return fmt.Errorf("交换机 %s 与 bridge-domain %d 不匹配（装配缺陷）", vs.Name, bdID)
	}
	br := LinkName(vs.Name)
	if !enable {
		return c.p.detachMasterIf(ctx, row.Ifname, br)
	}
	if err := c.p.ipReq(ctx, "link", "set", "dev", row.Ifname, "master", br); err != nil {
		return fmt.Errorf("把内核 tap %s 加入交换机内核 bridge %s 失败（该 bridge 是否已收敛？）: %w", row.Ifname, br, err)
	}
	vid, err := tapServiceVlanOf(vs)
	if err != nil {
		return err
	}
	if vid > 0 {
		// 内核 bridge 打开 vlan_filtering 后广播只在**同一 VID** 内洪泛（VPP 的 BD 无此概念：
		// tap 天然与所有端口同域）。不把 tap 钉进该 VID，VLAN 交换机上客户端的 DISCOVER 就到不了
		// 内置 tap——命令全部成功、客户端就是拿不到地址（典型静默失效）。
		if err := c.p.bridgeIdem(ctx, "vlan", "add", "dev", row.Ifname,
			"vid", fmt.Sprint(vid), "pvid", "untagged"); err != nil {
			return fmt.Errorf("把内核 tap %s 钉到交换机 %s 的 VLAN %d 失败: %w", row.Ifname, vs.Name, vid, err)
		}
	}
	return nil
}

// tapServiceVlanOf 内置 tap 的服务 VLAN（0 = 该交换机未启用 VLAN 过滤，tap 无需入 VID）。
//
// 判据与内核侧 l2.go 的 vlanFilteringValue **同一真源**（声明了 access/native/trunk 才开过滤）；
// 服务 VLAN 取**交换机 access VLAN 或第一个 native VLAN**——内置 tap 只有一条 PVID（内核约束：
// 一个端口只有一个 pvid，且 `untagged` 只能给 PVID），回包从 tap 进入 bridge 时按它归类。
//
// 如实边界：声明了 VLAN 却**没有** access/native VLAN（纯 trunk）时服务域无法确定——如实报错
// （提交失败/未收敛项），不静默让客户端拿不到地址；多 VLAN（access + trunk）时服务器服务
// access/native 那一档，其余 VLAN 的客户端不受理（VPP 数据面不受此限，见手册 §8.3）。
func tapServiceVlanOf(vs model.VirtualSwitch) (int, error) {
	if vlanFilteringValue(vs) != "1" {
		return 0, nil
	}
	if vs.VlanAccess > 0 {
		return vs.VlanAccess, nil
	}
	for _, port := range vs.Ports {
		if port.NativeVlan > 0 {
			return port.NativeVlan, nil
		}
	}
	return 0, fmt.Errorf("交换机 %s 声明了 VLAN 但没有 access/native VLAN（纯 trunk）："+
		"内核数据面下 DHCP 服务器的内置 tap 只有一条 PVID、无法确定服务域——"+
		"请给成员口声明 native VLAN，或把该域改用 vpp 数据面（set system dataplane vpp，需重启服务）", vs.Name)
}

// SetState 置内置 tap 管理员 up/down（up 是转发前提：down 的口不转发也不收发帧）。
func (c *kernelDHCPServerClient) SetState(swIfIndex uint32, up bool) error {
	ctx := context.Background()
	rows, err := kernelLinkRows(ctx, c.p.run)
	if err != nil {
		return err
	}
	row, ok := linkRowsByIndex(rows, swIfIndex)
	if !ok {
		return fmt.Errorf("内核里找不到接口索引 %d（内置 tap 是否已被带外删除？）", swIfIndex)
	}
	state := "down"
	if up {
		state = "up"
	}
	return c.p.ipReq(ctx, "link", "set", "dev", row.Ifname, state)
}

// dhcpTapRowByName 在接口清单里按名找一行。
func dhcpTapRowByName(rows []dhcpLinkRow, name string) (dhcpLinkRow, bool) {
	for _, r := range rows {
		if r.Ifname == name {
			return r, true
		}
	}
	return dhcpLinkRow{}, false
}

// ---------- seam 2：单播续租面（PuntClient 空实现 + UDP/67 socket 汇聚） ----------

// kernelDHCPPuntClient network.PuntClient 的内核实现：**空操作**。
//
// 内核数据面没有 VPP 的 punt 机制——单播续租由 dhcpUnicastManager 绑 BVI 地址的 UDP/67
// socket 直接接收（见文件头第 2 段）。provider 每次 Sync 都会重申注册（deregister+register，
// 幂等），内核侧如实返回成功即可：socket 在＝注册在，重申不是空话而是下次 Sync 的幂等路径。
type kernelDHCPPuntClient struct{}

func (kernelDHCPPuntClient) Register(string, uint16) error { return nil }
func (kernelDHCPPuntClient) Deregister(uint16) error       { return nil }
func (kernelDHCPPuntClient) Close()                        {}

// dhcpUnicastIO 一台交换机的单播接收 socket（真实现 = UDP/67 绑 BVI 地址 + 该域 VRF；
// 单测注入内存实现，见 dhcpserver_test.go）。
type dhcpUnicastIO interface {
	// Recv 收一个 UDP 载荷（阻塞；Close 后返回 net.ErrClosed）。from = 客户端地址。
	Recv(buf []byte) (n int, from *net.UDPAddr, err error)
	Close() error
}

// dhcpUnicastLayer 打开单播接收 socket 的底座（真实现见 dhcpserver_socket_{linux,other}.go）。
type dhcpUnicastLayer interface {
	Open(bvi net.IP, vrfDevice string) (dhcpUnicastIO, error)
}

// dhcpUnicastPacket 一条待交付给服务器核心的单播报文（raw = 已合成 IPv4 头 + 原 DHCP 载荷）。
type dhcpUnicastPacket struct {
	bridgeIndex uint32 // 该交换机内核 bridge 的 ifindex（provider 据此派发到交换机）
	raw         []byte
}

// dhcpUnicastInst 一台交换机的单播接收实例（一个 socket + 一个收包协程）。
type dhcpUnicastInst struct {
	plan  dhcpPlan
	index atomic.Uint32 // 内核 bridge 的 ifindex（每次 Sync 按名复核，bridge 重建后索引会变）
	sock  dhcpUnicastIO

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (inst *dhcpUnicastInst) alive() bool {
	select {
	case <-inst.done:
		return false
	default:
		return true
	}
}

// stopAndWait 停掉并等收包协程退出（有界）：先置停止位并关 socket（阻塞读随之返回），再等 done。
// 超时如实报错（不得声称「已停」而留下后台消费者）。
func (inst *dhcpUnicastInst) stopAndWait(timeout time.Duration) error {
	inst.stopOnce.Do(func() {
		close(inst.stop)
		_ = inst.sock.Close()
	})
	select {
	case <-inst.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("交换机 %s 的 DHCP 单播接收在 %s 内未停止（收包协程未退出）",
			inst.plan.switchName, timeout)
	}
}

// dhcpUnicastManager 各交换机单播接收 socket 的持有者（每台启用 dhcp-server 的交换机一个）。
//
// 生命周期口径（与 VPP 侧 punt 注册/回收同族）：
//   - **提交**：随交换机事务收敛（ApplyDHCPServer 第一步）——启用＝按声明建/复用，停用＝关；
//   - **交换机删除**：DeleteBridgeDomain 先停（先解引用、后删被引用）；
//   - **恢复重放**：EnsureConsistent 按 committed 重放（socket 活在进程内，重启后必然不在）；
//   - **巡检**：ReconcileDHCPServer（15s）对账——起失败/收包协程退出的补起、已不声明的停掉；
//   - **进程退出**：Provider.Close（socket/goroutine 不残留）。
type dhcpUnicastManager struct {
	layer dhcpUnicastLayer
	run   Runner // 解析交换机内核 bridge 的 ifindex（纯读）

	mu    sync.Mutex
	insts map[string]*dhcpUnicastInst

	// idxMu/byIndex 「bridge ifindex → 交换机名」反查（provider 的 switchOf）。单独一把锁：
	// 收包派发路径（switchOf）**不**与可能阻塞的起停操作（stopAndWait 最多等 3s）互等。
	idxMu   sync.Mutex
	byIndex map[uint32]string

	// in 交付队列（服务器核心的 servePunt 消费）；closed 随 Close 关闭。
	in        chan dhcpUnicastPacket
	closed    chan struct{}
	closeOnce sync.Once
}

func newDHCPUnicastManager(layer dhcpUnicastLayer, run Runner) *dhcpUnicastManager {
	return &dhcpUnicastManager{
		layer: layer, run: run,
		insts:   map[string]*dhcpUnicastInst{},
		byIndex: map[uint32]string{},
		in:      make(chan dhcpUnicastPacket, 32),
		closed:  make(chan struct{}),
	}
}

// Sync 收敛一台交换机的单播接收面：未启用（无池）⇒ 停；启用 ⇒ 按声明建/复用。
// 声明变化（BVI/VRF/bridge 变）⇒ 重建实例。幂等：声明未变且实例在跑时只复核 bridge 索引。
func (m *dhcpUnicastManager) Sync(ctx context.Context, vs model.VirtualSwitch) error {
	if !vs.DHCPServerEnabled() {
		return m.Stop(vs.Name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	plan, err := dhcpPlanOf(vs)
	if err != nil {
		return err
	}
	index, err := m.bridgeIndex(ctx, plan.bridge)
	if err != nil {
		return fmt.Errorf("交换机 %s 的内核 bridge %s 尚不可用（先确认交换机已收敛）: %w",
			vs.Name, plan.bridge, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst := m.insts[vs.Name]; inst != nil {
		if inst.plan.equal(plan) && inst.alive() {
			inst.index.Store(index) // bridge 可能被带外重建（索引会变）：按名复核后的现值为准
			m.setIndexLocked(vs.Name, index)
			return nil
		}
		if err := m.stopLocked(vs.Name, inst); err != nil {
			return err
		}
	}
	sock, err := m.layer.Open(plan.bvi, plan.vrfDevice)
	if err != nil {
		return fmt.Errorf("交换机 %s 的 DHCP 单播接收 socket（BVI %s:%d，VRF %s）绑定失败: %w；"+
			"该地址是否已下发（set virtual-switches %s gateway ip <ip-prefix> 是否已收敛）？",
			vs.Name, plan.bvi, dhcpUnicastPort, plan.vrfDevice, err, vs.Name)
	}
	inst := &dhcpUnicastInst{plan: plan, sock: sock, stop: make(chan struct{}), done: make(chan struct{})}
	inst.index.Store(index)
	m.insts[vs.Name] = inst
	m.setIndexLocked(vs.Name, index)
	go m.serve(inst)
	return nil
}

// setIndexLocked 记下「交换机名 → 当前 bridge ifindex」并清掉该名字的陈旧索引（bridge 被带外
// 重建后索引会变；陈旧条目会把另一个交换机的包派发错）。调用方须持 m.mu。
func (m *dhcpUnicastManager) setIndexLocked(name string, index uint32) {
	m.idxMu.Lock()
	defer m.idxMu.Unlock()
	for i, n := range m.byIndex {
		if n == name && i != index {
			delete(m.byIndex, i)
		}
	}
	m.byIndex[index] = name
}

// clearIndexLocked 清掉该交换机的索引映射（调用方须持 m.mu）。
func (m *dhcpUnicastManager) clearIndexLocked(name string) {
	m.idxMu.Lock()
	defer m.idxMu.Unlock()
	for i, n := range m.byIndex {
		if n == name {
			delete(m.byIndex, i)
		}
	}
}

// Stop 停掉一台交换机的单播接收（未启用/交换机删除；无实例即幂等空操作）。
func (m *dhcpUnicastManager) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.insts[name]
	if inst == nil {
		return nil
	}
	return m.stopLocked(name, inst)
}

// stopLocked 停掉并移除一台交换机的实例（调用方须持 m.mu）。
func (m *dhcpUnicastManager) stopLocked(name string, inst *dhcpUnicastInst) error {
	delete(m.insts, name)
	m.clearIndexLocked(name)
	return inst.stopAndWait(dhcpUnicastStopTimeout)
}

// Reconcile 15s 巡检对账：声明里的（且启用的）确保在跑；不在声明里的停掉。
func (m *dhcpUnicastManager) Reconcile(ctx context.Context, cfg model.Config) []error {
	var errs []error
	declared := map[string]bool{}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" || !vs.DHCPServerEnabled() {
			continue
		}
		declared[vs.Name] = true
		if err := m.Sync(ctx, vs); err != nil {
			errs = append(errs, fmt.Errorf("virtual-switches/%s/dhcp-server: %w", vs.Name, err))
		}
	}
	for _, name := range m.names() {
		if declared[name] {
			continue
		}
		if err := m.Stop(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// names 当前持有实例的交换机名（排序，便于错误/日志顺序确定）。
func (m *dhcpUnicastManager) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.insts))
	for n := range m.insts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Close 停掉全部实例（进程退出；幂等）。
func (m *dhcpUnicastManager) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	var errs []error
	for _, name := range m.names() {
		if err := m.Stop(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// serve 一台交换机的收包协程：socket → 合成裸 IPv4 包 → 交付队列（核心的 servePunt 消费）。
// 读错误（非 Close）⇒ 实例终止（由 15s 巡检按声明重建），如实记日志、不静默空转。
func (m *dhcpUnicastManager) serve(inst *dhcpUnicastInst) {
	defer close(inst.done)
	buf := make([]byte, 2048)
	for {
		n, from, err := inst.sock.Recv(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("内核 DHCP 服务器的单播接收失败，停止该交换机的接收（巡检将按声明重建）",
				"switch", inst.plan.switchName, "err", err)
			return
		}
		if n <= 0 {
			continue
		}
		pkt := dhcpUnicastPacket{
			bridgeIndex: inst.index.Load(),
			raw:         buildDHCPUnicastIP(inst.plan.bvi, from, buf[:n]),
		}
		select {
		case m.in <- pkt:
		case <-inst.stop:
			return
		case <-m.closed:
			return
		}
	}
}

// switchOf 按内核 bridge 的 ifindex 找所属交换机（provider 经 SetSwitchResolver 使用）。
func (m *dhcpUnicastManager) switchOf(index uint32) (string, bool) {
	m.idxMu.Lock()
	defer m.idxMu.Unlock()
	name, ok := m.byIndex[index]
	return name, ok
}

// bridgeIndex 该内核 bridge 的 ifindex（不存在/读不到 ⇒ 如实报错，不猜）。
func (m *dhcpUnicastManager) bridgeIndex(ctx context.Context, bridge string) (uint32, error) {
	rows, err := kernelLinkRows(ctx, m.run)
	if err != nil {
		return 0, err
	}
	row, ok := dhcpTapRowByName(rows, bridge)
	if !ok || row.Ifindex == 0 {
		return 0, fmt.Errorf("内核里没有接口 %s", bridge)
	}
	return row.Ifindex, nil
}

// transport 服务器核心侧的 punt transport 视图（provider 打开一次、Close 即收包循环退出）。
func (m *dhcpUnicastManager) transport() network.PuntTransport {
	return &dhcpUnicastTransport{m: m, closed: make(chan struct{})}
}

// dhcpUnicastTransport dhcpUnicastManager 的 network.PuntTransport 视图。
type dhcpUnicastTransport struct {
	m      *dhcpUnicastManager
	closed chan struct{}
	once   sync.Once
}

// Recv 交付一条单播报文（阻塞；transport 或 manager 关闭即返回 net.ErrClosed）。
func (t *dhcpUnicastTransport) Recv() (network.PuntDesc, []byte, error) {
	select {
	case <-t.closed:
		return network.PuntDesc{}, nil, net.ErrClosed
	case <-t.m.closed:
		return network.PuntDesc{}, nil, net.ErrClosed
	case pkt := <-t.m.in:
		return network.PuntDesc{SwIfIndex: pkt.bridgeIndex}, pkt.raw, nil
	}
}

// Send 内核数据面不用 punt 回注：应答一律经交换机 bridge 写回内置 tap（见文件头）。
func (t *dhcpUnicastTransport) Send(network.PuntDesc, []byte) error {
	return fmt.Errorf("内核数据面不用 punt 回注（应答经内置 tap 写回）")
}

// Close 关闭本 transport（provider 的 reset/Close 路径；socket 由 manager 自己收）。
func (t *dhcpUnicastTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// buildDHCPUnicastIP 把 UDP/67 socket 收到的 DHCP 载荷合成**裸 IPv4 包**（UDP/67），交给
// provider 与 VPP punt 上行**同一解析入口**（parseDHCPIP 只认 IPv4/UDP 目的端口 67 与 BOOTP
// 主体，故头部的源/目的地址只为如实表达「谁发到哪里」）。源取 socket 的对端（客户端此刻的
// 地址；RENEW/RELEASE 都有），目的取 BVI。
func buildDHCPUnicastIP(bvi net.IP, from *net.UDPAddr, payload []byte) []byte {
	pkt := make([]byte, 20+8+len(payload))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	src := net.IPv4zero
	if from != nil && from.IP.To4() != nil {
		src = from.IP.To4()
	}
	copy(pkt[12:16], src)
	copy(pkt[16:20], bvi.To4())
	// 复用同包 relay 的校验和助手（同一算法；UDP 校验和留 0 —— 入向不校验）。
	binary.BigEndian.PutUint16(pkt[10:12], relayIPChecksum(pkt[:20]))
	udp := pkt[20:]
	binary.BigEndian.PutUint16(udp[0:2], relayClientPort) // 68（客户端端口）
	binary.BigEndian.PutUint16(udp[2:4], dhcpUnicastPort) // 67
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(payload)))
	copy(udp[8:], payload)
	return pkt
}

// ---------- Provider 接线 ----------

// dhcpComponents 惰性装配内核侧 DHCP 服务器（复用 network.DHCPServerProvider + 内核侧两个
// seam 的实现）。返回（服务器核心, 单播 socket 管理器）。
func (p *Provider) dhcpComponents() (*network.DHCPServerProvider, *dhcpUnicastManager) {
	p.dhcpMu.Lock()
	defer p.dhcpMu.Unlock()
	if p.dhcpSrv != nil {
		return p.dhcpSrv, p.dhcpHub
	}
	hub := newDHCPUnicastManager(p.dhcpSockLayerOrDefault(), p.run)
	srv := network.NewDHCPServerProviderFunc(
		// 交换机声明**从调用点传下去**（Sync 的 vs / teardown 的只带名字）——apply 路径绝不回读
		// 配置引擎（提交期发动机锁由提交自己持有，重入即自死锁）。
		func(sw model.VirtualSwitch) (network.DHCPServerClient, error) {
			return &kernelDHCPServerClient{p: p, sw: sw}, nil
		},
		func() (network.PuntClient, error) { return kernelDHCPPuntClient{}, nil },
	)
	srv.SetPuntTransport(func() (network.PuntTransport, error) { return hub.transport(), nil })
	srv.SetSwitchResolver(hub.switchOf)
	srv.SetTapOpen(p.dhcpTapLayerOrDefault().Open) // 内核侧 = 打开 /dev/net/tun 长期持有（见 dhcpserver_tap.go）
	if p.dhcpLeaseDir != "" {
		srv.SetLeaseDir(p.dhcpLeaseDir) // 单测注入临时目录；生产用缺省（/var/lib/nfvis/dhcp）
	}
	if p.alarms != nil {
		srv.SetAlarms(p.alarms)
	}
	p.dhcpHub, p.dhcpSrv = hub, srv
	return srv, hub
}

// SetDHCPServerLeaseDir 注入租约持久化目录（**单测专用**：不写产品目录 /var/lib/nfvis/dhcp；
// 须在任何 DHCP 服务器收敛之前调用）。空串不改变缺省。
func (p *Provider) SetDHCPServerLeaseDir(dir string) {
	if dir == "" {
		return
	}
	p.dhcpMu.Lock()
	defer p.dhcpMu.Unlock()
	p.dhcpLeaseDir = dir
}

// SetDHCPServerSocketLayer 注入单播接收 socket 的底座（**单测专用**：不依赖真 socket 与 VRF；
// 须在任何 DHCP 服务器收敛之前调用）。nil 不改变平台默认实现。
func (p *Provider) SetDHCPServerSocketLayer(l dhcpUnicastLayer) {
	p.dhcpMu.Lock()
	defer p.dhcpMu.Unlock()
	p.dhcpSockLayer = l
}

// dhcpSockLayerOrDefault 生效的单播 socket 底座（未注入 = 平台默认；见 dhcpserver_socket_*.go）。
func (p *Provider) dhcpSockLayerOrDefault() dhcpUnicastLayer {
	if p.dhcpSockLayer != nil {
		return p.dhcpSockLayer
	}
	return defaultDHCPUnicastLayer()
}

// ApplyDHCPServer 收敛一台交换机的 DHCP 服务器声明（决策 #438）。调用时机与 VPP 侧**同源**：
// 提交编排把它作为 bridge-domain（与 dhcp-relay）之后的伴随操作（先有 BVI 地址与 bridge
// 才有 server）；恢复收敛的重放走 recovery.go 的同一段。未启用（无池）的声明＝teardown
// （关单播 socket + 删内置 tap + 清租约文件）。
func (p *Provider) ApplyDHCPServer(ctx context.Context, vs model.VirtualSwitch) error {
	srv, hub := p.dhcpComponents()
	// 1) 单播接收面按声明收敛（未启用 ⇒ 关；启用 ⇒ 绑 BVI 地址的 UDP/67）。
	//    起不来（bridge/地址尚未收敛、端口被占）在这里如实报错。
	if err := hub.Sync(ctx, vs); err != nil {
		return err
	}
	// 2) 复用与 VPP 侧同一份服务器核心：tap 建/删 + 入 bridge + 置 up + 租约池/状态机/报文
	//    处理 + 池耗尽告警（network.DHCPServerProvider，内核侧只换传输面）。
	return srv.Sync(ctx, vs)
}

// ReconcileDHCPServer 15s 巡检收敛（与 VPP 侧同一调用点）：补齐带外丢失的 tap/socket、
// 回收到期租约、复核池耗尽告警；已不声明的单播 socket 停掉。逐条如实返回错误。
func (p *Provider) ReconcileDHCPServer(ctx context.Context, cfg model.Config) []error {
	srv, hub := p.dhcpComponents()
	var errs []error
	errs = append(errs, hub.Reconcile(ctx, cfg)...)
	errs = append(errs, srv.Reconcile(ctx, cfg)...)
	return errs
}
