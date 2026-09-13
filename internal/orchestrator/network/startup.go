package network

// M3-2：VPP startup.conf 生成器（FR-SYS-008/009/010）。
//
// 按 committed 配置生成 /etc/vpp/startup.conf 文本；cpu/memory/buffers/dpdk/plugins
// 映射依据 nfvis-vm 上 VPP 26.06 的默认 startup.conf 与插件清单核对（AGENTS.md
// 「外部文档查询」：以实装版本为准）：
//   - cpu：main-core / corelist-workers / workers（workers-per-numa）
//   - memory：main-heap-size / default-hugepage-size（hugepage-preference）
//   - buffers：buffers-per-numa 属于独立 buffers 段（不在 memory）
//   - dpdk：dev default 全局默认 + dev <pci> 单网卡覆盖（key 为 PCI 地址，
//     ifname→PCI 由运行态解析，不入 committed 配置，见附录 A #30）
//   - plugins：<name>_plugin.so { enable|disable }
//
// FR-SYS-010 校验：worker/main 核须落在 resource-pools cpu isolated-cores 内；
// hugepage-preference 须与资源池页大小一致。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// PCIResolver 将物理口名解析为 DPDK 绑定所需的 PCI 地址（如 ens192 → 0000:03:00.0）。
// 运行态解析（sysfs），失败则应报错而非静默跳过。
type PCIResolver func(ifname string) (string, error)

// GenerateStartup 生成 startup.conf 文本。pciOf 为 nil 时，仅当存在单网卡覆盖或
// 需要绑定物理口时才报错；纯 cpu/memory 配置可离线生成。
func GenerateStartup(cfg *model.Config, pciOf PCIResolver) (string, error) {
	if cfg == nil {
		cfg = &model.Config{}
	}
	vpp := cfg.Vpp
	if vpp == nil {
		vpp = &model.VppConfig{}
	}
	if err := validateVPP(cfg, vpp); err != nil {
		return "", err
	}

	var b strings.Builder
	unixStanza(&b)
	cpuStanza(&b, vpp.CPU)
	if err := memoryStanza(&b, vpp.Memory); err != nil {
		return "", err
	}
	buffersStanza(&b, vpp.Memory)
	if err := dpdkStanza(&b, vpp.DPDK, pciOf); err != nil {
		return "", err
	}
	pluginsStanza(&b, vpp.Plugins)
	return b.String(), nil
}

// VppSectionHash 计算 vpp 配置段的稳定哈希（用于 pending_restart 判定：
// 已应用哈希 ≠ 当前 committed 哈希；与 PCI 解析等运行态无关）。
func VppSectionHash(vpp *model.VppConfig) string {
	if vpp == nil {
		vpp = &model.VppConfig{}
	}
	b, _ := json.Marshal(vpp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ParseCoreList 解析 VPP 核列表语法（"5,7,9-11" → [5,7,9,10,11]）。
func ParseCoreList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("核列表含空项: %q", s)
		}
		lo, hi := part, part
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, hi = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, fmt.Errorf("非法核号 %q（核列表 %q）", lo, s)
		}
		z, err := strconv.Atoi(hi)
		if err != nil || z < a {
			return nil, fmt.Errorf("非法核范围 %q（核列表 %q）", part, s)
		}
		for c := a; c <= z; c++ {
			out = append(out, c)
		}
	}
	return out, nil
}

// ---------- 校验（FR-SYS-010） ----------

