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
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

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

// ContainerFacts 一次容器 inspect 取回的运行态事实（决策 #432）。
//
// **一次 inspect 供两处读数**：契约状态（State/RawState）与重启次数（RestartCount）都从这里
// 取回——读数（CLI/REST）与巡检都不得为此再发一次 inspect。RawState 是 Docker `State.Status`
// 原文（`restarting` 等）：契约映射把 restarting 并入 running（规格 #44，刻意不改），巡检判
// 崩溃重启循环必须看原文。
type ContainerFacts struct {
	State    string // 契约枚举（running/exited/dead）
	RawState string // Docker State.Status 原文
	// RestartCount 为 Docker State.RestartCount；RestartsKnown=false 表示应答里**没有**该字段
	// （取不到），读视图据此**省略**而不给 0。
	RestartCount  int
	RestartsKnown bool
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
	// LoadImage 载入容器镜像归档（FR-CMP-031，经 Docker API `image load`）；
	// name 为仓库目录项名，载入后按它重打标签 `<名>:latest`（决策 #160）。
	LoadImage(ctx context.Context, path, name string) error
	State(ctx context.Context, name string) (state string, exists bool, err error)
	// Inspect 单次 inspect 的容器事实（决策 #432）：契约状态 + Docker 原始状态 + 重启次数。
	// 不存在 exists=false（非错误）。
	Inspect(ctx context.Context, name string) (ContainerFacts, bool, error)
	// ExitCode 返回容器退出码（不存在 exists=false）。
	ExitCode(ctx context.Context, name string) (code int, exists bool, err error)
	// OOMKilled 返回容器是否因内存超限被终止（Docker State.OOMKilled；不存在 exists=false）。
	// 用它与退出码共同判定「异常退出」：docker stop 的正常结果是 137/143，不以此为故障。
	OOMKilled(ctx context.Context, name string) (killed bool, exists bool, err error)
	Logs(ctx context.Context, name string, tail int) (string, error)
	// Exec 在运行中的容器内执行命令（决策 #357；非 TTY ⇒ 多路复用流，见 exec.go）。
	// timeout 只界定**客户端等待**：超时返回 TimedOut=true 且不带退出码（容器内进程可能仍在跑）。
	Exec(ctx context.Context, name, command string, timeout time.Duration) (ExecResult, error)
	// ExecShell 打开容器内的交互式 TTY（决策 #358）：返回全双工流，Close 即关会话。
	ExecShell(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

// Provider 容器编排实现。
type Provider struct {
	cfg Config
	api dockerAPI
	mu  sync.Mutex

	alarms orchestrator.AlarmSink // 恢复收敛告警落点（M4-9，可空）

	// restartSeen 上次巡检观察到的重启次数（容器 → 次数，决策 #432）。判据要的是「连续两次
	// 巡检间有增长」，故必须记住上一次读数；只记在内存里（重启 nfvisd 后重新起算，不编造历史）。
	restartSeen map[string]int
	// restartMu 只保护 restartSeen（巡检可能与 CLI/REST 读数并发，读数路径不碰它）。
	restartMu sync.Mutex
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
	st, _, _, err := p.ContainerStatus(ctx, name)
	return st, err
}

// ContainerStatus 容器运行态读数（决策 #432）：契约状态 + 重启次数，**同一次 inspect** 取回
// （读视图与巡检都不得为此再发第二次请求）。
//
// restartKnown=false ⇒ 重启次数取不到（底座应答里没有该字段）：调用方**省略**该读数，不得
// 回落为 0（「取不到」与「0 次」是两件事）。错误语义与 ContainerState 逐字一致：
// 不存在 ⇒ state=absent 且无错误；底座不可达/无响应 ⇒ ErrContainerUnavailable（API 503）；
// 调用方取消 ⇒ 原样透传。
func (p *Provider) ContainerStatus(ctx context.Context, name string) (state string, restartCount int, restartKnown bool, err error) {
	facts, exists, err := p.api.Inspect(ctx, name)
	if err != nil {
		// 决策 #375（R142 B8）：底座不可达/无响应归一为 ErrContainerUnavailable（API 503）；
		// 调用方取消原样透传。容器不存在走 exists=false（CTStateAbsent）而非错误。
		return "", 0, false, normalizeDockerErr(name, wrapDockerTimeout(err))
	}
	if !exists {
		return orchestrator.CTStateAbsent, 0, false, nil
	}
	return facts.State, facts.RestartCount, facts.RestartsKnown, nil
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

// ContainerExec 在**运行中**的容器内执行命令（决策 #357，非交互）。
//
// 前置由本层判定（不靠字符串匹配 Docker 错误）：容器不存在 ⇒ ErrVMNotFound（API 404）、
// 非运行态 ⇒ ErrContainerNotRunning（API 409）。命令跑完（哪怕非 0 退出码）不算失败——
// 退出码是**结果**；只有「没跑完」（超时/流中断）才由调用方按失败处置。
func (p *Provider) ContainerExec(ctx context.Context, name, command string, timeout time.Duration) (ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, fmt.Errorf("命令不能为空")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// State 检查有界（决策 #366，R142-12 同族）：dockerd 假死时 exec 的第一个挂点。
	// 真机 SIGSTOP 实测：漏掉这里，CLI 面（调用方 context.Background()）的 exec 会
	// 挂到底座恢复后幽灵完成（审计记 success）。
	ectx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	state, exists, err := p.api.State(ectx, name)
	if err != nil {
		return ExecResult{}, normalizeDockerErr(name, wrapDockerTimeout(err))
	}
	if !exists {
		return ExecResult{}, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if state != orchestrator.CTStateRunning {
		return ExecResult{}, fmt.Errorf("%w: %s（当前 %s）", orchestrator.ErrContainerNotRunning, name, state)
	}
	res, err := p.api.Exec(ctx, name, command, timeout)
	if err != nil {
		// 归一：State 与 Exec 之间容器被删的竞态（docker 404）⇒ ErrVMNotFound；
		// 底座不可达/无响应 ⇒ ErrContainerUnavailable（API 503）。已捕获的部分输出保留。
		return res, normalizeDockerErr(name, err)
	}
	return res, nil
}

// ContainerShell 打开**运行中**容器的交互式终端（决策 #358）。
//
// 前置判定与 exec 同口径（不存在 ⇒ ErrVMNotFound、非运行态 ⇒ ErrContainerNotRunning）；
// 返回的全双工流由调用方（WS 桥接）持有；Close 只关产品侧流——容器内的 shell 进程可能仍在运行
// （Docker 不提供 exec 进程的中止接口，round139 真机实测）。
func (p *Provider) ContainerShell(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	// State 检查有界（决策 #366，R142-12）：dockerd 假死时这里是 shell 打开路径的
	// 第一个挂点（真机 SIGSTOP 实测挂满观察窗口）——10s 内不答即如实报错。
	sctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	state, exists, err := p.api.State(sctx, name)
	if err != nil {
		return nil, normalizeDockerErr(name, wrapDockerTimeout(err))
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if state != orchestrator.CTStateRunning {
		return nil, fmt.Errorf("%w: %s（当前 %s）", orchestrator.ErrContainerNotRunning, name, state)
	}
	stream, err := p.api.ExecShell(ctx, name)
	if err != nil {
		return nil, normalizeDockerErr(name, err)
	}
	return stream, nil
}

// normalizeDockerErr 把底座错误归一为编排层 sentinel（决策 #375，R142 B8）：
//   - docker 404（State 与 Exec 之间容器被删的竞态）⇒ ErrVMNotFound（API 404）；
//   - 调用方取消（ctx canceled）⇒ 原样透传（不是底座故障，也不该 503）；
//   - 其余（底座不可达/无响应/超时、dockerd 侧异常）⇒ ErrContainerUnavailable（API 503），
//     以 %v 保留原因文案（含 wrapDockerTimeout 的「Docker 未在 … 内响应…」）。
//
// 既有前置语义（exists=false ⇒ ErrVMNotFound、非 running ⇒ ErrContainerNotRunning）在本
// 函数之前判定，不受影响。
func normalizeDockerErr(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errDockerNotFound) {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %v", orchestrator.ErrContainerUnavailable, err)
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

// exitOutcome exited 容器退出性质。
type exitOutcome int

const (
	// exitStopped 正常退出（码 0）或操作者主动停止（137/143，docker stop 的 SIGKILL/SIGTERM）：
	// 属于「已停止」，不告警。
	exitStopped exitOutcome = iota
	// exitOOM 因内存超限被终止（Docker State.OOMKilled）→ 异常，critical。
	exitOOM
	// exitAbnormal 其它非零退出码 → 异常，critical。
	exitAbnormal
)

// classifyExit 判定 exited 容器的退出性质（纯函数，单测覆盖）。
// 判据以 OOMKilled 为核心：docker stop 的正常结果就是 137（超时 SIGKILL）/143（SIGTERM），
// 若只看 `code != 0` 会把操作者的主动停止报成 critical「异常退出」（round86 缺陷 2）。
func classifyExit(code int, oomKilled bool) exitOutcome {
	if oomKilled {
		return exitOOM
	}
	if code == 0 || code == 137 || code == 143 {
		return exitStopped
	}
	return exitAbnormal
}

// dockerRestarting Docker 原始状态「正在重启」（契约映射把 restarting 并入 running，规格 #44）。
const dockerRestarting = "restarting"

// containerRestartLoopThreshold 崩溃重启循环的**次数**门槛（决策 #432）：Docker inspect 顶层的
// `RestartCount` 是容器创建以来的**累计**重启次数，`restarting` 且累计 ≥3 即报——偶发一次自动
// 重启（次数 1~2 且随后 running）不报，避免把正常的一次自动重启报成故障。**不以「两轮之间有
// 增长」为条件**（Docker 指数退避使成熟循环 40~60s 才涨一次，15s 巡检看不见增长——真机实测）。
const containerRestartLoopThreshold = 3

// CheckContainerAlarms 检测容器异常退出与崩溃重启循环并维护告警（FR-CMP-022）：
//   - dead → critical `CONTAINER_EXITED`（异常，口径不变）；
//   - exited 时按 OOMKilled / 退出码判定（见 classifyExit）：OOM 或其它非零码 → critical；
//     正常退出与主动停止（137/143）→ 消警（已停止，不是故障）；
//   - running → 消警；
//   - `restarting`（崩溃重启循环，决策 #432）→ warning `CONTAINER_RESTART_LOOP`：重启次数**较
//     上次巡检有增长**且**累计 ≥ containerRestartLoopThreshold** 才报（状态读数仍按既有口径显示
//     running，故这是操作者唯一能看见「容器反复起不来」的入口）；仍在循环中则保持告警不重复升级，
//     转为非 restarting 即消警。
//
// 对账清警（round86 缺陷 1）：状态查询全部成功时，还会把 scope 内**源已不在配置期望集合**
// （容器已从配置删除）的活动告警 Resolve 掉；任一查询失败则不清警，避免运行态未知时误清。
// 退出原因查询失败时该容器保持既有告警状态（既不清也不重复报）。
func (p *Provider) CheckContainerAlarms(ctx context.Context, cfg model.Config) []error {
	expect := make(map[string]bool, len(cfg.ContainerFunctions))
	var errs []error
	for _, ct := range cfg.ContainerFunctions {
		expect[ct.Name] = true
		// 决策 #432：一次 inspect 同时给出契约状态与重启次数（巡检与读数共用同一条事实）。
		facts, exists, err := p.api.Inspect(ctx, ct.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("容器 %s 状态查询: %w", ct.Name, err))
			continue
		}
		if !exists || p.alarms == nil {
			continue
		}
		if facts.State == orchestrator.CTStateDead {
			p.alarms.Raise(orchestrator.RecoveryScopeContainer, orchestrator.SeverityCritical, orchestrator.ContainerExited,
				fmt.Sprintf("容器 %s 异常退出（状态 %s）", ct.Name, facts.State), ct.Name)
			continue
		}
		if facts.State == orchestrator.CTStateExited {
			oom, _, oerr := p.api.OOMKilled(ctx, ct.Name)
			if oerr != nil {
				errs = append(errs, fmt.Errorf("容器 %s 退出原因查询: %w", ct.Name, oerr))
				continue
			}
			code, _, cerr := p.api.ExitCode(ctx, ct.Name)
			if cerr != nil {
				errs = append(errs, fmt.Errorf("容器 %s 退出码查询: %w", ct.Name, cerr))
				continue
			}
			switch classifyExit(code, oom) {
			case exitOOM:
				p.alarms.Raise(orchestrator.RecoveryScopeContainer, orchestrator.SeverityCritical, orchestrator.ContainerExited,
					fmt.Sprintf("容器 %s 因内存超限被终止（OOMKilled，退出码 %d）", ct.Name, code), ct.Name)
				continue
			case exitAbnormal:
				p.alarms.Raise(orchestrator.RecoveryScopeContainer, orchestrator.SeverityCritical, orchestrator.ContainerExited,
					fmt.Sprintf("容器 %s 异常退出（状态 %s，退出码 %d）", ct.Name, facts.State, code), ct.Name)
				continue
			}
			// 正常退出/主动停止：落到下面的 Resolve（已停止，不是故障）
		}
		p.alarms.Resolve(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, ct.Name)

		// 崩溃重启循环（决策 #432）：只看 Docker 原始状态——契约状态把 restarting 并入
		// running（规格 #44），用契约状态判不出来。大小写不敏感（与 dockerStateToContract 同口径）。
		//
		// 判据＝「原始状态 restarting」＋「累计重启次数 ≥ 门槛」。**不要求两次巡检间有增长**：
		// 真机实测（nfvis-vm，Docker 29.1.3）崩溃循环里 Docker 做**指数退避**，成熟循环约
		// 40~60s 才重启一次，而巡检 15s 一轮——「连续两轮都在涨」几乎不可达，首版按它判会让告警
		// 永不触发（真机：restart-count 已 15、告警仍空）。`restarting` 本身即「没在服务、Docker
		// 正在反复拉起」，故它＋累计次数足够；「重启过几次但此刻 running」不会被误报。
		if strings.EqualFold(facts.RawState, dockerRestarting) {
			prev, seen := p.restartSeenOf(ct.Name)
			if facts.RestartsKnown {
				p.setRestartSeen(ct.Name, facts.RestartCount)
			}
			if facts.RestartsKnown && facts.RestartCount >= containerRestartLoopThreshold {
				delta := ""
				if seen && facts.RestartCount > prev {
					delta = fmt.Sprintf("，较上次巡检增加 %d 次", facts.RestartCount-prev)
				}
				p.alarms.Raise(orchestrator.RecoveryScopeContainer, orchestrator.SeverityWarning, orchestrator.ContainerRestartLoop,
					fmt.Sprintf("容器 %s 反复重启（状态 restarting，累计重启 %d 次%s）；"+
						"排查入口：request container-functions %s log、宿主 docker inspect %s",
						ct.Name, facts.RestartCount, delta, ct.Name, ct.Name), ct.Name)
			}
		} else {
			p.alarms.Resolve(orchestrator.RecoveryScopeContainer, orchestrator.ContainerRestartLoop, ct.Name)
		}
	}
	if p.alarms != nil && len(errs) == 0 {
		orchestrator.ResolveStale(p.alarms, orchestrator.RecoveryScopeContainer, expect)
	}
	p.pruneRestartSeen(expect)
	return errs
}

// restartSeenOf 上一次巡检观察到的重启次数（无记录时 seen=false）。
func (p *Provider) restartSeenOf(name string) (count int, seen bool) {
	p.restartMu.Lock()
	defer p.restartMu.Unlock()
	count, seen = p.restartSeen[name]
	return count, seen
}

// setRestartSeen 记下本次观察到的重启次数。计数**回退**（容器被删除重建，Docker 计数从 0 重新
// 起算）时按新值重新起算——否则同名新容器永远追不上旧计数，永远不报循环。
func (p *Provider) setRestartSeen(name string, count int) {
	p.restartMu.Lock()
	defer p.restartMu.Unlock()
	if p.restartSeen == nil {
		p.restartSeen = map[string]int{}
	}
	p.restartSeen[name] = count
}

// pruneRestartSeen 丢弃不再声明的容器的计数记忆（配置里已删/改名），使记忆有界。
func (p *Provider) pruneRestartSeen(expect map[string]bool) {
	p.restartMu.Lock()
	defer p.restartMu.Unlock()
	for name := range p.restartSeen {
		if !expect[name] {
			delete(p.restartSeen, name)
		}
	}
}

// LoadImage 载入容器镜像归档（供镜像仓库导入容器镜像时调用）。
func (p *Provider) LoadImage(ctx context.Context, path, name string) error {
	return p.api.LoadImage(ctx, path, name)
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
