package netkernel

// 内核数据面 LLDP 自研收发代理（决策 #440；FR-NET-018 的内核侧落地）。
//
// VPP 侧形态 = lldp 插件（全局参数 LldpConfig + 按接口开关 LldpSetInterface + 邻居表
// LldpDump）；内核侧**没有任何对应物**，也没有可对接的现成守护进程（不引入 lldpd——
// 零外部依赖），故本文件是 nfvisd 内的**自研 LLDP 收发代理**：
//
//   - **收**：每启用接口一个 AF_PACKET socket（ethertype 0x88CC，见 lldp_linux.go），
//     解析 LLDPDU 的**基础 TLV**（chassis ID / port ID / TTL / system name / END；未知 TLV
//     跳过），维护进程内邻居表——键＝「本地接口 + 对端 chassis ID + port ID」，TTL 取
//     **对端** TTL，过期移除。
//   - **发**：按 advertisement-interval（缺省 30s）向 LLDP 保留组播地址 01:80:C2:00:00:0E
//     发广告，TTL＝4 × interval（与 VPP 侧 txHold=4 同口径）；内容固定为本机主机名（chassis
//     locally-assigned = 7）、接口名（port interface-name = 5）、接口 MAC、system name＝主机名
//     （与 VPP 侧一样不传自定义 sysname）。只收无 VLAN 标签的帧（与产品其它自研收包族同口径）。
//
// 口径（与 VPP 侧逐字对齐，见规格书附录 A 第 440 条与设计 §3.2e）：
//   - Enabled=false / nil ⇒ 全部关闭（socket 关、停发、邻居表清空）；
//   - Interfaces[] 逐口开关（只对 Enabled=true 的口开 socket / 发广告）；
//   - 间隔变更 ⇒ 只更新发送节奏与 TTL，**不重建 socket**（发送协程每轮现读间隔）；
//   - 接口起不来/消失 ⇒ **如实报错**（该口标记未收敛、下次 Sync 重试），其它口照常收敛；
//   - ID 渲染复用与 VPP 侧**同一份** network.LldpIDBySubtype（chassis MAC = 4、port MAC = 3）
//     ——两种数据面的读视图同形，不会各说各话。
//
// 生命周期（与 DHCP 中继/服务器、DNS 代理同族）：
//   - **提交**：随提交编排收敛（既有 ApplyLLDP 全局 op；undo 回旧声明）；
//   - **恢复重放**：EnsureConsistent 的接口段之后重放（socket 与收发协程活在进程内，
//     nfvisd 重启后必然不在；声明为空＝teardown，幂等）；
//   - **巡检**：ReconcileLLDP（15s）对账——起失败/带外丢失的补起、已不声明的关掉、
//     过期邻居回收；
//   - **进程退出**：Provider.Close 关全部 socket 并等收发协程退出（有界，超时如实报错）。
//
// 与 apply 路径的硬约束：本文件（及其调用链）**绝不回读配置发动机**——Sync 只吃调用方给的
// cfg 参数（提交期发动机锁由本次提交自己持有，重入＝自死锁；源码扫描用例守着这条）。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	// lldpEtherType LLDP 的以太类型（IEEE 802.1AB）。
	lldpEtherType = 0x88CC
	// lldpDefaultIntervalSeconds 广告间隔缺省值（秒；与命令树文档/v2 侧同值）。配置写了
	// ≤0 也按缺省处理——「没写有效间隔」不等于「不发广告」。
	lldpDefaultIntervalSeconds = 30
	// lldpTxHold TTL 倍数：TTL＝4 × interval（与 VPP 侧 lldp 插件的 txHold=4 同口径）。
	lldpTxHold = 4
	// lldpMaxFrameSize 收包缓冲上界（普通以太帧上限；LLDP 不带 VLAN 标签、更不会有巨型帧，
	// 超出的帧不是本产品认的广告，按畸形丢弃）。
	lldpMaxFrameSize = 1500
	// lldpMinFrameLen 以太最小帧长（60 字节，不含 FCS）：更短的发送帧显式补零——真机网卡
	// 自己也会补，显式补齐让字节级断言和抓包对齐。
	lldpMinFrameLen = 60
	// lldpRecvRetryDelay 收包出错后的重试间隔（不静默失聪、也不忙等）。
	lldpRecvRetryDelay = 100 * time.Millisecond
	// lldpStopTimeout 关停一批接口 socket/协程的**总**等待上界（不随接口数放大）：
	// 「起→停不残留协程」是契约要求，超时如实报错，不谎称已停。
	lldpStopTimeout = 3 * time.Second
)

