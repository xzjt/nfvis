package compute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// DefaultURI libvirt 系统域连接 URI（FR-CMP-010：qemu:///system）。
const DefaultURI = "qemu:///system"

// defaultLibvirtSocket unix 形态缺省 socket 路径 —— 与 go-libvirt socket/dialers
// 的 defaultSocket 同一取值（路径规则见 libvirtSocketPath）。
const defaultLibvirtSocket = "/var/run/libvirt/libvirt-sock"

// defaultRPCTimeout 单次 RPC 的缺省硬上界（决策 #362）：调用方 ctx 自带 deadline
// 时以调用方的为准；无 deadline 的调用（Version/HypervisorVersion 等元数据/版本类）
// 用此值。取 30s 的理由：正常 RPC 是本地 unix socket 上的毫秒级往返，即便过载也远
// 不至于到 30s；而对「socket 可连、守护进程零响应」的假死现场，它把无界挂起收敛成
// 有界失败。常驻探活另有更短的 5s 上界（cmd/nfvisd/async_connect.go），会先于本上界
// 到期并关闭连接，读路径因此也提前失败，而不是拖到客户端的 90s 超时。
const defaultRPCTimeout = 30 * time.Second

// handshakeDrain 超时关闭 conn 后等待握手 goroutine 退出的有界时长。conn 关闭后
// go-libvirt 的解除路径（listen 退出 → waitAndDisconnect → deregisterAll 解除挂起
// 调用）是纯内存操作、毫秒级完成；这里只给 Connect 的返回留上界，不让「等清理」
// 自己变成又一段无界等待。
const handshakeDrain = 2 * time.Second

// errLibvirtConnClosed 既有「连接已关闭」错误（Close 置空连接后由 call 复用同一文案）。
var errLibvirtConnClosed = errors.New("libvirt 连接已关闭")

// ErrConnBusy 连接「忙」而不是「坏」：调用在硬上界内**没轮到执行**（前面有合法地耗时
// 较长的 RPC 正持有 Conn 互斥——典型是快照创建/回滚、大 xml 定义）。此时**不得关闭
// 连接**：把长操作打断是误伤；探活侧据此跳过本轮（见 dynamicCompute.Probe），不误判
// 「连接中断」。与「执行中超时」（判假死、关连接）是两回事——决策 #362。
var ErrConnBusy = errors.New("libvirt 连接忙（前面的调用仍在执行）")

// libvirtSocketPath unix 形态的 socket 路径：URI query 参数 socket 优先，缺省
// /var/run/libvirt/libvirt-sock。与 go-libvirt 的 dialerForURI/defaultSocket 同一
// 规则 —— 必须同源，否则「产品拨的」与「go-libvirt 认为该拨的」不是同一个文件。
func libvirtSocketPath(u *url.URL) string {
	if s := u.Query().Get("socket"); s != "" {
		return s
	}
	return defaultLibvirtSocket
}

// libvirtTransport 按 go-libvirt dialerForURI 的同一规则推导传输形态：
// scheme 带「+」取后缀（qemu+tcp → tcp），无后缀但带 host 视为 tls，否则 unix。
func libvirtTransport(u *url.URL) string {
	if scheme := strings.SplitN(u.Scheme, "+", 2); len(scheme) > 1 {
		return scheme[1]
	}
	if u.Host != "" {
		return "tls"
	}
	return "unix"
}

// connectBounded 拨号 + go-libvirt 协议握手，全程受 ctx 上界约束。network/addr
// 参数化（unix 入口传 "unix"+socket 路径；单测在 Windows 上用 tcp 造假服务）。
//
// 成功时把**自持的原始 net.Conn** 一并返回（决策 #362）：后续单次 RPC 的硬上界
// （call）靠关闭它来中断挂起调用。失败路径行为与返回 conn 之前完全一致。
//
// 中断手段是「自持 conn + 超时关闭」而非 libvirt.Disconnect()——后者内部先发
// ConnectClose RPC，对假死守护进程同样会挂。已核实 go-libvirt 源码：conn 关闭后
// listen goroutine 的 pktlen 读到非临时错误退出 → listenAndRoute 关闭 disconnected
// → waitAndDisconnect 执行 removeAllStreams + deregisterAll，解除挂起的 getResponse
// （返回 ErrInterrupted），握手 goroutine 确定性返回（有单测守护不泄漏）。
func connectBounded(ctx context.Context, network, addr string, remote libvirt.ConnectURI) (*libvirt.Libvirt, net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, nil, err
	}
	lv := libvirt.New(conn)
	// 带缓冲（size 1）：即便走超时路径不收结果，握手 goroutine 发送后也能退出。
	errCh := make(chan error, 1)
	go func() { errCh <- lv.ConnectToURI(remote) }()
	select {
	case err := <-errCh:
		if err != nil {
			_ = conn.Close() // 握手失败：释放自持 conn（go-libvirt 内部已可能关过，重复关无害）
			return nil, nil, err
		}
		return lv, conn, nil
	case <-ctx.Done():
		_ = conn.Close()
		t := time.NewTimer(handshakeDrain)
		defer t.Stop()
		select {
		case <-errCh: // 顺手收掉握手结果，只丢弃
		case <-t.C:
		}
		return nil, nil, ctx.Err()
	}
}

