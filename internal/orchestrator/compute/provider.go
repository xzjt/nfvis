package compute

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// DefaultVhostUserQueues vhost-user vNIC 的 virtio 队列对数。
//
// 真机实测（libvirt 12/QEMU 10.2 + VPP 26.06，docs/M4-验收记录.md M4-4）：QEMU 未声明
// `<driver queues>`（1 对队列）时 VPP vhost-user 握手停在 protocol features，
// `show vhost-user` 报 `Memory regions (total 0)`、features 0x0，链路不 up；
// 显式置 2 对队列后握手完成（Memory regions ≥1、features 非零），链路随 VM 启停 up/down。
const DefaultVhostUserQueues = 2

// Provider libvirt 计算编排实现（FR-CMP-010~013）。
//
// 底座调用全部经 libvirtAPI / storageAPI / seedBuilder 接口注入：生产由
// Conn（conn_libvirt.go）与 qemuStorage/cloudLocaldsSeed（disk_libvirt.go）提供，
// 单测用 mock（provider_test.go）。本文件为业务逻辑，纳入覆盖率门槛。
type Provider struct {
	cfg   Config
	api   libvirtAPI
	store storageAPI
	seed  seedBuilder

	// vfPCI 解析 SR-IOV VF 的 PCI 地址（M4-4 注入 sysfs 实现）；nil = 明确报不支持。
	vfPCI func(pf string, vfID int) (string, error)

	// pciDeviceExists 通用 PCI 直通设备的存在性检查（FR-CMP-023，注入 sysfs 实现）；
	// nil = 明确报「未提供检查」（声明了设备就不静默放行，与 vfPCI 同口径）。
	pciDeviceExists func(bdf string) (bool, error)

	// alarms 恢复收敛告警落点（可空）。
	alarms orchestrator.AlarmSink

	// 测试注入：睡眠与轮询间隔（生产用 time.Sleep / 200ms）。
	sleep func(ctx context.Context, d time.Duration) error
	poll  time.Duration

	mu sync.Mutex
}

// NewProvider 构造计算编排 Provider（cfg 零值字段取 DefaultConfig）。
func NewProvider(cfg Config, api libvirtAPI, store storageAPI, seed seedBuilder) *Provider {
	def := DefaultConfig()
	if cfg.URI == "" {
		cfg.URI = def.URI
	}
	if cfg.VMsDir == "" {
		cfg.VMsDir = def.VMsDir
	}
	if cfg.VhostDir == "" {
		cfg.VhostDir = def.VhostDir
	}
	if cfg.ImagesDir == "" {
		cfg.ImagesDir = def.ImagesDir
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = def.StopTimeout
	}
	if cfg.StartProbeWindow <= 0 {
		cfg.StartProbeWindow = def.StartProbeWindow
	}
	if cfg.LibvirtLogDir == "" {
		cfg.LibvirtLogDir = def.LibvirtLogDir
	}
	return &Provider{
		cfg:   cfg,
		api:   api,
		store: store,
		seed:  seed,
		sleep: sleepCtx,
		poll:  200 * time.Millisecond,
	}
}

// SetAlarms 注入恢复收敛告警落点（M4-9；可空）。
func (p *Provider) SetAlarms(a orchestrator.AlarmSink) { p.alarms = a }

// SetVFResolver 注入 SR-IOV VF PCI 解析（M4-4）。
func (p *Provider) SetVFResolver(f func(pf string, vfID int) (string, error)) { p.vfPCI = f }

// SetPCIDeviceChecker 注入通用 PCI 直通设备的存在性检查（FR-CMP-023；生产见
// NewSysfsPCIDeviceChecker）。nil 且声明了设备时 define 明确报错，不静默放行。
func (p *Provider) SetPCIDeviceChecker(f func(bdf string) (bool, error)) { p.pciDeviceExists = f }

// Config 返回生效配置（只读）。
func (p *Provider) Config() Config { return p.cfg }

