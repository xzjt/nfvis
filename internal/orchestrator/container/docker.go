// Package container 实现 Docker 侧容器编排（M4-7，FR-CMP-020~022）。
//
// 分层（沿用网络/计算编排约定）：
//   - 本文件为业务逻辑（规格组装、状态映射、生命周期），单测注入 dockerAPI 假实现；
//   - docker_client_docker.go 为 Docker Engine API 薄适配层（文件名 _docker.go →
//     覆盖率排除），由 nfvis-vm 集成测试覆盖。
//
// memif：VPP 侧 endpoint 由网络编排（network.MemifProvider）创建（apply 计划把容器
// memif vNIC 与 VM vhost-user vNIC 一并下发）；本包只把 memif socket 挂载进容器。
package container

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// Config 容器编排配置。
type Config struct {
	Socket      string // Docker Engine API unix socket
	MemifDir    string // 宿主 memif socket 目录（与 network/applier 一致）
	SocketDir   string // 容器内 memif socket 挂载目录
	DefaultTail int    // 日志缺省行数
}

// DefaultConfig 生产缺省。
func DefaultConfig() Config {
	return Config{Socket: "/var/run/docker.sock", MemifDir: orchestrator.DefaultMemifDir,
		SocketDir: "/run/memif", DefaultTail: 100}
}

// DeviceMapping 容器设备映射。
type DeviceMapping struct {
	PathOnHost        string
	PathInContainer   string
	CgroupPermissions string
}

// CreateSpec 容器创建规格（与 Docker Engine API 字段一一对应，便于单测断言）。
type CreateSpec struct {
	Image         string
	Entrypoint    []string
	Cmd           []string
	Env           []string
	MemoryBytes   int64
	NanoCPUs      int64
	RestartPolicy string // no | on-failure
	Binds         []string
	Privileged    bool
	CapAdd        []string
	Devices       []DeviceMapping
}

// dockerAPI Docker Engine API 能力（真实实现 = dockerClient；单测用 mock）。
type dockerAPI interface {
	Create(ctx context.Context, name string, spec CreateSpec) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	Remove(ctx context.Context, name string, force bool) error
	// RemoveImage 删除容器镜像（FR-CMP-033，经 Docker API）。
	RemoveImage(ctx context.Context, ref string) error
	// LoadImage 载入容器镜像归档（FR-CMP-031，经 Docker API `image load`）。
	LoadImage(ctx context.Context, path string) error
	State(ctx context.Context, name string) (state string, exists bool, err error)
	// ExitCode 返回容器退出码（不存在 exists=false）。
	ExitCode(ctx context.Context, name string) (code int, exists bool, err error)
	Logs(ctx context.Context, name string, tail int) (string, error)
}

// Provider 容器编排实现。
type Provider struct {
	cfg Config
	api dockerAPI
	mu  sync.Mutex

	alarms orchestrator.AlarmSink // 恢复收敛告警落点（M4-9，可空）
}

// SetAlarms 注入恢复收敛告警落点。
func (p *Provider) SetAlarms(a orchestrator.AlarmSink) { p.alarms = a }

// NewProvider 构造容器编排 Provider。
func NewProvider(cfg Config, api dockerAPI) *Provider {
	def := DefaultConfig()
	if cfg.Socket == "" {
		cfg.Socket = def.Socket
	}
	if cfg.MemifDir == "" {
		cfg.MemifDir = def.MemifDir
	}
	if cfg.SocketDir == "" {
		cfg.SocketDir = def.SocketDir
	}
	if cfg.DefaultTail <= 0 {
		cfg.DefaultTail = def.DefaultTail
	}
	return &Provider{cfg: cfg, api: api}
}

// Config 返回生效配置。
func (p *Provider) Config() Config { return p.cfg }

// ApplyContainer 声明式创建/重建容器（幂等）：已存在则校正启动状态（autostart）。
func (p *Provider) ApplyContainer(ctx context.Context, ct model.ContainerFunction) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	spec := BuildCreateSpec(ct, p.cfg)
	state, exists, err := p.api.State(ctx, ct.Name)
	if err != nil {
		return err
	}
	if !exists {
		if err := p.api.Create(ctx, ct.Name, spec); err != nil {
			return fmt.Errorf("创建容器 %s: %w", ct.Name, err)
		}
	} else if state == orchestrator.CTStateRunning {
		return nil // 已运行：幂等返回（配置变更需先删除重建，M4-7 语义）
	}
	if ct.Autostart {
		if err := p.api.Start(ctx, ct.Name); err != nil {
			return fmt.Errorf("按 autostart 启动容器 %s: %w", ct.Name, err)
		}
	}
	return nil
}

// DeleteContainer 删除容器（强制，级联其网络；不存在幂等）。
func (p *Provider) DeleteContainer(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := p.api.Remove(ctx, name, true); err != nil {
		return fmt.Errorf("删除容器 %s: %w", name, err)
	}
	return nil
}

// StartContainer 启动（已运行幂等）。
func (p *Provider) StartContainer(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if state == orchestrator.CTStateRunning {
		return nil
	}
	if err := p.api.Start(ctx, name); err != nil {
		return fmt.Errorf("启动容器 %s: %w", name, err)
	}
	return nil
}

// StopContainer 停止（已停止幂等）。
func (p *Provider) StopContainer(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if state != orchestrator.CTStateRunning {
		return nil
	}
	if err := p.api.Stop(ctx, name); err != nil {
		return fmt.Errorf("停止容器 %s: %w", name, err)
	}
	return nil
}

