package network

// M3-6：LLDP 编排（FR-NET-018）：全局参数、按接口开关、邻居表运行态。

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// ErrLldpIfaceUnsupported 底座拒绝在某接口上启用 LLDP（VPP 对 DPDK 物理口返回 Invalid value -7）。
//
// 用途（决策 #451）：全局启用扫描到这类口时**跳过并如实登记**（不整次回滚）；显式逐口声明
// 遇到它仍如实报错（底座限制，不隐藏）。判定用 errors.Is。
var ErrLldpIfaceUnsupported = errors.New(
	"当前数据面底座不支持在该接口上启用 LLDP（DPDK 物理口；vhost-user/BVI 等虚拟接口可用，或改用内核数据面）")

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
	enabled map[string]bool   // 已成功启用的口
	skipped map[string]string // 全局启用扫描时底座拒绝的口 → 原因（如实登记，读视图呈现）
	global  bool              // 最近一次全局开关声明（读视图 Enabled 用）
}

// LLDPInterfaceState 一个接口的 LLDP 运行态读数（决策 #451）：Converged=true＝已在发广告；
// Converged=false 且 Error 非空＝底座服务不了/未收敛，原因如实给出。
type LLDPInterfaceState struct {
	Name      string `json:"name"`
	Converged bool   `json:"converged"`
	Error     string `json:"error,omitempty"`
	MAC       string `json:"mac,omitempty"`
}

// LLDPState LLDP 运行态读数（读视图运行态块；与 DNS 代理同形，决策 #451）。
// Interface 列出「实际在发广告的口」（converged）与「被底座跳过/未收敛的口」（error）。
type LLDPState struct {
	Enabled   bool                 `json:"enabled"`
	Interface []LLDPInterfaceState `json:"interfaces"`
}

// NewLldpProvider 以固定客户端构造（测试）。
func NewLldpProvider(c LldpClient) *LldpProvider {
	return &LldpProvider{client: func() (LldpClient, error) { return c, nil }, enabled: map[string]bool{}}
}

// NewLldpProviderFunc 以客户端工厂构造（连接可重连）。
func NewLldpProviderFunc(f func() (LldpClient, error)) *LldpProvider {
	return &LldpProvider{client: f, enabled: map[string]bool{}}
}

// reset 清空进程内登记表（恢复收敛前调用，按配置全量重放接口开关）。
func (p *LldpProvider) reset() {
	p.mu.Lock()
	p.enabled = map[string]bool{}
	p.skipped = map[string]string{}
	p.mu.Unlock()
}

// State 运行态读数：已成功启用的口（Converged=true）+ 全局启用扫描时被底座跳过/未收敛的口
// （Converged=false，Error 给原因）。Enabled＝最近一次全局开关声明。
func (p *LldpProvider) State() LLDPState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := LLDPState{Enabled: p.global, Interface: make([]LLDPInterfaceState, 0, len(p.enabled)+len(p.skipped))}
	names := make([]string, 0, len(p.enabled)+len(p.skipped))
	for n := range p.enabled {
		names = append(names, n)
	}
	for n := range p.skipped {
		if !p.enabled[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if reason, ok := p.skipped[n]; ok && !p.enabled[n] {
			st.Interface = append(st.Interface, LLDPInterfaceState{Name: n, Converged: false, Error: reason})
			continue
		}
		st.Interface = append(st.Interface, LLDPInterfaceState{Name: n, Converged: true})
	}
	return st
}