// LLDPDU 基础 TLV 类型（IEEE 802.1AB §8.4）；v1 只认这几条，其余（LLDP-MED / DCBX /
// 管理地址 / 系统描述等）解析时跳过。
const (
	lldpTlvEnd        = 0
	lldpTlvChassisID  = 1
	lldpTlvPortID     = 2
	lldpTlvTTL        = 3
	lldpTlvSystemName = 5
)

// ID TLV 的 subtype（§8.5/§8.6）。渲染侧的 chassis MAC = 4 / port MAC = 3 与
// internal/orchestrator/network 的 VPP 常量同值——两种数据面共同口径。
const (
	lldpChassisSubtypeLocallyAssigned = 7 // 发送用：local entity 名＝本机主机名
	lldpChassisSubtypeMAC             = 4 // 渲染用（对端 chassis ID 为 MAC 型）
	lldpPortSubtypeInterfaceName      = 5 // 发送用：接口名
	lldpPortSubtypeMAC                = 3 // 渲染用（对端 port ID 为 MAC 型）
)

// lldpDestMAC LLDP 保留组播地址 01:80:C2:00:00:0E（IEEE 802.1AB 规定，不转发、不泛洪）。
var lldpDestMAC = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}

// ---------- 报文编解码（传输无关，纯函数） ----------

// lldpID 一条标识 TLV 的解析结果（subtype + 原始值字节；值的渲染走 network.LldpIDBySubtype）。
type lldpID struct {
	subtype uint8
	value   []byte
}

// lldpTlvs 一条 LLDPDU 里基础 TLV 的解析结果；缺哪条 TLV 看 has*。
type lldpTlvs struct {
	chassis    lldpID
	hasChassis bool
	port       lldpID
	hasPort    bool
	ttl        int // 秒（对端广告的 TTL）
	hasTTL     bool
	systemName string // 可选 TLV（缺失＝空串）
}

