// Package compute 实现 libvirt/KVM 侧编排（M4，FR-CMP-010~019）。
//
// 分层约定（沿用 M3 网络编排 `_govpp.go` 的做法）：
//   - `domain_xml.go` 等为**纯函数**（domain XML 组装、路径/取值换算），单测直接断言，
//     纳入覆盖率门槛；
//   - `*_libvirt.go` 为**薄适配层**（真实 libvirt 调用），文件名以 `_libvirt.go` 结尾，
//     由 `contrib/scripts/check_coverage.sh` 的 COVER_EXCLUDE 排除，改由 nfvis-vm 上的
//     集成测试（build tag `integration`）覆盖。
//
// domain XML 元素以 nfvis-vm 实装 libvirt 12.0.0 的 schema
// `/usr/share/libvirt/schemas/domaincommon.rng` 与 `virsh dumpxml` 实测为准
// （见 docs/M4-P0-环境核验记录.md），不凭记忆写调用代码。
package compute

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/xzjt/nfvis/internal/model"
)

// 缺省值。真机实测：QEMU 10.2.1 位于 /usr/bin/qemu-system-x86_64，q35 机器类型可用。
const (
	DefaultEmulator   = "/usr/bin/qemu-system-x86_64"
	DefaultMachine    = "q35"
	DefaultDiskFormat = "qcow2"
	DefaultCPUMode    = "host-passthrough"
)

// 接口类型（与契约 VnfInterface.type 一致）。
const (
	IfaceVhostUser = "vhost-user"
	IfaceSriovVF   = "sriov-vf"
)

// DataDiskSpec 附加 virtio 数据盘（FR-CMP-018）。盘文件由 M4-6 创建，
// 本层只负责挂载（空盘/镜像克隆的差异已体现在 Path）。
type DataDiskSpec struct {
	Name   string // 数据盘名（配置 disks[].name）
	Path   string // qcow2 文件路径
	Format string // 缺省 qcow2
}

// InterfaceSpec 已解析的 vNIC 接入参数（FR-NET-020/021）。
// socket 路径与 VF PCI 地址由编排层解析后传入——本层是纯函数，不查 sysfs/VPP。
type InterfaceSpec struct {
	Name   string // vNIC 名（配置 interfaces[].name）
	Type   string // vhost-user | sriov-vf
	MAC    string // 为空时按 DefaultMAC 确定性生成
	Socket string // vhost-user：VPP 侧监听 socket（QEMU 作 client 连接）
	// Bridge 内核数据面下该 vNIC 接入的 L2 交换机（内核 bridge 名，v3 决策 #404）：
	// 域定义改用 `<interface type='bridge'>`，libvirt 自建宿主 tap 并挂上该 bridge，
	// 配合 `<driver name='vhost'/>` 由内核 vhost-net 加速 virtio 数据面。
	Bridge    string
	VLAN      int    // vNIC 的 VLAN（内核数据面：libvirt 侧给 tap 打标）
	VFPCI     string // sriov-vf：VF 的 PCI 地址，如 0000:0b:10.1
	Queues    uint   // virtio 队列数（0 = 不写 driver）
	TargetDev string // 为空时按 vnet<序号> 生成
}

// DomainSpec 组装一台 VM 的 domain 所需全部输入（纯数据，便于单测）。
type DomainSpec struct {
	VM model.VMFunction

	// 资源分配（M4-2 账本输出）
	Cores        []int  // 绑核 cpuset（升序）；空 = 不绑核
	NumaNode     *int   // 非 nil 时写 numatune（FR-CMP-002 NUMA 亲和）
	HugepageSize string // 已解析的页大小 2M|1G（backing=hugepage 时必填）

	// 落盘
	DiskPath   string // 主盘路径（M4-3 从镜像克隆，M4-8 产出）
	DiskFormat string // 缺省 qcow2；iso 时按 cdrom 挂载并改引导设备
	SeedISO    string // 非空则挂 NoCloud seed cdrom（FR-CMP-016）
	DataDisks  []DataDiskSpec

	Interfaces []InterfaceSpec
	// KernelDataPlane 数据面为 Linux 内核网络（v3 决策 #404）：vNIC 走 virtio + 宿主 tap +
	// vhost-net（`<interface type='bridge'>`），而不是 VPP 的 vhost-user socket。
	KernelDataPlane bool
	// PCIDevices 通用 PCI 直通设备（BDF，FR-CMP-023）：每设备一个 hostdev，
	// 排在 SR-IOV hostdev 之后。存在性检查在编排层（apply/define 前）完成，本层只做组装。
	PCIDevices []string

	Emulator string // 缺省 DefaultEmulator
	Machine  string // 缺省 q35
	CPUMode  string // 缺省 host-passthrough
}

