package model

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// 启动画像默认值（决策 #485）。
const (
	DefaultMachine   = "q35"
	DefaultCPUMode   = "host-passthrough"
	DefaultFirmware  = "bios"
	DefaultDiskIface = "virtio"
	DefaultBootOrder = "hd"
	DefaultAccel     = "kvm"
	DefaultConsole   = "serial"
)

// telnet 控制台端口分配区间（决策 #485）：由 VM 名确定性派生，稳定可复算，
// 落在高位非特权端口段，避免与产品其它监听撞车。**唯一实现**在 model 层——
// 域组装（compute）与端口冲突校验（model.validate）共用同一函数，避免两处派生漂移。
const (
	ConsolePortMin = 5700
	ConsolePortMax = 6699
)

// ConsoleTelnetPort 由 VM 名确定性派生 telnet 控制台绑定端口（[ConsolePortMin, ConsolePortMax]）。
// 同名恒等、跨进程可复算；访问路径据此定位端口。
func ConsoleTelnetPort(vmName string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte("nfvis:console:" + vmName))
	return ConsolePortMin + int(h.Sum32()%uint32(ConsolePortMax-ConsolePortMin+1))
}

// EffectiveStartup 合并有效启动画像：实例覆盖 > 镜像画像 > 产品默认。
// 资源类字段（memory/vcpu/hugepage/numa/绑核）不在画像内——账本单一真源（AGENTS #481）。
func EffectiveStartup(vm VMFunction, profiles []VMImageProfile) VMImageProfile {
	var p VMImageProfile
	for _, prof := range profiles {
		if prof.Image == vm.Image {
			p = prof
			break
		}
	}
	if o := vm.Startup; o != nil {
		if o.Machine != "" {
			p.Machine = o.Machine
		}
		if o.CPUMode != "" {
			p.CPUMode = o.CPUMode
		}
		if o.CPUModel != "" {
			p.CPUModel = o.CPUModel
		}
		if o.Firmware != "" {
			p.Firmware = o.Firmware
		}
		if o.DiskIface != "" {
			p.DiskIface = o.DiskIface
		}
		if o.BootOrder != "" {
			p.BootOrder = o.BootOrder
		}
		if o.Accel != "" {
			p.Accel = o.Accel
		}
		if o.Console != "" {
			p.Console = o.Console
		}
		if len(o.ExtraArgs) > 0 {
			p.ExtraArgs = o.ExtraArgs
		}
	}
	if p.Machine == "" {
		p.Machine = DefaultMachine
	}
	if p.CPUMode == "" {
		p.CPUMode = DefaultCPUMode
	}
	if p.Firmware == "" {
		p.Firmware = DefaultFirmware
	}
	if p.DiskIface == "" {
		p.DiskIface = DefaultDiskIface
	}
	if p.BootOrder == "" {
		p.BootOrder = DefaultBootOrder
	}
	if p.Accel == "" {
		p.Accel = DefaultAccel
	}
	if p.Console == "" {
		p.Console = DefaultConsole
	}
	return p
}

// EffectiveConsole 返回 VM 的**有效控制台形态**（serial|telnet|vnc）及该形态是否由
// 画像/实例覆盖**显式设定**（决策 #485 契约 §4.3）。
//
// 显式设定的形态**权威**——独立于既有的 `serial_console` 开关；未显式设定时回落产品默认
// serial，此时是否真的落进域仍受 `serial_console` 约束（由域组装与读视图按 explicit 判定）。
// 域组装（compute）与访问路径/端口冲突校验（api/model）共用本函数，保证模型层与数据面一致。
func EffectiveConsole(vm VMFunction, profiles []VMImageProfile) (form string, explicit bool) {
	form = strings.ToLower(strings.TrimSpace(EffectiveStartup(vm, profiles).Console))
	if form == "" {
		form = DefaultConsole
	}
	for _, p := range profiles {
		if p.Image == vm.Image && strings.TrimSpace(p.Console) != "" {
			return form, true
		}
	}
	if vm.Startup != nil && strings.TrimSpace(vm.Startup.Console) != "" {
		return form, true
	}
	return form, false
}

// extraArgDenyPrefixes 窄自由口的拒绝前缀（决策 #485）：覆盖资源账本 / 抢走产品的
// 网络·磁盘·console 模型 / 有结构化字段可代替 / 影响进程生命周期。
var extraArgDenyPrefixes = []string{
	"-m", "-mem", "-smp", "-object", "-numa",
	"-kernel", "-initrd", "-append",
	"-drive", "-blockdev", "-netdev", "-nic", "-net",
	"-chardev", "-monitor", "-qmp", "-mon", "-serial",
	"-cpu", "-machine", "-boot", "-device", "-M",
	"-uuid", "-daemonize", "-pidfile", "-S",
}

// ValidateExtraArgs 校验额外 QEMU 参数（denylist）。每个元素可含多个空白分隔的 token。
func ValidateExtraArgs(args []string) error {
	for _, a := range args {
		for _, tok := range strings.Fields(a) {
			for _, p := range extraArgDenyPrefixes {
				if strings.HasPrefix(tok, p) {
					return fmt.Errorf("额外 QEMU 参数 %q 不被允许（%s 前缀）：该类参数要么覆盖资源账本，要么与产品的网络/磁盘/console 模型冲突；请改用对应的结构化字段（machine/cpu mode/firmware/disk-interface/boot-order/accel/console）", tok, p)
				}
			}
		}
	}
	return nil
}