// parseLLDPDU 解析一条以太帧里的 LLDPDU。
//
// 判据：帧至少要有以太头 + 以太类型（14 字节）且以太类型 = 0x88CC；TLV 逐个按 9 位类型 +
// 9 位长度（大端）迭代，遇 END 或正常读完即止。**未知 TLV 类型跳过、不算错误**（v1 边界：
// 只认基础 TLV）。畸形/截断（TLV 头不全、值长度越过帧尾）返回 ok=false——调用方按链路噪声
// 丢弃并计数，绝不 panic。
func parseLLDPDU(frame []byte) (lldpTlvs, bool) {
	var out lldpTlvs
	if len(frame) < 14 {
		return lldpTlvs{}, false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != lldpEtherType {
		return lldpTlvs{}, false
	}
	body := frame[14:]
	for i := 0; i < len(body); {
		if i+2 > len(body) {
			return lldpTlvs{}, false // 连 TLV 头都不全：帧被截断
		}
		head := binary.BigEndian.Uint16(body[i : i+2])
		typ := uint8(head >> 9)
		length := int(head & 0x1ff)
		i += 2
		if i+length > len(body) {
			return lldpTlvs{}, false // 值长度越过帧尾：帧被截断
		}
		val := body[i : i+length]
		i += length
		switch typ {
		case lldpTlvEnd:
			return out, true
		case lldpTlvChassisID:
			if length >= 1 { // 至少要有 subtype 字节；0 长度是畸形，缺这条 TLV
				out.chassis = lldpID{subtype: val[0], value: val[1:]}
				out.hasChassis = true
			}
		case lldpTlvPortID:
			if length >= 1 {
				out.port = lldpID{subtype: val[0], value: val[1:]}
				out.hasPort = true
			}
		case lldpTlvTTL:
			if length == 2 {
				out.ttl = int(binary.BigEndian.Uint16(val))
				out.hasTTL = true
			}
		case lldpTlvSystemName:
			out.systemName = strings.TrimRight(string(val), "\x00")
		default:
			// 未知/未解析的 TLV：v1 跳过（不报错、不中断解析）——
			// LLDP-MED/DCBX/管理地址/系统描述都在此列。
		}
	}
	// 没有 END TLV 但 TLV 边界处正好读完：按完整帧接受（真实设备少见，但不报假错）。
	return out, true
}

// lldpTLV 组装一条 TLV：类型 7 位 + 长度 9 位，线上大端序。
func lldpTLV(typ uint8, value []byte) []byte {
	out := make([]byte, 2, 2+len(value))
	binary.BigEndian.PutUint16(out, uint16(typ)<<9|uint16(len(value)))
	return append(out, value...)
}

// lldpTTLSeconds 广告 TTL＝4 × interval（interval 非法时按缺省值算；与 VPP 侧 txHold=4 同口径）。
func lldpTTLSeconds(interval int) int {
	if interval <= 0 {
		interval = lldpDefaultIntervalSeconds
	}
	return interval * lldpTxHold
}

// buildLLDPFrame 组一条 LLDP 广告帧（传输无关，纯函数；单测做字节级断言）。
//
// 结构：目的 = lldpDestMAC、源 = 接口 MAC、ethertype 0x88CC，然后按序 Chassis ID（subtype 7，
// 值＝主机名）、Port ID（subtype 5，值＝接口名）、TTL（2 字节＝lldpTTLSeconds）、System Name
// （type 5，值＝主机名；空则不发）、END。短于 60 字节时补零到 lldpMinFrameLen。
func buildLLDPFrame(srcMAC net.HardwareAddr, hostname, ifname string, interval int) []byte {
	src := srcMAC
	if len(src) != 6 {
		// 取不到接口 MAC 时如实填全 0（不猜、不借别的口的地址）。
		src = make(net.HardwareAddr, 6)
	}
	frame := make([]byte, 0, 128)
	frame = append(frame, lldpDestMAC...)
	frame = append(frame, src...)
	frame = append(frame, byte(lldpEtherType>>8), byte(lldpEtherType&0xff))

	chassis := make([]byte, 0, 1+len(hostname))
	chassis = append(chassis, lldpChassisSubtypeLocallyAssigned)
	chassis = append(chassis, hostname...)
	frame = append(frame, lldpTLV(lldpTlvChassisID, chassis)...)

	port := make([]byte, 0, 1+len(ifname))
	port = append(port, lldpPortSubtypeInterfaceName)
	port = append(port, ifname...)
	frame = append(frame, lldpTLV(lldpTlvPortID, port)...)

	ttl := lldpTTLSeconds(interval)
	frame = append(frame, lldpTLV(lldpTlvTTL, []byte{byte(ttl >> 8), byte(ttl)})...)

	if hostname != "" {
		frame = append(frame, lldpTLV(lldpTlvSystemName, []byte(hostname))...)
	}
	frame = append(frame, lldpTLV(lldpTlvEnd, nil)...)
	if len(frame) < lldpMinFrameLen {
		frame = append(frame, make([]byte, lldpMinFrameLen-len(frame))...)
	}
	return frame
}

// ---------- 邻居表（进程内运行态） ----------

// lldpNeighborKey 邻居表键：本地接口 + 对端 chassis ID + 对端 port ID。同口同 chassis 但
// port 变了（换线/对端换口）是一条新邻居；同键重复收到＝原地刷新（不新增条目）。
type lldpNeighborKey struct {
	local   string
	chassis string
	port    string
}

// lldpNeighborRT 一条邻居的运行态。
type lldpNeighborRT struct {
	key     lldpNeighborKey
	ttl     int       // 对端 TTL（秒）；缺 TTL TLV 视为 0 ⇒ 立即过期（不显示）
	heardAt time.Time // 最近一次收到该邻居 LLDPDU 的时刻
}

// view 转读视图条目（LastHeard ＝距上次收到的秒数，与 VPP 侧同口径）。
func (n *lldpNeighborRT) view(now time.Time) network.LldpNeighbor {
	return network.LldpNeighbor{
		Interface: n.key.local,
		ChassisID: n.key.chassis,
		PortID:    n.key.port,
		TTL:       n.ttl,
		LastHeard: now.Sub(n.heardAt).Seconds(),
	}
}

// lldpIfaceRT 一个启用接口的运行态：socket + 收发协程句柄 + 最近状态。
type lldpIfaceRT struct {
	name string
	mac  net.HardwareAddr
	// io 仅在**打开成功**时非 nil，此后不可变（teardown 只关它、不置 nil）——收发协程
	// 与读视图因此不需要为它加锁（写只发生在协程启动之前）。
	io lldpIO

	stop   chan struct{} // 关：通知发送协程退出
	done   chan struct{} // 收包协程已退出
	txDone chan struct{} // 发送协程已退出

	lastTx time.Time // 最近一次广告发出时刻（运行态读数）
	err    string    // 最近一次失败原因（空 = 就绪）；下一次成功即清空
}

// lldpIfaceState 一个启用接口的运行态快照（State 读数；测试与巡检使用）。
type lldpIfaceState struct {
	Name      string
	Converged bool   // 就绪（socket 在位且最近一次收发成功）
	Error     string // 未收敛原因（空 = 就绪）
	MAC       string // 接口 MAC（读不到为空串）
}

// lldpIO 一个接口的 LLDP 帧收发面（真实现 = AF_PACKET 绑该口，见 lldp_linux.go；
// 单测注入内存实现，见 lldp_test.go）。
type lldpIO interface {
	// Recv 收一个以太帧（阻塞；Close 后返回 net.ErrClosed）。只收本口的 LLDP 帧。
	Recv(buf []byte) (int, error)
	// Send 发一个完整以太帧（目的 MAC 已在帧里；源＝本口 MAC）。
	Send(frame []byte) error
	Close() error
}

// lldpLayer 打开接口收发 socket 的底座（真实现见 lldp_{linux,other}.go）。
type lldpLayer interface {
	// Open 打开绑该接口的 LLDP 收发 socket，并如实返回该口的 MAC。
	Open(ifname string) (lldpIO, net.HardwareAddr, error)
}

// lldpPending 一个待确认「收发协程已退出」的接口（socket 关闭已执行，等待在出锁后做）。
type lldpPending struct {
	name string
	rt   *lldpIfaceRT
}

// lldpManager 各接口收发 socket 与邻居表的持有者（内核数据面 LLDP 的进程内运行态）。
//
// 并发：mu 保护全部可变字段（Sync/Close/State/hear/sendAdvertisement 的形状快照）。
// **关停等待一律在出锁之后**（持锁等待会与在途的收发协程互等成死锁，与 DNS 代理管理器同法）。
type lldpManager struct {
	layer      lldpLayer
	hostname   string        // 构造时读到的宿主主机名（来源函数读不到时的兜底）
	hostnameFn func() string // 系统名来源（缺省现读 os.Hostname；单测可注入）

	mu        sync.Mutex
	interval  int  // 广告间隔（秒；发送协程每轮现读——变更只改节奏，不重建 socket）
	enabled   bool // 全局开关的最近一次声明（运行态读数）
	ifaces    map[string]*lldpIfaceRT
	order     []string // 声明序（State/确定性输出）
	neighbors map[lldpNeighborKey]*lldpNeighborRT
	closed    bool // Close 后置位（进程退出路径）

	malformed uint64 // 畸形/非 LLDP 帧丢弃计数（原子；链路噪声的如实计数）
}

// newLLDPManager 构造管理器（平台默认底座见 lldpLayerOrDefault 的调用点）。
func newLLDPManager(layer lldpLayer) *lldpManager {
	host, _ := os.Hostname()
	m := &lldpManager{
		layer:     layer,
		hostname:  host,
		interval:  lldpDefaultIntervalSeconds,
		ifaces:    map[string]*lldpIfaceRT{},
		neighbors: map[lldpNeighborKey]*lldpNeighborRT{},
	}
	// 缺省来源＝每次现读系统主机名（#424 主机名落地宿主后可能运行期变更、进程不重启）；
	// 读不到回落构造时的值（不把「读不到」发成空系统名）。
	m.hostnameFn = func() string {
		if h, err := os.Hostname(); err == nil && h != "" {
			return h
		}
		return host
	}
	return m
}

// SetHostnameSource 覆盖系统名来源（**单测专用**；来源函数不得回调本管理器）。
// 缺省＝每次广告现读 os.Hostname（#424 主机名落地宿主后可能运行期变更，进程不重启）。
func (m *lldpManager) SetHostnameSource(fn func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostnameFn = fn
}

// hostnameNow 当前系统名：优先来源函数（现读），读不到回落构造时的宿主主机名；都取不到＝空串。
func (m *lldpManager) hostnameNow() string {
	m.mu.Lock()
	fn, fallback := m.hostnameFn, m.hostname
	m.mu.Unlock()
	if fn != nil {
		if h := strings.TrimSpace(fn()); h != "" {
			return h
		}
	}
	return fallback
}

// Sync 把 LLDP 声明收敛到内核运行态（决策 #440）。
//
// 未启用（cfg 为 nil 或 Enabled=false）⇒ 关掉并忘掉全部接口（socket 关、停发、邻居表清空）；
// 启用 ⇒ 对每个 Enabled=true 的口确保 socket 在位：新口开 socket + 起收/发协程（**启用即发
// 一帧**，不等首个间隔），已在位的口保留（幂等，不抖动；间隔变更只改节奏与 TTL）。不再声明/
// 被关掉的口：关 socket、停协程、该口邻居条目作废。
func (m *lldpManager) Sync(ctx context.Context, cfg *model.LldpConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	interval := lldpDefaultIntervalSeconds
	if cfg != nil && cfg.AdvertisementInterval > 0 {
		interval = cfg.AdvertisementInterval
	}
	enabled := cfg != nil && cfg.Enabled

	var errs []error
	desired := map[string]bool{}
	var declOrder []string
	if enabled {
		seen := map[string]bool{}
		for _, li := range cfg.Interfaces {
			if li.Interface == "" {
				// 纵深防御：配置校验保证接口名非空；空名如实报一条（不静默吞掉声明）。
				errs = append(errs, fmt.Errorf("lldp: 接口名为空的声明无法收敛（已跳过该条）"))
				continue
			}
			if seen[li.Interface] {
				errs = append(errs, fmt.Errorf("lldp/%s: 接口在声明里重复出现（保留首个）", li.Interface))
				continue
			}
			seen[li.Interface] = true
			desired[li.Interface] = li.Enabled
			if li.Enabled {
				declOrder = append(declOrder, li.Interface)
			}
		}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("lldp: 管理器已关闭（进程退出路径，不再收敛）")
	}
	m.interval = interval
	m.enabled = enabled

	// 停掉不再需要的口（全局关闭 / 该口被关或从声明里删掉）：关 socket、停收发协程；
	// 该口的邻居条目同时作废（口不再收帧，条目不该等 TTL 才消失）。
	var pending []lldpPending
	for name, rt := range m.ifaces {
		if desired[name] {
			continue
		}
		if rt.io != nil {
			pending = append(pending, lldpPending{name: name, rt: rt})
		}
		m.detachLocked(rt)
		delete(m.ifaces, name)
		m.dropNeighborsLocked(name)
	}

	// 按**声明序**确保需要的口在位：已在位保留（幂等）；新口（或上次打开失败的口）开 socket。
	var starts []*lldpIfaceRT
	order := make([]string, 0, len(declOrder))
	for _, name := range declOrder {
		order = append(order, name)
		rt := m.ifaces[name]
		if rt == nil {
			rt = &lldpIfaceRT{name: name}
			m.ifaces[name] = rt
		}
		if rt.io != nil {
			continue // 已在绑定状态：保留（声明未变的普通提交不抖动）
		}
		sock, mac, err := m.layer.Open(name)
		if err != nil {
			// 一个口起不来**不吞**：如实记录该口原因 + 进返回错误（提交失败并回滚 /
			// 恢复未收敛项 / 巡检告警），下次 Sync 幂等重试。
			rt.err = "打开收发 socket 失败: " + err.Error()
			errs = append(errs, fmt.Errorf("lldp/%s: 打开收发 socket 失败: %w"+
				"（该口是否存在、是否已被 DPDK 接管、权限是否足够？）", name, err))
			continue
		}
		rt.io = sock
		rt.mac = mac
		rt.err = ""
		rt.stop = make(chan struct{})
		rt.done = make(chan struct{})
		rt.txDone = make(chan struct{})
		starts = append(starts, rt)
	}
	m.order = order
	m.mu.Unlock()

	// 协程在出锁后启动（它们自己会取锁）。
	for _, rt := range starts {
		go m.runReader(rt)
		go m.runTx(rt)
	}
	if err := m.waitStopped(pending); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// detachLocked 关掉一个口并在位退出：先关 socket（唤醒阻塞的 Recv，AF_PACKET 实现带
// 500ms 超时轮转）再关 stop（通知发送协程）。**不置 rt.io**（不可变，见类型注释）。
// 调用方持 m.mu。
func (m *lldpManager) detachLocked(rt *lldpIfaceRT) {
	if rt.io == nil {
		return
	}
	_ = rt.io.Close()
	close(rt.stop)
}

// dropNeighborsLocked 作废某本地接口的全部邻居条目（调用方持 m.mu）。
func (m *lldpManager) dropNeighborsLocked(ifname string) {
	for k, n := range m.neighbors {
		if n.key.local == ifname {
			delete(m.neighbors, k)
		}
	}
}

// waitStopped 等一批接口的收发协程退出（**总**等待上界 lldpStopTimeout，不随数量放大）。
// 超时如实报错（不得声称「已停」而留下后台收发）；调用方不得持 m.mu。
func (m *lldpManager) waitStopped(pending []lldpPending) error {
	if len(pending) == 0 {
		return nil
	}
	deadline := time.Now().Add(lldpStopTimeout)
	var errs []error
	for _, p := range pending {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			errs = append(errs, fmt.Errorf("lldp/%s 的收包协程未在期限内退出", p.name))
			continue
		}
		select {
		case <-p.rt.done:
		case <-time.After(remaining):
			errs = append(errs, fmt.Errorf("lldp/%s 的收包协程未在期限内退出", p.name))
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			errs = append(errs, fmt.Errorf("lldp/%s 的发送协程未在期限内退出", p.name))
			continue
		}
		select {
		case <-p.rt.txDone:
		case <-time.After(remaining):
			errs = append(errs, fmt.Errorf("lldp/%s 的发送协程未在期限内退出", p.name))
		}
	}
	return errors.Join(errs...)
}

// Close 关掉全部接口 socket 并等收发协程退出（进程优雅退出；幂等）。
// 先关闭全部 socket（唤醒阻塞的 Recv）再出锁等待——持锁等待会与在途的收发协程互等。
func (m *lldpManager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.enabled = false
	var pending []lldpPending
	for name, rt := range m.ifaces {
		if rt.io != nil {
			pending = append(pending, lldpPending{name: name, rt: rt})
		}
		m.detachLocked(rt)
	}
	m.ifaces = map[string]*lldpIfaceRT{}
	m.order = nil
	m.neighbors = map[lldpNeighborKey]*lldpNeighborRT{}
	m.mu.Unlock()
	return m.waitStopped(pending)
}

// Neighbors 邻居表快照：先按 TTL 过期（对端 TTL，0 ⇒ 立即过期），再按
// 〔本地接口、chassis ID、port ID〕**确定性排序**返回。LastHeard＝距上次收到的秒数。
func (m *lldpManager) Neighbors(now time.Time) []network.LldpNeighbor {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	out := make([]network.LldpNeighbor, 0, len(m.neighbors))
	for _, n := range m.neighbors {
		out = append(out, n.view(now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Interface != out[j].Interface {
			return out[i].Interface < out[j].Interface
		}
		if out[i].ChassisID != out[j].ChassisID {
			return out[i].ChassisID < out[j].ChassisID
		}
		return out[i].PortID < out[j].PortID
	})
	return out
}

// expire 清理已过期邻居（15s 巡检调用：读视图之外也定期回收，内存不随历史邻居增长）。
func (m *lldpManager) expire(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
}

// expireLocked 过期判据：距上次收到超过**对端** TTL 即移除；TTL ≤ 0（缺 TTL TLV）立即过期，
// 不留在表里（不合规的帧不该被显示成邻居）。调用方持 m.mu。
func (m *lldpManager) expireLocked(now time.Time) {
	for k, n := range m.neighbors {
		if n.ttl <= 0 || now.Sub(n.heardAt) > time.Duration(n.ttl)*time.Second {
			delete(m.neighbors, k)
		}
	}
}

// State 运行态快照：按**声明序**报每个启用接口的〔名称、是否就绪、未收敛原因、口 MAC〕。
func (m *lldpManager) State() []lldpIfaceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]lldpIfaceState, 0, len(m.order))
	for _, name := range m.order {
		rt := m.ifaces[name]
		if rt == nil {
			continue
		}
		mac := ""
		if len(rt.mac) == 6 {
			mac = rt.mac.String()
		}
		out = append(out, lldpIfaceState{
			Name:      rt.name,
			Converged: rt.err == "",
			Error:     rt.err,
			MAC:       mac,
		})
	}
	return out
}

