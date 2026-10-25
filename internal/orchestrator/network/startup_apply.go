package network

// M3-2：startup.conf 落地与 pending_restart 状态（FR-SYS-008/009）。
// 写文件与重启数据面经注入的函数/接口隔离，单测用假实现；真机由 nfvisd 装配。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// DefaultStartupPath VPP startup.conf 路径。
const DefaultStartupPath = "/etc/vpp/startup.conf"

// Restarter 重启 VPP 数据面（真实实现见 systemctlRestarter）。
type Restarter interface {
	Restart(ctx context.Context) error
}

// Applier 生成并落地 startup.conf，必要时重启 VPP（FR-SYS-008/009）。
type Applier struct {
	Path           string
	Mgr            *Manager
	PCI            PCIResolver
	Write          func(path string, data []byte) error // 缺省 os.WriteFile(0644)
	Restarter      Restarter
	RestartOnApply bool // true = 落地后立即重启（request vpp restart）
	// Bindings DPDK 绑定记录（决策 #100）：用于在重生成前发现「已接管但未声明」的口。
	Bindings *Bindings
	// Warn 告警回调（掉口风险等；缺省丢弃）。
	Warn func(string)

	// lastWarnings 最近一次 Apply 产生的告警（掉口提示 + 生成期跳过条目）。
	// 保留是为了让「重启返回成功、但某个声明口没进数据面」这类事实在命令输出里可见——
	// 只落日志的话，操作者跑完 `request vpp restart` 看不到它。
	//
	// Apply 有两个并发来源（CLI/REST 的 request vpp restart、恢复收敛的后台 goroutine），
	// 故写入用锁保护，并按「本次调用自己攒、结束时一次性发布」的方式避免两次 Apply 的告警串味。
	mu           sync.Mutex
	lastWarnings []string
}

// setWarnings 发布本次 Apply 的告警集（nil 表示无告警）。
func (a *Applier) setWarnings(w []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(w) == 0 {
		a.lastWarnings = nil
		return
	}
	a.lastWarnings = append([]string(nil), w...)
}

// Warnings 最近一次成功 Apply 期间的告警（按时间顺序；无告警返回 nil）。
//
// 为什么留这个读口：`request vpp restart` 的返回值只有 (文本, error)，且它由 api 层注入，
// 装配处拿不到生成期的细节。把事实留在 Applier 上，cmd/nfvisd 的 Restart 实现就能在成功路径上
// 一并把它报给操作者（见交付说明的接线项），而不是让人去翻日志。
func (a *Applier) Warnings() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.lastWarnings) == 0 {
		return nil
	}
	return append([]string(nil), a.lastWarnings...)
}

func (a *Applier) path() string {
	if a.Path != "" {
		return a.Path
	}
	return DefaultStartupPath
}