// Conn libvirt 薄适配层：真实 RPC 调用集中于此，不含业务逻辑。
//
// 文件名以 `_libvirt.go` 结尾 → 被 check_coverage.sh 的 COVER_EXCLUDE 排除（沿用
// `_govpp.go` 约定），改由 nfvis-vm 上的集成测试（build tag integration）覆盖。
//
// 并发安全：内部串行化（libvirt RPC 连接非并发安全），且**每次 RPC 都有硬上界**
// （决策 #362）：全部调用统一经 call 包装，上界到期即关闭自持 raw conn 中断挂起调用。
type Conn struct {
	mu  sync.Mutex
	l   *libvirt.Libvirt
	uri string
	// raw 自持的原始连接（仅 unix 形态非 nil；非 unix 形态由 go-libvirt 自拨，
	// 产品不持有、无法中断）。只在 Connect 中写入，此后只读：interrupt 不持锁也能安全读取。
	raw net.Conn
}

// 编译期断言：Conn 满足 Provider 的 libvirt 能力接口。
var _ libvirtAPI = (*Conn)(nil)

// Connect 连接 libvirt。uri 为空时用 DefaultURI。
//
// 连接全程有界（决策 #349）：go-libvirt 的包级 ConnectToURI 不收 context，对
// 「socket 可连但守护进程不回应任何字节」的假死 libvirtd，会在 AuthList 认证握手
// 上无限阻塞 —— 把「底座假死」放大成「nfvisd 到不了 READY、被 systemd 90s 启动
// 超时反复重启」。unix 形态（产品唯一支持形态）由产品自拨并自持 conn，超时关 conn
// 即确定性中断；非 unix 形态不在产品支持范围，只能在外层有界等待（如实注明可能的
// 内部 goroutine 遗留）。成功路径行为与有界化之前完全一致。
func Connect(ctx context.Context, uri string) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uri == "" {
		uri = DefaultURI
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("libvirt URI %q 非法: %w", uri, err)
	}
	if libvirtTransport(u) == "unix" {
		lv, raw, cerr := connectBounded(ctx, "unix", libvirtSocketPath(u), libvirt.RemoteURI(u))
		if cerr != nil {
			if errors.Is(cerr, context.DeadlineExceeded) || errors.Is(cerr, context.Canceled) {
				return nil, fmt.Errorf("连接 libvirt %s 超时：libvirt 可能未就绪、假死或过载。"+
					"请查底座实况：systemctl status libvirtd、journalctl -u libvirtd、virsh -c %s list；"+
					"底座恢复后 systemctl restart nfvis 即恢复接入（%w）", uri, uri, cerr)
			}
			return nil, fmt.Errorf("连接 libvirt %s 失败: %w", uri, cerr)
		}
		return &Conn{l: lv, uri: uri, raw: raw}, nil
	}
	// 非 unix 形态：沿原包级 ConnectToURI 路径，仅在外层用同一 ctx 有界等待。超时时
	// 无法中断其内部拨号/握手（conn 由 go-libvirt 自拨，产品不持有，Conn.raw 为 nil ⇒
	// call 的硬上界在该形态下只能失败返回、无法真正中断），极端情况下可能遗留其内部
	// goroutine，直到 libvirt 侧自身超时 —— 该形态不在产品支持范围，仅作开发试验之用，
	// 如实注明即可（不做无法确定的清理）。
	errCh := make(chan error, 1)
	var lv *libvirt.Libvirt
	go func() {
		l, err := libvirt.ConnectToURI(u)
		if l != nil {
			lv = l
		}
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return nil, fmt.Errorf("连接 libvirt %s 失败: %w", uri, err)
		}
		return &Conn{l: lv, uri: uri}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("连接 libvirt %s 超时（%w）：该 URI 的传输形态不在产品支持范围，"+
			"连接可能遗留内部 goroutine，直到 libvirt 侧超时返回", uri, ctx.Err())
	}
}

