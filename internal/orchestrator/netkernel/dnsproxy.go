package netkernel

// 内核数据面 DNS 代理（v3 决策 #439；FR-NET-010 的内核侧落地）。
//
// VPP 侧形态（决策 #345/#400）= nfvisd 内自研域内转发器 + VPP punt socket；内核侧**没有
// punt**，但域的 IPv4 地址（L2 的 BVI 网关地址、type=l3/VRF 的 l3-interface 地址）在内核里
// 就是**本机地址**——一个绑该**具体地址**的 UDP/53 socket 天然收到指向它的域内查询，应答就是
// 普通 UDP 回包（不需要构造/注入裸 IP 包）。故本文件只做**域落点 socket 的收敛与报文转发**，
// 报文级处理（SERVFAIL 构造与 question 回显、上游去重、宿主栈查询）**不复制**：
// 从 network 包共享导出（network.DNSServfail / network.DNSServersDedupe /
// network.DNSHostUDPQuery / network.DNSQueryUpstreams），与 VPP 侧单一真源、行为同源。
//
// 口径（与 VPP 侧逐字同源，见规格书附录 A 第 439 条与设计 §3.2d）：
//   - 落点集：由**调用点**（提交编排从目标配置派生）经 orchestrator.DNSProxyUpstreams.Domains
//     传入——apply/Sync/巡检路径**绝不回读配置发动机**（提交期发动机锁由本次提交自己持有，
//     重入＝自死锁；真机 SIGQUIT 全栈实证过同型事故，见 provider.go 的 configSnapshot 注释）。
//   - 上游选择：按域优先、回落全局；两者皆空 ⇒ 对该查询回 **SERVFAIL**（快速失败，不静默丢弃）。
//   - 纯 UDP：不缓存、不解析资源记录内容、TCP 不在覆盖内（与 VPP 侧同边界）。
//   - 起不来（地址未就绪、VRF 设备不存在、53 被占）**如实报错**：Sync 返回错误（提交失败并
//     回滚 / 恢复未收敛项 / 巡检告警），不静默。
//
// 生命周期（与 DHCP 服务器/中继同族）：
//   - **提交**：随提交编排收敛（ApplyDNSProxy 作为交换机/VRF 之后的伴随操作；undo 回旧声明）；
//   - **恢复重放**：EnsureConsistent 的交换机/VRF 段之后重放（socket 活在进程内，nfvisd
//     重启后必然不在；声明为空＝teardown，幂等）；
//   - **巡检**：ReconcileDNSProxy（15s）对账——起失败/带外丢失的补起、已不声明的关掉；
//   - **进程退出**：Provider.Close 关全部 socket 并等收包协程退出。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	// dnsProxyPort 数据面 DNS 代理的服务端口（仅 UDP 53；TCP 不在覆盖内——与 VPP 侧同口径，
	// 也是真实现里 socket 绑定的端口）。
	dnsProxyPort = 53
	// dnsProxyStopTimeout 关停一批落点 socket 的**总**等待上界（不随 socket 数放大）：
	// 「起→停不残留收包协程」是契约要求，超时如实报错，不谎称已停。
	dnsProxyStopTimeout = 3 * time.Second
	// dnsProxyRecvRetryDelay 落点收包出错后的重试间隔（不静默空转、也不忙等）。
	dnsProxyRecvRetryDelay = 100 * time.Millisecond
)

// dnsProxyIO 一个域落点 socket（真实现 = UDP/53 绑具体地址 + 该域 VRF 设备；单测注入内存实现，
// 见 dnsproxy_test.go）。
type dnsProxyIO interface {
	// Recv 收一个 UDP 载荷（阻塞；Close 后返回 net.ErrClosed）。from = 客户端地址。
	Recv(buf []byte) (n int, from *net.UDPAddr, err error)
	// Send 经同一 socket 把应答写回客户端（源地址自动＝落点地址、源端口 53）。
	Send(b []byte, to *net.UDPAddr) error
	Close() error
}

// dnsProxyLayer 打开域落点 socket 的底座（真实现见 dnsproxy_{linux,other}.go）。
type dnsProxyLayer interface {
	Open(addr net.IP, vrfDevice string) (dnsProxyIO, error)
}

// dnsProxySocketRT 一个已绑定的落点 socket 及其收包协程句柄。
type dnsProxySocketRT struct {
	addr string
	io   dnsProxyIO
	done chan struct{}
}