// DefineVM 声明式定义/重定义 domain（幂等）：准备落盘（主盘/数据盘/seed ISO）→
// 组装 XML → DomainDefineXML；autostart=true 且未运行时启动（FR-CMP-010）。
func (p *Provider) DefineVM(ctx context.Context, vm model.VMFunction, alloc model.AllocatedResources) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// FR-CMP-023：直通 PCI 设备的存在性检查（define/apply 前）——不存在的设备
	// 如实拒绝（否则 libvirt 定义会“成功”、启动才失败，且报错点离配置很远）。
	if err := p.checkPCIDevices(vm); err != nil {
		return err
	}
	spec, err := p.specFor(vm, alloc)
	if err != nil {
		return err
	}
	if err := p.prepare(ctx, vm, spec); err != nil {
		return err
	}
	xml, err := BuildDomainXML(spec)
	if err != nil {
		return fmt.Errorf("组装 VM %s domain XML: %w", vm.Name, err)
	}
	if err := p.api.Define(ctx, xml); err != nil {
		return fmt.Errorf("定义 VM %s: %w", vm.Name, err)
	}
	if err := p.api.SetAutostart(ctx, vm.Name, vm.Autostart); err != nil {
		return fmt.Errorf("设置 VM %s 自启标志: %w", vm.Name, err)
	}
	if vm.Autostart {
		state, exists, err := p.api.State(ctx, vm.Name)
		if err != nil {
			return err
		}
		if !exists || !isActiveState(state) {
			if err := p.api.Start(ctx, vm.Name); err != nil {
				return fmt.Errorf("按 autostart 启动 VM %s: %w", vm.Name, err)
			}
		}
	}
	return nil
}

// checkPCIDevices 直通 PCI 设备的存在性检查（FR-CMP-023，define/apply 前）。
//
// 逐设备查 sysfs（经 SetPCIDeviceChecker 注入的单一事实源）：不存在 ⇒ 如实拒绝并给
// 照做路径（lspci / ls /sys/bus/pci/devices 核对；若被数据面占用先 unbind-dpdk 释放）。
// **不做** vfio 绑定/解绑——预绑定由操作者/安装器负责（用户手册 §9.2）。
// 未注入检查器（nil）而声明了设备 ⇒ 明确报错，不静默放行（与 vfPCI 同口径）。
func (p *Provider) checkPCIDevices(vm model.VMFunction) error {
	if len(vm.PCIDevices) == 0 {
		return nil
	}
	if p.pciDeviceExists == nil {
		return fmt.Errorf("VM %s: 声明了直通 PCI 设备，但当前环境未提供存在性检查（需要 %s）", vm.Name, PCIDevicesDir)
	}
	seen := map[string]bool{}
	for _, raw := range vm.PCIDevices {
		norm, err := model.NormalizeBDF(raw)
		if err != nil {
			return fmt.Errorf("VM %s: %w", vm.Name, err)
		}
		if seen[norm] {
			// 模型校验已拦「同 VM 重复」；此处防御 REST/直接调用等绕过校验的路径。
			return fmt.Errorf("VM %s: 直通 PCI 设备 %s 重复声明", vm.Name, norm)
		}
		seen[norm] = true
		ok, err := p.pciDeviceExists(norm)
		if err != nil {
			return fmt.Errorf("VM %s: 检查直通 PCI 设备 %s 失败: %w", vm.Name, norm, err)
		}
		if !ok {
			return fmt.Errorf("VM %s: 直通 PCI 设备 %s 不在本机（%s/%s 不存在）——"+
				"请用 lspci 或 ls %s 核对设备地址；若该设备已被数据面占用，先 request interfaces <ifname|pci> unbind-dpdk 释放后再试",
				vm.Name, norm, PCIDevicesDir, norm, PCIDevicesDir)
		}
	}
	return nil
}

// DeleteVM 级联删除（FR-CMP-013）：运行中先强停 → 删除 domain 定义 → 清理落盘。
// 目标不存在时幂等返回 nil（重复删除不报错）。
func (p *Provider) DeleteVM(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		if isActiveState(state) {
			if err := p.api.Destroy(ctx, name); err != nil {
				return fmt.Errorf("停止 VM %s: %w", name, err)
			}
		}
		if err := p.api.Undefine(ctx, name); err != nil {
			return fmt.Errorf("删除 domain %s: %w", name, err)
		}
	}
	if err := p.store.RemoveAll(NewLayout(p.cfg.VMsDir, name).Dir); err != nil {
		return fmt.Errorf("清理 VM %s 落盘: %w", name, err)
	}
	return nil
}

// StartVM 启动（已运行则幂等返回）。
func (p *Provider) StartVM(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if isActiveState(state) {
		return nil
	}
	if err := p.checkDataPlaneForStart(name); err != nil {
		return err
	}
	if err := p.api.Start(ctx, name); err != nil {
		return fmt.Errorf("启动 VM %s: %w", name, err)
	}
	return nil
}

