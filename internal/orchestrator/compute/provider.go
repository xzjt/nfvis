package compute

import (
	"context"
	"fmt"
	"path"
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
	return &Provider{
		cfg:   cfg,
		api:   api,
		store: store,
		seed:  seed,
		sleep: sleepCtx,
		poll:  200 * time.Millisecond,
	}
}

// SetVFResolver 注入 SR-IOV VF PCI 解析（M4-4）。
func (p *Provider) SetVFResolver(f func(pf string, vfID int) (string, error)) { p.vfPCI = f }

// Config 返回生效配置（只读）。
func (p *Provider) Config() Config { return p.cfg }

// DefineVM 声明式定义/重定义 domain（幂等）：准备落盘（主盘/数据盘/seed ISO）→
// 组装 XML → DomainDefineXML；autostart=true 且未运行时启动（FR-CMP-010）。
func (p *Provider) DefineVM(ctx context.Context, vm model.VMFunction, alloc model.AllocatedResources) error {
	p.mu.Lock()
	defer p.mu.Unlock()

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
	if err := p.api.Start(ctx, name); err != nil {
		return fmt.Errorf("启动 VM %s: %w", name, err)
	}
	return nil
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

// VMState 运行态（契约枚举；未定义返回 absent）。
func (p *Provider) VMState(ctx context.Context, name string) (string, error) {
	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return orchestrator.VMStateAbsent, nil
	}
	return VMStateFromLibvirt(state), nil
}

// EnsureConsistent 恢复收敛（FR-OPS-010/012）：按 committed 配置补建/修正缺失 domain。
// 单个对象失败不阻塞其余对象，错误由调用方转告警。
func (p *Provider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, vm := range cfg.VirtualMachineFunctions {
		if err := p.DefineVM(ctx, vm, model.AllocationFor(cfg, vm)); err != nil {
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
				return fmt.Errorf("VM %s 的镜像 %q 不在仓库中（%s；导入见 M4-8）", vm.Name, vm.Image, image)
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
			return fmt.Errorf("VM %s 配置了 cloud-init，但 seed 生成未启用（FR-CMP-016）", vm.Name)
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
				return DomainSpec{}, fmt.Errorf("VM %s: vNIC %s 为 sriov-vf，但当前环境未提供 VF PCI 解析（SR-IOV 接入见 M4-4）", vm.Name, nic.Name)
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