// DeterministicUUID 由 VM 名派生 UUIDv5 风格标识（版本 5、RFC 4122 variant），
// 保证同一 VM 名字恒等，重定义/收敛幂等。
func DeterministicUUID(name string) string {
	sum := sha1.Sum([]byte("nfvis:vm:" + name))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// BuildDomainXML 生成 domain XML（纯函数）。
func BuildDomainXML(spec DomainSpec) (string, error) {
	d, err := BuildDomain(spec)
	if err != nil {
		return "", err
	}
	return d.Marshal()
}

// BuildDomain 由 DomainSpec 组装 libvirt domain 结构。
//
// 覆盖 M4-1 验收项：vCPU/绑核、大页内存（2M/1G 页池）、vhost-user vNIC、
// SR-IOV hostdev、串口、cloud-init seed ISO 挂载、附加数据盘。
func BuildDomain(spec DomainSpec) (*libvirtxml.Domain, error) {
	vm := spec.VM
	if strings.TrimSpace(vm.Name) == "" {
		return nil, errors.New("VM 名不能为空")
	}
	if vm.VCPU.Count <= 0 {
		return nil, fmt.Errorf("VM %s: vcpu.count 必须 > 0", vm.Name)
	}
	if vm.Memory.SizeMB <= 0 {
		return nil, fmt.Errorf("VM %s: memory.size-mb 必须 > 0", vm.Name)
	}
	if strings.TrimSpace(spec.DiskPath) == "" {
		return nil, fmt.Errorf("VM %s: 缺少主盘路径", vm.Name)
	}

	emulator := spec.Emulator
	if emulator == "" {
		emulator = DefaultEmulator
	}
	machine := spec.Machine
	if machine == "" {
		machine = DefaultMachine
	}
	cpuMode := spec.CPUMode
	if cpuMode == "" {
		cpuMode = DefaultCPUMode
	}

	memMB := uint(vm.Memory.SizeMB)
	diskFormat := spec.DiskFormat
	if diskFormat == "" {
		diskFormat = DefaultDiskFormat
	}
	isoDisk := strings.EqualFold(diskFormat, "iso")
	bootDev := "hd"
	if isoDisk {
		bootDev = "cd"
	}

	d := &libvirtxml.Domain{
		Type: "kvm",
		Name: vm.Name,
		// 确定性 UUID（由 VM 名派生）：重定义/恢复收敛时 libvirt 身份稳定，
		// 避免「同名不同 uuid」导致 DomainDefineXML 报 already exists。
		UUID:          DeterministicUUID(vm.Name),
		Description:   vm.Description,
		Memory:        &libvirtxml.DomainMemory{Value: memMB, Unit: "MiB"},
		CurrentMemory: &libvirtxml.DomainCurrentMemory{Value: memMB, Unit: "MiB"},
		VCPU:          &libvirtxml.DomainVCPU{Placement: "static", Value: uint(vm.VCPU.Count)},
		OS: &libvirtxml.DomainOS{
			Type:        &libvirtxml.DomainOSType{Arch: "x86_64", Machine: machine, Type: "hvm"},
			BootDevices: []libvirtxml.DomainBootDevice{{Dev: bootDev}},
		},
		Clock:    &libvirtxml.DomainClock{Offset: "utc"},
		Features: &libvirtxml.DomainFeatureList{ACPI: &libvirtxml.DomainFeature{}},
		CPU:      &libvirtxml.DomainCPU{Mode: cpuMode},
		// on_crash=preserve：保留 crashed 态供 M4-10 告警（FR-CMP-017），
		// 与契约 state 枚举 {running,shutoff,crashed,paused} 对齐。
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "preserve",
		Devices:    &libvirtxml.DomainDeviceList{Emulator: emulator},
	}

	if useHugepages(vm) {
		size, unit, err := HugepageSpec(spec.HugepageSize)
		if err != nil {
			return nil, fmt.Errorf("VM %s: %w", vm.Name, err)
		}
		d.MemoryBacking = &libvirtxml.DomainMemoryBacking{
			MemoryHugePages: &libvirtxml.DomainMemoryHugepages{
				Hugepages: []libvirtxml.DomainMemoryHugepage{{Size: size, Unit: unit}},
			},
			MemoryLocked: &libvirtxml.DomainMemoryLocked{},
		}
		// vhost-user 要求 VM 内存与 VPP 共享（FR-NET-020）：显式置 shared。
		// 内核数据面无共享内存对端（tap + vhost-net 在内核侧），不置 shared。
		if !spec.KernelDataPlane && hasVhostUser(spec.Interfaces) {
			d.MemoryBacking.MemoryAccess = &libvirtxml.DomainMemoryAccess{Mode: "shared"}
		}
	}

	if len(spec.Cores) > 0 {
		d.CPUTune = &libvirtxml.DomainCPUTune{VCPUPin: vcpuPins(vm.VCPU.Count, spec.Cores)}
	}
	if spec.NumaNode != nil {
		d.NUMATune = &libvirtxml.DomainNUMATune{
			Memory: &libvirtxml.DomainNUMATuneMemory{
				Mode:    "strict",
				Nodeset: strconv.Itoa(*spec.NumaNode),
			},
		}
	}

	disks, err := buildDisks(vm.Name, spec, diskFormat, isoDisk)
	if err != nil {
		return nil, err
	}
	d.Devices.Disks = disks

	// vhost-user 接口与 SR-IOV VF hostdev 分别落到不同元素（FR-NET-020/021）。
	for i, is := range spec.Interfaces {
		switch is.Type {
		case IfaceVhostUser:
			if spec.KernelDataPlane {
				// 内核数据面：virtio 网卡 + 宿主 tap + vhost-net，挂到交换机对应的内核 bridge。
				if strings.TrimSpace(is.Bridge) == "" {
					return nil, fmt.Errorf("VM %s: vNIC %s 缺少接入的虚拟交换机（内核数据面需要它定位 bridge）", vm.Name, is.Name)
				}
				d.Devices.Interfaces = append(d.Devices.Interfaces, buildBridgeIface(vm.Name, is, i))
				break
			}
			if strings.TrimSpace(is.Socket) == "" {
				return nil, fmt.Errorf("VM %s: vNIC %s（vhost-user）缺少 socket 路径", vm.Name, is.Name)
			}
			d.Devices.Interfaces = append(d.Devices.Interfaces, buildVhostUserIface(vm.Name, is, i))
		case IfaceSriovVF:
			hostdev, err := buildSriovHostdev(vm.Name, is)
			if err != nil {
				return nil, err
			}
			d.Devices.Hostdevs = append(d.Devices.Hostdevs, hostdev)
		default:
			return nil, fmt.Errorf("VM %s: vNIC %s 类型 %q 不受支持（仅 %s/%s）",
				vm.Name, is.Name, is.Type, IfaceVhostUser, IfaceSriovVF)
		}
	}

	// 通用 PCI 直通（FR-CMP-023）：与 SR-IOV 同结构，**排在 SR-IOV hostdev 之后**。
	for _, bdf := range spec.PCIDevices {
		hostdev, err := buildPCIHostdev(vm.Name, bdf)
		if err != nil {
			return nil, err
		}
		d.Devices.Hostdevs = append(d.Devices.Hostdevs, hostdev)
	}

	if serialConsoleEnabled(vm) {
		d.Devices.Serials = []libvirtxml.DomainSerial{{
			Source: &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}},
			Target: &libvirtxml.DomainSerialTarget{Port: uintPtr(0)},
		}}
		d.Devices.Consoles = []libvirtxml.DomainConsole{{
			Source: &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}},
			Target: &libvirtxml.DomainConsoleTarget{Type: "serial", Port: uintPtr(0)},
		}}
	}

	// 无人值守：无显卡、无内存气球（气球与大页不兼容）。
	d.Devices.Videos = []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: "none"}}}
	d.Devices.MemBalloon = &libvirtxml.DomainMemBalloon{Model: "none"}

	return d, nil
}