// NewConnectedProvider 连接 libvirt 并装配生产 Provider（真实存储/seed 实现）。
// 调用方负责在退出时 Close 返回的 *Conn。
func NewConnectedProvider(ctx context.Context, cfg Config) (*Provider, *Conn, error) {
	if cfg.URI == "" {
		cfg.URI = DefaultURI
	}
	conn, err := Connect(ctx, cfg.URI)
	if err != nil {
		return nil, nil, err
	}
	return NewProvider(cfg, conn, newQemuStorage(), newCloudLocaldsSeed()), conn, nil
}

// URI 返回连接使用的 URI。
func (c *Conn) URI() string { return c.uri }

// Close 断开连接。
//
// 停机路径保持原样（决策 #362 明确不动）：libvirt.Disconnect() 内部先发
// ConnectClose RPC，对假死守护进程可能挂起——停机由 systemd 强杀兜底；中断挂起调用
// 用的是自持 raw conn 的原始关闭（interrupt），与这里无关。
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.l == nil {
		return nil
	}
	err := c.l.Disconnect()
	c.l = nil
	return err
}

// interrupt 关闭自持的原始连接，中断可能永久挂起的 RPC（决策 #362；与 #349 的
// 握手中断同一手段）。不持 c.mu 即可调用：上界到期时 RPC goroutine 正持有互斥并
// 阻塞在 getResponse 上，关闭 conn 才能让 go-libvirt 的 listen 退出 → 等待清理
// → deregisterAll 解除挂起调用并释放互斥（净效果 = 调用确定性返回、连接失效）。
// 幂等（net.Conn.Close 重复调用返回错误但无害）；raw 为 nil（非 unix 形态）时是空操作。
func (c *Conn) interrupt() {
	if c.raw != nil {
		_ = c.raw.Close()
	}
}

// call 在硬上界内执行一次 libvirt RPC（决策 #362）。
//
// 为什么需要它：go-libvirt 的单次 RPC 没有 per-call deadline（request → getResponse
// 裸等响应通道），对「socket 可连、零响应」的假死 libvirtd，仅靠调用方 ctx 加 deadline
// 是不够的——只有被调方尊重 ctx 才有界。这里把 fn 放进 goroutine（内部持 Conn 互斥，
// 保持既有串行语义），select 等结果或上界：
//
//   - 执行阶段（fn 已拿到互斥）到期 ⇒ 判连接假死：interrupt() 关闭自持 conn ⇒
//     go-libvirt 的 getResponse 经 deregisterAll 确定性返回 ErrInterrupted、goroutine
//     随之释放互斥（#349 已核实该解除路径），本函数返回包装超时的可辨识错误。下次调用
//     落在已关闭的连接上会快速失败，由常驻状态机（cmd/nfvisd/async_connect.go）判定
//     中断并复连。
//   - 排队阶段（还没轮到执行）到期 ⇒ 连接只是**忙**：返回 ErrConnBusy、**不关连接**
//     —— 前面可能有合法地耗时较长的 RPC（快照创建/回滚）正在执行，关连接即误杀。
//
// 无 deadline 的 ctx 套 defaultRPCTimeout（缺省硬上界，仅约束执行段）。
func (c *Conn) call(ctx context.Context, fn func(l *libvirt.Libvirt) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound := defaultRPCTimeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < bound {
			bound = left
		}
	}
	// 结果通道带缓冲（size 1）：上界路径即便不收结果，goroutine 发完也能退出。
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		close(started) // 已拿到互斥：此后阻塞即「执行中」，执行阶段的上界开始适用
		if c.l == nil {
			done <- errLibvirtConnClosed
			return
		}
		done <- fn(c.l)
	}()
	// 排队阶段：只受调用方 ctx 约束（连接忙 ≠ 连接坏，不关连接、不打断前面正在执行的
	// 合法长 RPC——审查修正：早先版本「上界一到无条件关连接」会误杀快照创建这类操作）。
	select {
	case err := <-done:
		return err
	case <-started:
	case <-ctx.Done():
		return fmt.Errorf("%w（%s 内未轮到执行）: %v", ErrConnBusy, bound.Round(time.Millisecond), ctx.Err())
	}
	// 执行阶段：fn 已开始；bound 到期即判假死并关连接中断（#349 同一中断手段）。
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		c.interrupt()
		// 包装 context.DeadlineExceeded 保持调用方 errors.Is 判超时的既有口径。
		return fmt.Errorf("libvirt 调用超时（执行中未在 %s 内返回，连接已关闭，将由常驻状态机自动复连）: %w",
			bound.Round(time.Millisecond), context.DeadlineExceeded)
	}
}

