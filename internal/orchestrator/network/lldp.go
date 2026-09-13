package network

// M3-6：LLDP 编排（FR-NET-018）：全局参数、按接口开关、邻居表运行态。

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// LldpNeighbor 一条 LLDP 邻居（运行态）。
type LldpNeighbor struct {
	Interface string  `json:"interface"`
	ChassisID string  `json:"chassis_id"`
	PortID    string  `json:"port_id"`
	TTL       int     `json:"ttl"`
	LastHeard float64 `json:"last_heard"`
}

// LldpClient VPP lldp binary API 的最小能力集。
type LldpClient interface {
	LldpConfig(txInterval, txHold uint32, systemName string) error
	LldpSetInterface(ifname string, enable bool) error
	LldpNeighbors() ([]LldpNeighbor, error)
	Close()
}

// LldpProvider LLDP 编排。
type LldpProvider struct {
	client func() (LldpClient, error)

	mu      sync.Mutex
	enabled map[string]bool // 已按接口启停
}

// NewLldpProvider 以固定客户端构造（测试）。
func NewLldpProvider(c LldpClient) *LldpProvider {
	return &LldpProvider{client: func() (LldpClient, error) { return c, nil }, enabled: map[string]bool{}}
}

// NewLldpProviderFunc 以客户端工厂构造（连接可重连）。
func NewLldpProviderFunc(f func() (LldpClient, error)) *LldpProvider {
	return &LldpProvider{client: f, enabled: map[string]bool{}}
}

// ApplyLLDP 收敛 LLDP：全局参数 + 按接口开关；Enabled=false 或 nil 时全部关闭。
func (p *LldpProvider) ApplyLLDP(ctx context.Context, cfg *model.LldpConfig) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	interval := uint32(0)
	if cfg != nil && cfg.AdvertisementInterval > 0 {
		interval = uint32(cfg.AdvertisementInterval)
	}
	if cfg != nil && cfg.Enabled {
		if err := c.LldpConfig(interval, 0, ""); err != nil {
			return fmt.Errorf("LLDP 全局配置: %w", err)
		}
	}

	desired := map[string]bool{}
	if cfg != nil && cfg.Enabled {
		for _, li := range cfg.Interfaces {
			desired[li.Interface] = li.Enabled
		}
	}
	p.mu.Lock()
	for ifname := range p.enabled {
		if _, ok := desired[ifname]; !ok {
			desired[ifname] = false // 关闭已移除/全局关闭的接口
		}
	}
	p.mu.Unlock()

	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	for _, ifname := range names {
		if err := c.LldpSetInterface(ifname, desired[ifname]); err != nil {
			return fmt.Errorf("LLDP 接口 %s 开关: %w", ifname, err)
		}
	}
	p.mu.Lock()
	for ifname, on := range desired {
		if on {
			p.enabled[ifname] = true
		} else {
			delete(p.enabled, ifname)
		}
	}
	p.mu.Unlock()
	return nil
}

// Neighbors 返回 LLDP 邻居表（运行态，FR-NET-018）。
func (p *LldpProvider) Neighbors(ctx context.Context) ([]LldpNeighbor, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LldpNeighbors()
}

// lldpIDString 解码 LLDP 标识字节串（去尾部 NUL）。
func lldpIDString(b []byte) string {
	return strings.TrimRight(string(b), "\x00")
}