func (a *Applier) write(path string, data []byte) error {
	if a.Write != nil {
		return a.Write(path, data)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// Apply 生成 startup.conf 并写盘；RestartOnApply 时随后重启数据面。返回生成文本。
// 重启成功后记录已应用哈希，pending_restart 随之清除（FR-SYS-009）。
func (a *Applier) Apply(ctx context.Context, cfg *model.Config) (string, error) {
	// 本次调用的告警先攒在局部，成功落地后再一次性发布到 Applier（失败时不留半份事实）。
	var runWarnings []string
	report := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		runWarnings = append(runWarnings, msg)
		if a.Warn != nil {
			a.Warn(msg)
		}
	}
	a.warnDroppedPorts(cfg, report)
	conf, warnings, err := GenerateStartupWithWarnings(cfg, a.PCI)
	if err != nil {
		return "", err
	}
	// 生成期告警（某个声明口解析不出 PCI 被跳过）必须在**写盘与重启之前**报出来：
	// 重启成功后数据面里就没有那个口，这条是唯一的信号。
	for _, w := range warnings {
		report("%s", w)
	}
	if err := a.write(a.path(), []byte(conf)); err != nil {
		return "", err
	}
	if a.RestartOnApply {
		if a.Restarter == nil {
			return "", fmt.Errorf("RestartOnApply 需要 Restarter")
		}
		if err := a.Restarter.Restart(ctx); err != nil {
			return "", fmt.Errorf("重启 VPP 失败: %w", err)
		}
	}
	if a.Mgr != nil {
		var vpp *model.VppConfig
		if cfg != nil {
			vpp = cfg.Vpp
		}
		a.Mgr.SetApplied(vpp)
	}
	a.setWarnings(runWarnings)
	return conf, nil
}

// warnDroppedPorts 在重生成前提示「已交 DPDK 但本次未声明」的口会掉出数据面。
//
// startup.conf 的 dpdk dev 段只由 `vpp.dpdk.per-dev` 生成，故未声明的口不会出现在新文件里，
// 数据面重启后即从 VPP 消失——真机上「掉口」正是这么发生的（手写播种被重生成覆盖）。
// 这是配置的应有之义（声明式），但操作者必须被告知，否则是静默的功能损失。
//
// v3.0.30：记录里**口名未知**的设备（旧格式的 BDF→BDF 遗留）单独一行如实报出，且不给
// `set vpp dpdk dev <BDF>` 这种无法执行的建议（接口名校验不允许冒号，真机实测）——
// 只给能补齐口名的路径（先交还内核让它重新有 netdev，再按 PCI 接管）。
func (a *Applier) warnDroppedPorts(cfg *model.Config, report func(string, ...any)) {
	if a.Bindings == nil {
		return
	}
	declared := map[string]bool{}
	if cfg != nil && cfg.Vpp != nil && cfg.Vpp.DPDK != nil {
		for _, d := range cfg.Vpp.DPDK.PerDev {
			declared[d.Interface] = true
		}
	}
	var dropped []string
	for ifname := range a.Bindings.All() {
		if !declared[ifname] {
			dropped = append(dropped, ifname)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		report("以下已由 DPDK 接管的物理口未在配置中声明，重启数据面后将不再出现在数据面："+
			"%s；如需保留，请先 set vpp dpdk dev <口> 并提交", strings.Join(dropped, "、"))
	}
	if unnamed := a.Bindings.UnnamedPCIs(); len(unnamed) > 0 {
		report("以下 PCI 设备已由 DPDK 接管，但绑定记录里没有口名（旧版记录或按 PCI 地址接管）："+
			"%s。它们无法按名字声明，重启数据面后不会出现；若要按口名给数据面用，先交还内核让它重新"+
			"拿到网卡名、再按 PCI 接管以补齐记录（request interfaces <BDF> unbind-dpdk --yes → "+
			"request interfaces <BDF> bind-dpdk --yes）", strings.Join(unnamed, "、"))
	}
}

// systemctlRestarter 经 systemctl 重启 VPP（M3-P0：VPP 有意不自启，由 nfvisd/手工管理）。
type systemctlRestarter struct{ Unit string }

// NewSystemctlRestarter 返回真实重启实现（Unit 缺省 vpp）。
func NewSystemctlRestarter() Restarter { return systemctlRestarter{Unit: "vpp"} }

func (r systemctlRestarter) Restart(ctx context.Context) error {
	unit := r.Unit
	if unit == "" {
		unit = "vpp"
	}
	cmd := exec.CommandContext(ctx, "systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Starter 启动 VPP 数据面（真实实现见 systemctlStarter）。
type Starter interface {
	Start(ctx context.Context) error
}

// systemctlStarter 经 systemctl 发起启动 VPP（决策 #348：VPP 有意不自启，由 nfvisd 启动时确保运行）。
// 与 systemctlRestarter 同一调用方式（同一个 systemctl 二进制的 start 动作），不另造拉起路径。
type systemctlStarter struct{ Unit string }

// NewSystemctlStarter 返回真实启动实现（Unit 缺省 vpp）。
func NewSystemctlStarter() Starter { return systemctlStarter{Unit: "vpp"} }

func (s systemctlStarter) Start(ctx context.Context) error {
	unit := s.Unit
	if unit == "" {
		unit = "vpp"
	}
	cmd := exec.CommandContext(ctx, "systemctl", systemctlStartArgs(unit)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl start --no-block %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// systemctlStartArgs 发起启动 VPP 的参数：--no-block 让 systemctl **提交启动 job 后立即返回**、
// 不等 job 完成——nfvisd 的启动序列绝不能被 VPP 拉起阻塞（决策 #348）。数据面就绪由连接管理器的
// 连接重试循环承担（VPP 起来后自然连上并进入恢复收敛），而非在启动路径上等待。
func systemctlStartArgs(unit string) []string {
	return []string{"start", "--no-block", unit}
}