// buildDisks 主盘 + 附加数据盘 + cloud-init seed ISO。
// 设备命名确定：主盘 vda，数据盘 vdb/vdc…，seed 接在数据盘之后（**均为 virtio**，决策 #139）。
func buildDisks(vmName string, spec DomainSpec, diskFormat string, isoDisk bool) ([]libvirtxml.DomainDisk, error) {
	var out []libvirtxml.DomainDisk

	main := libvirtxml.DomainDisk{
		Device: "disk",
		Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: diskFormat},
		Source: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: spec.DiskPath}},
	}
	if isoDisk {
		// ISO 作为只读光盘引导。
		main.Device = "cdrom"
		main.Target = &libvirtxml.DomainDiskTarget{Dev: "sda", Bus: "sata"}
		main.ReadOnly = &libvirtxml.DomainDiskReadOnly{}
	} else {
		main.Target = &libvirtxml.DomainDiskTarget{Dev: "vda", Bus: "virtio"}
	}
	out = append(out, main)

	for i, dd := range spec.DataDisks {
		if strings.TrimSpace(dd.Path) == "" {
			return nil, fmt.Errorf("VM %s: 数据盘 %s 缺少路径", vmName, dd.Name)
		}
		format := dd.Format
		if format == "" {
			format = DefaultDiskFormat
		}
		out = append(out, libvirtxml.DomainDisk{
			Device: "disk",
			Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: format},
			Source: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: dd.Path}},
			Target: &libvirtxml.DomainDiskTarget{Dev: diskDevName(i + 1), Bus: "virtio"},
		})
	}

	if spec.SeedISO != "" {
		// seed 以 **virtio 磁盘**交付（决策 #139，收口 #22）：
		// 此前挂 SATA 光盘（q35 → ich9-ahci），而 Debian cloud / Alpine virt 这类常用精简内核
		// **不含 ahci 驱动** → guest 看不到该盘 → cloud-init 按 notfound=disabled **静默自我禁用**
		// （VM 起来了、user-data 没生效、全程无报错）。NoCloud 数据源扫的是「任何带 CIDSA/CIDATA
		// 文件系统的块设备」，不要求是光盘，故以 virtio 磁盘交付即可，而 virtio_blk 在这些内核里都有。
		seedDev := diskDevName(len(spec.DataDisks) + 1) // 接在数据盘之后，不与之撞名
		out = append(out, libvirtxml.DomainDisk{
			Device:   "disk",
			Driver:   &libvirtxml.DomainDiskDriver{Name: "qemu", Type: "raw"},
			Source:   &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: spec.SeedISO}},
			Target:   &libvirtxml.DomainDiskTarget{Dev: seedDev, Bus: "virtio"},
			ReadOnly: &libvirtxml.DomainDiskReadOnly{},
		})
	}
	return out, nil
}

