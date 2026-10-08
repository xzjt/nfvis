package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	// diagDefaultPingCount 未指定 count 时的发包数（既有语义）。
	diagDefaultPingCount = 5
	// diagCountMax count 的上界：超出直接报可读错误，**不静默截断**（用户要的就是这个数）。
	diagCountMax = 100
)

// Diag 内核数据面的诊断命令实现（与 api.DiagRuntime 同形）。
//
// 与 VPP 实现的差别：不用数据面自身的 ping（VPP 的 ping 走 FIB），而是用宿主网络栈的
// ping/traceroute；目标在某个 VRF 内时经 `ip vrf exec <vrf>` 进入该表发起（等价于在域内发起）。
type Diag struct{ run Runner }

// NewDiag 构造内核诊断实现。
func NewDiag(run Runner) *Diag {
	if run == nil {
		run = NewExecRunner()
	}
	return &Diag{run: run}
}

// Ping 执行 ping；未通（命令非零退出）时如实返回错误，输出原样带回供排查。
//
// 有界：内部 30s 上界（父 ctx 更紧时以父为准）；count 缺省 5、上界 diagCountMax（越界报错不截断）。
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
	if count > diagCountMax {
		return "", fmt.Errorf("ping count %d 超出上限 %d（不静默截断）：请改为不超过 %d 的值",
			count, diagCountMax, diagCountMax)
	}
	if count < 0 {
		return "", fmt.Errorf("ping count %d 不合法：须为 1-%d（0 = 未指定，用默认 %d）",
			count, diagCountMax, diagDefaultPingCount)
	}
	if count == 0 {
		count = diagDefaultPingCount
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
func (d *Diag) runVrf(ctx context.Context, vrf string, args ...string) (string, error) {
	if vrf == "" {
		return d.run.Run(ctx, args[0], args[1:]...)
	}
	full := append([]string{"vrf", "exec", LinkName(vrf), args[0]}, args[1:]...)
	return d.run.Run(ctx, "ip", full...)
}
