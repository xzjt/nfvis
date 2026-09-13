package compute

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/digitalocean/go-libvirt"
)

// DefaultURI libvirt 系统域连接 URI（FR-CMP-010：qemu:///system）。
const DefaultURI = "qemu:///system"

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

// Connect 连接 libvirt。uri 为空时用 DefaultURI。
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
	l, err := libvirt.ConnectToURI(u)
	if err != nil {
		return nil, fmt.Errorf("连接 libvirt %s 失败: %w", uri, err)
	}
	return &Conn{l: l, uri: uri}, nil
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

// Define 定义（或按名重定义）domain，返回定义后的 domain。
func (c *Conn) Define(ctx context.Context, xml string) (libvirt.Domain, error) {
	if err := ctx.Err(); err != nil {
		return libvirt.Domain{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dom, err := c.l.DomainDefineXML(xml)
	if err != nil {
		return libvirt.Domain{}, fmt.Errorf("定义 domain 失败: %w", err)
	}
	return dom, nil
}

// Undefine 按名删除 domain 定义。
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
	if err := c.l.DomainUndefine(dom); err != nil {
		return fmt.Errorf("删除 domain %s 失败: %w", name, err)
	}
	return nil
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
