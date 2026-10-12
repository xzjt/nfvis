package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ErrStatsClearUnsupported 内核数据面不支持清零接口统计。
var ErrStatsClearUnsupported = errors.New("内核数据面不支持清零接口统计")

// 诊断命令的有界口径。
//
// 内核侧直接 exec 宿主的 ping/traceroute：不给内部上界时，一次挂住的调用会被拖到调用方
// （CLI/REST）自己的超时为止，而 `count` 不设上界时可以要求发几十万个包（诊断是长耗时
// 操作，「有界」是本仓库的硬要求）。上界数值与 VPP 侧同档（`network.pingTimeout` = 30s）。
const (
	// diagPingTimeout ping 的内部上界（与 VPP 侧 pingTimeout 同档）。
	diagPingTimeout = 30 * time.Second
	// diagTracerouteTimeout traceroute 允许更长（30 跳 × 1s 每跳超时），但同样必须有界。
	diagTracerouteTimeout = 60 * time.Second
)

// VRFResolver 把用户给出的 vrf/交换机名解析为**内核数据面实际创建的 VRF 设备名**（决策 #457）。
//
// 空名表示默认表（返回空串）。解析不出时返回**可照做**的错误（含本机已知设备清单）。
type VRFResolver func(name string) (string, error)

// Diag 内核数据面的诊断命令实现（与 api.DiagRuntime 同形）。
//
// 与 VPP 实现的差别：不用数据面自身的 ping（VPP 的 ping 走 FIB），而是用宿主网络栈的
// ping/traceroute；目标在某个 VRF 内时经 `ip vrf exec <设备名>` 进入该表发起（等价于在域内发起）。
//
// resolver（决策 #457）：CLI 传的是**交换机名/VRF 名**，而内核 VRF 设备名对网关域是
// `vr-<交换机名>`——须经解析器换成设备名，否则 `ip vrf exec <交换机名>` 找不到设备。为 nil 时
// 沿用按对象名映射的既有行为（单测/未接线场景）。
type Diag struct {
	run      Runner
	resolver VRFResolver
}

// NewDiag 构造内核诊断实现（resolver 见 Diag 注释；nil = 按对象名映射）。
func NewDiag(run Runner, resolver VRFResolver) *Diag {
	if run == nil {
		run = NewExecRunner()
	}
	return &Diag{run: run, resolver: resolver}
}

// Ping 执行 ping；未通（命令非零退出）时如实返回错误，输出原样带回供排查。
//
// 有界：内部 30s 上界（父 ctx 更紧时以父为准）；count 口径与 VPP 侧**同源**——
// network.ValidateDiagCount（未指定=默认 5、上界 network.DiagCountMax、越界报错不截断）。
func (d *Diag) Ping(ctx context.Context, host, source, vrf string, count int, ipv6 bool) (string, error) {
	if host == "" {
		return "", errors.New("缺少 ping 目标")
	}
	if err := guardDiagArg("ping 目标", host); err != nil {
		return "", err
	}
	if err := guardDiagArg("ping 源地址", source); err != nil {
		return "", err
	}
	if err := guardDiagArg("ping vrf", vrf); err != nil {
		return "", err
	}
	// 诊断 count 口径的**单一事实源**在 network 包（决策 #475）：0=未指定⇒默认 5、上界
	// network.DiagCountMax（越界报同一可读错误、不静默截断）、负数报错——与 VPP 侧逐字同源。
	count, err := network.ValidateDiagCount(count)
	if err != nil {
		return "", err
	}
	args := []string{"ping", "-c", strconv.Itoa(count)}
	if ipv6 {
		args = append(args, "-6")
	}
	if source != "" {
		args = append(args, "-I", source)
	}
	args = append(args, host)
	cctx, cancel := context.WithTimeout(ctx, diagPingTimeout)
	defer cancel()
	out, err := d.runVrf(cctx, vrf, args...)
	if err != nil {
		return out, fmt.Errorf("ping %s 未通: %w", host, err)
	}
	return out, nil
}

// Traceroute 执行 traceroute；VRF 作用域经 `ip vrf exec` 进入。
//
// 有界：内部 60s 上界（父 ctx 更紧时以父为准）。
func (d *Diag) Traceroute(ctx context.Context, host, vrf string, ipv6 bool) (string, error) {
	if host == "" {
		return "", errors.New("缺少 traceroute 目标")
	}
	if err := guardDiagArg("traceroute 目标", host); err != nil {
		return "", err
	}
	if err := guardDiagArg("traceroute vrf", vrf); err != nil {
		return "", err
	}
	args := []string{"traceroute"}
	if ipv6 {
		args = append(args, "-6")
	}
	args = append(args, host)
	cctx, cancel := context.WithTimeout(ctx, diagTracerouteTimeout)
	defer cancel()
	out, err := d.runVrf(cctx, vrf, args...)
	if err != nil {
		return out, fmt.Errorf("traceroute %s 未通: %w", host, err)
	}
	return out, nil
}