// ApplyLLDP 收敛 LLDP：全局参数 + 按接口开关。
//
// 启用集推导（决策 #450/#451）：
//   - **全局开关是默认值**：`Enabled=true` ⇒ 对**全部已声明物理口**（declaredPorts）启用；
//     `Enabled=false`/nil ⇒ 默认关闭（但**不**主动去关从未启用过的口——只关本进程登记过、
//     且这次不再需要的口，避免对 DPDK 物理口做无谓的 set 触发底座限制）。
//   - **逐口声明覆盖默认**：`cfg.Interfaces` 里每条按 `enabled` 生效，**不受全局开关阻断**
//     （全局 false 也能用逐口声明单独启用某口）。
//   - 全局参数（间隔/TTL）只要有任一口需要启用就下发一次。
//   - **全局启用遇底座拒绝跳过**：全局扫描到的口（非逐口声明）若底座返回 -7
//     （ErrLldpIfaceUnsupported），**跳过并如实登记**、继续处理其它口，**不整次回滚**；
//     显式逐口声明遇同类拒绝仍如实报错（底座限制，不隐藏）。
func (p *LldpProvider) ApplyLLDP(ctx context.Context, cfg *model.LldpConfig, declaredPorts []string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	interval := uint32(0)
	if cfg != nil && cfg.AdvertisementInterval > 0 {
		interval = uint32(cfg.AdvertisementInterval)
	}
	global := cfg != nil && cfg.Enabled

	// calls：这次要下发的口 → 目标开关。全局开关是默认值（对全部已声明口），逐口声明覆盖之
	// 且**逐条都要下发**（含显式 false），本进程登记过、这次不再需要的口一律关掉。
	calls := map[string]bool{}
	explicit := map[string]bool{} // 逐口声明点名的口（这些口遇底座拒绝仍如实报错）
	if global {
		for _, name := range declaredPorts {
			if name != "" {
				calls[name] = true
			}
		}
	}
	if cfg != nil {
		for _, li := range cfg.Interfaces {
			if li.Interface == "" {
				continue
			}
			calls[li.Interface] = li.Enabled
			explicit[li.Interface] = true
		}
	}
	p.mu.Lock()
	for name := range p.enabled {
		if _, ok := calls[name]; !ok {
			calls[name] = false // 关闭已移除/全局关闭的接口
		}
	}
	p.mu.Unlock()

	// 任一目标为启用 ⇒ 下发一次全局参数（间隔/TTL 生效）。
	anyOn := false
	for _, on := range calls {
		if on {
			anyOn = true
			break
		}
	}
	if anyOn || global {
		if err := c.LldpConfig(interval, 0, ""); err != nil {
			return fmt.Errorf("LLDP 全局配置: %w", err)
		}
	}

	skipped := map[string]string{} // 全局启用扫描时底座拒绝的口 → 原因（如实登记）
	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names) // 确定性调用序（读视图/单测可复现）
	for _, ifname := range names {
		on := calls[ifname]
		if err := c.LldpSetInterface(ifname, on); err != nil {
			// 全局启用**扫描**到的口（非逐口声明）遇底座拒绝（VPP 对 DPDK 物理口返回 -7）：
			// 该口本底座服务不了 ⇒ 跳过并如实登记，**不整次回滚**（全局开关不该因一条底座限制
			// 就全盘失败）。显式逐口声明不在此列——仍如实报错（底座限制，不隐藏）。
			if on && !explicit[ifname] && errors.Is(err, ErrLldpIfaceUnsupported) {
				skipped[ifname] = err.Error()
				continue
			}
			return fmt.Errorf("LLDP 接口 %s 开关: %w", ifname, err)
		}
	}
	p.mu.Lock()
	on := map[string]bool{}
	for name, v := range calls {
		if v && skipped[name] == "" {
			on[name] = true
		}
	}
	p.enabled = on
	p.skipped = skipped
	p.global = global
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
//
// 仅适用于「文本型」标识（interface alias/name、chassis component、locally assigned）。
// MAC/网络地址型标识是二进制的，必须走 LldpIDBySubtype——真实交换机绝大多数用
// MAC 型 chassis-ID，直接当字符串输出会得到乱码（决策 #70）。
func lldpIDString(b []byte) string {
	return strings.TrimRight(string(b), "\x00")
}

// LldpIDBySubtype 按 subtype 解码 LLDP 标识（IEEE 802.1AB §8.5/§8.6）。
//
// 注意 chassis 与 port 的 subtype 编号不同：chassis MAC = 4、port MAC = 3，
// 故 macSubtype 由调用方按 CHASSIS_ID_SUBTYPE_MAC_ADDR / PORT_ID_SUBTYPE_MAC_ADDR 传入。
// 二进制型标识格式化为 MAC 或十六进制，避免把二进制字节直接当字符串输出。
//
// 导出共享（决策 #440）：内核数据面的自研 LLDP 收发代理复用本函数渲染邻居标识——
// 两种数据面共用**同一份**标识渲染（同 subtype 语义：chassis MAC = 4、port MAC = 3），
// 读视图不会各说各话。
func LldpIDBySubtype(subtype, macSubtype uint32, b []byte) string {
	raw := bytes.TrimRight(b, "\x00")
	if len(raw) == 0 {
		return ""
	}
	if subtype == macSubtype && len(raw) == 6 {
		return net.HardwareAddr(raw).String()
	}
	if isPrintableASCII(raw) {
		return string(raw)
	}
	return "0x" + hex.EncodeToString(raw)
}

// isPrintableASCII 判定字节串是否为可打印 ASCII（区分文本型与二进制型标识）。
func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