// diskDevName 数据盘序号 → virtio 设备名，沿用 Linux 命名序列
// （vda…vdz、vdaa…vdaz、vdba…），避免静默重复命名。
func diskDevName(idx int) string {
	if idx <= 0 {
		idx = 0
	}
	if idx < 26 {
		return "vd" + string(rune('a'+idx))
	}
	idx -= 26
	return "vd" + string(rune('a'+idx/26)) + string(rune('a'+idx%26))
}

// buildVhostUserIface VM 侧 virtio-net 指向 VPP 监听的 vhost-user socket。
// QEMU 为 client（VPP `create vhost-user socket` 为 server 端）。
func buildVhostUserIface(vmName string, is InterfaceSpec, idx int) libvirtxml.DomainInterface {
	mac := is.MAC
	if mac == "" {
		mac = DefaultMAC(vmName, is.Name)
	}
	target := is.TargetDev
	if target == "" {
		target = fmt.Sprintf("vnet%d", idx)
	}
	iface := libvirtxml.DomainInterface{
		MAC: &libvirtxml.DomainInterfaceMAC{Address: mac},
		Source: &libvirtxml.DomainInterfaceSource{
			VHostUser: &libvirtxml.DomainInterfaceSourceVHostUser{
				Chardev: &libvirtxml.DomainChardevSource{
					// reconnect：VPP 重启后 socket 重建，QEMU 自动重连（FR-OPS-011）。
					UNIX: &libvirtxml.DomainChardevSourceUNIX{
						Path: is.Socket, Mode: "client",
						Reconnect: &libvirtxml.DomainChardevSourceReconnect{Enabled: "yes", Timeout: uintPtr(5)},
					},
				},
			},
		},
		Model:  &libvirtxml.DomainInterfaceModel{Type: "virtio"},
		Target: &libvirtxml.DomainInterfaceTarget{Dev: target},
	}
	if is.Queues > 0 {
		iface.Driver = &libvirtxml.DomainInterfaceDriver{Queues: is.Queues}
	}
	return iface
}