func validateVPP(cfg *model.Config, vpp *model.VppConfig) error {
	isolated := map[int]bool{}
	var pageSizes []string
	if cfg.ResourcePools != nil {
		if cfg.ResourcePools.CPU != nil {
			for _, c := range cfg.ResourcePools.CPU.IsolatedCores {
				isolated[c] = true
			}
		}
		for _, hp := range cfg.ResourcePools.Hugepages {
			pageSizes = append(pageSizes, hp.PageSize)
		}
	}

	if vpp.CPU != nil {
		if len(isolated) > 0 {
			if vpp.CPU.MainCore > 0 && !isolated[vpp.CPU.MainCore] {
				return fmt.Errorf("vpp cpu main-core %d 不在 resource-pools cpu isolated-cores 内（FR-SYS-010）", vpp.CPU.MainCore)
			}
			if vpp.CPU.CorelistWorkers != "" {
				cores, err := ParseCoreList(vpp.CPU.CorelistWorkers)
				if err != nil {
					return err
				}
				for _, c := range cores {
					if !isolated[c] {
						return fmt.Errorf("vpp cpu corelist-workers 核 %d 不在 resource-pools cpu isolated-cores 内（FR-SYS-010）", c)
					}
				}
			}
		}
	}
	if vpp.Memory != nil && vpp.Memory.HugepagePreference != "" && len(pageSizes) > 0 {
		found := false
		for _, ps := range pageSizes {
			if ps == vpp.Memory.HugepagePreference {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("vpp memory hugepage-preference %q 与资源池页大小 %v 不一致（FR-SYS-010）", vpp.Memory.HugepagePreference, pageSizes)
		}
	}
	return nil
}

// ---------- 各段渲染 ----------

func unixStanza(b *strings.Builder) {
	b.WriteString("unix {\n")
	b.WriteString("  nodaemon\n")
	b.WriteString("  log /var/log/vpp/vpp.log\n")
	b.WriteString("  cli-listen /run/vpp/cli.sock\n")
	b.WriteString("  gid vpp\n")
	b.WriteString("}\n\n")
	b.WriteString("api-segment {\n  gid vpp\n}\n\n")
	b.WriteString("socksvr {\n  default\n}\n\n")
}

func cpuStanza(b *strings.Builder, cpu *model.VppCPU) {
	if cpu == nil {
		return
	}
	b.WriteString("cpu {\n")
	if cpu.MainCore > 0 {
		fmt.Fprintf(b, "  main-core %d\n", cpu.MainCore)
	}
	if cpu.CorelistWorkers != "" {
		fmt.Fprintf(b, "  corelist-workers %s\n", cpu.CorelistWorkers)
	} else if cpu.WorkersPerNuma > 0 {
		fmt.Fprintf(b, "  workers %d\n", cpu.WorkersPerNuma)
	}
	b.WriteString("}\n\n")
}

func memoryStanza(b *strings.Builder, mem *model.VppMemory) error {
	if mem == nil || (mem.MainHeapSize == "" && mem.HugepagePreference == "") {
		return nil
	}
	b.WriteString("memory {\n")
	if mem.MainHeapSize != "" {
		fmt.Fprintf(b, "  main-heap-size %s\n", mem.MainHeapSize)
	}
	if mem.HugepagePreference != "" {
		// hugepage-preference 映射 VPP 的 default-hugepage-size（NFViS 命名 → VPP 键）
		fmt.Fprintf(b, "  default-hugepage-size %s\n", mem.HugepagePreference)
	}
	b.WriteString("}\n\n")
	return nil
}

func buffersStanza(b *strings.Builder, mem *model.VppMemory) {
	if mem == nil || mem.BuffersPerNuma <= 0 {
		return
	}
	fmt.Fprintf(b, "buffers {\n  buffers-per-numa %d\n}\n\n", mem.BuffersPerNuma)
}

func dpdkStanza(b *strings.Builder, dpdk *model.VppDPDK, pciOf PCIResolver) error {
	if dpdk == nil {
		return nil
	}
	d := dpdk.Dev
	hasDefault := d.RxQueues > 0 || d.TxQueues > 0 || d.RxDescriptors > 0 || d.TxDescriptors > 0
	if !hasDefault && len(dpdk.PerDev) == 0 && dpdk.UIODriver == "" {
		return nil
	}
	b.WriteString("dpdk {\n")
	if hasDefault {
		b.WriteString("  dev default {\n")
		writeDevOptions(b, d.RxQueues, d.TxQueues, d.RxDescriptors, d.TxDescriptors, "    ")
		b.WriteString("  }\n")
	}
	for _, o := range dpdk.PerDev {
		if pciOf == nil {
			return fmt.Errorf("vpp dpdk 单网卡覆盖 %q 需要运行态解析 PCI 地址", o.Interface)
		}
		pci, err := pciOf(o.Interface)
		if err != nil {
			return fmt.Errorf("解析 %q 的 PCI 地址失败: %w", o.Interface, err)
		}
		fmt.Fprintf(b, "  dev %s {\n", pci)
		if o.Interface != "" {
			// 固定 VPP 接口名与配置中的物理口名一致，便于按名解析 sw_if_index
			fmt.Fprintf(b, "    name %s\n", o.Interface)
		}
		writeDevOptions(b, o.RxQueues, o.TxQueues, o.RxDescriptors, o.TxDescriptors, "    ")
		b.WriteString("  }\n")
	}
	if dpdk.UIODriver != "" {
		fmt.Fprintf(b, "  uio-driver %s\n", dpdk.UIODriver)
	}
	b.WriteString("}\n\n")
	return nil
}

// writeDevOptions 只输出非零项，实现「单网卡覆盖项缺省回落全局默认」。
func writeDevOptions(b *strings.Builder, rxq, txq, rxdesc, txdesc int, pad string) {
	if rxq > 0 {
		fmt.Fprintf(b, "%snum-rx-queues %d\n", pad, rxq)
	}
	if txq > 0 {
		fmt.Fprintf(b, "%snum-tx-queues %d\n", pad, txq)
	}
	if rxdesc > 0 {
		fmt.Fprintf(b, "%snum-rx-desc %d\n", pad, rxdesc)
	}
	if txdesc > 0 {
		fmt.Fprintf(b, "%snum-tx-desc %d\n", pad, txdesc)
	}
}

func pluginsStanza(b *strings.Builder, plugins []model.VppPlugin) {
	if len(plugins) == 0 {
		return
	}
	sorted := append([]model.VppPlugin{}, plugins...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	b.WriteString("plugins {\n")
	for _, p := range sorted {
		file := p.Name
		if !strings.HasSuffix(file, ".so") {
			file += "_plugin.so"
		}
		state := p.State
		if state == "" {
			state = "enable"
		}
		fmt.Fprintf(b, "  plugin %s { %s }\n", file, state)
	}
	b.WriteString("}\n")
}
