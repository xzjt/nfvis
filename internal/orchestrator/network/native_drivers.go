package network

// 决策 #464：PCI → **原生驱动**的持久记录。
//
// 与决策 #100 的 `Bindings`（口名 → PCI）同族、同一套文件约定，但**语义不同**，故单独成件：
//
//	Bindings       口名 → PCI      —— 解释「netdev 已消失，这个口对应哪个设备」
//	NativeDrivers  PCI → 驱动名    —— 解释「这个设备绑到 DPDK 之前用的是哪个内核驱动」
//
// 为什么必须落盘（round17 真机实测）：清 `driver_override` + `rescan` **不会**让内核自动把设备
// 交还原生驱动（停 VPP 后实测 3s 内无驱动；模块与 modalias alias 都在位），所以不带 `to-driver`
// 的解绑在今天只能报错并让操作者报出驱动名。而**绑定那一刻**产品是知道答案的（`Bind` 里读到的
// `cur`）——那一刻记下来，解绑就能确定性地还回去（`Unbind` 的取值顺序：显式 `to-driver` >
// 本记录 > rescan 试探）。
//
// 不入 committed 配置、也不进 format-data 受管目录：它是**本机运行态事实**（与 #100 的
// dpdk-bindings.json 同类——那份同样不在 format-data 清单里），随机器走，清配置不该抹掉它。
//
// 写失败不算致命：绑定动作本身已经发生，记录失败只让解绑退回「报错并要 to-driver」的现状。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultNativeDriversPath 原生驱动记录缺省路径。
const DefaultNativeDriversPath = "/var/lib/nfvis/dpdk-native-drivers.json"

// dpdkUioDrivers 已知的 DPDK 用户态驱动（**不**作为「原生驱动」记录）。
//
// 判据是「这三个之外一律视为内核驱动」——刻意的**保守猜测**：把 `igb_uio`/`uio_pci_generic`
// 记成原生驱动会让解绑把设备又交回 DPDK 驱动，正好与操作者意图相反。自定义的第三方 DPDK
// 驱动不在表内（会按内核驱动记），此时解绑会尝试绑回它并失败——如实报错，不静默。
var dpdkUioDrivers = map[string]bool{
	"vfio-pci":        true,
	"igb_uio":         true,
	"uio_pci_generic": true,
}

// IsDPDKUioDriver 该驱动名是否为已知的 DPDK 用户态驱动。
func IsDPDKUioDriver(driver string) bool {
	return dpdkUioDrivers[strings.TrimSpace(driver)]
}

// NativeDrivers PCI → 原生驱动名的持久记录（并发安全；首次访问时读盘）。
type NativeDrivers struct {
	Path      string
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error

	mu     sync.Mutex
	m      map[string]string
	loaded bool
}

// NewNativeDrivers 构造（path 空取缺省）。
func NewNativeDrivers(path string) *NativeDrivers {
	if strings.TrimSpace(path) == "" {
		path = DefaultNativeDriversPath
	}
	return &NativeDrivers{Path: path}
}

func (b *NativeDrivers) read(path string) ([]byte, error) {
	if b.ReadFile != nil {
		return b.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (b *NativeDrivers) write(path string, data []byte) error {
	if b.WriteFile != nil {
		return b.WriteFile(path, data)
	}
	return os.WriteFile(path, data, 0o644)
}

// load 惰性读盘（文件缺失视为空记录；损坏则报错，不静默丢弃既有映射）。
func (b *NativeDrivers) load() error {
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

// Get 查询 PCI 对应的原生驱动。
func (b *NativeDrivers) Get(pci string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return "", false
	}
	d, ok := b.m[normPCI(strings.TrimSpace(pci))]
	return d, ok && d != ""
}

// Set 记录 PCI → 原生驱动（绑定接管**之前**调用）。
func (b *NativeDrivers) Set(pci, driver string) error {
	pci, driver = normPCI(strings.TrimSpace(pci)), strings.TrimSpace(driver)
	if pci == "" || driver == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.load(); err != nil {
		return err
	}
	if b.m[pci] == driver {
		return nil
	}
	b.m[pci] = driver
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

// All 返回记录快照（诊断用）。
func (b *NativeDrivers) All() map[string]string {
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