// RestartContainer 重启。
func (p *Provider) RestartContainer(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists, err := p.api.State(ctx, name); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if err := p.api.Restart(ctx, name); err != nil {
		return fmt.Errorf("重启容器 %s: %w", name, err)
	}
	return nil
}

// ContainerState 契约枚举（absent=不存在）。
func (p *Provider) ContainerState(ctx context.Context, name string) (string, error) {
	state, exists, err := p.api.State(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return orchestrator.CTStateAbsent, nil
	}
	return state, nil
}

// ContainerLogs 容器 stdout/stderr 最近 tail 行。
func (p *Provider) ContainerLogs(ctx context.Context, name string, tail int) (string, error) {
	if tail <= 0 {
		tail = p.cfg.DefaultTail
	}
	if _, exists, err := p.api.State(ctx, name); err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	return p.api.Logs(ctx, name, tail)
}

// EnsureConsistent 恢复收敛（FR-OPS-010/012）：补建缺失容器；单对象失败不阻塞其余。
func (p *Provider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, ct := range cfg.ContainerFunctions {
		err := p.ApplyContainer(ctx, ct)
		if p.alarms != nil {
			if err != nil {
				p.alarms.Raise(orchestrator.RecoveryScopeContainer, "warning", orchestrator.RecoveryUnconverged,
					fmt.Sprintf("容器 %s 未收敛：%v", ct.Name, err), ct.Name)
			} else {
				p.alarms.Resolve(orchestrator.RecoveryScopeContainer, orchestrator.RecoveryUnconverged, ct.Name)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("容器 %s: %w", ct.Name, err))
		}
	}
	return errs
}

// CheckContainerAlarms 检测容器异常退出并维护告警（FR-CMP-022）：
// dead 或 exited 且退出码非零 → critical `CONTAINER_EXITED`；running/正常退出 → 消警。
func (p *Provider) CheckContainerAlarms(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, ct := range cfg.ContainerFunctions {
		state, exists, err := p.api.State(ctx, ct.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("容器 %s 状态查询: %w", ct.Name, err))
			continue
		}
		if !exists || p.alarms == nil {
			continue
		}
		abnormal := state == orchestrator.CTStateDead
		if state == orchestrator.CTStateExited {
			if code, _, cerr := p.api.ExitCode(ctx, ct.Name); cerr == nil && code != 0 {
				abnormal = true
			}
		}
		if abnormal {
			p.alarms.Raise(orchestrator.RecoveryScopeContainer, orchestrator.SeverityCritical, orchestrator.ContainerExited,
				fmt.Sprintf("容器 %s 异常退出（状态 %s，FR-CMP-022）", ct.Name, state), ct.Name)
			continue
		}
		p.alarms.Resolve(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, ct.Name)
	}
	return errs
}

// LoadImage 载入容器镜像归档（供镜像仓库导入容器镜像时调用）。
func (p *Provider) LoadImage(ctx context.Context, path string) error {
	return p.api.LoadImage(ctx, path)
}

// RemoveImage 删除容器镜像（供镜像仓库删除容器镜像时调用）。
func (p *Provider) RemoveImage(ctx context.Context, ref string) error {
	if err := p.api.RemoveImage(ctx, ref); err != nil {
		return fmt.Errorf("删除容器镜像 %s: %w", ref, err)
	}
	return nil
}

// BuildCreateSpec 由容器配置组装创建规格（纯函数，单测覆盖）。
func BuildCreateSpec(ct model.ContainerFunction, cfg Config) CreateSpec {
	spec := CreateSpec{
		Image:         ct.Image,
		MemoryBytes:   int64(ct.MemoryMB) * 1024 * 1024,
		NanoCPUs:      int64(ct.VCPU) * 1_000_000_000,
		RestartPolicy: ct.RestartPolicy,
		Privileged:    len(ct.Interfaces) > 0, // memif 需要网络特权
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = "no"
	}
	// command → Entrypoint（覆盖镜像 entrypoint）；args → Cmd。
	if strings.TrimSpace(ct.Command) != "" {
		spec.Entrypoint = []string{strings.TrimSpace(ct.Command)}
	}
	spec.Cmd = append([]string(nil), ct.Args...)
	// env：map 顺序不确定，键排序保证可复现。
	keys := make([]string, 0, len(ct.Env))
	for k := range ct.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.Env = append(spec.Env, k+"="+ct.Env[k])
	}
	for _, nic := range ct.Interfaces {
		if nic.Type != "memif" {
			continue
		}
		spec.Binds = append(spec.Binds,
			orchestrator.MemifSocketPath(cfg.MemifDir, ct.Name, nic.Name)+":"+cfg.SocketDir+"/"+nic.Name+".sock")
		spec.CapAdd = append(spec.CapAdd, "NET_ADMIN")
	}
	return spec
}

// dockerStateToContract Docker 状态 → 契约枚举（running/exited/dead）。
func dockerStateToContract(status string) string {
	switch strings.ToLower(status) {
	case "running", "restarting", "paused", "created", "removing":
		// created/paused/restarting 均视为“存在且未正常退出”；created 尚未启动，归 running
		// 会误导，故 created 归 exited（未运行）。
		if strings.EqualFold(status, "created") {
			return orchestrator.CTStateExited
		}
		return orchestrator.CTStateRunning
	case "dead":
		return orchestrator.CTStateDead
	default: // exited / 未知
		return orchestrator.CTStateExited
	}
}