// checkDataPlaneForStart 启动路径的数据面前置判定（决策 #314）。
//
// 由来（round95 真机实证）：VPP 未运行时，带 vhost-user vNIC 的域在 libvirt DomainCreate
// （p.api.Start）内**阻塞**等待 VPP 的 vhost-user socket——请求在到达决策 #311 的探测窗口之前
// 就耗到客户端超时（90s），域还停在 paused。故判定必须落在**进入 api.Start 之前**：命中即
// 立即失败（不碰 libvirt，故不阻塞、不留 paused 残域），并给出恢复指引。
//
// 判定复用装配层注入的**既有** VPP 连接状态查询（单一事实源）；未注入（nil）时不判定——
// 正常路径与既有语义逐字/逐秒不变。
//
// 只在**启动动作**（start，以及 restart 的 off→start 分支）调用：已运行域的 start 是幂等
// 空操作、运行中域的 restart 走 ACPI 重启，二者都不经这里（不改变正常语义）。
func (p *Provider) checkDataPlaneForStart(name string) error {
	if p.cfg.DataPlaneProbe == nil {
		return nil
	}
	if err := p.cfg.DataPlaneProbe(); err != nil {
		return fmt.Errorf("%w：未启动虚拟机 %s（%v）；请先恢复数据面后重试——request vpp restart（自查：show vpp）",
			orchestrator.ErrDataPlaneUnavailable, name, err)
	}
	return nil
}

// StartVMChecked 启动并在探测窗口内回读域状态（决策 #311）。
//
// 复用 StartVM（幂等、错误语义不变），随后 probeStart 在窗口内观测：达到运行态即 OK；
// 窗口内停在非预期态则返回诊断（域状态 + reason + 域日志摘录 + 恢复建议）。
// 「已运行」的幂等情形会在首个观测点看到 running，故正常路径不额外等待。
func (p *Provider) StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error) {
	if err := p.StartVM(ctx, name); err != nil {
		return orchestrator.VMStartProbe{}, err
	}
	return p.probeStart(ctx, name), nil
}

// probeStart 在有界窗口内轮询域状态：running/blocked 即 OK；窗口内未达运行态则组装诊断。
func (p *Provider) probeStart(ctx context.Context, name string) orchestrator.VMStartProbe {
	deadline := time.Now().Add(p.cfg.StartProbeWindow)
	var lastState, lastReason int
	for {
		st, rs, exists, err := p.api.StateReason(ctx, name)
		if err != nil {
			// 状态读不出来：如实报「查询失败」，不臆测（不把它当启动失败的原因）。
			return orchestrator.VMStartProbe{
				State:   "",
				Reason:  "查询域状态失败: " + err.Error(),
				LogPath: p.domainLogPath(name),
				Hints:   startRecoveryHints(""),
			}
		}
		lastState, lastReason = st, rs
		state := orchestrator.VMStateAbsent
		if exists {
			state = VMStateFromLibvirtReason(st, rs)
		}
		if state == orchestrator.VMStateRunning {
			return orchestrator.VMStartProbe{OK: true, State: state}
		}
		if !time.Now().Before(deadline) {
			break
		}
		if err := p.sleep(ctx, p.poll); err != nil {
			break // ctx 取消：按当前观测如实返回
		}
	}
	out := orchestrator.VMStartProbe{
		State:   VMStateFromLibvirtReason(lastState, lastReason),
		Reason:  StateReasonText(lastState, lastReason),
		LogPath: p.domainLogPath(name),
		Hints:   startRecoveryHints(VMStateFromLibvirtReason(lastState, lastReason)),
	}
	// 域已不存在（缺定义）：StateReason 会返回 exists=false，reason 文本无意义。
	if _, _, exists, err := p.api.StateReason(ctx, name); err == nil && !exists {
		out.State = orchestrator.VMStateAbsent
		out.Reason = "域已不存在（定义缺失或被删除）"
	}
	out.Detail = p.readDomainLogTail(name)
	return out
}

// domainLogPath libvirt 域日志路径（缺省 /var/log/libvirt/qemu/<name>.log）。
func (p *Provider) domainLogPath(name string) string {
	return path.Join(p.cfg.LibvirtLogDir, name+".log")
}

// readDomainLogTail 读域日志末尾若干行（取不到返回空串——调用方据此如实说「未取到」，不编造）。
func (p *Provider) readDomainLogTail(name string) string {
	if p.store == nil {
		return ""
	}
	txt, err := p.store.ReadTail(p.domainLogPath(name), 15)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(txt)
}