// ClearInterfaceStats 内核数据面不支持（清零内核计数需要重建接口，代价与影响面过大）。
func (d *Diag) ClearInterfaceStats(context.Context, string) error {
	return ErrStatsClearUnsupported
}

// guardDiagArg 拒绝**以 `-` 开头**的用户可控参数值（目标 / 源地址 / VRF 名）。
//
// 这些值原样进 argv 交给宿主的 ping/traceroute：`-e`、`-f` 一类会被底层工具当成**选项**
// 解析（不是命令注入，但语义被改写——如 `-f` 洪水 ping）。用 `--` 分隔并不够（下游是
// 经典 getopt 组合命令行，且位置随参数拼接漂移），故在入口直接给可读错误。
func guardDiagArg(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s %q 以 - 开头：会被底层工具当作选项解析，请给出主机名/IP/接口名", what, v)
	}
	return nil
}

// runVrf 在指定 VRF 内执行命令（vrf 为空时直接在默认表执行）。
//
// vrf 是**用户给出的交换机名/VRF 名**，与内核设备名不一定相同（L2 网关域的设备名是
// `vr-<交换机名>`）——先经 resolver 解析成设备名，再 `ip vrf exec <设备名>`（决策 #457）。
func (d *Diag) runVrf(ctx context.Context, vrf string, args ...string) (string, error) {
	if strings.TrimSpace(vrf) == "" {
		return d.run.Run(ctx, args[0], args[1:]...)
	}
	dev, err := d.resolveVRFDevice(vrf)
	if err != nil {
		return "", err
	}
	full := append([]string{"vrf", "exec", dev, args[0]}, args[1:]...)
	return d.run.Run(ctx, "ip", full...)
}

// resolveVRFDevice 把 vrf 参数解析为内核设备名（resolver 未注入时按对象名映射的既有行为）。
func (d *Diag) resolveVRFDevice(vrf string) (string, error) {
	if d.resolver == nil {
		return LinkName(vrf), nil
	}
	dev, err := d.resolver(vrf)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dev) == "" {
		return "", fmt.Errorf("VRF %q 未解析出内核设备名，无法在该 VRF 内发起诊断", vrf)
	}
	return dev, nil
}

// KernelVRFDeviceName 把用户给出的 vrf/交换机名解析为内核数据面实际创建的 VRF 设备名（决策 #457）。
//
// cfg 为当前 committed 配置；name 是 CLI/API 传入的交换机名或 VRF 名。规则与内核数据面的设备
// 派生**同源**（复用 model 的导出入口，不另抄一份命名规则）：
//   - L2 交换机且声明了网关地址 ⇒ 网关 VRF 设备（显式 gateway.vrf 优先，否则派生 `vr-<交换机名>`）；
//   - `vrfs` 条目（含 type=l3 交换机的同名条目）⇒ 该名派生的设备名；
//   - 其它（未声明网关的 L2 交换机没有专属 VRF、未知名）⇒ 报错并列出本机已知的内核 VRF 设备。
func KernelVRFDeviceName(cfg model.Config, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name != name || vs.Type != "l2" {
			continue
		}
		if vs.Gateway != nil && len(vs.Gateway.Addresses) > 0 {
			return model.KernelGatewayVRFDevice(vs.Name, vs.Gateway.Vrf), nil
		}
		// 未声明网关的 L2 交换机没有专属 VRF：落到下面的「未知设备」报错。
	}
	for _, vrf := range cfg.Vrfs {
		if vrf.Name == name {
			return model.KernelVRFDevice(vrf.Name), nil
		}
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name && vs.Type == "l3" {
			return model.KernelVRFDevice(vs.Name), nil
		}
	}
	known := knownKernelVRFDevices(cfg)
	if len(known) == 0 {
		return "", fmt.Errorf("未知的 VRF/交换机 %q；本机当前没有任何内核 VRF 设备（L2 交换机需先声明网关地址、或建立 L3 交换机/VRF）", name)
	}
	return "", fmt.Errorf("未知的 VRF/交换机 %q；本机已知的内核 VRF 设备: %s", name, strings.Join(known, "、"))
}

// knownKernelVRFDevices 本配置在内核数据面下会创建的 VRF 设备名（去重、按声明序固定，报错可复现）。
func knownKernelVRFDevices(cfg model.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(dev string) {
		if dev == "" || seen[dev] {
			return
		}
		seen[dev] = true
		out = append(out, dev)
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l2" && vs.Gateway != nil && len(vs.Gateway.Addresses) > 0 {
			add(model.KernelGatewayVRFDevice(vs.Name, vs.Gateway.Vrf))
		}
	}
	for _, vrf := range cfg.Vrfs {
		add(model.KernelVRFDevice(vrf.Name))
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" {
			add(model.KernelVRFDevice(vs.Name))
		}
	}
	return out
}
