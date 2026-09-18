package network

// 决策 #100（发现 #8）：物理口 → PCI 的绑定记录。
//
// 为什么必须落盘：物理口一旦交 vfio-pci/igb-uio，内核里**就没有 netdev 了**
// （`/sys/class/net/<ifname>` 消失），而 startup.conf 的 dpdk 段以 **PCI 地址**为键，
// 需要把 committed 里的口名解析成 PCI。绑定那一刻（`DPDKBinder.Bind` 已返回该 PCI）
// 是最后一次能拿到这个映射的时机，故在此落盘，供之后的每次生成回退使用。
//
// 不入 committed 配置：决策 #30 要求配置可移植（PCI 随槽位变化，不进配置）；
// 本文件属**本机运行态**，随机器走。
//
// 写失败不算致命：绑定动作本身已经发生，记录失败只影响后续的 startup.conf 生成，
// 故调用方按告警处理（见 cmd/nfvisd 的装配）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// DefaultBindingsPath DPDK 绑定记录缺省路径。
const DefaultBindingsPath = "/var/lib/nfvis/dpdk-bindings.json"

// Bindings 口名 → PCI 地址的持久记录（并发安全；首次访问时读盘）。
type Bindings struct {
	Path      string
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error

	mu     sync.Mutex
	m      map[string]string
	loaded bool
}

// NewBindings 构造（path 空取缺省）。
func NewBindings(path string) *Bindings {
	if strings.TrimSpace(path) == "" {
		path = DefaultBindingsPath
	}
	return &Bindings{Path: path}
}

func (b *Bindings) read(path string) ([]byte, error) {
	if b.ReadFile != nil {
		return b.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (b *Bindings) write(path string, data []byte) error {
	if b.WriteFile != nil {
		return b.WriteFile(path, data)
	}
	return os.WriteFile(path, data, 0o644)
}

// load 惰性读盘（文件缺失视为空记录；损坏则报错，不静默丢弃既有映射）。
func (b *Bindings) load() error {
	if b.loaded {
		return nil
	}
	b.loaded = true
	b.m = map[string]string{}
	raw, err := b.read(b.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &b.m)
}

// flush 落盘（有变更时调用）。
func (b *Bindings) flush() error {
	raw, err := json.MarshalIndent(b.m, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(b.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return b.write(b.Path, append(raw, '\n'))
}

// Get 查询口名对应的 PCI。
func (b *Bindings) Get(ifname string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return "", false
	}
	pci, ok := b.m[strings.TrimSpace(ifname)]
	return pci, ok && pci != ""
}

// Set 记录口名 → PCI（绑定成功后调用）。
func (b *Bindings) Set(ifname, pci string) error {
	ifname, pci = strings.TrimSpace(ifname), strings.TrimSpace(pci)
	if ifname == "" || pci == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return err
	}
	if b.m[ifname] == pci {
		return nil
	}
	b.m[ifname] = pci
	return b.flush()
}

// DeleteByPCI 按 PCI 删除记录（解绑成功后调用）。
//
// 按 PCI 而非口名：解绑时操作者给的多半是 **PCI 地址**（netdev 已消失，只能按 PCI 定位），
// 而记录是以口名为键的。
func (b *Bindings) DeleteByPCI(pci string) error {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return err
	}
	changed := false
	for name, p := range b.m {
		if strings.EqualFold(p, pci) {
			delete(b.m, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return b.flush()
}

// All 返回记录快照（诊断与「掉口」告警用）。
func (b *Bindings) All() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return nil
	}
	out := make(map[string]string, len(b.m))
	for k, v := range b.m {
		out[k] = v
	}
	return out
}

// devStanzaRe 匹配 startup.conf 里的一条 dpdk dev 声明（产品生成或手册里带外手写都适用）：
//
//	dev <pci> {            ← 产品生成：多行
//	  name <ifname>
//	}
//	dev 0000:0b:00.0 { name ens192 }   ← 手册写法：压一行
var devStanzaRe = regexp.MustCompile(`(?s)dev\s+(\S+)\s*\{([^}]*)\}`)
var nameRe = regexp.MustCompile(`(?m)\bname\s+(\S+)`)

// ImportStartupConf 把当前部署的 startup.conf 里的 `dev <pci> { name <口> }` 映射并入记录。
//
// 这是**迁移**用的：存量安装按旧手册带外手写了 startup.conf，这些口在内核里已无 netdev，
// 记录里又没有它们的映射，产品就永远解析不到 PCI。启动时导入一次即可让产品接手维护，
// 无需中断流量重绑。文件缺失返回 (0, nil)。
func (b *Bindings) ImportStartupConf(path string) (int, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultStartupPath
	}
	raw, err := b.read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	text := stripConfComments(string(raw))
	n := 0
	for _, m := range devStanzaRe.FindAllStringSubmatch(text, -1) {
		pci := strings.TrimSpace(m[1])
		if !IsPCIAddr(pci) {
			continue // dev default { ... } 之类不是设备声明
		}
		nm := nameRe.FindStringSubmatch(m[2])
		if nm == nil {
			continue
		}
		ifname := strings.TrimSpace(nm[1])
		if ifname == "" {
			continue
		}
		if b.setIfAbsent(ifname, normPCI(pci)) {
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	b.mu.Lock()
	err = b.flush()
	b.mu.Unlock()
	return n, err
}

// setIfAbsent 记录映射但**不落盘**（由调用方统一 flush）；返回是否新增。
func (b *Bindings) setIfAbsent(ifname, pci string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return false
	}
	if _, ok := b.m[ifname]; ok {
		return false
	}
	b.m[ifname] = pci
	return true
}

// stripConfComments 去掉 startup.conf 的行注释（`#` 起）。
func stripConfComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if idx := strings.Index(l, "#"); idx >= 0 {
			lines[i] = l[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// PCIResolverWithBindings 返回「先系统事实、后绑定记录」的解析器。
//
// 顺序有意如此：netdev 还在时 sysfs 是**活的事实**（记录可能因换槽位而过期）；
// netdev 已消失恰恰说明该口确已交 DPDK，此时记录是唯一来源。
func PCIResolverWithBindings(primary PCIResolver, rec *Bindings) PCIResolver {
	return func(ifname string) (string, error) {
		pci, err := primary(ifname)
		if err == nil && strings.TrimSpace(pci) != "" {
			return pci, nil
		}
		if rec != nil {
			if pci, ok := rec.Get(ifname); ok {
				return pci, nil
			}
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("接口 %s 的 PCI 地址未知（内核中已无该网卡，且它不在 DPDK 绑定记录中）："+
			"请确认该口已由 DPDK 接管，或用 request interfaces <PCI地址> 指定", ifname)
	}
}