// startRecoveryHints 启动失败时的恢复建议（决策 #311）：只给产品**已验证**的可照做路径，
// 不臆测根因。state 为观测到的契约运行态（可为空）。
func startRecoveryHints(state string) []string {
	hints := []string{
		"确认数据面 VPP 正在运行（show vpp status）；vhost-user vNIC 需要 VPP 先建立该 VM 的 socket",
	}
	switch state {
	case orchestrator.VMStatePaused:
		hints = append(hints,
			"按上面的 socket 事实排查后 request vpp restart 重建数据面，再重新 start",
			"用 show virtual-machine-functions <n> detail 与 request virtual-machine-functions <n> console 深入")
	case orchestrator.VMStateCrashed:
		hints = append(hints,
			"域已崩溃并保留现场：查看上方 libvirt 日志与 guest 控制台（request virtual-machine-functions <n> console）",
			"request vpp restart 后重新 start；仍失败请用 show virtual-machine-functions <n> detail 核对资源分配")
	case orchestrator.VMStateShutoff, orchestrator.VMStateAbsent:
		hints = append(hints,
			"域已退出/不存在：确认大页与内存足够、主盘镜像就绪（show images）",
			"request vpp restart 后重新 start；仍失败请查 libvirt 日志")
	default:
		hints = append(hints, "request vpp restart 后重新 start；仍失败请查 libvirt 日志")
	}
	return hints
}

