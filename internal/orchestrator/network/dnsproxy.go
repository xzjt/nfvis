package network

// 数据面 DNS 代理（决策 #338，FR-NET-010）：域内 VNF/容器把 resolver 指向网关地址即可解析
// ——VPP 内置 dns 插件代为转发/缓存。上游是**域名服务**（不是宿主解析器）：
//
//   - 与 SystemConfig.DNSServers（`set dns server`，宿主 resolv/systemd-resolved）**两回事**，
//     本文件只碰 VPP 侧；不自动把宿主上游喂给 VPP（上游须在 VPP 的 FIB 内可达）。
//   - **非空即启用**：逐条 `dns name-server <ip> add` 后 `dns enable`；
//     **清空即禁用**：按登记 `dns name-server <ip> del` 全部后 `dns disable`——
//     避免「启用但无上游」的坏态。上游列表/条数上限与既有 `dns server` 同风格（不设上限）。
//
// 生命周期口径（与 DHCP relay/learn-limit 同族）：
//   - 随提交编排收敛：声明变化才下发（进程内登记比较，幂等）；
//   - **恢复重放必须含它**：VPP 重启后 dns 插件的 name-server 与 enable 状态一并消失，
//     不重放即静默丢代理——recovery.go 在 vpp 段按配置重放（独立记源，失败不阻塞其余对象）；
//   - `request vpp restart` 后由恢复收敛重新下发。
//
// 边界（决策 #338 如实登记）：只做转发/代理，静态记录（`dns cache add`）与按域划分留后续；
// 客户端 resolver 指向由操作者/DHCP 完成（option 6 下发属 relay/server 深化）。

import (
	"context"
	"fmt"
	"sync"
)

// DNSClient VPP dns 插件 binary API 的最小能力集（govpp 适配/单测假实现）。
type DNSClient interface {
	// NameServerAddDel 增删一条上游（add=false 按登记撤销）。地址 v4/v6 由实现按 IP 形态判定。
	NameServerAddDel(server string, add bool) error
	// EnableDisable 启用/禁用 dns 解析。
	EnableDisable(enable bool) error
	Close()
}

// DNSProxyProvider 数据面 DNS 代理（VPP dns 插件）编排。
type DNSProxyProvider struct {
	client func() (DNSClient, error)

	mu      sync.Mutex
	servers []string // 已下发的上游（按声明序）
	applied bool     // 是否已 dns enable
}

// NewDNSProxyProvider 以固定客户端构造（测试）。
func NewDNSProxyProvider(c DNSClient) *DNSProxyProvider {
	return &DNSProxyProvider{client: func() (DNSClient, error) { return c, nil }}
}

// NewDNSProxyProviderFunc 以客户端工厂构造（连接可重连）。
func NewDNSProxyProviderFunc(f func() (DNSClient, error)) *DNSProxyProvider {
	return &DNSProxyProvider{client: f}
}

// reset 清空进程内登记（恢复收敛前调用）：登记只反映「本进程下发过什么」，
// VPP 重启后 dns 配置已不在，按配置重放会重新下发并重建登记。
func (p *DNSProxyProvider) reset() {
	p.mu.Lock()
	p.servers = nil
	p.applied = false
	p.mu.Unlock()
}

// Sync 把数据面 DNS 代理声明收敛到 VPP（决策 #338）：
//   - 非空：差集增删上游（登记比较，幂等）+ 未启用时 `dns enable`；
//   - 空：按登记 del 全部上游后 `dns disable`（幂等；无登记且未启用即无事可做）。
func (p *DNSProxyProvider) Sync(ctx context.Context, servers []string) error {
	want := dedupeStrings(servers)

	p.mu.Lock()
	cur := append([]string(nil), p.servers...)
	applied := p.applied
	p.mu.Unlock()

	if len(want) == 0 {
		if !applied && len(cur) == 0 {
			return nil
		}
		c, err := p.client()
		if err != nil {
			return err
		}
		defer c.Close()
		for _, s := range cur {
			if err := c.NameServerAddDel(s, false); err != nil {
				return fmt.Errorf("撤销数据面 DNS 代理上游 %s: %w", s, err)
			}
		}
		if err := c.EnableDisable(false); err != nil {
			return fmt.Errorf("关闭数据面 DNS 代理: %w", err)
		}
		p.mu.Lock()
		p.servers = nil
		p.applied = false
		p.mu.Unlock()
		return nil
	}

	if applied && sameStringSet(cur, want) {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	// 先删登记里有、声明里没有的，再补声明里有、登记里没有的，最后确保启用。
	for _, s := range cur {
		if !containsString(want, s) {
			if err := c.NameServerAddDel(s, false); err != nil {
				return fmt.Errorf("撤销数据面 DNS 代理上游 %s: %w", s, err)
			}
		}
	}
	for _, s := range want {
		if !containsString(cur, s) {
			if err := c.NameServerAddDel(s, true); err != nil {
				return fmt.Errorf("下发数据面 DNS 代理上游 %s: %w", s, err)
			}
		}
	}
	if !applied {
		if err := c.EnableDisable(true); err != nil {
			return fmt.Errorf("启用数据面 DNS 代理: %w", err)
		}
	}
	p.mu.Lock()
	p.servers = want
	p.applied = true
	p.mu.Unlock()
	return nil
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, s := range a {
		if !containsString(b, s) {
			return false
		}
	}
	return true
}

func containsString(arr []string, s string) bool {
	for _, e := range arr {
		if e == s {
			return true
		}
	}
	return false
}