// Version 返回 libvirt 库版本，如 "12.0.0"（连接/版本探测）。
func (c *Conn) Version() (string, error) {
	var v uint64
	if err := c.call(context.Background(), func(l *libvirt.Libvirt) error {
		got, err := l.ConnectGetLibVersion()
		if err != nil {
			return fmt.Errorf("读取 libvirt 版本失败: %w", err)
		}
		v = got
		return nil
	}); err != nil {
		return "", err
	}
	return FormatLibVersion(v), nil
}

// HypervisorVersion 返回 hypervisor（QEMU）版本。
func (c *Conn) HypervisorVersion() (string, error) {
	var v uint64
	if err := c.call(context.Background(), func(l *libvirt.Libvirt) error {
		got, err := l.ConnectGetVersion()
		if err != nil {
			return fmt.Errorf("读取 hypervisor 版本失败: %w", err)
		}
		v = got
		return nil
	}); err != nil {
		return "", err
	}
	return FormatLibVersion(v), nil
}

// Define 定义（或按名重定义）domain。
func (c *Conn) Define(ctx context.Context, xml string) error {
	return c.call(ctx, func(l *libvirt.Libvirt) error {
		if _, err := l.DomainDefineXML(xml); err != nil {
			return fmt.Errorf("定义 domain 失败: %w", err)
		}
		return nil
	})
}

// Undefine 删除 domain 定义（连带快照元数据，避免「has snapshots」错误，FR-CMP-013）。
func (c *Conn) Undefine(ctx context.Context, name string) error {
	return c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			return fmt.Errorf("查找 domain %s 失败: %w", name, err)
		}
		if err := l.DomainUndefineFlags(dom, libvirt.DomainUndefineSnapshotsMetadata); err != nil {
			return fmt.Errorf("删除 domain %s 失败: %w", name, err)
		}
		return nil
	})
}

// State 返回 libvirt 原始状态；exists=false 表示域未定义（不作为错误）。
func (c *Conn) State(ctx context.Context, name string) (int, bool, error) {
	state, _, exists, err := c.StateReason(ctx, name)
	return state, exists, err
}

// StateReason 返回 libvirt 状态与 reason（reason 用于区分正常关机与被杀/崩溃）。
func (c *Conn) StateReason(ctx context.Context, name string) (int, int, bool, error) {
	// 输出经结构承载：上界路径不读它（goroutine 可能还没返回），避免与闭包的写并发。
	var out struct {
		state, reason int
		exists        bool
	}
	err := c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			if libvirt.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("查找 domain %s 失败: %w", name, err)
		}
		state, reason, err := l.DomainGetState(dom, 0)
		if err != nil {
			return fmt.Errorf("读取 domain %s 状态失败: %w", name, err)
		}
		out.state, out.reason, out.exists = int(state), int(reason), true
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return out.state, out.reason, out.exists, nil
}

// Start 启动域。
func (c *Conn) Start(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(l *libvirt.Libvirt, dom libvirt.Domain) error { return l.DomainCreate(dom) })
}

// Shutdown ACPI 优雅关机。
func (c *Conn) Shutdown(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(l *libvirt.Libvirt, dom libvirt.Domain) error { return l.DomainShutdown(dom) })
}

// Destroy 立即断电（超时强杀）。
func (c *Conn) Destroy(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(l *libvirt.Libvirt, dom libvirt.Domain) error { return l.DomainDestroy(dom) })
}

// Reboot ACPI 重启。
func (c *Conn) Reboot(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(l *libvirt.Libvirt, dom libvirt.Domain) error {
		return l.DomainReboot(dom, libvirt.DomainRebootDefault)
	})
}

// SetAutostart 设置域自启标志（libvirt 层面，独立于 nfvisd 存活）。
func (c *Conn) SetAutostart(ctx context.Context, name string, autostart bool) error {
	v := int32(0)
	if autostart {
		v = 1
	}
	return c.withDomain(ctx, name, func(l *libvirt.Libvirt, dom libvirt.Domain) error { return l.DomainSetAutostart(dom, v) })
}