// StopVM ACPI 关机，超时强杀（契约 stop 语义，FR-CMP-011）。
func (p *Provider) StopVM(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if !isActiveState(state) {
		return nil
	}
	if err := p.api.Shutdown(ctx, name); err != nil {
		return fmt.Errorf("关机 VM %s: %w", name, err)
	}
	deadline := time.Now().Add(p.cfg.StopTimeout)
	for {
		state, exists, err = p.api.State(ctx, name)
		if err != nil {
			return err
		}
		if !exists || !isActiveState(state) {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		if err := p.sleep(ctx, p.poll); err != nil {
			return err
		}
	}
	if err := p.api.Destroy(ctx, name); err != nil {
		return fmt.Errorf("VM %s 关机超时后强杀: %w", name, err)
	}
	return nil
}

// RestartVM 运行中 ACPI 重启；已关机则直接启动。
func (p *Provider) RestartVM(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if !isActiveState(state) {
		// off→start 分支同样做数据面前置判定（决策 #314）：与 start 一样会在 api.Start 阻塞。
		if err := p.checkDataPlaneForStart(name); err != nil {
			return err
		}
		if err := p.api.Start(ctx, name); err != nil {
			return fmt.Errorf("启动 VM %s: %w", name, err)
		}
		return nil
	}
	if err := p.api.Reboot(ctx, name); err != nil {
		return fmt.Errorf("重启 VM %s: %w", name, err)
	}
	return nil
}

// RefreshSeed 按当前配置重建 cloud-init seed（决策 #114）。
//
// 由来：user-data 的取值可以是**文件路径**（命令树承诺「文本或文件」），文件内容变化
// 不改变配置值——而 seed 只在配置变更（DefineVM）时重建，于是「改了 user-data 文件、
// 重启 VM」会静默无效（round34 真机实证：guest 里 cloud-init 报 previously ran、
// 跑的还是旧脚本）。启动/重启前调用即幂等修正（cloud-localds 生成 ~50ms）。
func (p *Provider) RefreshSeed(ctx context.Context, vm model.VMFunction) error {
	if vm.CloudInit == nil {
		return nil
	}
	if p.seed == nil {
		return fmt.Errorf("VM %s 配置了 cloud-init，但 seed 生成未启用", vm.Name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	iso := NewLayout(p.cfg.VMsDir, vm.Name).SeedISO
	if err := p.store.EnsureDir(path.Dir(iso)); err != nil {
		return err
	}
	if err := p.seed.Build(ctx, vm, iso); err != nil {
		return fmt.Errorf("重建 VM %s cloud-init seed: %w", vm.Name, err)
	}
	return nil
}

// VMState 运行态（契约枚举；未定义返回 absent）。reason 感知：被 kill 的 QEMU
// 报 SHUTOFF+CRASHED，映射为 crashed（FR-CMP-017）。
func (p *Provider) VMState(ctx context.Context, name string) (string, error) {
	state, reason, exists, err := p.api.StateReason(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return orchestrator.VMStateAbsent, nil
	}
	return VMStateFromLibvirtReason(state, reason), nil
}

// Console 打开 VM 串口双向流（FR-CMP-014）：须域存在且运行中。
func (p *Provider) Console(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if !isActiveState(state) {
		return nil, fmt.Errorf("VM %s 未运行，串口 console 不可用（当前 %s）", name, VMStateFromLibvirt(state))
	}
	return p.api.OpenConsole(ctx, name)
}

// EnsureConsistent 恢复收敛（FR-OPS-010/012）：按 committed 配置补建/修正缺失 domain。
// 单个对象失败不阻塞其余对象，错误由调用方转告警。
func (p *Provider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, vm := range cfg.VirtualMachineFunctions {
		err := p.DefineVM(ctx, vm, model.AllocationFor(cfg, vm))
		if p.alarms != nil {
			if err != nil {
				p.alarms.Raise(orchestrator.RecoveryScopeCompute, "warning", orchestrator.RecoveryUnconverged,
					fmt.Sprintf("VM %s 未收敛：%v", vm.Name, err), vm.Name)
			} else {
				p.alarms.Resolve(orchestrator.RecoveryScopeCompute, orchestrator.RecoveryUnconverged, vm.Name)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("VM %s: %w", vm.Name, err))
		}
	}
	return errs
}

// prepare 落盘准备：主盘克隆、数据盘创建、cloud-init seed 生成。均幂等。
func (p *Provider) prepare(ctx context.Context, vm model.VMFunction, spec DomainSpec) error {
	if spec.DiskFormat != "iso" {
		if !p.store.Exists(spec.DiskPath) {
			image := p.imagePath(vm.Image)
			if !p.store.Exists(image) {
				return fmt.Errorf("VM %s 的镜像 %q 不在仓库中（%s；先用 request images … 取回镜像仓库）", vm.Name, vm.Image, image)
			}
			if err := p.store.EnsureDir(path.Dir(spec.DiskPath)); err != nil {
				return err
			}
			if err := p.store.Clone(ctx, image, spec.DiskPath); err != nil {
				return fmt.Errorf("克隆 VM %s 主盘: %w", vm.Name, err)
			}
		}
	}
	for _, dd := range spec.DataDisks {
		if p.store.Exists(dd.Path) {
			continue
		}
		if err := p.store.EnsureDir(path.Dir(dd.Path)); err != nil {
			return err
		}
		var src string
		for _, d := range vm.Disks {
			if d.Name == dd.Name {
				src = d.Image
			}
		}
		if src != "" {
			image := p.imagePath(src)
			if !p.store.Exists(image) {
				return fmt.Errorf("VM %s 数据盘 %s 引用的镜像 %q 不在仓库中", vm.Name, dd.Name, src)
			}
			if err := p.store.Clone(ctx, image, dd.Path); err != nil {
				return fmt.Errorf("克隆 VM %s 数据盘 %s: %w", vm.Name, dd.Name, err)
			}
			continue
		}
		sizeGB := 1
		for _, d := range vm.Disks {
			if d.Name == dd.Name && d.SizeGB > 0 {
				sizeGB = d.SizeGB
			}
		}
		if err := p.store.CreateBlank(ctx, dd.Path, sizeGB); err != nil {
			return fmt.Errorf("创建 VM %s 数据盘 %s: %w", vm.Name, dd.Name, err)
		}
	}
	if vm.CloudInit != nil {
		if p.seed == nil {
			return fmt.Errorf("VM %s 配置了 cloud-init，但 seed 生成未启用", vm.Name)
		}
		if err := p.store.EnsureDir(path.Dir(spec.SeedISO)); err != nil {
			return err
		}
		if err := p.seed.Build(ctx, vm, spec.SeedISO); err != nil {
			return fmt.Errorf("生成 VM %s cloud-init seed: %w", vm.Name, err)
		}
	}
	return nil
}

// specFor 由配置 + 资源分配组装 DomainSpec（纯逻辑）。
// vhost-user socket 路径确定性派生（M4-4 由 VPP 侧创建同路径 socket）。
func (p *Provider) specFor(vm model.VMFunction, alloc model.AllocatedResources) (DomainSpec, error) {
	layout := NewLayout(p.cfg.VMsDir, vm.Name)
	diskPath, diskFormat := layout.DiskPath, DefaultDiskFormat
	if ImageIsISO(vm.Image) {
		diskPath, diskFormat = p.imagePath(vm.Image), "iso"
	}
	spec := DomainSpec{
		VM:           vm,
		Cores:        alloc.Cores,
		HugepageSize: alloc.HugepageSize,
		DiskPath:     diskPath,
		DiskFormat:   diskFormat,
		Emulator:     p.cfg.Emulator,
		Machine:      p.cfg.Machine,
		CPUMode:      p.cfg.CPUMode,
		PCIDevices:   append([]string(nil), vm.PCIDevices...), // FR-CMP-023（存在性检查在 DefineVM 前完成）
	}
	if vm.CloudInit != nil {
		spec.SeedISO = layout.SeedISO
	}
	for _, d := range vm.Disks {
		spec.DataDisks = append(spec.DataDisks, DataDiskSpec{
			Name: d.Name,
			Path: DataDiskPath(p.cfg.VMsDir, vm.Name, d.Name),
		})
	}
	for _, nic := range vm.Interfaces {
		is := InterfaceSpec{Name: nic.Name, MAC: nic.MAC, Type: nic.Type}
		switch nic.Type {
		case IfaceVhostUser:
			is.Socket = VhostSocketPath(p.cfg.VhostDir, vm.Name, nic.Name)
			if is.Queues == 0 {
				is.Queues = DefaultVhostUserQueues
			}
		case IfaceSriovVF:
			if nic.Sriov == nil {
				return DomainSpec{}, fmt.Errorf("VM %s: vNIC %s（sriov-vf）缺少 sriov 绑定", vm.Name, nic.Name)
			}
			if p.vfPCI == nil {
				return DomainSpec{}, fmt.Errorf("VM %s: vNIC %s 为 sriov-vf，但当前环境未提供 VF PCI 解析（SR-IOV 在该环境不可用）", vm.Name, nic.Name)
			}
			pci, err := p.vfPCI(nic.Sriov.PhysicalInterface, nic.Sriov.VFID)
			if err != nil {
				return DomainSpec{}, fmt.Errorf("VM %s: vNIC %s 解析 VF PCI: %w", vm.Name, nic.Name, err)
			}
			is.VFPCI = pci
		default:
			return DomainSpec{}, fmt.Errorf("VM %s: vNIC %s 类型 %q 不受支持", vm.Name, nic.Name, nic.Type)
		}
		spec.Interfaces = append(spec.Interfaces, is)
	}
	return spec, nil
}

func (p *Provider) imagePath(image string) string { return path.Join(p.cfg.ImagesDir, image) }

// sleepCtx 可取消睡眠。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CheckVMAlarms 检测 VM 异常退出并维护告警（FR-CMP-017）：
// libvirt 状态 crashed（on_crash=preserve 保留）→ critical `VM_CRASHED`；恢复 running/shutoff → 消警。
// absent（配置存在但域未定义）由 EnsureConsistent 的收敛告警负责，不在此重复。
func (p *Provider) CheckVMAlarms(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, vm := range cfg.VirtualMachineFunctions {
		state, err := p.VMState(ctx, vm.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("VM %s 状态查询: %w", vm.Name, err))
			continue
		}
		if p.alarms == nil {
			continue
		}
		if state == orchestrator.VMStateCrashed {
			p.alarms.Raise(orchestrator.RecoveryScopeCompute, orchestrator.SeverityCritical, orchestrator.VMCrashed,
				fmt.Sprintf("VM %s 异常退出（crashed）", vm.Name), vm.Name)
			continue
		}
		p.alarms.Resolve(orchestrator.RecoveryScopeCompute, orchestrator.VMCrashed, vm.Name)
	}
	return errs
}

// ---------- 快照（FR-CMP-015） ----------

// SnapshotCreate 创建快照（含全部磁盘；qcow2 内部快照）。
func (p *Provider) SnapshotCreate(ctx context.Context, domain, name, description string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	xml, err := p.api.DumpDomainXML(ctx, domain)
	if err != nil {
		return err
	}
	snapXML, err := BuildSnapshotXML(name, description, DiskTargetsOf(xml))
	if err != nil {
		return err
	}
	return p.api.SnapshotCreate(ctx, domain, snapXML)
}

// Snapshots 列出快照。
func (p *Provider) Snapshots(ctx context.Context, domain string) ([]SnapshotInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.api.SnapshotList(ctx, domain)
}

// SnapshotRevert 回滚到快照。
func (p *Provider) SnapshotRevert(ctx context.Context, domain, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.api.SnapshotRevert(ctx, domain, name)
}

// SnapshotDelete 删除快照。
func (p *Provider) SnapshotDelete(ctx context.Context, domain, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.api.SnapshotDelete(ctx, domain, name)
}