// buildBridgeIface 内核数据面的 vNIC：virtio 网卡由 libvirt 以 `<interface type='bridge'>`
// 接入宿主内核 bridge——libvirt 自建 tap 设备并 enslave 到该 bridge，`<driver name='vhost'/>`
// 让内核 vhost-net 处理 virtio 环（数据面不经 QEMU 用户态）。
//
// VLAN：声明了 vlan 时用 libvirt 的 `<vlan><tag id='n'/></vlan>`（tap 侧自动打标），
// 与 VPP 侧「vNIC 带 vlan」的语义一致。
func buildBridgeIface(vmName string, is InterfaceSpec, idx int) libvirtxml.DomainInterface {
	mac := is.MAC
	if mac == "" {
		mac = DefaultMAC(vmName, is.Name)
	}
	target := is.TargetDev
	if target == "" {
		target = fmt.Sprintf("vnet%d", idx)
	}
	// 注意：`type="bridge"` 属性由 libvirtxml 依据 Source.Bridge 自动生成，不手写。
	iface := libvirtxml.DomainInterface{
		MAC:    &libvirtxml.DomainInterfaceMAC{Address: mac},
		Source: &libvirtxml.DomainInterfaceSource{Bridge: &libvirtxml.DomainInterfaceSourceBridge{Bridge: is.Bridge}},
		Model:  &libvirtxml.DomainInterfaceModel{Type: "virtio"},
		Target: &libvirtxml.DomainInterfaceTarget{Dev: target},
		// vhost-net：把 virtio 数据面下沉到内核，与 VPP 的 vhost-user 位置对应。
		Driver: &libvirtxml.DomainInterfaceDriver{Name: "vhost"},
	}
	if is.Queues > 0 {
		iface.Driver.Queues = is.Queues
	}
	if is.VLAN > 0 {
		iface.VLan = &libvirtxml.DomainInterfaceVLan{
			Tags: []libvirtxml.DomainInterfaceVLanTag{{ID: uint(is.VLAN)}},
		}
	}
	return iface
}

// buildSriovHostdev SR-IOV VF 直通（FR-NET-021）：hostdev + VF 的 PCI 地址。
// 本环境（vmxnet3）无 PF/VF，真机不可验证，仅 XML 组装 + 单测；见 M4-P0 记录 §4。
func buildSriovHostdev(vmName string, is InterfaceSpec) (libvirtxml.DomainHostdev, error) {
	if strings.TrimSpace(is.VFPCI) == "" {
		return libvirtxml.DomainHostdev{}, fmt.Errorf("VM %s: vNIC %s（sriov-vf）缺少 VF PCI 地址", vmName, is.Name)
	}
	addr, err := ParsePCI(is.VFPCI)
	if err != nil {
		return libvirtxml.DomainHostdev{}, fmt.Errorf("VM %s: vNIC %s: %w", vmName, is.Name, err)
	}
	return libvirtxml.DomainHostdev{
		Managed: "yes",
		SubsysPCI: &libvirtxml.DomainHostdevSubsysPCI{
			Source: &libvirtxml.DomainHostdevSubsysPCISource{Address: addr},
		},
	}, nil
}

// buildPCIHostdev 通用 PCI 设备直通（FR-CMP-023）：与 SR-IOV VF **同结构**的 hostdev。
// 地址先经 model.NormalizeBDF 归一（接受省略 domain/短写形态），再复用 ParsePCI 组装；
// 设备是否真在本机（/sys/bus/pci/devices）由编排层在 define 前检查，本层不查。
func buildPCIHostdev(vmName, bdf string) (libvirtxml.DomainHostdev, error) {
	norm, err := model.NormalizeBDF(bdf)
	if err != nil {
		return libvirtxml.DomainHostdev{}, fmt.Errorf("VM %s: %w", vmName, err)
	}
	addr, err := ParsePCI(norm)
	if err != nil {
		return libvirtxml.DomainHostdev{}, fmt.Errorf("VM %s: PCI 设备 %s: %w", vmName, bdf, err)
	}
	return libvirtxml.DomainHostdev{
		Managed: "yes",
		SubsysPCI: &libvirtxml.DomainHostdevSubsysPCI{
			Source: &libvirtxml.DomainHostdevSubsysPCISource{Address: addr},
		},
	}, nil
}

// vcpuPins vCPU 序号 → 绑核 cpuset。核数不足时按模取（确定性），
// 正常路径下账本分配的核数等于 vCPU 数（FR-CMP-002）。
func vcpuPins(count int, cores []int) []libvirtxml.DomainCPUTuneVCPUPin {
	pins := make([]libvirtxml.DomainCPUTuneVCPUPin, 0, count)
	for i := 0; i < count; i++ {
		pins = append(pins, libvirtxml.DomainCPUTuneVCPUPin{
			VCPU:   uint(i),
			CPUSet: strconv.Itoa(cores[i%len(cores)]),
		})
	}
	return pins
}

