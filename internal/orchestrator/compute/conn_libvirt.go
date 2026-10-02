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

// handshakeDrain 超时关闭 conn 后等待握手 goroutine 退出的有界时长。conn 关闭后
// go-libvirt 的解除路径（listen 退出 → waitAndDisconnect → deregisterAll 解除挂起
// 调用）是纯内存操作、毫秒级完成；这里只给 Connect 的返回留上界，不让「等清理」
// 自己变成又一段无界等待。
const handshakeDrain = 2 * time.Second

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
// 中断手段是「自持 conn + 超时关闭」而非 libvirt.Disconnect()——后者内部先发
// ConnectClose RPC，对假死守护进程同样会挂。已核实 go-libvirt 源码：conn 关闭后
// listen goroutine 的 pktlen 读到非临时错误退出 → listenAndRoute 关闭 disconnected
// → waitAndDisconnect 执行 removeAllStreams + deregisterAll，解除挂起的 getResponse
// （返回 ErrInterrupted），握手 goroutine 确定性返回（有单测守护不泄漏）。
func connectBounded(ctx context.Context, network, addr string, remote libvirt.ConnectURI) (*libvirt.Libvirt, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	lv := libvirt.New(conn)
	// 带缓冲（size 1）：即便走超时路径不收结果，握手 goroutine 发送后也能退出。
	errCh := make(chan error, 1)
	go func() { errCh <- lv.ConnectToURI(remote) }()
	select {
	case err := <-errCh:
		if err != nil {
			_ = conn.Close() // 握手失败：释放自持 conn（go-libvirt 内部已可能关过，重复关无害）
			return nil, err
		}
		return lv, nil
	case <-ctx.Done():
		_ = conn.Close()
		t := time.NewTimer(handshakeDrain)
		defer t.Stop()
		select {
		case <-errCh: // 顺手收掉握手结果，只丢弃
		case <-t.C:
		}
		return nil, ctx.Err()
	}
}

// Conn libvirt 薄适配层：真实 RPC 调用集中于此，不含业务逻辑。
//
// 文件名以 `_libvirt.go` 结尾 → 被 check_coverage.sh 的 COVER_EXCLUDE 排除（沿用
// `_govpp.go` 约定），改由 nfvis-vm 上的集成测试（build tag integration）覆盖。
//
// 并发安全：内部串行化（libvirt RPC 连接非并发安全）。
type Conn struct {
	mu  sync.Mutex
	l   *libvirt.Libvirt
	uri string
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
		lv, cerr := connectBounded(ctx, "unix", libvirtSocketPath(u), libvirt.RemoteURI(u))
		if cerr != nil {
			if errors.Is(cerr, context.DeadlineExceeded) || errors.Is(cerr, context.Canceled) {
				return nil, fmt.Errorf("连接 libvirt %s 超时：libvirt 可能未就绪、假死或过载。"+
					"请查底座实况：systemctl status libvirtd、journalctl -u libvirtd、virsh -c %s list；"+
					"底座恢复后 systemctl restart nfvis 即恢复接入（%w）", uri, uri, cerr)
			}
			return nil, fmt.Errorf("连接 libvirt %s 失败: %w", uri, cerr)
		}
		return &Conn{l: lv, uri: uri}, nil
	}
	// 非 unix 形态：沿原包级 ConnectToURI 路径，仅在外层用同一 ctx 有界等待。超时时
	// 无法中断其内部拨号/握手（conn 由 go-libvirt 自拨，产品不持有），极端情况下可能
	// 遗留其内部 goroutine，直到 libvirt 侧自身超时 —— 该形态不在产品支持范围，仅作
	// 开发试验之用，如实注明即可（不做无法确定的清理）。
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

// Version 返回 libvirt 库版本，如 "12.0.0"（连接/版本探测）。
func (c *Conn) Version() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.l == nil {
		return "", fmt.Errorf("libvirt 连接已关闭")
	}
	v, err := c.l.ConnectGetLibVersion()
	if err != nil {
		return "", fmt.Errorf("读取 libvirt 版本失败: %w", err)
	}
	return FormatLibVersion(v), nil
}

// HypervisorVersion 返回 hypervisor（QEMU）版本。
func (c *Conn) HypervisorVersion() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.l == nil {
		return "", fmt.Errorf("libvirt 连接已关闭")
	}
	v, err := c.l.ConnectGetVersion()
	if err != nil {
		return "", fmt.Errorf("读取 hypervisor 版本失败: %w", err)
	}
	return FormatLibVersion(v), nil
}

// Define 定义（或按名重定义）domain。
func (c *Conn) Define(ctx context.Context, xml string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.l.DomainDefineXML(xml); err != nil {
		return fmt.Errorf("定义 domain 失败: %w", err)
	}
	return nil
}

// Undefine 删除 domain 定义（连带快照元数据，避免「has snapshots」错误，FR-CMP-013）。
func (c *Conn) Undefine(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		return fmt.Errorf("查找 domain %s 失败: %w", name, err)
	}
	if err := c.l.DomainUndefineFlags(dom, libvirt.DomainUndefineSnapshotsMetadata); err != nil {
		return fmt.Errorf("删除 domain %s 失败: %w", name, err)
	}
	return nil
}