// dnsProxyDomainRT 一个域（交换机/VRF）的运行态：生效上游 + 按落点地址索引的 socket 集合。
type dnsProxyDomainRT struct {
	name      string
	addresses []string // 已绑定的落点地址（声明序；State 只报服务中的）
	vrfDevice string
	upstreams []string // 该域生效的上游（按域优先、回落全局；空 = 回 SERVFAIL）
	sockets   map[string]*dnsProxySocketRT
	err       string // 该域未收敛的如实原因（空 = 就绪）
}

// dnsProxyManager 各域落点 socket 的持有者（内核数据面 DNS 代理的进程内运行态）。
//
// 并发：mu 保护全部字段（Sync/Close/State/handle 的上游快照）。收包协程经 handle 只在取
// 上游快照与读 upstream 时短暂持锁——**关停等待一律在出锁之后**（持锁等待会与在途的
// handle 互等成死锁）。
type dnsProxyManager struct {
	layer    dnsProxyLayer
	upstream func(string, []byte) ([]byte, error)

	mu        sync.Mutex
	global    []string
	perSwitch map[string][]string
	order     []string // 声明序（State/确定性输出）
	domains   map[string]*dnsProxyDomainRT
	closed    bool // Close 后置位：在途处理不再查询上游（进程退出路径）

	answered uint64 // 成功转发计数（原子；如实计数）
	servfail uint64 // 回 SERVFAIL 计数（原子）
	sendFail uint64 // 回包失败计数（原子；回包失败不静默）
}

func newDNSProxyManager(layer dnsProxyLayer) *dnsProxyManager {
	return &dnsProxyManager{
		layer:    layer,
		upstream: network.DNSHostUDPQuery,
		domains:  map[string]*dnsProxyDomainRT{},
	}
}

