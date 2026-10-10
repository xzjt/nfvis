package api

// M5-9 收尾：`request interfaces <ifname> enable|disable`（契约 §1.2 → 等价接口配置 PUT）
// 与 `request sriov create-vfs|delete-vfs`（FR-NET-004）的守护进程侧实现。
//
// enable/disable 经 candidate+commit 一步事务落地（契约把该命令映射为 PUT /interfaces/{n}）；
// SR-IOV 经 SRIOVSetter（sysfs sriov_numvfs 写入，PF 不支持时由底座明确报错，vmxnet3 即此类）。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// NetReconcileRuntime 数据面「按已提交声明整段重放」的收敛入口（决策 #449 扩展）。
//
// 与启动/重连时的恢复收敛是**同一条路径**（orchestrator.NetworkProvider.EnsureConsistent，
// 两个数据面同签名、各自幂等），故直接复用它而不新造第二套重放。装配处按数据面注入：
// 内核数据面注入（按 committed 全量重放）；VPP 数据面不注入（nil ＝ 不触发，理由见装配处）。
type NetReconcileRuntime interface {
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// ifaceReconcileTimeout 运维动作触发的整段收敛上界（与启动恢复收敛的 30s 上界同档）：
// 收敛是同步补齐，超时即带着已收集的未收敛项返回，由调用方如实上报（不谎报成功）。
const ifaceReconcileTimeout = 30 * time.Second

// reconcileNote 按**已提交声明**触发一次数据面整段收敛，返回追加到命令输出的说明
// （能力未接入 ＝ 空串，输出保持原样）。
//
// 为什么必须有这一步（真机缺口 A，决策 #449 扩展）：commit 只下发**变更**——`request
// interfaces <n> enable` 在稳态（模型 enabled 本就是 true）是空修订，apply 段根本不跑，
// 「命令成功、什么也没发生」；`unbind-dpdk` 交还内核后设备才出现，而引用该口的交换机段 /
// VRF 段重放早在它出现之前跑完了。整段重放覆盖引用该口的全部声明（L2 交换机段：成员 + VLAN；
// L3 交换机 VRF 段：master + 地址 + 域兜底路由），比逐口补一句 master 更全——逐口兜底
// （ApplyInterface 的域归属收敛）保留不动，二者叠加幂等、不冲突。
//
// 口径：**不改写已成功的提交结论**——收敛未完成时如实追加原因（让操作者看清「提交成功但数据面
// 未完全收敛」），读不到已提交配置同样如实说明。
func (x *cliExecutor) reconcileNote() string {
	if x.netReconcile == nil {
		return ""
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return fmt.Sprintf("；接口状态收敛未执行（读取已提交配置失败：%v）", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ifaceReconcileTimeout)
	defer cancel()
	errs := x.netReconcile.EnsureConsistent(ctx, cfg)
	if len(errs) == 0 {
		return "；接口状态已按声明收敛"
	}
	msgs := make([]string, 0, 4)
	for i, e := range errs {
		if i == 3 {
			msgs = append(msgs, fmt.Sprintf("…（共 %d 项）", len(errs)))
			break
		}
		msgs = append(msgs, e.Error())
	}
	return "；接口状态收敛未完全成功：" + strings.Join(msgs, "；")
}

// requestInterfaces：request interfaces <ifname> enable|disable | bind-dpdk | unbind-dpdk。
func (x *cliExecutor) requestInterfaces(user, source string, t []string) string {
	if len(t) >= 2 && (t[1] == "bind-dpdk" || t[1] == "unbind-dpdk") {
		return x.requestInterfacesDPDK(user, source, t)
	}
	if len(t) < 2 || (t[1] != "enable" && t[1] != "disable") {
		return "%% 语法: request interfaces <ifname> enable|disable | bind-dpdk [uio-driver <d>] | unbind-dpdk\n"
	}
	ifname, action := t[0], t[1]
	enable := action == "enable"
	summary, err := x.commitMutate(user, source, func(c *model.Config) error {
		for i := range c.Interfaces {
			if c.Interfaces[i].Name != ifname {
				continue
			}
			v := enable
			c.Interfaces[i].Enabled = &v
			return nil
		}
		return fmt.Errorf("接口 %s 未在配置中声明（先 set interfaces %s …）", ifname, ifname)
	})
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	// 决策 #449 扩展：「request = 确保声明状态」——提交只下发**变更**，值未变化（稳态）时是空
	// 修订、apply 段根本不跑。提交成功后一律再触发一次整段收敛（未接入＝空串，输出保持不变）。
	return fmt.Sprintf("接口 %s 已置为 %s；%s%s\n", ifname, action,
		strings.TrimSuffix(summary, "\n"), x.reconcileNote())
}

// requestSRIOV：request sriov create-vfs <ifname> count <n> | delete-vfs <ifname> vf <n>。
func (x *cliExecutor) requestSRIOV(user, source string, t []string) string {
	if x.sriov == nil {
		return "%% SR-IOV 不可用（编排器未装配）\n"
	}
	if len(t) < 2 {
		return "%% 语法: request sriov create-vfs <ifname> count <n> | delete-vfs <ifname> vf <n>\n"
	}
	ifname := t[1]
	switch t[0] {
	case "create-vfs":
		if len(t) < 4 || t[2] != "count" {
			return "%% 语法: request sriov create-vfs <ifname> count <n>\n"
		}
		n, err := strconv.Atoi(t[3])
		if err != nil || n <= 0 {
			return "%% count 必须为正整数\n"
		}
		if err := x.sriov.SetVFCount(context.Background(), ifname, n); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return fmt.Sprintf("接口 %s SR-IOV VF 数量已置为 %d\n", ifname, n)
	case "delete-vfs":
		if len(t) < 4 || t[2] != "vf" {
			return "%% 语法: request sriov delete-vfs <ifname> vf <n>\n"
		}
		vfNo, err := strconv.Atoi(t[3])
		if err != nil {
			return "%% vf 编号必须为整数\n"
		}
		// VF 回收语义（附录 A #94）：V1 的 VF 是**数量型**配置，本条按数量回收一个；
		// 「删除指定编号」需 PF 侧逐 VF 操作，本版未实现。此处**不假装用了 vf <n>**——
		// 语法上仍要求它（契约如此），但结果里明确说明编号不参与定位，避免操作者以为
		// 删的是自己指定的那一个（此前只回「数量 %d → %d」，看不出编号被忽略）。
		cur := 0
		if cfg, err := x.engine.Committed(); err == nil {
			for _, ifc := range cfg.Interfaces {
				if ifc.Name == ifname && ifc.Sriov != nil {
					cur = ifc.Sriov.VFCount
				}
			}
		}
		if cur == 0 {
			return fmt.Sprintf("%% 接口 %s 当前未配置 VF（无可回收）\n", ifname)
		}
		next := cur - 1
		if err := x.sriov.SetVFCount(context.Background(), ifname, next); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return fmt.Sprintf("接口 %s SR-IOV VF 数量 %d → %d\n"+
			"  说明：本版按**数量**回收（回收一个）；你给的 vf %d 只用于满足语法，**不参与定位**——\n"+
			"  从指定编号逐个删除需 PF 侧逐 VF 操作，尚未支持。\n", ifname, cur, next, vfNo)
	}
	return fmt.Sprintf("%% 无效命令: request sriov %s（可用：create-vfs|delete-vfs）\n", strings.Join(t, " "))
}

// requestInterfacesDPDK：request interfaces <ifname|pci> bind-dpdk [uio-driver <d>] | unbind-dpdk [to-driver <d>]
// （FR-NET-001，决策 #72）。绑定/解绑会中断该网卡流量，故需确认。
func (x *cliExecutor) requestInterfacesDPDK(user, source string, t []string) string {
	if x.dpdk == nil {
		return "%% DPDK 接管不可用（编排器未装配）\n"
	}
	ifname, action := t[0], t[1]
	// 交互确认：REPL 在用户答 yes 后追加 --yes；脚本可直接带上（与 delete/reboot 同语义）。
	t, confirmed := splitConfirm(t)
	bound := action == "bind-dpdk"
	driver := ""
	switch {
	case bound && len(t) == 2:
		// 缺省驱动
	case bound && len(t) == 4 && t[2] == "uio-driver":
		driver = t[3]
	case bound:
		return "%% 语法: request interfaces <ifname> bind-dpdk [uio-driver <vfio-pci|igb-uio>]\n"
	case len(t) == 2:
		// 解绑：交由内核自动探测（实测常不足，见下 to-driver）
	case len(t) == 4 && t[2] == "to-driver":
		driver = t[3]
	default:
		return "%% 语法: request interfaces <ifname|pci> unbind-dpdk [to-driver <驱动名>]\n"
	}
	verb := "绑定到 DPDK 驱动"
	if !bound {
		verb = "解绑并交还内核驱动"
	}
	if ask, ok := confirmOrAsk("将接口 "+ifname+" "+verb+"（会中断该网卡流量）", "", confirmed); !ok {
		return ask
	}
	pci, cur, err := x.dpdk.SetDPDKBound(context.Background(), ifname, bound, driver)
	if err != nil {
		x.audit(user, "interfaces.dpdk", fmt.Sprintf("%s %s failure: %v", action, ifname, err), err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "interfaces.dpdk", fmt.Sprintf("%s %s pci=%s driver=%s", action, ifname, pci, cur), nil)
	if cur == "" {
		cur = "(无驱动)"
	}
	out := fmt.Sprintf("接口 %s（PCI %s）已%s，当前驱动 %s", ifname, pci, verb, cur)
	if !bound {
		// 决策 #449 扩展：交还内核后设备才出现——手册承诺「交还后按声明自动收敛」的落点
		// （既有恢复重放发生在口还握在 vfio-pci 手里的时候）。bind 方向不触发：口交 DPDK，
		// 内核数据面下本就拒绝（决策 #426②），无收敛对象可补。
		out += x.reconcileNote()
	}
	return out + "\n"
}
