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
//
// 目录版本 2（v3.0.30 四套件复跑的死锁）：旧文件是**单表** `{<口名或 PCI>: <PCI>}`——
// 按 PCI 地址接管（`request interfaces 0000:0b:00.0 bind-dpdk`）时它只记下 `PCI → PCI`，
// 「口名 → PCI」这一维**在绑定时就丢了**。此后口在内核里没有 netdev、记录里又没有口名，
// `vpp dpdk dev <口名>` 永远解析不到 PCI 而被跳过（口不在数据面），而告警给的「重新
// bind-dpdk」在无 netdev 时也走不通——操作者被卡死，只能清空整族声明或手改配置库。
//
// 故记录改为**两个方向各自成表**：
//
//	ifname_to_pci  口名 → PCI   （生成 startup.conf 时的回退来源，核心）
//	pci_to_ifname  PCI → 口名   （按 PCI 操作时的反查，如管理口守卫/解绑）
//	pci_only       已知已接管但口名未知的 PCI（旧格式遗留，不猜名字，只如实保留）
//
// 旧格式仍可读（只有 `{k:v}` 平表）：值是 PCI、键能归一成 BDF 的条目进 pci_only
// （口名未知），其余进 ifname_to_pci。**不报错、不编造口名**，向后兼容。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// DefaultBindingsPath DPDK 绑定记录缺省路径。
const DefaultBindingsPath = "/var/lib/nfvis/dpdk-bindings.json"

// bindingsFormatV2 当前落盘格式版本（1/缺省 = 旧平表）。
const bindingsFormatV2 = 2

// bindingsDoc 落盘结构（v2）。字段名刻意用蛇形，便于运维直接读。
type bindingsDoc struct {
	Format      int               `json:"format"`
	IfnameToPCI map[string]string `json:"ifname_to_pci"`
	PCIToIfname map[string]string `json:"pci_to_ifname"`
	// PCIOnly 已知已接管、但口名未知的 PCI（旧格式遗留或按 PCI 接管且内核已无 netdev）。
	// 只作诊断/回读保留，**不**用来猜口名。
	PCIOnly []string `json:"pci_only,omitempty"`
}

// Bindings 口名 ↔ PCI 地址的持久记录（并发安全；首次访问时读盘）。
type Bindings struct {
	Path      string
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error

	mu        sync.Mutex
	nameToPCI map[string]string // 口名 → PCI
	pciToName map[string]string // 归一 PCI（小写定宽） → 口名
	pciOnly   map[string]bool   // 归一 PCI → 已知已接管但口名未知
	loaded    bool
}

// normBindKey PCI 地址的**索引键**：归一（补域前缀）后转小写。
//
// 大小写不敏感是既有行为（`IfaceOfPCI("0000:1A:00.0")` 要能命中 `0000:1a:00.0`），
// 而 sysfs 目录名恒小写，故索引统一小写。
func normBindKey(pci string) string { return strings.ToLower(normPCI(strings.TrimSpace(pci))) }

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
	b.resetLocked()
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
	return b.decodeLocked(raw)
}

// resetLocked 清空内存表（只在 load 时调用）。
func (b *Bindings) resetLocked() {
	b.nameToPCI = map[string]string{}
	b.pciToName = map[string]string{}
	b.pciOnly = map[string]bool{}
}

// decodeLocked 解析落盘内容：先按 v2 试，不是 v2 就按旧平表读（向后兼容）。
func (b *Bindings) decodeLocked(raw []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	if f, ok := probe["format"]; ok {
		var ver int
		if json.Unmarshal(f, &ver) == nil && ver >= bindingsFormatV2 {
			var doc bindingsDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return err
			}
			for name, pci := range doc.IfnameToPCI {
				b.recordLocked(name, pci)
			}
			// 正向表是**唯一真源**；反向表只是索引的落盘副本，读回时按正向表重建，
			// 不从它反推正向值（否则会因归一化把原始取值改写）。
			for _, pci := range doc.PCIOnly {
				b.addPCISeenLocked(pci)
			}
			return nil
		}
	}
	// 旧格式：扁平 {<口名或 PCI>: <PCI>}。键能归一成 BDF 的条目**口名未知**
	// （按 PCI 地址绑定的形态：只有 PCI→PCI），其余才是真正的口名映射。
	var legacy map[string]string
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	for k, v := range legacy {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if IsPCIAddr(k) {
			b.addPCISeenLocked(v)
			continue
		}
		b.recordLocked(k, v)
	}
	return nil
}

// recordLocked 记一条「口名 → PCI」（两侧索引同步；BDF 形态的"口名"不当作名字）。
func (b *Bindings) recordLocked(ifname, pci string) {
	ifname, pci = strings.TrimSpace(ifname), strings.TrimSpace(pci)
	if ifname == "" || pci == "" {
		return
	}
	if IsPCIAddr(ifname) {
		// 按 PCI 地址接管且内核已无 netdev：只知道设备，不知道口名——不编造。
		b.addPCISeenLocked(pci)
		return
	}
	if old, ok := b.nameToPCI[ifname]; ok && !strings.EqualFold(old, pci) {
		// 同一口换了设备（换槽位/换卡）：旧的反向索引指向它时要一并摘掉，否则按 PCI 反查会答错口名。
		if n, ok := b.pciToName[normBindKey(old)]; ok && n == ifname {
			delete(b.pciToName, normBindKey(old))
		}
	}
	b.nameToPCI[ifname] = pci
	b.pciToName[normBindKey(pci)] = ifname
	delete(b.pciOnly, normBindKey(pci))
}