// useHugepages 内存 backing 是否为大页（缺省 hugepage；normal 仅限无 vhost-user 的 VM）。
func useHugepages(vm model.VMFunction) bool {
	return !strings.EqualFold(vm.Memory.Backing, "normal")
}

func hasVhostUser(ifaces []InterfaceSpec) bool {
	for _, is := range ifaces {
		if is.Type == IfaceVhostUser {
			return true
		}
	}
	return false
}

// serialConsoleEnabled 缺省启用串口（FR-CMP-014）。
func serialConsoleEnabled(vm model.VMFunction) bool {
	return vm.SerialConsole == nil || *vm.SerialConsole
}

// HugepageSpec 页大小字符串 → (size, unit)（libvirt page 元素用）。
func HugepageSpec(pageSize string) (uint, string, error) {
	switch pageSize {
	case "1G":
		return 1, "G", nil
	case "2M":
		return 2, "M", nil
	case "":
		return 0, "", errors.New("未指定大页页大小（hugepage_size / 资源池主池）")
	default:
		return 0, "", fmt.Errorf("不支持的大页页大小 %q（仅 2M/1G）", pageSize)
	}
}

// FormatCPUSet 核列表 → cpuset 字符串（紧凑区间，如 [1 2 3 5] → "1-3,5"）。
func FormatCPUSet(cores []int) string {
	if len(cores) == 0 {
		return ""
	}
	seen := make(map[int]bool, len(cores))
	uniq := make([]int, 0, len(cores))
	for _, c := range cores {
		if !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	sort.Ints(uniq)

	var b strings.Builder
	start, prev := uniq[0], uniq[0]
	flush := func(end int) {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if start == end {
			b.WriteString(strconv.Itoa(start))
			return
		}
		b.WriteString(strconv.Itoa(start))
		b.WriteByte('-')
		b.WriteString(strconv.Itoa(end))
	}
	for _, c := range uniq[1:] {
		if c == prev+1 {
			prev = c
			continue
		}
		flush(prev)
		start, prev = c, c
	}
	flush(prev)
	return b.String()
}

// DefaultMAC 由 (VM 名, vNIC 名) 确定性派生本地管理 MAC（FR-CFG-011③ 不重复）。
// 前缀 52:54:00 保留 QEMU 风格可识别性；同一配置重算恒等，便于恢复收敛。
func DefaultMAC(vmName, ifaceName string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(vmName + "/" + ifaceName))
	sum := h.Sum32()
	return fmt.Sprintf("52:54:%02x:%02x:%02x:%02x",
		byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
}

// ParsePCI 解析 "0000:0b:10.1"（可带 0x 前缀）为 libvirt PCI 地址。
func ParsePCI(addr string) (*libvirtxml.DomainAddressPCI, error) {
	parts := strings.Split(strings.TrimSpace(addr), ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("PCI 地址 %q 格式非法（应为 domain:bus:slot.function）", addr)
	}
	domain, err := parseHexUint(parts[0])
	if err != nil {
		return nil, fmt.Errorf("PCI 地址 %q 的 domain 非法: %w", addr, err)
	}
	bus, err := parseHexUint(parts[1])
	if err != nil {
		return nil, fmt.Errorf("PCI 地址 %q 的 bus 非法: %w", addr, err)
	}
	slotFn := strings.Split(parts[2], ".")
	if len(slotFn) != 2 {
		return nil, fmt.Errorf("PCI 地址 %q 的 slot.function 非法", addr)
	}
	slot, err := parseHexUint(slotFn[0])
	if err != nil {
		return nil, fmt.Errorf("PCI 地址 %q 的 slot 非法: %w", addr, err)
	}
	fn, err := parseHexUint(slotFn[1])
	if err != nil {
		return nil, fmt.Errorf("PCI 地址 %q 的 function 非法: %w", addr, err)
	}
	return &libvirtxml.DomainAddressPCI{
		Domain:   uintPtr(domain),
		Bus:      uintPtr(bus),
		Slot:     uintPtr(slot),
		Function: uintPtr(fn),
	}, nil
}

func parseHexUint(s string) (uint, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"), 16, 32)
	if err != nil {
		return 0, err
	}
	return uint(v), nil
}

func uintPtr(v uint) *uint { return &v }
