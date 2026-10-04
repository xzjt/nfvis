package network

// DHCP 服务器 provider（决策 #359，v1）：域内租约池/状态机的**用户态**实现。
//
// 能力前提与架构由 round140 真机 spike 定形（证据 docs/evidence/v2-round140-dhcp-server-spike.txt）：
//   - 每台启用 dhcp-server 的交换机由产品创建**一条内置 L2 tap**（tapv2 binapi；VPP 侧名 tapN
//     由 VPP 分配、内核侧名由 host-if-name 指定且 ≤15 字符、按交换机名确定性生成）；
//   - tap 必须显式入该交换机的 bridge-domain 并置 up；内核侧默认已 up；
//   - 入径两条：广播（DISCOVER 等）经 BD 洪泛到 tap；单播（RENEW/RELEASE，到 BVI:67）经
//     ip4-local → UDP/67 → punt socket；
//   - 回程一律写**以太帧到 tap**（不用 punt 回注——round140 实证回注会被源地址反欺骗拒掉）。
//
// 生命周期口径（与 L2/DHCP relay 同族）：
//   - 随提交编排在 bridge-domain **之后**收敛（Sync；未声明/池被删＝teardown）；
//   - **恢复重放必须含它**：VPP 重启后 tap 与 punt 注册全失，recovery.go 按 committed 重放；
//   - **punt 注册不得只在启用/停用边沿做**：relay 的 proxy 会夺走 UDP/67 的注册（round140
//     R140-1），故每次 Sync 都**重申注册**（deregister+register，幂等）；
//   - 租约持久化到独立文件（原子写），**不进配置备份/恢复语义**（与 #356 的 metrics.db 同口径）；
//     文件损坏 ⇒ 如实日志 + 空表起步（不崩）。
//
// 范围（v1 如实边界）：不做静态绑定/保留/排除列表；不做 option 15 之外的扩展选项；
// 不做跨 VRF/跨域的 server；不做 HA/多池；不做 v6（DHCPv6 另立项）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

const (
	// DHCPServerClientSock nfvisd 侧 punt client socket（VPP 用 sendmsg 把上行 UDP/67 发到这里）。
	DHCPServerClientSock = "/run/vpp/nfvis-dhcp.sock"
	// DHCPServerServerSock VPP 侧 punt server socket（startup.conf 的 `punt { socket … }`）。
	DHCPServerServerSock = "/run/vpp/punt.sock"
	// DefaultDHCPLeaseDir 租约持久化目录（独立于配置库；不进备份/恢复语义）。
	DefaultDHCPLeaseDir = "/var/lib/nfvis/dhcp"
	// dhcpServerScope DHCP 服务器告警作用域（与恢复收敛/提交期/环路等作用域分开）。
	dhcpServerScope = "dhcp-server"
	// AlarmDHCPPoolExhausted 租约池耗尽（warning，source=交换机名）：池内无可用地址可应答
	// 新的 DISCOVER；有地址释放/租约到期/扩容即自动消解。
	AlarmDHCPPoolExhausted = "DHCP_POOL_EXHAUSTED"
	// dhcpDedupeWindow 双入径去重窗（round141 实测后补）：同一条客户端广播报文会经「BD 洪泛
	// 到内置 tap」与「经 BVI 进 UDP/67 的 punt」各达一次，窗内只应答一次。窗长 3s 远小于
	// 客户端重传退避（RFC 2131 建议首重试 ≥4s），不会吞掉真重传。
	dhcpDedupeWindow = 3 * time.Second
	// dhcpTapTag 内置 tap 的 VPP 侧 tag（仅供人工排查；**不作查找键** —— round140 实测
	// SwInterfaceTapV2Dump 不回 tag，恢复查找只能按 HostIfName）。
	dhcpTapTagPrefix = "nfvis-dhcp:"
)

// DHCPServerTapName 由交换机名确定性生成内核侧 tap 名：`nfvisdh` + sha256 前 4 字节的
// 十六进制（共 15 字符 = Linux IFNAMSIZ 上限）。确定性保证恢复期能按 HostIfName 找存量，
// 且不依赖 VPP 分配的 tapN（VPP 侧名与内核侧名不同，round140 实测）。
func DHCPServerTapName(switchName string) string {
	sum := sha256.Sum256([]byte(switchName))
	return "nfvisdh" + hex.EncodeToString(sum[:4])
}

