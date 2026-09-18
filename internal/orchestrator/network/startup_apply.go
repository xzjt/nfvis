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
}

func (a *Applier) warnf(format string, args ...any) {
	if a.Warn != nil {
		a.Warn(fmt.Sprintf(format, args...))
	}
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
	a.warnDroppedPorts(cfg)
	conf, err := GenerateStartup(cfg, a.PCI)
	if err != nil {
		return "", err
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
	return conf, nil
}

// warnDroppedPorts 在重生成前提示「已交 DPDK 但本次未声明」的口会掉出数据面。
//
// startup.conf 的 dpdk dev 段只由 `vpp.dpdk.per-dev` 生成，故未声明的口不会出现在新文件里，
// 数据面重启后即从 VPP 消失——真机上「掉口」正是这么发生的（手写播种被重生成覆盖）。
// 这是配置的应有之义（声明式），但操作者必须被告知，否则是静默的功能损失。
func (a *Applier) warnDroppedPorts(cfg *model.Config) {
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
	if len(dropped) == 0 {
		return
	}
	sort.Strings(dropped)
	a.warnf("以下已由 DPDK 接管的物理口未在配置中声明，重启数据面后将不再出现在数据面："+
		"%s；如需保留，请先 set vpp dpdk dev <口> 并提交", strings.Join(dropped, "、"))
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