// State 返回 libvirt 原始状态；exists=false 表示域未定义（不作为错误）。
func (c *Conn) State(ctx context.Context, name string) (int, bool, error) {
	state, _, exists, err := c.StateReason(ctx, name)
	return state, exists, err
}

// StateReason 返回 libvirt 状态与 reason（reason 用于区分正常关机与被杀/崩溃）。
func (c *Conn) StateReason(ctx context.Context, name string) (int, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("查找 domain %s 失败: %w", name, err)
	}
	state, reason, err := c.l.DomainGetState(dom, 0)
	if err != nil {
		return 0, 0, false, fmt.Errorf("读取 domain %s 状态失败: %w", name, err)
	}
	return int(state), int(reason), true, nil
}

// Start 启动域。
func (c *Conn) Start(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(dom libvirt.Domain) error { return c.l.DomainCreate(dom) })
}

// Shutdown ACPI 优雅关机。
func (c *Conn) Shutdown(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(dom libvirt.Domain) error { return c.l.DomainShutdown(dom) })
}

// Destroy 立即断电（超时强杀）。
func (c *Conn) Destroy(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(dom libvirt.Domain) error { return c.l.DomainDestroy(dom) })
}

// Reboot ACPI 重启。
func (c *Conn) Reboot(ctx context.Context, name string) error {
	return c.withDomain(ctx, name, func(dom libvirt.Domain) error {
		return c.l.DomainReboot(dom, libvirt.DomainRebootDefault)
	})
}

// SetAutostart 设置域自启标志（libvirt 层面，独立于 nfvisd 存活）。
func (c *Conn) SetAutostart(ctx context.Context, name string, autostart bool) error {
	v := int32(0)
	if autostart {
		v = 1
	}
	return c.withDomain(ctx, name, func(dom libvirt.Domain) error { return c.l.DomainSetAutostart(dom, v) })
}

// DumpXML 返回 libvirt 规范化后的 domain XML（与配置比对用，M4-1 验收）。
func (c *Conn) DumpXML(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		return "", fmt.Errorf("查找 domain %s 失败: %w", name, err)
	}
	xml, err := c.l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return "", fmt.Errorf("读取 domain %s XML 失败: %w", name, err)
	}
	return xml, nil
}

func (c *Conn) withDomain(ctx context.Context, name string, fn func(libvirt.Domain) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
		}
		return fmt.Errorf("查找 domain %s 失败: %w", name, err)
	}
	if err := fn(dom); err != nil {
		return fmt.Errorf("操作 domain %s 失败: %w", name, err)
	}
	return nil
}

// DumpDomainXML 返回域 XML（快照据 target dev 判定磁盘集合）。
func (c *Conn) DumpDomainXML(ctx context.Context, name string) (string, error) {
	return c.DumpXML(ctx, name)
}

// SnapshotCreate 创建域快照（XML 由 BuildSnapshotXML 生成，qcow2 内部快照）。
func (c *Conn) SnapshotCreate(ctx context.Context, name, snapshotXML string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
		}
		return fmt.Errorf("查找 domain %s: %w", name, err)
	}
	if _, err := c.l.DomainSnapshotCreateXML(dom, snapshotXML, 0); err != nil {
		return fmt.Errorf("创建 VM %s 快照: %w", name, err)
	}
	return nil
}

// SnapshotList 列出域快照（名称/描述/创建时间）。
func (c *Conn) SnapshotList(ctx context.Context, name string) ([]SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
		}
		return nil, fmt.Errorf("查找 domain %s: %w", name, err)
	}
	snaps, _, err := c.l.DomainListAllSnapshots(dom, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("列出 VM %s 快照: %w", name, err)
	}
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		doc, err := c.l.DomainSnapshotGetXMLDesc(s, 0)
		if err != nil {
			return nil, fmt.Errorf("读取快照 %s XML: %w", s.Name, err)
		}
		info, err := SnapshotInfoFromXML(doc)
		if err != nil {
			return nil, err
		}
		if info.Name == "" {
			info.Name = s.Name
		}
		out = append(out, info)
	}
	return out, nil
}

// SnapshotRevert 回滚到指定快照。
func (c *Conn) SnapshotRevert(ctx context.Context, name, snapshot string) error {
	return c.withSnapshot(ctx, name, snapshot, func(s libvirt.DomainSnapshot) error {
		return c.l.DomainRevertToSnapshot(s, 0)
	})
}

// SnapshotDelete 删除指定快照（含其元数据）。
func (c *Conn) SnapshotDelete(ctx context.Context, name, snapshot string) error {
	return c.withSnapshot(ctx, name, snapshot, func(s libvirt.DomainSnapshot) error {
		return c.l.DomainSnapshotDelete(s, 0)
	})
}

func (c *Conn) withSnapshot(ctx context.Context, domain, snapshot string, fn func(libvirt.DomainSnapshot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainLookupByName(domain)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, domain)
		}
		return fmt.Errorf("查找 domain %s: %w", domain, err)
	}
	snap, err := c.l.DomainSnapshotLookupByName(dom, snapshot, 0)
	if err != nil {
		return fmt.Errorf("查找 VM %s 快照 %s: %w", domain, snapshot, err)
	}
	if err := fn(snap); err != nil {
		return fmt.Errorf("操作 VM %s 快照 %s: %w", domain, snapshot, err)
	}
	return nil
}