// addPCISeenLocked 记「该 PCI 已被接管」，但口名未知（有名字时以名字为准，不覆盖）。
func (b *Bindings) addPCISeenLocked(pci string) {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return
	}
	norm := normBindKey(pci)
	if _, ok := b.pciToName[norm]; ok {
		return
	}
	if b.pciOnly[norm] {
		return
	}
	b.pciOnly[norm] = true
}

// flush 落盘（有变更时调用）。
func (b *Bindings) flush() error {
	doc := bindingsDoc{
		Format:      bindingsFormatV2,
		IfnameToPCI: make(map[string]string, len(b.nameToPCI)),
		PCIToIfname: make(map[string]string, len(b.pciToName)),
	}
	for k, v := range b.nameToPCI {
		doc.IfnameToPCI[k] = v
	}
	for k, v := range b.pciToName {
		doc.PCIToIfname[k] = v
	}
	for pci := range b.pciOnly {
		doc.PCIOnly = append(doc.PCIOnly, pci)
	}
	sort.Strings(doc.PCIOnly)
	raw, err := json.MarshalIndent(doc, "", "  ")
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
//
// 兼容旧行为：入参本身就是**已知的已接管 PCI** 时返回它自己（旧平表里那条 PCI→PCI
// 记录的含义）；其余情况一律以「口名 → PCI」为准，不猜。
func (b *Bindings) Get(ifname string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return "", false
	}
	k := strings.TrimSpace(ifname)
	if pci, ok := b.nameToPCI[k]; ok && pci != "" {
		return pci, true
	}
	if IsPCIAddr(k) && b.pciOnly[normBindKey(k)] {
		return k, true
	}
	return "", false
}

// NameOfPCI 按 PCI 反查口名（口名未知时 ok=false，不编造）。
func (b *Bindings) NameOfPCI(pci string) (string, bool) {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return "", false
	}
	name, ok := b.pciToName[normBindKey(pci)]
	return name, ok && name != ""
}

// Set 记录口名 → PCI（绑定成功后调用）。
//
// ifname 是 **PCI 地址**时（按 PCI 接管的形态）只登记「该设备已被接管」，口名未知——
// 口名由 `DPDKBinder.Bind` 在**绑定前**从内核读出来另记一条（见 learnBindName）。
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
	before := b.signatureLocked()
	b.recordLocked(ifname, pci)
	if b.signatureLocked() == before {
		return nil
	}
	return b.flush()
}

// signatureLocked 记录内容指纹（判「有没有变化」，避免每次 Set 都重写文件）。
func (b *Bindings) signatureLocked() string {
	var sb strings.Builder
	names := make([]string, 0, len(b.nameToPCI))
	for k := range b.nameToPCI {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(b.nameToPCI[k])
		sb.WriteByte(';')
	}
	only := make([]string, 0, len(b.pciOnly))
	for k := range b.pciOnly {
		only = append(only, k)
	}
	sort.Strings(only)
	sb.WriteString("|")
	sb.WriteString(strings.Join(only, ","))
	return sb.String()
}

// DeleteByPCI 按 PCI 删除记录（解绑成功后调用）。
//
// 按 PCI 而非口名：解绑时操作者给的多半是 **PCI 地址**（netdev 已消失，只能按 PCI 定位），
// 而记录是以口名为键的。两侧索引与该设备的 pci_only 条目一并清掉。
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
	norm := normBindKey(pci)
	changed := false
	for name, p := range b.nameToPCI {
		if strings.EqualFold(p, pci) || normBindKey(p) == norm {
			delete(b.nameToPCI, name)
			changed = true
		}
	}
	if _, ok := b.pciToName[norm]; ok {
		delete(b.pciToName, norm)
		changed = true
	}
	if b.pciOnly[norm] {
		delete(b.pciOnly, norm)
		changed = true
	}
	if !changed {
		return nil
	}
	return b.flush()
}

// All 返回**口名 → PCI** 快照（诊断与「掉口」告警用）。
//
// 只含口名可解的条目：pci_only 里的设备口名未知，把它当「口」报给操作者只会得到
// 一句无法照做的提示（`set vpp dpdk dev 0000:0b:00.0` 过不了接口名校验）。
func (b *Bindings) All() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return nil
	}
	out := make(map[string]string, len(b.nameToPCI))
	for k, v := range b.nameToPCI {
		out[k] = v
	}
	return out
}

// UnnamedPCIs 返回「已知已接管但口名未知」的 PCI（归一地址，已排序）。
func (b *Bindings) UnnamedPCIs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return nil
	}
	out := make([]string, 0, len(b.pciOnly))
	for pci := range b.pciOnly {
		out = append(out, pci)
	}
	sort.Strings(out)
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return 0, err
	}
	before := b.signatureLocked()
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
		if ifname == "" || IsPCIAddr(ifname) {
			continue
		}
		if _, ok := b.nameToPCI[ifname]; ok {
			continue
		}
		b.recordLocked(ifname, normPCI(pci))
		n++
	}
	if n == 0 || b.signatureLocked() == before {
		return 0, nil
	}
	return n, b.flush()
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