// SetUpstream 覆盖上游查询实现（**单测专用**：不依赖真实上游）；nil 恢复默认（宿主栈 UDP）。
func (m *dnsProxyManager) SetUpstream(fn func(string, []byte) ([]byte, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn == nil {
		fn = network.DNSHostUDPQuery
	}
	m.upstream = fn
}

// Sync 把数据面 DNS 代理声明收敛到内核运行态（决策 #439）。
//
// 未启用（全局与按域皆空）⇒ 关掉并忘掉全部域（空操作路径：未声明＝不服务）；
// 启用 ⇒ 按声明序为每个域确保落点 socket：
//   - 新地址开 socket（非 IPv4/非 IP 的声明**如实报该域的错误**，不静默跳过）；
//   - 已绑定地址保留（幂等；声明未变的普通提交不抖动）；
//   - 不再声明的地址关掉；域被删/域的设备变更 ⇒ 该域的 socket 关掉重建。
//
// 一个域的一项失败**不吞**：逐域收集后 errors.Join 返回（提交失败并回滚 / 恢复未收敛项 /
// 巡检告警）；该域其余地址与其它域照常收敛，下次 Sync 幂等重试。
func (m *dnsProxyManager) Sync(ctx context.Context, want orchestrator.DNSProxyUpstreams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	global := network.DNSServersDedupe(want.Global)
	perSwitch := map[string][]string{}
	for name, s := range want.PerSwitch {
		if d := network.DNSServersDedupe(s); len(d) > 0 {
			perSwitch[name] = d
		}
	}
	enabled := len(global) > 0 || len(perSwitch) > 0

	m.mu.Lock()
	m.global = global
	m.perSwitch = perSwitch

	if !enabled {
		pending := m.detachAllLocked()
		m.order = nil

		m.mu.Unlock()
		return m.waitStopped(pending)
	}

	var errs []error
	var pending []dnsProxyPending
	newDomains := make(map[string]*dnsProxyDomainRT, len(want.Domains))
	newOrder := make([]string, 0, len(want.Domains))
	declared := map[string]bool{}
	for _, d := range want.Domains {
		if declared[d.Name] {
			// 纵深防御：配置校验保证域唯一；声明重复时保留首个，另如实报一条。
			errs = append(errs, fmt.Errorf("dns-proxy/%s: 域在落点声明里重复出现（保留首个声明）", d.Name))
			continue
		}
		declared[d.Name] = true
		newOrder = append(newOrder, d.Name)

		rt := m.domains[d.Name]
		if rt == nil {
			rt = &dnsProxyDomainRT{name: d.Name, sockets: map[string]*dnsProxySocketRT{}}
		}
		// 设备（作用域）变更：旧 socket 不能沿用（它绑在旧 VRF 上），关掉重建。
		if rt.vrfDevice != "" && rt.vrfDevice != d.VRFDevice {
			for key, s := range rt.sockets {
				_ = s.io.Close()
				pending = append(pending, dnsProxyPending{domain: d.Name, sock: s})
				delete(rt.sockets, key)
			}
			rt.addresses = nil
		}
		rt.vrfDevice = d.VRFDevice
		// 按域优先、回落全局（两处都已去重；皆空 ⇒ 该域查询回 SERVFAIL）。
		if ups := perSwitch[d.Name]; len(ups) > 0 {
			rt.upstreams = append([]string(nil), ups...)
		} else {
			rt.upstreams = append([]string(nil), global...)
		}

		var domainErr error
		setErr := func(err error) {
			if domainErr == nil {
				domainErr = err
			}
		}
		bound := make([]string, 0, len(d.Addresses))
		seenAddr := map[string]bool{}
		for _, a := range d.Addresses {
			v4 := net.ParseIP(a).To4()
			if v4 == nil {
				setErr(fmt.Errorf("落点地址 %q 不是可用的 IPv4 地址（内核数据面只服务 IPv4 UDP/53）", a))
				continue
			}
			key := v4.String()
			if seenAddr[key] {
				continue
			}
			seenAddr[key] = true
			if s := rt.sockets[key]; s != nil {
				bound = append(bound, key) // 已在绑定状态：保留（幂等，不抖动）
				continue
			}
			sock, err := m.layer.Open(v4, d.VRFDevice)
			if err != nil {
				setErr(fmt.Errorf("落点 %s:53（VRF %s）绑定失败: %w；该地址是否已下发（网关/BVI 或 l3-interface 是否已收敛）？",
					key, d.VRFDevice, err))
				continue
			}
			s := &dnsProxySocketRT{addr: key, io: sock, done: make(chan struct{})}
			rt.sockets[key] = s
			go m.serve(rt, s)
			bound = append(bound, key)
		}
		// 不再声明的地址：关掉（先关闭唤醒阻塞的 Recv，等待在出锁后统一做）。
		for key, s := range rt.sockets {
			if seenAddr[key] {
				continue
			}
			_ = s.io.Close()
			pending = append(pending, dnsProxyPending{domain: d.Name, sock: s})
			delete(rt.sockets, key)
		}
		rt.addresses = bound
		newDomains[d.Name] = rt
		if domainErr != nil {
			rt.err = domainErr.Error()
			errs = append(errs, fmt.Errorf("dns-proxy/%s: %w", d.Name, domainErr))
		} else {
			rt.err = ""
		}
	}
	// 已从声明里删掉的域：关掉并忘掉。
	for name, rt := range m.domains {
		if declared[name] {
			continue
		}
		for key, s := range rt.sockets {
			_ = s.io.Close()
			pending = append(pending, dnsProxyPending{domain: name, sock: s})
			delete(rt.sockets, key)
		}
	}
	m.domains = newDomains
	m.order = newOrder

	m.mu.Unlock()
	if err := m.waitStopped(pending); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// dnsProxyPending 一个待确认「收包协程已退出」的落点（关闭已执行，等待在出锁后做）。
type dnsProxyPending struct {
	domain string
	sock   *dnsProxySocketRT
}

// detachAllLocked 关掉并摘除全部域落点（调用方持 m.mu；Sync 的未启用路径）。
func (m *dnsProxyManager) detachAllLocked() []dnsProxyPending {
	var pending []dnsProxyPending
	for name, rt := range m.domains {
		for key, s := range rt.sockets {
			_ = s.io.Close()
			pending = append(pending, dnsProxyPending{domain: name, sock: s})
			delete(rt.sockets, key)
		}
	}
	m.domains = map[string]*dnsProxyDomainRT{}
	return pending
}

// waitStopped 等一批落点收包协程退出（**总**等待上界 dnsProxyStopTimeout，不随数量放大）。
// 超时如实报错（不得声称「已停」而留下后台消费者）；调用方不得持 m.mu。
func (m *dnsProxyManager) waitStopped(pending []dnsProxyPending) error {
	if len(pending) == 0 {
		return nil
	}
	deadline := time.Now().Add(dnsProxyStopTimeout)
	var errs []error
	for _, p := range pending {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			errs = append(errs, fmt.Errorf("域 %s 的落点 %s 收包协程未在期限内退出", p.domain, p.sock.addr))
			continue
		}
		select {
		case <-p.sock.done:
		case <-time.After(remaining):
			errs = append(errs, fmt.Errorf("域 %s 的落点 %s 收包协程未在期限内退出", p.domain, p.sock.addr))
		}
	}
	return errors.Join(errs...)
}

// Close 关掉全部域落点并等收包协程退出（进程优雅退出；幂等）。
// 先关闭全部 socket（唤醒阻塞的 Recv）再出锁等待——持锁等待会与在途的 handle 互等。
func (m *dnsProxyManager) Close() error {
	m.mu.Lock()
	m.closed = true
	pending := m.detachAllLocked()
	m.order = nil
	m.mu.Unlock()
	return m.waitStopped(pending)
}

// serve 一个落点的收包循环：收到载荷 ⇒ 交 handle；Close ⇒ 退出；其它收包错误 ⇒ 记日志 +
// 小睡重试（不静默停止——底层瞬态错误不该让该落点从此失聪）。
func (m *dnsProxyManager) serve(dom *dnsProxyDomainRT, s *dnsProxySocketRT) {
	defer close(s.done)
	// 收包缓冲按常见 EDNS0 缓冲取 4096：查询通常很小，但大于缓冲的 UDP 报文会被内核**静默截断**
	// （截断后转发出去上游只会丢弃 ⇒ 无辜的 SERVFAIL），故留足余量。
	buf := make([]byte, 4096)
	for {
		n, from, err := s.io.Recv(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("内核 DNS 代理的域落点收包失败，稍后重试", "domain", dom.name, "addr", s.addr, "err", err)
			time.Sleep(dnsProxyRecvRetryDelay)
			continue
		}
		if n <= 0 || from == nil {
			continue
		}
		m.handle(dom, s, buf[:n], from)
	}
}

// handle 处理一个查询：取该域当前上游（持锁快照）→ 无上游/上游失败 ⇒ SERVFAIL，
// 成功 ⇒ 上游应答原样；随后经**同一 socket** 写回客户端。回包失败如实计数并记日志。
func (m *dnsProxyManager) handle(dom *dnsProxyDomainRT, s *dnsProxySocketRT, query []byte, from *net.UDPAddr) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	ups := append([]string(nil), dom.upstreams...)
	upstream := m.upstream
	m.mu.Unlock()

	var resp []byte
	if len(ups) == 0 {
		// 无上游：快速失败（SERVFAIL 与 question 回显由共享实现保证，与 VPP 侧逐字同形）。
		resp = network.DNSServfail(query)
		atomic.AddUint64(&m.servfail, 1)
	} else {
		resp = network.DNSQueryUpstreams(ups, query, upstream)
		if len(resp) == 0 {
			resp = network.DNSServfail(query)
			atomic.AddUint64(&m.servfail, 1)
		} else {
			atomic.AddUint64(&m.answered, 1)
		}
	}
	if err := s.io.Send(resp, from); err != nil {
		atomic.AddUint64(&m.sendFail, 1)
		slog.Warn("内核 DNS 代理的应答回包失败（已如实计数）", "domain", dom.name, "addr", s.addr, "client", from, "err", err)
	}
}

// State 运行态读数（读视图）：按**声明序**报每个域的〔地址、生效上游、VRF 设备、未收敛原因〕
// 与三个计数。o 快照式，供 API/CLI/Web 三面同源渲染。
func (m *dnsProxyManager) State() network.DNSProxyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := network.DNSProxyState{
		Domains:  make([]network.DNSProxyDomainState, 0, len(m.order)),
		Answered: atomic.LoadUint64(&m.answered),
		Servfail: atomic.LoadUint64(&m.servfail),
		SendFail: atomic.LoadUint64(&m.sendFail),
	}
	for _, name := range m.order {
		rt := m.domains[name]
		if rt == nil {
			continue
		}
		st.Domains = append(st.Domains, network.DNSProxyDomainState{
			Name:      rt.name,
			Addresses: append([]string{}, rt.addresses...),
			Upstreams: append([]string{}, rt.upstreams...),
			VRFDevice: rt.vrfDevice,
			Error:     rt.err,
		})
	}
	return st
}