// ---------- 收 / 发协程 ----------

// runReader 一个接口的收包协程：解析 LLDPDU 更新邻居表；非 LLDP/畸形帧按链路噪声丢弃
// （计数、不打断循环、不刷日志）；收包错误如实记录并小睡重试（不静默失聪）；socket 关闭即退出。
func (m *lldpManager) runReader(rt *lldpIfaceRT) {
	defer close(rt.done)
	io := rt.io // 不可变（见 lldpIfaceRT 注释）
	buf := make([]byte, lldpMaxFrameSize)
	for {
		n, err := io.Recv(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			m.setIfaceErr(rt, "收包失败: "+err.Error())
			slog.Warn("内核 LLDP 收包失败，稍后重试", "interface", rt.name, "err", err)
			time.Sleep(lldpRecvRetryDelay)
			continue
		}
		if n <= 0 {
			continue
		}
		tlvs, ok := parseLLDPDU(buf[:n])
		if !ok {
			atomic.AddUint64(&m.malformed, 1)
			continue
		}
		m.hear(rt.name, tlvs, time.Now())
	}
}

// runTx 一个接口的发送协程：**启用即发一帧**（不等首个间隔，对端立刻看得到），此后按当前
// 间隔每轮现读（间隔变更只改节奏与 TTL，不重建 socket）。socket 关闭/进程退出即退出。
func (m *lldpManager) runTx(rt *lldpIfaceRT) {
	defer close(rt.txDone)
	if !m.sendAdvertisement(rt) {
		return
	}
	for {
		m.mu.Lock()
		interval := m.interval
		m.mu.Unlock()
		if interval <= 0 {
			interval = lldpDefaultIntervalSeconds
		}
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-rt.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !m.sendAdvertisement(rt) {
			return
		}
	}
}