// DumpXML 返回 libvirt 规范化后的 domain XML（与配置比对用，M4-1 验收）。
func (c *Conn) DumpXML(ctx context.Context, name string) (string, error) {
	var out string
	err := c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			return fmt.Errorf("查找 domain %s 失败: %w", name, err)
		}
		xml, err := l.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return fmt.Errorf("读取 domain %s XML 失败: %w", name, err)
		}
		out = xml
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func (c *Conn) withDomain(ctx context.Context, name string, fn func(l *libvirt.Libvirt, dom libvirt.Domain) error) error {
	return c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			if libvirt.IsNotFound(err) {
				return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
			}
			return fmt.Errorf("查找 domain %s 失败: %w", name, err)
		}
		if err := fn(l, dom); err != nil {
			return fmt.Errorf("操作 domain %s 失败: %w", name, err)
		}
		return nil
	})
}

// DumpDomainXML 返回域 XML（快照据 target dev 判定磁盘集合）。
func (c *Conn) DumpDomainXML(ctx context.Context, name string) (string, error) {
	return c.DumpXML(ctx, name)
}

// SnapshotCreate 创建域快照（XML 由 BuildSnapshotXML 生成，qcow2 内部快照）。
func (c *Conn) SnapshotCreate(ctx context.Context, name, snapshotXML string) error {
	return c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			if libvirt.IsNotFound(err) {
				return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
			}
			return fmt.Errorf("查找 domain %s: %w", name, err)
		}
		if _, err := l.DomainSnapshotCreateXML(dom, snapshotXML, 0); err != nil {
			return fmt.Errorf("创建 VM %s 快照: %w", name, err)
		}
		return nil
	})
}

// SnapshotList 列出域快照（名称/描述/创建时间）。
func (c *Conn) SnapshotList(ctx context.Context, name string) ([]SnapshotInfo, error) {
	var out []SnapshotInfo
	err := c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(name)
		if err != nil {
			if libvirt.IsNotFound(err) {
				return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
			}
			return fmt.Errorf("查找 domain %s: %w", name, err)
		}
		snaps, _, err := l.DomainListAllSnapshots(dom, 1, 0)
		if err != nil {
			return fmt.Errorf("列出 VM %s 快照: %w", name, err)
		}
		infos := make([]SnapshotInfo, 0, len(snaps))
		for _, s := range snaps {
			doc, err := l.DomainSnapshotGetXMLDesc(s, 0)
			if err != nil {
				return fmt.Errorf("读取快照 %s XML: %w", s.Name, err)
			}
			info, err := SnapshotInfoFromXML(doc)
			if err != nil {
				return err
			}
			if info.Name == "" {
				info.Name = s.Name
			}
			infos = append(infos, info)
		}
		out = infos
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotRevert 回滚到指定快照。
func (c *Conn) SnapshotRevert(ctx context.Context, name, snapshot string) error {
	return c.withSnapshot(ctx, name, snapshot, func(l *libvirt.Libvirt, s libvirt.DomainSnapshot) error {
		return l.DomainRevertToSnapshot(s, 0)
	})
}

// SnapshotDelete 删除指定快照（含其元数据）。
func (c *Conn) SnapshotDelete(ctx context.Context, name, snapshot string) error {
	return c.withSnapshot(ctx, name, snapshot, func(l *libvirt.Libvirt, s libvirt.DomainSnapshot) error {
		return l.DomainSnapshotDelete(s, 0)
	})
}

func (c *Conn) withSnapshot(ctx context.Context, domain, snapshot string, fn func(l *libvirt.Libvirt, s libvirt.DomainSnapshot) error) error {
	return c.call(ctx, func(l *libvirt.Libvirt) error {
		dom, err := l.DomainLookupByName(domain)
		if err != nil {
			if libvirt.IsNotFound(err) {
				return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, domain)
			}
			return fmt.Errorf("查找 domain %s: %w", domain, err)
		}
		snap, err := l.DomainSnapshotLookupByName(dom, snapshot, 0)
		if err != nil {
			return fmt.Errorf("查找 VM %s 快照 %s: %w", domain, snapshot, err)
		}
		if err := fn(l, snap); err != nil {
			return fmt.Errorf("操作 VM %s 快照 %s: %w", domain, snapshot, err)
		}
		return nil
	})
}