// DHCPLease 一条租约的运行态读视图（与 openapi DhcpLease 同形）。**json tag 即契约**：
// REST GET /virtual-switches/{n}/dhcp-leases 直接序列化本结构，字段名与契约逐字一致
// （小写 ip/mac/state/expires_in_seconds）——漏 tag 会按 Go 字段名输出（IP/MAC/…），
// 照契约开发的客户端会全部取空（形状守护 TestDHCPServerShapeMatchesContract 钉住）。
type DHCPLease struct {
	IP               string `json:"ip"`
	MAC              string `json:"mac"`
	State            string `json:"state"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// DHCPLeaseFile 租约持久化文件（独立于配置库；目录 0700、文件 0600、原子写）。
type dhcpLeaseFile struct {
	Switch string      `json:"switch"`
	Leases []dhcpLease `json:"leases"`
}

// dhcpServerSpec 一台交换机已收敛的服务器规格（由 model 派生，Sync 时重算）。
type dhcpServerSpec struct {
	switchName string
	bdID       uint32
	tapName    string
	poolLo     uint32
	poolHi     uint32
	lease      time.Duration
	bvi        net.IP
	mask       net.IPMask
	dns        net.IP
	domain     string
	tapMAC     net.HardwareAddr
}

// replySpec 构造应答所需参数（含内核 tap MAC；tap 未打开时无法应答）。
func (s dhcpServerSpec) replySpec() dhcpReplySpec {
	return dhcpReplySpec{
		serverMAC: s.tapMAC, bvi: s.bvi, mask: s.mask, dns: s.dns,
		domain: s.domain, lease: int(s.lease / time.Second),
	}
}

// dhcpServerRT 一台交换机的运行态（配置规格 + 租约表 + 内核 tap + 发送用参数）。
type dhcpServerRT struct {
	spec     dhcpServerSpec
	tapIndex uint32 // VPP 侧 sw_if_index（0 = 未建立）
	leases   *dhcpLeaseTable
	tap      dhcpTapTransport // nil = 未打开（VPP 重启后 reset / 首次 Sync 失败）

	// recent 记录最近应答过的 (chaddr|xid|消息类型) → 时刻：同一条客户端报文会经**两条入径**
	// 各到一次（广播帧在 BD 里既洪泛到内置 tap、又经 BVI 进 UDP/67 的 punt——round141 真机实测
	// 一 REQUEST 两 ACK），去重窗内只应答一次。窗长 3s：远小于客户端重传退避（RFC 2131 建议
	// 首重试 ≥4s），不会吞掉真重传；entries 在每次查表时顺带清理。
	recent map[string]time.Time
}

// DHCPServerProvider 域内 DHCP 服务器编排（决策 #359）。
type DHCPServerProvider struct {
	client     func() (DHCPServerClient, error)
	punt       func() (PuntClient, error)
	transport  func() (dnsPuntTransport, error)
	tapOpen    func(name string) (dhcpTapTransport, error)
	clientSock string
	leaseDir   string
	now        func() time.Time

	// opMu 串行化 Sync/teardown/Reconcile（含 VPP 调用，不持 mu——收包路径只等 mu）。
	opMu sync.Mutex
	// mu 保护 servers / puntTr / puntReg 与各运行态的租约表（收包与读视图共用）。
	mu      sync.Mutex
	servers map[string]*dhcpServerRT
	puntTr  dnsPuntTransport // 共享的 punt 接收 socket（存在启用中的服务器期间）
	puntReg bool             // VPP 侧 UDP/67 注册是否在场（reset 置 false）

	switchOf func(uint32) (string, bool) // sw_if_index(域 BVI) → 交换机名（punt 路径派发）
	alarms   *AlarmStore
}

// NewDHCPServerProviderFunc 以客户端工厂构造（连接可重连）；传输/内核 tap 用真实实现。
func NewDHCPServerProviderFunc(client func() (DHCPServerClient, error), punt func() (PuntClient, error)) *DHCPServerProvider {
	return &DHCPServerProvider{
		client: client,
		punt:   punt,
		transport: func() (dnsPuntTransport, error) {
			return newUnixPuntTransport(DHCPServerClientSock, DHCPServerServerSock)
		},
		tapOpen:    openDHCPTap,
		clientSock: DHCPServerClientSock,
		leaseDir:   DefaultDHCPLeaseDir,
		now:        time.Now,
		servers:    map[string]*dhcpServerRT{},
	}
}

// SetSwitchResolver 注入「sw_if_index → 交换机名」反查（punt 单播路径按上行域派发）。
func (p *DHCPServerProvider) SetSwitchResolver(fn func(uint32) (string, bool)) { p.switchOf = fn }

// SetAlarms 注入告警表（池耗尽告警落点；可空）。
func (p *DHCPServerProvider) SetAlarms(a *AlarmStore) { p.alarms = a }

// SetLeaseDir 设置租约持久化目录（单测注入临时目录；空 = 保持缺省）。
func (p *DHCPServerProvider) SetLeaseDir(dir string) {
	if dir != "" {
		p.leaseDir = dir
	}
}

// SetTapOpen 设置内核侧 tap 打开器（单测注入内存实现；生产走平台默认 AF_PACKET）。
func (p *DHCPServerProvider) SetTapOpen(fn func(string) (dhcpTapTransport, error)) {
	if fn != nil {
		p.tapOpen = fn
	}
}

// SetPuntTransport 设置 punt 接收传输（单测注入内存实现；生产走 AF_UNIX SOCK_DGRAM）。
func (p *DHCPServerProvider) SetPuntTransport(fn func() (dnsPuntTransport, error)) {
	if fn != nil {
		p.transport = fn
	}
}

// SetClock 注入时钟（单测确定性）。
func (p *DHCPServerProvider) SetClock(fn func() time.Time) {
	if fn != nil {
		p.now = fn
	}
}

// Close 优雅退出（与 DNSProxyProvider.Close 同理）：注销 UDP/67 的 punt 注册并关闭接收
// socket、关闭各内核侧 tap——否则留一个指向已消失 socket 的注册＝域内 DHCP 黑洞
// （客户端 DISCOVER 被静默吞掉，直到下次注册或数据面重启）。VPP 侧 tap 对象不删
// （BD 成员关系保留，下次启动 Sync 按 HostIfName 找到存量直接复用）。
func (p *DHCPServerProvider) Close() error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	tr := p.puntTr
	reg := p.puntReg
	p.puntTr, p.puntReg = nil, false
	for _, rt := range p.servers {
		if rt.tap != nil {
			_ = rt.tap.Close()
			rt.tap = nil
		}
	}
	p.mu.Unlock()
	var errs []error
	if tr != nil {
		if err := tr.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if reg {
		if c, err := p.punt(); err == nil {
			if err := c.Deregister(dhcpServerPort); err != nil {
				errs = append(errs, err)
			}
			c.Close()
		}
	}
	return errors.Join(errs...)
}

// reset 连接（重）建立时调用：VPP 侧 tap 与 punt 注册都已随 VPP 重启消失——关闭内核侧
// tap（收包协程随之退出）、标记注册失效；**租约表保留**（服务器自己的状态，客户端续租不受
// VPP 重启影响，恢复重放会重建 tap/注册）。不在此时删 VPP 对象（VPP 已重启，删只会失败）。
func (p *DHCPServerProvider) reset() {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	for _, rt := range p.servers {
		if rt.tap != nil {
			_ = rt.tap.Close()
			rt.tap = nil
		}
	}
	p.puntReg = false
	p.mu.Unlock()
}

// Sync 收敛一台交换机的 dhcp-server 声明（决策 #359）。vs 未启用（无池）时＝teardown。
func (p *DHCPServerProvider) Sync(ctx context.Context, vs model.VirtualSwitch) error {
	if !vs.DHCPServerEnabled() {
		return p.teardown(vs.Name)
	}
	spec, err := dhcpServerSpecOf(vs)
	if err != nil {
		return err
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()

	// 1) tap 生命期：按内核侧名（HostIfName）找存量；无则建。**不用 tag 作查找键**
	//（round140 实测 SwInterfaceTapV2Dump 不回 tag）。
	c, err := p.client()
	if err != nil {
		return err
	}
	taps, err := c.TapDump()
	if err != nil {
		c.Close()
		return fmt.Errorf("查询交换机 %s 的内置 tap 存量: %w", vs.Name, err)
	}
	tapIndex := uint32(0)
	found := false
	for _, t := range taps {
		if t.HostIfName == spec.tapName {
			tapIndex, found = t.SwIfIndex, true
			break
		}
	}
	if !found {
		idx, err := c.TapCreate(spec.tapName, dhcpTapTagPrefix+vs.Name)
		if err != nil {
			c.Close()
			return fmt.Errorf("创建交换机 %s 的 DHCP 内置 tap（内核侧名 %s）: %w", vs.Name, spec.tapName, err)
		}
		tapIndex = idx
	}
	// 2) 入该交换机的 bridge-domain + 置 up（幂等；恢复重放与新建同路径）。
	if err := c.SetL2Bridge(tapIndex, spec.bdID, true); err != nil {
		c.Close()
		return fmt.Errorf("把交换机 %s 的 DHCP 内置 tap 加入 bridge-domain: %w", vs.Name, err)
	}
	if err := c.SetState(tapIndex, true); err != nil {
		c.Close()
		return fmt.Errorf("置交换机 %s 的 DHCP 内置 tap 为 up: %w", vs.Name, err)
	}
	c.Close()

	// 3) punt 注册重申（每次收敛都重注册；relay 的 proxy 会夺走 UDP/67——round140 R140-1）。
	if err := p.assertPunt(); err != nil {
		return err
	}

	// 4) 运行态装配：登记/更新规格与租约表（首次从文件恢复；池变更清越界条目）。
	p.mu.Lock()
	rt := p.servers[vs.Name]
	if rt == nil {
		rt = &dhcpServerRT{leases: newDHCPLeaseTable(spec.poolLo, spec.poolHi, p.now),
			recent: map[string]time.Time{}}
		rt.leases.restore(p.loadLeaseFile(vs.Name))
		p.servers[vs.Name] = rt
	} else {
		rt.leases.setPool(spec.poolLo, spec.poolHi)
	}
	// VPP 侧索引变了（tap 被带外删后重建 / VPP 重启）：旧内核 tap 已不可用，关了重开。
	stale := rt.tap != nil && rt.tapIndex != tapIndex
	if stale {
		_ = rt.tap.Close()
		rt.tap = nil
	}
	rt.spec, rt.tapIndex = spec, tapIndex
	needTap := rt.tap == nil
	p.mu.Unlock()

	p.refreshPoolAlarm(vs.Name)

	if !needTap {
		return nil
	}
	// 5) 打开内核侧 tap 并起收包协程（按交换机名；内核接口可能刚创建，openDHCPTap 自带重试）。
	tr, err := p.tapOpen(spec.tapName)
	if err != nil {
		return fmt.Errorf("打开交换机 %s 的 DHCP 内置 tap（内核侧 %s）: %w", vs.Name, spec.tapName, err)
	}
	p.mu.Lock()
	if rt.tap != nil {
		// 并发装配（理论不可达：opMu 串行）；保留先到者。
		p.mu.Unlock()
		_ = tr.Close()
		return nil
	}
	rt.tap = tr
	rt.spec.tapMAC = tr.MAC()
	p.mu.Unlock()
	go p.serveTap(vs.Name, tr)
	return nil
}

// teardown 停用一台交换机的服务器：删内置 tap + 清租约文件；最后一台停用时注销 punt。
// 幂等（未启用过即无事可做）。
func (p *DHCPServerProvider) teardown(name string) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	rt := p.servers[name]
	delete(p.servers, name)
	remaining := len(p.servers)
	tr := p.puntTr
	reg := p.puntReg
	if remaining == 0 {
		p.puntTr, p.puntReg = nil, false
	}
	p.mu.Unlock()

	var errs []error
	if rt != nil && rt.tap != nil {
		_ = rt.tap.Close()
	}
	known := uint32(0)
	if rt != nil {
		known = rt.tapIndex
	}
	errs = append(errs, p.deleteTap(name, known)...)
	// 清租约文件（运行态，不属配置；停用即回收）——**不依赖进程内登记**：nfvisd 重启后
	// 停用（servers 表为空、rt 为 nil）同样要清掉磁盘上的租约文件，否则孤儿文件滞留、
	// 与「停用即回收」的契约不符。
	if err := os.Remove(p.leasePath(name)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("清除交换机 %s 的 DHCP 租约文件: %w", name, err))
	}
	if remaining == 0 {
		if tr != nil {
			_ = tr.Close()
		}
		if reg {
			if c, err := p.punt(); err == nil {
				_ = c.Deregister(dhcpServerPort)
				c.Close()
			}
		}
	}
	if p.alarms != nil {
		p.alarms.Resolve(dhcpServerScope, AlarmDHCPPoolExhausted, name)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// deleteTap 回收该交换机的内置 tap（决策 #359：停用/删交换机即删）。known 是进程内登记的
// sw_if_index（0 = 未登记——nfvisd 重启后先停用、或恢复收敛尚未跑到）：未登记时按内核侧名
// （HostIfName）dump 找存量再删，与 Sync 的恢复查找**同一判据**（不用 tag 作查找键——
// round140 实测 dump 不回 tag）。VPP 里本就没有（VPP 重启/从未建成）＝已达成，不报错。
func (p *DHCPServerProvider) deleteTap(name string, known uint32) []error {
	c, err := p.client()
	if err != nil {
		return []error{fmt.Errorf("删除交换机 %s 的 DHCP 内置 tap: %w", name, err)}
	}
	defer c.Close()
	idx := known
	if idx == 0 {
		taps, err := c.TapDump()
		if err != nil {
			return []error{fmt.Errorf("查询交换机 %s 的 DHCP 内置 tap: %w", name, err)}
		}
		for _, t := range taps {
			if t.HostIfName == DHCPServerTapName(name) {
				idx = t.SwIfIndex
				break
			}
		}
		if idx == 0 {
			return nil // 数据面里没有该 tap：已达成
		}
	}
	// tap 已不存在（VPP 重启/带外删）按已达成处理（isMissingIfaceErr，同 #342 容错口径）。
	if err := c.TapDelete(idx); err != nil && !isMissingIfaceErr(err) {
		return []error{fmt.Errorf("删除交换机 %s 的 DHCP 内置 tap: %w", name, err)}
	}
	return nil
}

// assertPunt 确保 punt socket 存在并**重申** UDP/67 注册（deregister+register，幂等）。
// 硬口径（round140 R140-1）：relay 的 proxy 会夺走 UDP/67 的注册，且撤销后残留仍吞包——
// 故不能只在「启用↔停用」边沿注册，每次收敛都要重申。
func (p *DHCPServerProvider) assertPunt() error {
	p.mu.Lock()
	tr := p.puntTr
	p.mu.Unlock()
	if tr == nil {
		t, err := p.transport()
		if err != nil {
			return fmt.Errorf("绑定 DHCP punt socket（%s）: %w", p.clientSock, err)
		}
		p.mu.Lock()
		if p.puntTr == nil {
			p.puntTr = t
			go p.servePunt(t)
		} else {
			t.Close()
			t = p.puntTr
		}
		p.mu.Unlock()
		tr = t
	}
	c, err := p.punt()
	if err != nil {
		return fmt.Errorf("打开 VPP punt API 通道: %w", err)
	}
	defer c.Close()
	// 先撤再注册（撤销失败＝本就没注册，按已达成处理——重申本身是幂等的）。
	_ = c.Deregister(dhcpServerPort)
	if err := c.Register(p.clientSock, dhcpServerPort); err != nil {
		return fmt.Errorf("注册 DHCP punt socket（UDP/%d → %s）: %w；若刚升级，请先执行 request vpp restart 让 startup.conf 的 punt 段生效",
			dhcpServerPort, p.clientSock, err)
	}
	p.mu.Lock()
	p.puntReg = true
	p.mu.Unlock()
	return nil
}

// serveTap 内核侧 tap 收包协程（一台交换机一个；tap 关闭即退出）。
func (p *DHCPServerProvider) serveTap(name string, tr dhcpTapTransport) {
	for {
		frame, err := tr.Recv()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 收包错误不静默中止：稍候重试（tap 被带外删时 Recv 会持续报错，由巡检自愈）。
			time.Sleep(20 * time.Millisecond)
			continue
		}
		p.handleTapFrame(name, frame)
	}
}

// handleTapFrame 处理一个 tap 以太帧（广播入径；非 DHCP 一律忽略）。
func (p *DHCPServerProvider) handleTapFrame(name string, frame []byte) {
	msg, ok := parseDHCPEtherFrame(frame)
	if !ok || !msg.giaddr.IsUnspecified() {
		return // 非 DHCP/非 IPv4/带中继：静默忽略
	}
	p.handleMessage(name, msg)
}

// servePunt punt 收包协程（所有交换机共用；socket 关闭即退出）。
func (p *DHCPServerProvider) servePunt(tr dnsPuntTransport) {
	for {
		desc, raw, err := tr.Recv()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		p.handlePuntPacket(desc, raw)
	}
}

// handlePuntPacket 处理一个 punt 上行包（单播续租/释放）：按 sw_if_index（域 BVI）找交换机。
func (p *DHCPServerProvider) handlePuntPacket(desc dnsPuntDesc, raw []byte) {
	if p.switchOf == nil {
		return
	}
	name, ok := p.switchOf(desc.swIfIndex)
	if !ok {
		return // 不属于任何已知交换机（不该出现）：不猜测
	}
	ipPkt, ok := extractIP(raw) // 包体可能带 14 字节以太头；按 ethertype→版本 nibble 兜底
	if !ok {
		return
	}
	msg, ok := parseDHCPIP(ipPkt)
	if !ok || !msg.giaddr.IsUnspecified() {
		return
	}
	p.handleMessage(name, msg)
}

// handleMessage 服务器状态机：DISCOVER→OFFER、REQUEST→ACK/NAK、RELEASE→释放、DECLINE→隔离。
// 非法/不匹配报文静默忽略（不构造应答）。
func (p *DHCPServerProvider) handleMessage(name string, msg dhcpMessage) {
	p.mu.Lock()
	rt := p.servers[name]
	if rt == nil {
		p.mu.Unlock()
		return
	}
	mac := macString(msg.chaddr)
	// 双入径去重（round141 实测：广播帧既洪泛到 tap 又经 BVI 进 punt，一 REQUEST 两 ACK）——
	// 同一 (chaddr, xid, 消息类型) 在去重窗内只应答一次；顺带清理过期条目。
	dedupeKey := mac + "|" + strconv.FormatUint(uint64(msg.xid), 16) + "|" + string(rune(msg.msgType))
	now := p.now()
	for k, t := range rt.recent {
		if now.Sub(t) > dhcpDedupeWindow {
			delete(rt.recent, k)
		}
	}
	if t, seen := rt.recent[dedupeKey]; seen && now.Sub(t) <= dhcpDedupeWindow {
		p.mu.Unlock()
		return
	}
	rt.recent[dedupeKey] = now
	var reply []byte
	changed := false
	switch msg.msgType {
	case dhcpDiscover:
		if ip, ok := rt.leases.allocate(mac); ok {
			changed = true
			reply = buildDHCPReply(msg, dhcpOffer, net.ParseIP(ip), rt.spec.replySpec())
		} else {
			// 池内无可用地址 ⇒ 不发 OFFER，且**当场**复核池耗尽告警（决策 #359：无可用地址
			// 可应答 DISCOVER 时 raise；有地址释放/到期/扩容即消解——不等下一轮巡检）。
			p.refreshPoolAlarmLocked(name, rt)
		}
	case dhcpRequest:
		// 客户端选定了别的 server-id：静默忽略（不抢答，也不 NAK 干扰）。
		if msg.serverID != nil && !msg.serverID.IsUnspecified() && !msg.serverID.Equal(rt.spec.bvi) {
			p.mu.Unlock()
			return
		}
		req := msg.requestedIP
		if req == nil || req.IsUnspecified() {
			req = msg.ciaddr
		}
		reqV, inPool := uint32(0), false
		if req != nil && !req.IsUnspecified() {
			reqV, inPool = ipToU32(req.String())
			inPool = inPool && rt.leases.inPool(reqV)
		}
		switch {
		case req == nil || req.IsUnspecified():
			// 既无 option 50 也无 ciaddr：不是可判定的 REQUEST，静默忽略。
		case !inPool:
			reply = buildDHCPReply(msg, dhcpNak, nil, rt.spec.replySpec())
		case rt.leases.ownerOf(req.String()) != nil && rt.leases.ownerOf(req.String()).MAC != mac:
			reply = buildDHCPReply(msg, dhcpNak, nil, rt.spec.replySpec())
		default:
			rt.leases.commit(mac, req.String(), rt.spec.lease)
			changed = true
			reply = buildDHCPReply(msg, dhcpAck, req, rt.spec.replySpec())
		}
	case dhcpRelease:
		if rt.leases.release(mac) {
			changed = true
		}
	case dhcpDecline:
		ip := msg.requestedIP
		if (ip == nil || ip.IsUnspecified()) && msg.ciaddr != nil && !msg.ciaddr.IsUnspecified() {
			ip = msg.ciaddr
		}
		if ip == nil || ip.IsUnspecified() {
			if l := rt.leases.lookupMAC(mac); l != nil {
				ip = net.ParseIP(l.IP)
			}
		}
		if ip != nil && !ip.IsUnspecified() && rt.leases.decline(ip.String(), rt.spec.lease) {
			changed = true
		}
	default:
		// OFFER/ACK/NAK 等服务器向消息不该出现在入向，静默忽略。
	}
	if changed {
		p.persistLocked(name, rt)
		p.refreshPoolAlarmLocked(name, rt)
	}
	tap := rt.tap
	p.mu.Unlock()
	if len(reply) > 0 && tap != nil {
		if err := tap.Send(reply); err != nil {
			slog.Warn("DHCP 应答写入内置 tap 失败", "switch", name, "err", err)
		}
	}
}

// refreshPoolAlarm 复核该交换机的池耗尽告警（有可用地址即消解）。
func (p *DHCPServerProvider) refreshPoolAlarm(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.servers[name]; rt != nil {
		p.refreshPoolAlarmLocked(name, rt)
	}
}

// refreshPoolAlarmLocked 池耗尽告警的建/消（调用方持 p.mu）。
// 只按「池内是否还有可用地址」的事实；文案带池范围与在租数（不含内部引用）。
func (p *DHCPServerProvider) refreshPoolAlarmLocked(name string, rt *dhcpServerRT) {
	if p.alarms == nil {
		return
	}
	if rt.leases.free() {
		p.alarms.Resolve(dhcpServerScope, AlarmDHCPPoolExhausted, name)
		return
	}
	p.alarms.Raise(dhcpServerScope, SeverityWarning, AlarmDHCPPoolExhausted, fmt.Sprintf(
		"交换机 %s 的 DHCP 租约池 %s-%s 已无可用地址（在租 %d 个）：新的客户端拿不到地址；"+
			"地址释放或租约到期后自动恢复，也可扩大池（set virtual-switches %s dhcp-server pool <start> <end>）",
		name, model.Uint32ToIPv4(rt.spec.poolLo).String(), model.Uint32ToIPv4(rt.spec.poolHi).String(),
		rt.leases.activeCount(), name), name)
}

// Reconcile 15s 巡检：对配置里启用 dhcp-server 的交换机做幂等收敛（补齐 VPP 重启后丢失的
// tap/注册，并把到期租约回收、告警复核）——与提交/恢复收敛同一段 Sync，不另造第二套。
func (p *DHCPServerProvider) Reconcile(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, vs := range cfg.VirtualSwitches {
		if !vs.DHCPServerEnabled() {
			continue
		}
		if err := p.Sync(ctx, vs); err != nil {
			errs = append(errs, fmt.Errorf("virtual-switches/%s/dhcp-server: %w", vs.Name, err))
		}
	}
	// 到期回收与告警复核（不依赖 VPP 可用性：租约是服务器自己的状态）。
	p.mu.Lock()
	for name, rt := range p.servers {
		if rt.leases.sweep() {
			p.persistLocked(name, rt)
		}
		p.refreshPoolAlarmLocked(name, rt)
	}
	p.mu.Unlock()
	return errs
}

// Leases 某交换机的租约表读视图（运行态）。ok=false = 该交换机没有运行中的服务器
// （未启用/尚未收敛）。未到期条目按 IP 升序，剩余秒数取整（<1s 记 1，避免显示 0 却仍在表内）。
func (p *DHCPServerProvider) Leases(name string) ([]DHCPLease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.servers[name]
	if rt == nil {
		return nil, false
	}
	now := p.now()
	out := make([]DHCPLease, 0, len(rt.leases.byIP))
	for _, l := range rt.leases.snapshot() {
		left := int(l.ExpiresAt.Sub(now) / time.Second)
		if left < 1 {
			left = 1
		}
		out = append(out, DHCPLease{IP: l.IP, MAC: l.MAC, State: l.State, ExpiresInSeconds: left})
	}
	return out, true
}

// ActiveLeases 生效租约数（state=active）；ok=false 同 Leases。
func (p *DHCPServerProvider) ActiveLeases(name string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.servers[name]
	if rt == nil {
		return 0, false
	}
	return rt.leases.activeCount(), true
}

// TapIndexes 产品自持的内置 tap 的 sw_if_index 集合（端口读视图按它过滤；**不用名字匹配**）。
func (p *DHCPServerProvider) TapIndexes() map[uint32]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[uint32]bool, len(p.servers))
	for _, rt := range p.servers {
		if rt.tapIndex != 0 {
			out[rt.tapIndex] = true
		}
	}
	return out
}

// ---------- 租约持久化 ----------

// leasePath 租约文件路径（交换机名经模型校验为 字母数字-_.；仍取 Base 防御路径穿越）。
func (p *DHCPServerProvider) leasePath(name string) string {
	return filepath.Join(p.leaseDir, filepath.Base(name)+".json")
}

// loadLeaseFile 读租约文件并恢复（目录/文件不存在＝空表；损坏 ⇒ 如实日志 + 空表起步，不崩）。
func (p *DHCPServerProvider) loadLeaseFile(name string) []dhcpLease {
	path := p.leasePath(name)
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("DHCP 租约文件读取失败，按空表起步", "switch", name, "path", path, "err", err)
		}
		return nil
	}
	var f dhcpLeaseFile
	if err := json.Unmarshal(b, &f); err != nil {
		slog.Warn("DHCP 租约文件损坏，按空表起步（不删除原文件，供排查）", "switch", name, "path", path, "err", err)
		return nil
	}
	return f.Leases
}

// persistLocked 原子写租约文件（写临时文件 + rename；目录 0700、文件 0600）。
// 写失败如实记日志、不中断服务（内存表仍正确；下轮变更会重试）。
func (p *DHCPServerProvider) persistLocked(name string, rt *dhcpServerRT) {
	if p.leaseDir == "" {
		return
	}
	if err := os.MkdirAll(p.leaseDir, 0o700); err != nil {
		slog.Warn("DHCP 租约目录创建失败，租约不落盘", "switch", name, "dir", p.leaseDir, "err", err)
		return
	}
	b, err := json.Marshal(dhcpLeaseFile{Switch: name, Leases: rt.leases.snapshot()})
	if err != nil {
		slog.Warn("DHCP 租约序列化失败", "switch", name, "err", err)
		return
	}
	path := p.leasePath(name)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Warn("DHCP 租约临时文件创建失败", "switch", name, "path", tmp, "err", err)
		return
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		slog.Warn("DHCP 租约写入失败", "switch", name, "path", tmp, "err", err)
		return
	}
	if err := f.Sync(); err != nil {
		slog.Warn("DHCP 租约落盘同步失败（继续以原子写为准）", "switch", name, "err", err)
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		slog.Warn("DHCP 租约文件替换失败", "switch", name, "path", path, "err", err)
	}
}

// ---------- 规格派生 ----------

// dhcpServerSpecOf 由交换机声明派生服务器规格（池/BVI/掩码/缺省 DNS/BD id/tap 名）。
// 取值域校验在 model（提交期），此处只做数据面必需的解析，失败即报错（不静默用零值）。
func dhcpServerSpecOf(vs model.VirtualSwitch) (dhcpServerSpec, error) {
	lo, hi, ok := model.DHCPServerPoolRange(vs.DhcpServerPoolStart, vs.DhcpServerPoolEnd)
	if !ok {
		return dhcpServerSpec{}, fmt.Errorf("交换机 %s 的 DHCP 租约池配置无效（%s-%s）",
			vs.Name, vs.DhcpServerPoolStart, vs.DhcpServerPoolEnd)
	}
	bvi, ipnet, ok := vs.GatewayIPv4()
	if !ok {
		return dhcpServerSpec{}, fmt.Errorf("交换机 %s 没有 IPv4 网关地址，无法作为 DHCP 服务器域", vs.Name)
	}
	dns := bvi
	if vs.DhcpServerDNS != "" {
		if v4 := net.ParseIP(vs.DhcpServerDNS).To4(); v4 != nil {
			dns = v4
		}
	}
	return dhcpServerSpec{
		switchName: vs.Name,
		bdID:       BDID(vs.Name),
		tapName:    DHCPServerTapName(vs.Name),
		poolLo:     lo,
		poolHi:     hi,
		lease:      time.Duration(vs.DHCPServerLeaseSeconds()) * time.Second,
		bvi:        bvi,
		mask:       ipnet.Mask,
		dns:        dns,
		domain:     vs.DhcpServerDomainName,
	}, nil
}