// sendAdvertisement 发一帧广告；返回 false ＝该口的收发已终结（socket 已关/进程退出），
// 发送协程据此退出。发送失败（非关闭）如实记录到该口的错误并按间隔重试，不静默。
func (m *lldpManager) sendAdvertisement(rt *lldpIfaceRT) bool {
	m.mu.Lock()
	closed := m.closed
	io := rt.io
	interval := m.interval
	m.mu.Unlock()
	if closed || io == nil {
		return false
	}
	frame := buildLLDPFrame(rt.mac, m.hostnameNow(), rt.name, interval)
	if err := io.Send(frame); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return false // teardown/Close 已关掉该 socket
		}
		m.setIfaceErr(rt, "发送广告失败: "+err.Error())
		slog.Warn("内核 LLDP 广告发送失败，按间隔重试", "interface", rt.name, "err", err)
		return true
	}
	m.mu.Lock()
	rt.lastTx = time.Now()
	rt.err = "" // 成功即清：仍在的失败会被下一轮立刻重新记录
	m.mu.Unlock()
	return true
}

// hear 收编一条已解析的 LLDPDU 到邻居表：缺 chassis/port 的帧没法定键 ⇒ 按畸形丢弃；
// 同键（本地口 + chassis + port）原地刷新 heardAt 与 TTL，不新增条目。
func (m *lldpManager) hear(local string, t lldpTlvs, now time.Time) {
	if !t.hasChassis || !t.hasPort {
		atomic.AddUint64(&m.malformed, 1)
		return
	}
	key := lldpNeighborKey{
		local:   local,
		chassis: network.LldpIDBySubtype(uint32(t.chassis.subtype), lldpChassisSubtypeMAC, t.chassis.value),
		port:    network.LldpIDBySubtype(uint32(t.port.subtype), lldpPortSubtypeMAC, t.port.value),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.neighbors[key]
	if n == nil {
		n = &lldpNeighborRT{key: key}
		m.neighbors[key] = n
	}
	n.heardAt = now
	n.ttl = t.ttl // 缺 TTL TLV＝0 ⇒ expireLocked 视为立即过期（不合规帧不显示）
	if rt := m.ifaces[local]; rt != nil {
		rt.err = "" // 收到有效 LLDPDU＝收路径健康
	}
}

// setIfaceErr 记录某口最近一次失败原因（读视图/巡检据此如实报未收敛）。
func (m *lldpManager) setIfaceErr(rt *lldpIfaceRT, msg string) {
	m.mu.Lock()
	rt.err = msg
	m.mu.Unlock()
}

// ---------- 配置取用 ----------

// lldpOf nil 安全地取 LLDP 声明（nil = 未声明/未启用任何内容）。
func lldpOf(cfg model.Config) *model.LldpConfig {
	if cfg.Protocols == nil {
		return nil
	}
	return cfg.Protocols.LLDP
}
